package api

import (
	"bufio"
	"bytes"
	"context"
	"crypto/md5"    // #nosec G501 — S3 spec requires MD5 for ETags
	"crypto/sha256" // chunk integrity verification on read
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/FairForge/vaultaire/internal/crypto"
	"github.com/FairForge/vaultaire/internal/drivers"
	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/FairForge/vaultaire/internal/flags"
	"github.com/FairForge/vaultaire/internal/tenant"
	"github.com/FairForge/vaultaire/internal/usage"
	"go.uber.org/zap"
)

// chunkContainer is the container that stores deduplicated content-defined
// chunks. Dedup is global — identical unencrypted content is stored once
// across all tenants — so a chunk must have ONE address, outside every
// tenant's namespace: container `_global` AND the tenant
// engine.ChunkAddressTenant in the driver context (a fixed-bucket backend keys
// `t-<tenant in the context>/<container>/…`; with the uploader's tenant there
// a chunk was reachable by its uploader only — WP-R8-7). Every chunk blob
// call goes through chunkStore (chunk_store.go), which builds that context.
//
// Isolation is preserved at the manifest layer: a tenant can only reach a chunk
// through its own tenant_chunk_refs rows (queried by tenant_id in
// GetObjectChunks). The container itself is not addressable via the S3 API:
// a tenant's objects live in containers named `<tenant>_<bucket>`, under its
// own tenant id, never `_global` under the reserved one.
const chunkContainer = engine.ChunkContainer

// S3ToEngine adapts S3 requests to engine operations.
type S3ToEngine struct {
	engine            engine.Engine
	db                *sql.DB
	logger            *zap.Logger
	notifySvc         *NotificationDispatcher
	sseService        *crypto.SSEService
	chunkEncSvc       *crypto.ChunkEncryptionService
	gci               *crypto.GlobalContentIndex
	chunkingThreshold int64 // minimum object size for chunking (default 64 MB)

	// chunkStoreConcurrency bounds the parallel chunk-store workers of one
	// chunked PUT (default defaultChunkStoreConcurrency; env
	// CHUNK_PUT_CONCURRENCY via the Server; 1 = sequential stores).
	chunkStoreConcurrency int

	// chunkPendingTakeover: how old a pending chunk claim (WP-R8-2) must be
	// before another writer takes it over (chunkPendingTakeoverDefault).
	chunkPendingTakeover time.Duration
	// chunkPutBound bounds one chunk blob's backend PUT, failover included
	// (chunkPutBoundDefault).
	chunkPutBound time.Duration
	// chunkPuts is the server's registry of chunk PUTs in flight (RP14-1;
	// nil = a package default, for adapters built without a server).
	chunkPuts *chunkPutRegistry
	// chunkPutBudget is the process budget of chunked writes' buffers
	// (WP-R2-6; nil = the package default).
	chunkPutBudget *chunkPutBudget

	// chunkGetPrefetch bounds how many chunks a chunked GET holds at once
	// — in flight, fetched and being written (default
	// defaultChunkGetPrefetch; env CHUNK_GET_PREFETCH via the Server; 1 =
	// sequential fetches). The byte window is largeGet.chunkWindowBytes.
	chunkGetPrefetch int

	// largeGet shapes the parallel large-GET paths (s3_large_get.go):
	// chunked read-ahead window and hedging, the `parallel_get` ranged
	// reads of whole objects, the process-wide budget (Server-owned; nil =
	// unlimited in tests).
	largeGet largeGetConfig

	// smartPromoter brings Smart-demoted objects back hot on read (5.15.8
	// PR B). Nil = no promotion (tests, callers that never set it).
	smartPromoter *SmartPromoter

	// vaultParity serves a vault-floor object from its parity copy when its
	// backend fails (WP-VAULT-1) and erases the shards with the object.
	// Nil-safe.
	vaultParity *VaultParity

	// flags gates the chunked PUT path (1.13 `chunking` kill-switch +
	// per-tenant override). Nil (tests, callers that never set it) means
	// chunking stays on — the pre-flag behavior.
	flags *flags.Service

	// quota, when set, is used by HandleDelete to release the deleted
	// object's logical bytes (WP-1), inside the delete's transaction.
	quota QuotaManager
	// bill is a PUT's reservation (the Server layer reserves before the
	// body is read); the write's commit settles it in its transaction and
	// sets billSettled (WP-R10-8 Part A). A PUT that committed nothing
	// leaves billSettled false and the Server gives the reservation back.
	bill        *writeBill
	billSettled bool

	// storageClass, when storageClassResolved, is the class the Server
	// layer already resolved (and reserved quota on) for this PUT; HandlePut
	// uses it instead of resolving again so billing and placement agree.
	storageClass         string
	storageClassResolved bool
}

// errDecodedLengthMismatch signals an aws-chunked body whose decoded byte
// count differs from the declared x-amz-decoded-content-length. Billing and
// object metadata key off the declared size, so a mismatch must reject the
// request — recording the declared size for different actual bytes would
// let a client store data billed at an arbitrary self-declared size.
var errDecodedLengthMismatch = errors.New("aws-chunked decoded length does not match x-amz-decoded-content-length")

// errBadDigest signals a body whose MD5 differs from the Content-MD5 the
// client sent (S3: 400 BadDigest).
var errBadDigest = errors.New("content-md5 does not match the received body")

// putSizeDeclared reports whether the request carries a logical body size at
// all — a Content-Length, or the aws-chunked decoded length. The S3 handler
// refuses PUTs without one (411), so this is false only for internal callers
// and tests handing HandlePut an unsized reader.
func putSizeDeclared(r *http.Request) bool {
	return r.ContentLength >= 0 || (isAWSChunked(r) && r.Header.Get("x-amz-decoded-content-length") != "")
}

// parseContentMD5 returns the hex MD5 a client declared in Content-MD5 ("" when
// absent). A value that is not the base64 of 16 bytes is S3's InvalidDigest.
func parseContentMD5(r *http.Request) (string, error) {
	v := strings.TrimSpace(r.Header.Get("Content-MD5"))
	if v == "" {
		return "", nil
	}
	raw, err := base64.StdEncoding.DecodeString(v)
	if err != nil || len(raw) != md5.Size {
		return "", fmt.Errorf("invalid Content-MD5 %q", v)
	}
	return hex.EncodeToString(raw), nil
}

// NewS3ToEngine creates a new adapter
func NewS3ToEngine(e engine.Engine, db *sql.DB, logger *zap.Logger) *S3ToEngine {
	return &S3ToEngine{
		engine:                e,
		db:                    db,
		logger:                logger,
		notifySvc:             NewNotificationDispatcher(db, logger),
		chunkStoreConcurrency: defaultChunkStoreConcurrency,
		chunkPendingTakeover:  chunkPendingTakeoverDefault,
		chunkPutBound:         chunkPutBoundDefault,
		chunkGetPrefetch:      defaultChunkGetPrefetch,
		largeGet:              defaultLargeGetConfig(),
	}
}

// chunks is the adapter's chunk store: the one way a chunk blob is addressed.
func (a *S3ToEngine) chunks() *chunkStore {
	return newChunkStore(a.engine, a.db, a.logger)
}

// TranslateRequest converts S3 terminology to engine terminology
func (a *S3ToEngine) TranslateRequest(req *S3Request) engine.Operation {
	return engine.Operation{
		Type:      req.Operation,
		Container: req.Bucket,
		Artifact:  req.Object,
		Context:   context.Background(),
		Metadata:  make(map[string]interface{}),
	}
}

// awsChunkedReader decodes the aws-chunked transfer encoding used by the
// AWS SDK v2. Each chunk is preceded by a hex size line (optionally followed
// by a semicolon-delimited chunk extension such as a chunk signature), then
// the payload bytes, then CRLF. A zero-length chunk terminates the stream.
// Trailing headers (e.g. x-amz-checksum-*) are discarded.
//
// This is distinct from standard HTTP chunked transfer encoding, which Go's
// net/http server decodes automatically. aws-chunked is an application-level
// encoding that must be stripped before the payload reaches the storage
// backend.
type awsChunkedReader struct {
	r         *bufio.Reader
	chunkLeft int  // bytes remaining in the current chunk
	done      bool // true once the terminal 0-size chunk is seen
}

func newAWSChunkedReader(r io.Reader) *awsChunkedReader {
	return &awsChunkedReader{r: bufio.NewReader(r)}
}

func (a *awsChunkedReader) Read(p []byte) (int, error) {
	if a.done {
		return 0, io.EOF
	}

	// If the current chunk is exhausted, read the next chunk header. A
	// stream that is not well framed is the client's (errAWSChunkedFraming,
	// WP-R2-3): it used to end "cleanly" wherever it stopped, and a size
	// line that was not hex was a 500.
	for a.chunkLeft == 0 {
		// Read the chunk size line: "<hex-size>[;chunk-extension]\r\n"
		line, err := a.r.ReadString('\n')
		if err != nil {
			if errors.Is(err, io.EOF) {
				return 0, fmt.Errorf("%w: the stream ended before its terminal chunk: %w", errAWSChunkedFraming, io.ErrUnexpectedEOF)
			}
			return 0, err
		}
		line = strings.TrimRight(line, "\r\n")

		// Strip chunk extensions (e.g. ";chunk-signature=...")
		if idx := strings.IndexByte(line, ';'); idx >= 0 {
			line = line[:idx]
		}
		line = strings.TrimSpace(line)

		size, err := strconv.ParseInt(line, 16, 32)
		if err != nil || size < 0 {
			return 0, fmt.Errorf("%w: invalid chunk size %q", errAWSChunkedFraming, line)
		}

		if size == 0 {
			// Terminal chunk — trailing headers are ignored; signal EOF.
			a.done = true
			return 0, io.EOF
		}

		a.chunkLeft = int(size)
	}

	// Read up to chunkLeft bytes.
	if len(p) > a.chunkLeft {
		p = p[:a.chunkLeft]
	}
	n, err := a.r.Read(p)
	a.chunkLeft -= n
	if errors.Is(err, io.EOF) && a.chunkLeft > 0 {
		return n, fmt.Errorf("%w: the stream ended inside a chunk: %w", errAWSChunkedFraming, io.ErrUnexpectedEOF)
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return n, err
	}

	// A fully consumed chunk is followed by CRLF.
	if a.chunkLeft == 0 {
		var crlf [2]byte
		if _, err := io.ReadFull(a.r, crlf[:]); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return n, fmt.Errorf("%w: the stream ended after a chunk: %w", errAWSChunkedFraming, io.ErrUnexpectedEOF)
			}
			return n, err
		}
		if crlf != [2]byte{'\r', '\n'} {
			return n, fmt.Errorf("%w: a chunk is not followed by CRLF", errAWSChunkedFraming)
		}
	}

	return n, nil
}

// isAWSChunked returns true when the request body uses aws-chunked encoding.
// The AWS SDK v2 signals this via the x-amz-content-sha256 header value or
// the Content-Encoding header.
func isAWSChunked(r *http.Request) bool {
	sha := r.Header.Get("x-amz-content-sha256")
	if strings.HasPrefix(sha, "STREAMING-") {
		return true
	}
	return strings.Contains(r.Header.Get("Content-Encoding"), "aws-chunked")
}

// HandleGet processes S3 GET requests using the engine
func (a *S3ToEngine) HandleGet(w http.ResponseWriter, r *http.Request, bucket, object string) {
	t, err := tenant.FromContext(r.Context())
	if err != nil {
		a.logger.Warn("no tenant in context", zap.Error(err))
		WriteS3Error(w, ErrAccessDenied, r.URL.Path, generateRequestID())
		return
	}

	container := t.NamespaceContainer(bucket)
	artifact := object

	reqVersionID := r.URL.Query().Get("versionId")
	vStatus := getBucketVersioningStatus(r.Context(), a.db, t.ID, bucket)

	a.logger.Debug("GET with tenant isolation",
		zap.String("tenant_id", t.ID),
		zap.String("original_bucket", bucket),
		zap.String("namespaced_container", container),
		zap.String("artifact", artifact),
		zap.String("version_id", reqVersionID))

	// version: a non-current version whose bytes were kept under their own
	// address (object_versions.storage_key, WP-R2-1) — served from its row,
	// never from the head row, which describes the current object.
	var version *versionBlob
	if reqVersionID != "" && a.db != nil {
		var isDeleteMarker, isLatest bool
		var vETag, vContentType, vStorageKey, vBackend, vEncAlgo string
		var vSize int64
		var vCreatedAt time.Time
		var vAttrs objectAttrs
		var vMeta []byte
		var vBodyMD5, vKeyMD5 string
		err := a.db.QueryRowContext(r.Context(), `
			SELECT is_delete_marker, is_latest, etag, content_type, size_bytes, created_at,
			       COALESCE(storage_key, ''), COALESCE(backend_name, ''), COALESCE(encryption_algorithm, ''), `+versionAttrColumns+`,
			       COALESCE(body_md5, ''), COALESCE(ssec_key_md5, '')
			FROM object_versions
			WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3 AND version_id = $4`,
			t.ID, bucket, artifact, reqVersionID).Scan(&isDeleteMarker, &isLatest, &vETag, &vContentType, &vSize, &vCreatedAt,
			&vStorageKey, &vBackend, &vEncAlgo, &vMeta, &vAttrs.ContentDisposition, &vAttrs.ContentEncoding, &vAttrs.ContentLanguage,
			&vAttrs.CacheControl, &vAttrs.Expires, &vAttrs.WebsiteRedirect, &vBodyMD5, &vKeyMD5)
		switch {
		case errors.Is(err, sql.ErrNoRows) && isUnanchoredNullVersion(r.Context(), a.db, t.ID, bucket, artifact, reqVersionID):
			// The key has no version row: its current object is the null
			// version (written before versioning was enabled) — served as
			// the current object, as ListObjectVersions lists it.
			isLatest = true
		case errors.Is(err, sql.ErrNoRows):
			WriteS3Error(w, ErrNoSuchVersion, r.URL.Path, generateRequestID())
			return
		case err != nil:
			a.logger.Error("GET ?versionId: version query failed", zap.Error(err))
			WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
			return
		}
		if !isDeleteMarker && !isLatest && vStorageKey != "" {
			version = &versionBlob{etag: vETag, contentType: vContentType, size: vSize, createdAt: vCreatedAt,
				storageKey: vStorageKey, backend: vBackend, encAlgo: vEncAlgo, attrs: vAttrs, metadata: vMeta,
				bodyMD5: vBodyMD5, ssecKeyMD5: vKeyMD5}
		}
		intact := false
		if !isDeleteMarker && !isLatest && version == nil {
			intact = a.versionBytesIntact(r.Context(), t.ID, bucket, artifact, reqVersionID)
		}
		if intact {
			// A version written before C1 whose bytes are intact at <key>
			// (behind a delete marker: no head row) is read from its own
			// row — its backend and its encryption — never the primary at
			// a guess (Stage 1a Prompt 6 Part 0: it was read with no
			// recorded backend, and a sealed one returned as ciphertext).
			version = &versionBlob{etag: vETag, contentType: vContentType, size: vSize, createdAt: vCreatedAt,
				backend: vBackend, encAlgo: vEncAlgo, bodyMD5: vBodyMD5, ssecKeyMD5: vKeyMD5}
		}
		if !isDeleteMarker && !isLatest && version == nil {
			// R2-03: a version written before WP-R2-1's writers (C1,
			// 2026-10-12) kept no bytes — every PUT overwrote the one blob
			// at this key — so the bytes of such a non-current version no
			// longer exist. Answering 200 with the CURRENT object's bytes
			// under the requested version id (the old behaviour) made
			// restore tools restore the wrong data. Every version written
			// since keeps its own blob (above); the one older non-current
			// version whose bytes ARE intact — the newest live version
			// behind a delete marker (nothing has overwritten it) — is
			// still served, so an accidental delete stays recoverable.
			w.Header().Set("x-amz-version-id", reqVersionID)
			WriteS3ErrorWithContext(w, ErrNotImplemented, r.URL.Path, generateRequestID(),
				WithSuggestion("This version was written before 2026-10-12, when versions began keeping their own bytes; its data is not retained."))
			return
		}
		if isDeleteMarker {
			// A version id naming a delete marker: 405, as AWS (WP-R2-4).
			writeDeleteMarkerVersion(w, r, reqVersionID, vCreatedAt)
			return
		}
		w.Header().Set("x-amz-version-id", reqVersionID)
	}

	if a.db != nil && (vStatus == "Enabled" || vStatus == "Suspended") && reqVersionID == "" {
		var isDeleteMarker bool
		var latestVersionID string
		err := a.db.QueryRowContext(r.Context(), `
			SELECT version_id, is_delete_marker FROM object_versions
			WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3 AND is_latest = TRUE`,
			t.ID, bucket, artifact).Scan(&latestVersionID, &isDeleteMarker)
		if err == nil && isDeleteMarker {
			w.Header().Set("x-amz-version-id", latestVersionID)
			w.Header().Set("x-amz-delete-marker", "true")
			reqID := generateRequestID()
			if suggestion := keySuggestion(r.Context(), a.db, t.ID, bucket, artifact); suggestion != "" {
				WriteS3ErrorWithContext(w, ErrNoSuchKey, r.URL.Path, reqID, WithSuggestion(suggestion))
			} else {
				WriteS3Error(w, ErrNoSuchKey, r.URL.Path, reqID)
			}
			return
		}
		if err == nil && latestVersionID != "" {
			w.Header().Set("x-amz-version-id", latestVersionID)
		}
	}

	var cachedContentType string
	var cachedSize int64
	var cachedETag string
	var cachedUpdatedAt time.Time
	var cachedMetadata []byte
	var cachedBackendName string
	var cachedEncAlgo string
	var cachedTags []byte
	var cachedContentDisposition string
	var cachedContentEncoding string
	var cachedContentLanguage string
	var cachedEcho putEchoHeaders
	var cachedIsChunked bool
	var cachedFloor string
	var cachedStorageKey string
	var cachedBodyMD5, cachedSSECKeyMD5 string // what the row knows about its bytes (094)
	var lockMode string                        // the key's lock row (RP17-1), joined
	var lockUntil sql.NullTime
	var legalHold sql.NullBool
	var cacheHit bool
	if a.db != nil {
		err := a.db.QueryRowContext(r.Context(), `
			SELECT h.content_type, h.size_bytes, h.etag, h.updated_at, COALESCE(h.metadata, '{}'), COALESCE(h.backend_name, ''), COALESCE(h.encryption_algorithm, ''), COALESCE(h.tags, '{}'), COALESCE(h.content_disposition, ''), COALESCE(h.content_encoding, ''), COALESCE(h.content_language, ''), COALESCE(h.cache_control, ''), COALESCE(h.http_expires, ''), COALESCE(h.website_redirect_location, ''), h.is_chunked, h.floor, COALESCE(h.storage_key, ''),
			       COALESCE(h.body_md5, ''), COALESCE(h.ssec_key_md5, ''),
			       `+objectLockColumns+`
			FROM object_head_cache h
			LEFT JOIN object_locks l ON l.tenant_id = h.tenant_id AND l.bucket = h.bucket AND l.object_key = h.object_key
			WHERE h.tenant_id = $1 AND h.bucket = $2 AND h.object_key = $3`,
			t.ID, bucket, artifact).Scan(&cachedContentType, &cachedSize, &cachedETag, &cachedUpdatedAt, &cachedMetadata, &cachedBackendName, &cachedEncAlgo, &cachedTags, &cachedContentDisposition, &cachedContentEncoding, &cachedContentLanguage, &cachedEcho.CacheControl, &cachedEcho.Expires, &cachedEcho.WebsiteRedirect, &cachedIsChunked, &cachedFloor, &cachedStorageKey,
			&cachedBodyMD5, &cachedSSECKeyMD5, &lockMode, &lockUntil, &legalHold)
		switch {
		case err == nil:
			cacheHit = true
		case !errors.Is(err, sql.ErrNoRows):
			// Never a guess on the primary when the row cannot be read
			// (Stage 1a Prompt 6 Part 0); DELETE refuses the same way.
			a.logger.Error("GET: head row read failed", zap.Error(err), zap.String("bucket", bucket), zap.String("object", artifact))
			WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
			return
		}
	}
	if version != nil {
		// The version's own row describes its bytes and what a GET of it
		// sends (R61-8, 092: none of it for a version written before); the
		// head row's belong to the current object. Tags are the key's
		// (per-version tags are not kept). The floor stays the key's. A
		// chunked version is read through its own manifest (WP-VM-1).
		cacheHit = true
		cachedSize, cachedETag, cachedContentType, cachedUpdatedAt = version.size, version.etag, version.contentType, version.createdAt
		cachedBackendName, cachedEncAlgo, cachedStorageKey = version.backend, version.encAlgo, version.storageKey
		cachedBodyMD5, cachedSSECKeyMD5 = version.bodyMD5, version.ssecKeyMD5
		cachedIsChunked = version.backend == chunkedBackendSentinel
		cachedMetadata, cachedTags = version.metadata, []byte("{}")
		if len(cachedMetadata) == 0 {
			cachedMetadata = []byte("{}")
		}
		cachedContentDisposition, cachedContentEncoding, cachedContentLanguage = version.attrs.ContentDisposition, version.attrs.ContentEncoding, version.attrs.ContentLanguage
		cachedEcho = putEchoHeaders{CacheControl: version.attrs.CacheControl, Expires: version.attrs.Expires, WebsiteRedirect: version.attrs.WebsiteRedirect}
	}
	// No row, no object: the row is the one authority for where an object
	// is (WP-R6-1) — HEAD already answers from it alone — so a key with no
	// row is NoSuchKey without asking any backend (it was asked of the
	// primary at <key>, a guess; Stage 1a Prompt 6 Part 0).
	if a.db != nil && !cacheHit {
		reqID := generateRequestID()
		if suggestion := keySuggestion(r.Context(), a.db, t.ID, bucket, artifact); suggestion != "" {
			WriteS3ErrorWithContext(w, ErrNoSuchKey, r.URL.Path, reqID, WithSuggestion(suggestion))
		} else {
			WriteS3Error(w, ErrNoSuchKey, r.URL.Path, reqID)
		}
		return
	}
	// A chunked row is read through the content index alone; without one
	// this process cannot read it, and the whole-object path would read a
	// stale blob at <key> from the primary.
	if cacheHit && cachedIsChunked && a.gci == nil {
		a.logger.Error("GET: a chunked object on a process with no content index", zap.String("bucket", bucket), zap.String("object", artifact))
		WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
		return
	}
	// Where the object's bytes are (WP-R2-1): the row's own address, <key>
	// when it records none.
	_, blob := objectBlobAddress(t.ID, bucket, artifact, cachedStorageKey)

	// Smart-tier read-time promotion: a demoted object being read comes
	// back hot (routing flip inside the grace window, async copy-back after).
	if cacheHit && version == nil && a.smartPromoter != nil && cachedBackendName == a.smartPromoter.ColdBackend && !cachedIsChunked {
		cachedBackendName = a.smartPromoter.OnRead(r.Context(), t.ID, bucket, artifact, cachedETag)
	}

	// The read goes to the backend the row records (gctx below, WP-R6-1).
	if cacheHit && cachedBackendName != "" && !cachedIsChunked { // a chunked row's name is a sentinel, not a route
		noteRecordedBackend(a.engine, a.logger, "get", cachedBackendName)
	}
	// The class this response reports: the floor decides, not the backend
	// the bytes are read from (WP-R13-1) — the same value HEAD and the
	// listings give.
	storageClass := engine.CustomerStorageClass(cachedFloor, cachedBackendName)

	if cacheHit && version == nil && a.db != nil {
		go func() { // #nosec G118 -- fire-and-forget access-time touch; must outlive the request, request ctx would cancel it
			_, _ = a.db.ExecContext(context.Background(), `
				UPDATE object_head_cache SET last_accessed = NOW()
				WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3`,
				t.ID, bucket, artifact)
		}()
	}

	contentType := cachedContentType
	if contentType == "" {
		contentType = a.detectContentType(artifact)
	}
	// What every response of this object says about it — HEAD's set, one
	// writer (object_headers.go, WP-R2-4). The version id the response
	// names was set above (a requested or the latest version); a versioned
	// bucket's object with no version row is the null version; a bucket
	// never versioned names none.
	disposition := r.URL.Query().Get("response-content-disposition")
	if disposition == "" {
		disposition = cachedContentDisposition
	}
	oh := objectHeaders{ETag: cachedETag, LastModified: cachedUpdatedAt, ContentType: contentType, Size: cachedSize,
		StorageClass: storageClass, VersionID: w.Header().Get("x-amz-version-id"), Metadata: cachedMetadata,
		TagCount: tagCount(cachedTags), Disposition: disposition, Encoding: cachedContentEncoding, Language: cachedContentLanguage,
		CacheControl: cachedEcho.CacheControl, Expires: cachedEcho.Expires, WebsiteRedirect: cachedEcho.WebsiteRedirect,
		SSE: sseOf(cachedEncAlgo, crypto.IsSSEC(cachedEncAlgo))}
	oh.LockMode, oh.LockRetainUntil, oh.LegalHold = lockHeadersOf(lockMode, lockUntil, legalHold)
	if oh.VersionID == "" && (vStatus == "Enabled" || vStatus == "Suspended") {
		oh.VersionID = "null"
	}
	// An SSE-C object without the customer's key: AWS's 400, before any
	// object header (R8-14).
	if cacheHit && crypto.IsSSEC(cachedEncAlgo) && !crypto.HasSSECHeaders(r) {
		w.Header().Del("x-amz-version-id")
		WriteS3ErrorWithContext(w, ErrInvalidRequest, r.URL.Path, generateRequestID(), WithMessage(sseCRefusal))
		return
	}
	// A key of another MD5 than the row recorded (RP14-11, 094): AWS's 403,
	// from the row, before any object header and any backend call. A row
	// with no MD5 (written before 094) finds out at the blob, as before.
	if cacheHit && crypto.IsSSEC(cachedEncAlgo) && ssecKeyMismatch(r, cachedSSECKeyMD5) {
		w.Header().Del("x-amz-version-id")
		WriteS3ErrorWithContext(w, ErrAccessDenied, r.URL.Path, generateRequestID(), WithMessage(sseCKeyRefusal))
		return
	}

	if cacheHit {
		if code := evaluateConditionalGET(r, cachedETag, cachedUpdatedAt); code == http.StatusNotModified {
			w.Header().Del("x-amz-version-id")
			oh.write(w, headersNotModified, nil)
			w.WriteHeader(http.StatusNotModified)
			return
		} else if code == http.StatusPreconditionFailed {
			w.WriteHeader(http.StatusPreconditionFailed)
			return
		}
	}

	// Chunked objects are reassembled from their individual chunk storage
	// keys. A failure here must FAIL the request — never fall through to the
	// plain path: a whole-object blob can exist at this key from before the
	// object was chunked (or before an overwrite), and falling through served
	// the previous object's bytes under the current version's ETag/size. The
	// preflight resolves every chunk before any byte is written, so the
	// status is always clean: 503 + Retry-After when a chunk's backend could
	// not be asked (sync and backup clients retry a 503 and alert on a 500 —
	// Prompt 2b.5 F1.2), else 500.
	if cacheHit && cachedIsChunked && a.gci != nil {
		chunkErr := a.handleChunkedGet(w, r, t, bucket, artifact, manifestAddress(artifact, cachedStorageKey), oh)
		if chunkErr != nil {
			a.logger.Error("chunked get failed",
				zap.Error(chunkErr),
				zap.String("bucket", bucket),
				zap.String("artifact", artifact))
			if errors.Is(chunkErr, engine.ErrAllBackendsUnavailable) {
				w.Header().Set("Retry-After", "30")
				WriteS3Error(w, ErrServiceUnavailable, r.URL.Path, generateRequestID())
				return
			}
			WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
		}
		return
	}

	// A ranged read of a cached, unencrypted, whole object goes straight to
	// the backend's native range GET. The full-object Get below used to be
	// opened first and abandoned once the Range branch took over, which
	// charged every 1 MB range read the first-byte latency of a whole-object
	// GET (~200 ms against iDrive and Wasabi in the 2026-10-04 bench; raw
	// ranges on the same backends: 30–70 ms). An encrypted object still needs
	// the full ciphertext (R2-02); a multi-range request is served whole.
	var reader io.ReadCloser
	var nativeRange, wantRange *httpRange
	// The recorded size goes with the read: a backend holding two versions
	// of the key after an interrupted commit (the multi-bridge Sync
	// driver's plain file and striped manifest) serves the one of this
	// size, never the other (Prompt 2b B3).
	gctx := r.Context()
	if cacheHit {
		gctx = engine.WithExpectedSize(gctx, cachedSize)
		// The one backend the bytes are read from: the row's (WP-R6-1). A
		// chunked row's name is a sentinel; its chunks have their own rows.
		if !cachedIsChunked {
			gctx = engine.WithRecordedBackend(gctx, cachedBackendName)
		}
	}
	// A sealed object — SSE-S3 under the tenant's key, SSE-C under the
	// customer's — opens as it streams (WP-R8-5, sse_stream.go): the status
	// line follows its verified header and first segment; a Range reads the
	// header and the segments it spans. Sized by its stored length (a
	// backend holding two versions of a key picks by it).
	sealed := cacheHit && cachedEncAlgo != ""
	if sealed {
		var op blobOpener
		var ssecKey []byte
		if crypto.IsSSEC(cachedEncAlgo) {
			var parseErr error
			if ssecKey, parseErr = crypto.ParseSSECHeaders(r); parseErr != nil {
				WriteS3ErrorWithContext(w, ErrInvalidRequest, r.URL.Path, generateRequestID(),
					WithSuggestion(parseErr.Error()))
				return
			}
			op = sseCOpener{key: ssecKey, legacy: cachedEncAlgo == crypto.SSECAlgorithm,
				id: crypto.StreamIdentity{Tenant: t.ID, Bucket: bucket, Key: artifact}}
		} else {
			if a.sseService == nil {
				// The row says the blob is SSE-S3 ciphertext and this process
				// has no master key (unset, mistyped, rotated on one node).
				// Serving the raw blob as a 200 hands the client garbage cut
				// to the plaintext Content-Length (R8-04) — fail closed
				// instead, before any backend call.
				a.logger.Error("encrypted object read with no SSE service: ENCRYPTION_MASTER_KEY is not configured on this process",
					zap.String("bucket", bucket), zap.String("object", artifact), zap.String("algorithm", cachedEncAlgo))
				WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
				return
			}
			op = sseS3Opener{svc: a.sseService, id: crypto.StreamIdentity{Tenant: t.ID, Bucket: bucket, Key: artifact}}
		}
		if rh := r.Header.Get("Range"); rh != "" && !errors.Is(rangeParseErr(rh, cachedSize), errMultiRange) {
			rng, parseErr := parseRangeHeader(rh, cachedSize)
			if parseErr != nil {
				clear(ssecKey)
				writeRangeNotSatisfiable(w, cachedSize)
				return
			}
			wantRange = rng
		}
		var fromParity bool
		stored := storedBlobLength(cachedEncAlgo, cachedSize, a.sseService)
		// A large sealed object on a backend with cheap ranges is read as
		// parallel ranges aligned to its segments, each opened as it
		// arrives (`parallel_get`, s3_large_get.go; Prompt 8 Part B). Never
		// a vault-floor object: tape and its restore semantics, and the
		// parity fallback, stay on the single stream.
		if cachedFloor != usage.FloorVault {
			off, n := int64(0), cachedSize
			if wantRange != nil {
				off, n = wantRange.start, wantRange.length
			}
			reader = a.openParallelSealed(gctx, op, t.ID, container, blob, cachedSize, stored, off, n)
		}
		if reader == nil {
			reader, fromParity, err = a.openSealedGet(r.Context(), op, t.ID, bucket, artifact, container, blob, cachedETag,
				cachedSize, stored, cachedFloor, cachedBackendName, wantRange)
		}
		// Every open has derived its key by now (the DEK of a streaming
		// blob, the old format's whole read): the customer's key goes.
		clear(ssecKey)
		switch {
		case err == nil:
			nativeRange = wantRange
			if fromParity {
				w.Header().Set("x-vaultaire-served-from", "parity")
			}
		case errors.Is(err, crypto.ErrSSECKeyMismatch):
			// Decided at the first segment, before the status line.
			WriteS3ErrorWithContext(w, ErrAccessDenied, r.URL.Path, generateRequestID(),
				WithSuggestion("The provided encryption key does not match."))
			return
		case errors.Is(err, errSealedUnreadable):
			a.logger.Error("encrypted object does not open", zap.Error(err),
				zap.String("bucket", bucket), zap.String("object", artifact), zap.String("algorithm", cachedEncAlgo))
			WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
			return
		}
	}
	parallelRead := false // the bytes come from the parallel ranged reader
	if rh := r.Header.Get("Range"); rh != "" && cacheHit && cachedEncAlgo == "" &&
		!errors.Is(rangeParseErr(rh, cachedSize), errMultiRange) {
		rng, parseErr := parseRangeHeader(rh, cachedSize)
		if parseErr != nil {
			// Unsatisfiable against the cached size: 416 before any backend call.
			writeRangeNotSatisfiable(w, cachedSize)
			return
		}
		wantRange = rng
		// A large range of a whole object on a backend with cheap ranges
		// is read as parallel ranges within it (`parallel_get`, s3_large_get.go).
		if cachedFloor != usage.FloorVault {
			if pr := a.openParallelGet(gctx, t.ID, container, blob, cachedSize, rng.start, rng.length); pr != nil {
				reader, nativeRange, parallelRead = pr, rng, true
			}
		}
		if reader == nil {
			if ce, ok := a.engine.(*engine.CoreEngine); ok {
				rr, rangeErr := ce.GetRange(gctx, container, blob, rng.start, rng.length)
				switch {
				case rangeErr == nil:
					reader, nativeRange = rr, rng
				case errors.Is(rangeErr, engine.ErrAllBackendsUnavailable) && engine.IsStrictTarget(cachedBackendName):
					// A strict backend (sync) that could not be asked: the
					// whole-object GET below would ask it alone again and
					// charge its breaker a second time for one request (3
					// ranged reads opened it instead of 5 — Prompt 2b.5
					// F3.7). Elsewhere the fallback stays: a vendor whose
					// range path fails may still serve the whole object.
					err = rangeErr
				}
			}
		}
	}
	// A whole large object, same rule. Never an encrypted object (the
	// ciphertext is decrypted whole), never a vault-floor one (tape and
	// restore semantics stay on the single stream).
	if reader == nil && cacheHit && cachedEncAlgo == "" && cachedFloor != usage.FloorVault && r.Header.Get("Range") == "" {
		reader = a.openParallelGet(gctx, t.ID, container, blob, cachedSize, 0, cachedSize)
		parallelRead = reader != nil
	}
	if reader == nil && err == nil && !sealed {
		reader, err = a.engine.Get(gctx, container, blob)
	}
	// A vault object whose backend failed — an error, not found through the
	// failover chain, an open breaker — is rebuilt from its parity copy when
	// there is one (WP-VAULT-1). On tape (ErrArchived) is the object's state,
	// not a failure: the restore semantics stay.
	if err != nil && !sealed && cacheHit && cachedFloor == usage.FloorVault && a.vaultParity != nil && !errors.Is(err, engine.ErrArchived) {
		off, n := int64(0), cachedSize
		if wantRange != nil {
			off, n = wantRange.start, wantRange.length
		}
		// The parity reads its shards from the legs on this context (never
		// an expected size: a shard is not the object) and the data ranges
		// from the object's own backend.
		pr, perr := a.vaultParity.Open(engine.WithRecordedBackend(r.Context(), cachedBackendName), t.ID, bucket, artifact, blob, cachedETag, cachedSize, off, n)
		if perr == nil {
			a.logger.Warn("vault object served from its parity copy", zap.String("bucket", bucket), zap.String("key", artifact),
				zap.NamedError("backend", err))
			vaultParityFallbacks.WithLabelValues("served").Inc()
			reader, err = pr, nil
			nativeRange = wantRange
			w.Header().Set("x-vaultaire-served-from", "parity")
		} else {
			a.logger.Error("vault object: its backend failed and the parity copy cannot serve it",
				zap.String("bucket", bucket), zap.String("key", artifact), zap.NamedError("backend", err), zap.NamedError("parity", perr))
			vaultParityFallbacks.WithLabelValues("unavailable").Inc()
		}
	}
	if err != nil {
		if errors.Is(err, engine.ErrAllBackendsUnavailable) {
			w.Header().Set("Retry-After", "30")
			WriteS3Error(w, ErrServiceUnavailable, r.URL.Path, generateRequestID())
		} else if errors.Is(err, engine.ErrArchived) {
			// The bytes are on tape. A downstairs object is restored on the
			// reader's behalf and the answer is a retryable 503 (Smart
			// customers never issue restores themselves); an attic object
			// answers Glacier's 403 InvalidObjectState.
			writeArchivedRead(w, r, a.smartPromoter, t.ID, bucket, artifact, blob, cachedFloor,
				"This object is archived on tape. Request a restore (POST ?restore or the dashboard Restore button), then retry — restores typically begin within minutes.")
		} else if isObjectMissingErr(err) {
			reqID := generateRequestID()
			if suggestion := keySuggestion(r.Context(), a.db, t.ID, bucket, artifact); suggestion != "" {
				WriteS3ErrorWithContext(w, ErrNoSuchKey, r.URL.Path, reqID, WithSuggestion(suggestion))
			} else {
				WriteS3Error(w, ErrNoSuchKey, r.URL.Path, reqID)
			}
		} else {
			a.logger.Error("engine get failed",
				zap.Error(err),
				zap.String("container", container),
				zap.String("artifact", artifact))
			WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
		}
		return
	}
	defer func() { _ = reader.Close() }()

	var dataReader io.Reader = reader
	w.Header().Set("x-amz-request-id", generateRequestID())

	// Which path served the bytes, for the integrity counters (WP-R8-3).
	integrityPath := integrityPathPlain
	switch {
	case w.Header().Get("x-vaultaire-served-from") == "parity":
		integrityPath = integrityPathParity
	case sealed:
		integrityPath = integrityPathSealed
	case parallelRead:
		integrityPath = integrityPathParallel
	}

	rangeHeader := r.Header.Get("Range")
	if rangeHeader != "" && cacheHit && errors.Is(rangeParseErr(rangeHeader, cachedSize), errMultiRange) {
		rangeHeader = "" // RFC 9110 §14.2: a multi-range request may be served whole
	}
	if rangeHeader != "" && cacheHit {
		rng, parseErr := parseRangeHeader(rangeHeader, cachedSize)
		if parseErr != nil {
			writeRangeNotSatisfiable(w, cachedSize)
			return
		}
		// A range does not hash to the ETag: served size-checked only.
		objectIntegrityUnverified.WithLabelValues(integrityPath, "range").Inc()

		// Use backend-native range GET when available (avoids downloading the
		// full object and discarding prefix bytes — 10-50× faster for large files).
		// Encrypted objects are sliced from the decrypted bytes.Reader below:
		// the backend-native range reads the stored CIPHERTEXT (R2-02 — a
		// ranged download of an SSE object returned ciphertext).
		rangeReader := io.Reader(dataReader)
		var rr io.ReadCloser
		var rangeErr error
		if nativeRange != nil {
			rr = reader // opened above, already positioned at rng.start
		} else if ce, ok := a.engine.(*engine.CoreEngine); ok && cachedEncAlgo == "" {
			rr, rangeErr = ce.GetRange(gctx, container, blob, rng.start, rng.length)
			if rangeErr == nil {
				defer func() { _ = rr.Close() }()
			}
		}
		if rr != nil && rangeErr == nil {
			{
				rangeReader = rr
				// rangeReader already positioned at rng.start — write headers and copy directly
				oh.write(w, headersRange, rng)
				w.WriteHeader(http.StatusPartialContent)
				if _, err := io.CopyN(w, rangeReader, rng.length); err != nil {
					a.logger.Error("range stream failed",
						zap.Error(err),
						zap.String("container", container),
						zap.String("artifact", artifact))
					cutUnlessClientGone(r.Context())
				}
				return
			}
		}

		// Fallback: serveRange with full-object reader (old path)
		oh.write(w, headersRange, rng)
		if err := serveRange(w, rangeReader, rng, cachedSize, contentType); err != nil {
			a.logger.Error("range serve failed",
				zap.Error(err),
				zap.String("container", container),
				zap.String("artifact", artifact))
			cutUnlessClientGone(r.Context())
		}
		return
	}

	oh.write(w, headersWhole, nil)

	// The whole body is checked against the row's single-part ETag as it
	// streams; its last byte waits for the match (object_integrity.go).
	if cacheHit && cachedSize > 0 {
		var armed bool
		digest := integrityDigest(cachedBodyMD5, cachedETag) // a -N object's whole digest (A.4, 094)
		if dataReader, armed = newIntegrityReader(dataReader, digest, cachedSize); !armed {
			objectIntegrityUnverified.WithLabelValues(integrityPath, unverifiedReason(digest)).Inc()
		}
	}

	written, err := io.Copy(w, dataReader)
	var ie *integrityError
	if errors.As(err, &ie) && r.Context().Err() == nil {
		objectIntegrityFailures.WithLabelValues(integrityPath, ie.reason).Inc()
		a.logger.Error("object integrity: the body served does not match its row — the response is cut",
			zap.String("tenant_id", t.ID), zap.String("bucket", bucket), zap.String("key", artifact),
			zap.String("backend", cachedBackendName), zap.String("storage_key", cachedStorageKey),
			zap.String("etag", cachedETag), zap.String("expected_md5", ie.recorded), zap.String("measured_md5", ie.measured),
			zap.String("reason", ie.reason), zap.String("path", integrityPath), zap.Int64("bytes_sent", written))
		abortResponse(r.Context())
		return
	}
	if err != nil {
		a.logger.Error("failed to stream artifact",
			zap.Error(err),
			zap.String("container", container),
			zap.String("artifact", artifact))
		// A source that failed mid-body (a backend read, the parallel
		// ranged reader) cuts the connection: the status line is out and a
		// client without a Content-Length to count against would take a
		// clean end for the object (Stage 1a Part A, same bug as chunked).
		cutUnlessClientGone(r.Context())
		return
	}

	a.logger.Info("artifact retrieved",
		zap.String("s3.bucket", bucket),
		zap.String("s3.object", object),
		zap.String("engine.container", container),
		zap.String("engine.artifact", artifact),
		zap.Int64("bytes", written))

	emitEvent(r.Context(), a.db, a.logger, "object.downloaded", t.ID, map[string]interface{}{
		"bucket": bucket, "key": object, "size": written,
	})
}

// isUnanchoredNullVersion reports whether versionID is "null" and the key has
// no version row at all: its current object (if any) was written before
// versioning was enabled and IS the null version (AWS; ListObjectVersions
// lists it so). Once a write or a marker anchors it (anchorNullVersion) it
// has its own row. A failed lookup answers false: the caller then treats
// the id as a version id it does not know.
func isUnanchoredNullVersion(ctx context.Context, db *sql.DB, tenantID, bucket, key, versionID string) bool {
	if versionID != "null" || db == nil {
		return false
	}
	var any bool
	if err := db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM object_versions
		WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3)`, tenantID, bucket, key).Scan(&any); err != nil {
		return false
	}
	return !any
}

// versionBytesIntact reports whether a non-current version's bytes are still
// the ones on the backend: true only when the current version is a delete
// marker and the requested version is the newest non-marker version (no PUT
// has overwritten the blob since). Everything else has been overwritten.
func (a *S3ToEngine) versionBytesIntact(ctx context.Context, tenantID, bucket, key, versionID string) bool {
	var latestIsMarker bool
	if err := a.db.QueryRowContext(ctx, `
		SELECT is_delete_marker FROM object_versions
		WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3 AND is_latest = TRUE`,
		tenantID, bucket, key).Scan(&latestIsMarker); err != nil || !latestIsMarker {
		return false
	}
	var newestLive string
	if err := a.db.QueryRowContext(ctx, `
		SELECT version_id FROM object_versions
		WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3 AND is_delete_marker = FALSE
		ORDER BY created_at DESC LIMIT 1`,
		tenantID, bucket, key).Scan(&newestLive); err != nil {
		return false
	}
	return newestLive == versionID
}

// versionBlob is a non-current version whose bytes were kept (WP-R2-1):
// everything a GET of it needs, from its object_versions row.
type versionBlob struct {
	etag, contentType   string
	size                int64
	createdAt           time.Time
	storageKey, backend string
	encAlgo             string
	attrs               objectAttrs // what the version row recorded (R61-8)
	metadata            []byte      // its x-amz-meta-* as JSON
	bodyMD5, ssecKeyMD5 string      // what the row knows about its bytes (094)
}

// detectContentType determines MIME type from extension
func (a *S3ToEngine) detectContentType(artifact string) string {
	dotIdx := strings.LastIndex(artifact, ".")
	if dotIdx == -1 {
		return "application/octet-stream"
	}
	ext := strings.ToLower(artifact[dotIdx:])

	mimeTypes := map[string]string{
		".txt":  "text/plain",
		".html": "text/html",
		".css":  "text/css",
		".js":   "application/javascript",
		".json": "application/json",
		".xml":  "application/xml",
		".jpg":  "image/jpeg",
		".jpeg": "image/jpeg",
		".png":  "image/png",
		".gif":  "image/gif",
		".pdf":  "application/pdf",
		".zip":  "application/zip",
	}

	if mime, ok := mimeTypes[ext]; ok {
		return mime
	}
	return "application/octet-stream"
}

// HandlePut processes S3 PUT requests using the engine.
//
// The AWS SDK v2 sends uploads using aws-chunked transfer encoding —
// each chunk is prefixed with a hex size line and may include a chunk
// signature extension. This is distinct from standard HTTP chunked
// transfer encoding (which Go decodes automatically). If aws-chunked
// is detected the body is wrapped in awsChunkedReader to strip the
// framing before the payload reaches the storage backend.
//
// A TeeReader computes the MD5 ETag in a single streaming pass.
// The backend name returned by engine.Put is persisted to
// object_head_cache so GET can route to the same backend after restart.
func (a *S3ToEngine) HandlePut(w http.ResponseWriter, r *http.Request, bucket, object string) {
	t, err := tenant.FromContext(r.Context())
	if err != nil {
		a.logger.Warn("no tenant in context", zap.Error(err))
		WriteS3Error(w, ErrAccessDenied, r.URL.Path, generateRequestID())
		return
	}

	container := t.NamespaceContainer(bucket)
	artifact := object

	// Determine the actual content length for metadata.
	// x-amz-decoded-content-length carries the real size when the body
	// uses aws-chunked encoding (r.ContentLength is the encoded size).
	size := r.ContentLength
	if decoded := r.Header.Get("x-amz-decoded-content-length"); decoded != "" && isAWSChunked(r) {
		if n, err := strconv.ParseInt(decoded, 10, 64); err == nil {
			size = n
		}
	}
	if size < 0 {
		size = 0
	}

	// The key must leave room for its blob address (WP-R2-1): refused
	// before any body byte is read.
	if why, tooLong := blobKeyTooLong(r.Context(), a.db, t.ID, bucket, artifact); tooLong {
		WriteS3ErrorWithContext(w, ErrKeyTooLong, r.URL.Path, generateRequestID(), WithSuggestion(why))
		return
	}

	if a.db != nil {
		// The lock lives in object_locks, not on the head row: a retained
		// key whose head row is gone (delete marker, crash between deletes)
		// must still refuse the overwrite — the write below replaces the
		// backend bytes in place (R2-01).
		if lockErr := checkObjectLock(r.Context(), a.db, t.ID, bucket, artifact, isObjectLockBypass(r)); lockErr != nil {
			writeObjectLockRefusal(w, r, a.logger, lockErr)
			return
		}
		var existingETag string
		existsErr := a.db.QueryRowContext(r.Context(), `
			SELECT etag FROM object_head_cache
			WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3`,
			t.ID, bucket, artifact).Scan(&existingETag)
		if existsErr == nil && r.Header.Get("If-Match") != "" && checkIfMatch(r, existingETag) {
			w.WriteHeader(http.StatusPreconditionFailed)
			return
		}
	}

	// Everything that can reject the request for a client-side reason is
	// checked BEFORE a body byte is read: the backend write replaces the
	// previous object's bytes in place, so a rejection after it corrupts the
	// customer's existing object (R2-05: a PUT with too many x-amz-meta-*
	// headers answered 400 and left the new bytes under the old head row).
	userMeta := extractS3Metadata(r)
	if err := validateMetadata(userMeta); err != nil {
		WriteS3ErrorWithContext(w, ErrInvalidRequest, r.URL.Path, generateRequestID(), WithSuggestion(err.Error()))
		return
	}
	metaJSON, _ := json.Marshal(userMeta)
	// PutObject replaces the tag set: the x-amz-tagging header, or none.
	putTags, tagErr := parseTaggingHeader(r.Header.Get("x-amz-tagging"))
	if tagErr != nil {
		WriteS3ErrorWithContext(w, ErrInvalidTag, r.URL.Path, generateRequestID(), WithSuggestion(tagErr.Error()))
		return
	}
	tagsJSON, _ := json.Marshal(putTags)
	wantMD5, md5Err := parseContentMD5(r)
	if md5Err != nil {
		WriteS3Error(w, ErrInvalidDigest, r.URL.Path, generateRequestID())
		return
	}
	sizeDeclared := putSizeDeclared(r)

	chunked := isAWSChunked(r)
	a.logger.Debug("PUT with tenant isolation",
		zap.String("tenant_id", t.ID),
		zap.String("original_bucket", bucket),
		zap.String("namespaced_container", container),
		zap.String("artifact", artifact),
		zap.Bool("aws_chunked", chunked),
		zap.Int64("size", size))

	// Wrap body: decode aws-chunked framing if present, then tee into
	// MD5 hasher so the ETag is computed in a single streaming pass.
	// bodyCounter measures the decoded logical bytes actually consumed —
	// billing must never trust the client-declared size alone.
	var body io.Reader = r.Body
	if chunked {
		body = newAWSChunkedReader(r.Body)
	}
	// Every read error of the body is the client's, tagged where it is
	// read (WP-R2-3): the sealer, the chunker and the driver all read
	// through it.
	src := newClientBody(body)

	bodyCounter := &countingReader{r: src}
	hasher := md5.New() // #nosec G401 — S3 spec requires MD5 for ETags
	hashingBody := io.TeeReader(bodyCounter, hasher)

	metadataSize := size
	var encryptionAlgorithm string
	var ssecKeyMD5 string         // the SSE-C key's base64 MD5 (RP14-11, 094)
	var encryptionKeyVersion *int // SSE-S3: the tenant key version the blob is sealed under (WP-R8-1)

	// Where the bytes go — chunked, whole and sealed, whole, or refused — is
	// ONE decision, made the same way for a CompleteMultipartUpload of the
	// same bytes and class (placement.go, WP-R3-1). A chunked object is
	// encrypted chunk by chunk (never SSE-S3 as well: GET peels one layer,
	// and a random seal would defeat the deterministic chunk dedup, WP-7);
	// the region pin, versioning, the tier and the `chunking` flag are read
	// once, the bucket's encryption setting strictly (R8-03, R8-04).
	resolvedStorageClass := a.storageClass
	if !a.storageClassResolved {
		resolvedStorageClass = resolvePutStorageClass(r.Context(), a.db, a.engine, syncPlacementGate(a.flags), t.ID, bucket,
			r.Header.Get("x-amz-storage-class"))
	}
	placement := a.decidePlacement(r.Context(), placementRequest{
		TenantID: t.ID, Bucket: bucket, Class: resolvedStorageClass, Size: metadataSize,
		SSEC: crypto.HasSSECHeaders(r), SSERequested: r.Header.Get("x-amz-server-side-encryption") == "AES256",
	})
	if placement.Refusal != nil {
		placement.Refusal.write(w, r)
		return
	}

	if crypto.HasSSECHeaders(r) {
		if r.Header.Get("x-amz-server-side-encryption") != "" {
			WriteS3ErrorWithContext(w, ErrInvalidRequest, r.URL.Path, generateRequestID(),
				WithSuggestion("Cannot use SSE-S3 and SSE-C simultaneously."))
			return
		}
		ssecKey, parseErr := crypto.ParseSSECHeaders(r)
		if parseErr != nil {
			WriteS3ErrorWithContext(w, ErrInvalidRequest, r.URL.Path, generateRequestID(),
				WithSuggestion(parseErr.Error()))
			return
		}

		// Sealed as it streams under the customer's key (WP-R8-5 Part B,
		// the codec's kind 0x02, any size): the DEK is derived from the key
		// and the blob's own salt before SSECSeal returns, so the key is
		// zeroed at once. The MD5 tee sees the plaintext; a body short or
		// long of its size fails the sealer's read (the client's 400), a
		// Content-MD5 mismatch retires the written blob below — as on the
		// plain path.
		ssecKeyMD5 = crypto.SSECKeyMD5(ssecKey) // kept with the object (RP14-11); the key is not
		sealed, encErr := crypto.SSECSeal(ssecKey, crypto.StreamIdentity{Tenant: t.ID, Bucket: bucket, Key: object}, hashingBody, metadataSize)
		clear(ssecKey)
		if encErr != nil {
			a.logger.Error("SSE-C encryption failed", zap.Error(encErr))
			WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
			return
		}
		hashingBody = sealed
		size = crypto.SSECSealedLength(metadataSize)
		encryptionAlgorithm = crypto.SSECAlgorithmV3
	} else {
		shouldEncrypt := placement.Kind == placeWholeSealed

		if shouldEncrypt {
			// Sealed as it streams (WP-R8-5, format 0x03): the MD5 tee sees
			// the plaintext, the backend PUT carries the ciphertext's length
			// (a pure function of the size). A body shorter or longer than
			// its declared size fails the sealer's read — the engine tags a
			// source failure as the caller's (IncompleteBody, no breaker
			// charge); a Content-MD5 mismatch is found after the write and
			// retires the orphan below, as on the plain path.
			sealed, keyVersion, encErr := a.sseService.Seal(r.Context(), crypto.StreamIdentity{Tenant: t.ID, Bucket: bucket, Key: object}, hashingBody, metadataSize)
			if encErr != nil {
				a.logger.Error("SSE-S3: tenant key unavailable", zap.Error(encErr))
				WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
				return
			}
			encryptionKeyVersion = &keyVersion
			hashingBody = sealed
			size = a.sseService.SealedLength(metadataSize)
			encryptionAlgorithm = crypto.SSEAlgorithm
		}
	}

	// Chunked upload path (the placement said so): content-defined chunks,
	// deduplicated via the GCI, per-chunk convergent encryption when
	// chunkEncSvc is set (Phase 10).
	// Only UNENCRYPTED bodies may enter the chunked path. At this point
	// encryptionAlgorithm != "" means the body is already ciphertext:
	// SSE-C always (encrypted above with the customer's key — chunking it
	// would chunk ciphertext, then stamp AES256-CE over the SSE-C marker and
	// serve raw ciphertext on GET with no key check), or SSE-S3 when the
	// per-chunk service wasn't available. Per-chunk encryption (AES256-CE)
	// happens INSIDE handleChunkedPut on plaintext; whole-object SSE-S3 is
	// never decided for a body the placement sends here.
	if placement.Kind == placeChunked && encryptionAlgorithm == "" {
		{
			// WP-C: no uuid.Parse gate — tenant IDs are strings ("tenant-<hex>"
			// from registration). The old gate silently skipped chunking for
			// every real tenant.
			chunkErr := a.handleChunkedPut(r, w, t, bucket, artifact, metadataSize, hashingBody, hasher, wantMD5)
			if chunkErr == nil {
				return
			}
			if clientBodyFault(r.Context(), src, chunkErr) {
				// R108-9: the body broke off, or the client went away and
				// the workers saw only their cancelled context — a 500
				// and an Error line before.
				answerClientBody(w, r, a.logger, "PutObject", src, chunkErr,
					zap.String("bucket", bucket), zap.String("key", artifact), zap.Bool("chunked", true))
				return
			}
			if errors.Is(chunkErr, errChunkPutBudget) {
				// The process cannot take another large upload now (WP-R2-6):
				// nothing was read or stored; the client retries. Real
				// saturation — it counts in vaultaire_errors_total.
				a.logger.Warn("chunked upload refused: the chunk-buffer budget could not spare one worker's buffers — 503",
					zap.String("bucket", bucket), zap.String("key", artifact), zap.Int64("budget_in_use", a.chunkBudget().inUse()))
				w.Header().Set("Retry-After", "30")
				WriteS3ErrorWithContext(w, ErrSlowDown, r.URL.Path, generateRequestID(),
					WithMessage("Please reduce your request rate: the server cannot take another large upload right now."),
					WithSuggestion("Retry in 30 seconds."))
				return
			}
			a.logger.Error("chunked upload failed",
				zap.Error(chunkErr),
				zap.String("bucket", bucket),
				zap.String("key", artifact))
			switch {
			case errors.Is(chunkErr, errQuotaRace):
				WriteS3ErrorWithContext(w, ErrQuotaExceeded, r.URL.Path, generateRequestID(),
					WithSuggestion("Upgrade at "+billingURL()))
			case errors.Is(chunkErr, errDecodedLengthMismatch):
				WriteS3Error(w, ErrIncompleteBody, r.URL.Path, generateRequestID())
			case errors.Is(chunkErr, errBadDigest):
				WriteS3Error(w, ErrBadDigest, r.URL.Path, generateRequestID())
			case errors.Is(chunkErr, engine.ErrAllBackendsUnavailable):
				w.Header().Set("Retry-After", "30")
				WriteS3Error(w, ErrServiceUnavailable, r.URL.Path, generateRequestID())
			default:
				WriteS3Error(w, bodyReadErrorCode(chunkErr), r.URL.Path, generateRequestID())
			}
			return
		}
	}

	storageClass := resolvedStorageClass // header ?: bucket tier, computed above
	putOpts := []engine.PutOption{engine.WithContentLength(size)}
	if storageClass != "" {
		putOpts = append(putOpts, engine.WithStorageClass(storageClass))
	}

	// The bytes go to a NEW address (WP-R2-1): nothing before the head-row
	// flip touches the object the key holds now, so any failure below
	// leaves it intact (R2-05), and two concurrent PUTs each commit a row
	// that describes their own blob (R2-15).
	blob, addrErr := blobAddressFor(a.db, artifact)
	if addrErr != nil {
		a.logger.Error("PUT: no blob address", zap.Error(addrErr))
		WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
		return
	}

	// Placement (region driver for a pinned bucket, else the engine) is the
	// shared helper so multipart complete and CopyObject cannot drift from
	// it again (R3-08).
	var backendName string
	backendName, err = placeObject(r.Context(), a.db, a.engine, t.ID, bucket, container, blob, hashingBody, putOpts...)
	if err != nil {
		switch {
		case errors.Is(err, errRegionDriverUnavailable):
			// Never fall through to the primary: the bucket promised a region.
			a.logger.Error("region-pinned bucket has no driver — PUT refused",
				zap.String("bucket", bucket), zap.Error(err))
			w.Header().Set("Retry-After", "300")
			WriteS3ErrorWithContext(w, ErrServiceUnavailable, r.URL.Path, generateRequestID(),
				WithSuggestion("This bucket's region is not enabled on this deployment."))
		case clientBodyFault(r.Context(), src, err):
			// The client's body broke off or trickled below the minimum
			// rate: its request, never the backend's (Prompt 2b.2 C1,
			// WP-R2-3).
			answerClientBody(w, r, a.logger, "PutObject", src, err,
				zap.String("container", container), zap.String("artifact", artifact))
		case errors.Is(err, engine.ErrAllBackendsUnavailable):
			w.Header().Set("Retry-After", "30")
			WriteS3Error(w, ErrServiceUnavailable, r.URL.Path, generateRequestID())
		case errors.Is(err, engine.ErrQuotaExceeded):
			// Quota exhaustion is a client condition, never a 500.
			WriteS3ErrorWithContext(w, ErrQuotaExceeded, r.URL.Path, generateRequestID(),
				WithSuggestion("Storage quota exceeded. Upgrade at "+billingURL()))
		case errors.Is(err, engine.ErrInvalidInput):
			// The bucket's backend cannot hold this key (a name Sync's
			// bridge refuses): the client's request, never a retryable 503
			// — and never silent (bench 2026-10-12 finding 2).
			a.logger.Warn("put: the backend refused the key — 400",
				zap.Error(err), zap.String("container", container), zap.String("artifact", artifact))
			WriteS3ErrorWithContext(w, ErrInvalidArgument, r.URL.Path, generateRequestID(),
				WithSuggestion(refusedKeySuggestion))
		default:
			a.logger.Error("engine put failed",
				zap.Error(err),
				zap.String("container", container),
				zap.String("artifact", artifact))
			WriteS3Error(w, bodyReadErrorCode(err), r.URL.Path, generateRequestID())
		}
		return
	}

	// The stored size is the MEASURED size. The declared size comes from
	// headers the client controls (x-amz-decoded-content-length was honoured
	// on plain bodies too, R2-06) and a driver may commit a body that ended
	// early; either way the head row and the bill must never describe bytes
	// that were not received. A body with no declared size at all records
	// what arrived. A refusal here leaves the blob at its own address, no
	// row naming it: queued as an orphan (R7-12), never committed.
	if !sizeDeclared {
		metadataSize = bodyCounter.n
	} else if bodyCounter.n != metadataSize {
		a.logger.Warn("PUT body length mismatch",
			zap.String("tenant_id", t.ID),
			zap.Bool("aws_chunked", chunked),
			zap.Int64("declared", metadataSize),
			zap.Int64("measured", bodyCounter.n))
		retireOrphanWrite(r.Context(), a.db, a.logger, t.ID, bucket, blob, backendName)
		WriteS3Error(w, ErrIncompleteBody, r.URL.Path, generateRequestID())
		return
	}

	etag := fmt.Sprintf("%x", hasher.Sum(nil))
	if wantMD5 != "" && etag != wantMD5 {
		retireOrphanWrite(r.Context(), a.db, a.logger, t.ID, bucket, blob, backendName)
		WriteS3Error(w, ErrBadDigest, r.URL.Path, generateRequestID())
		return
	}

	contentType := r.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	contentDisposition := sanitizeContentDisposition(r.Header.Get("Content-Disposition"))
	contentEncoding := requestContentEncoding(r)
	contentLanguage := requestContentLanguage(r)
	echoHdrs := requestEchoHeaders(r)

	// The displaced row's blob — on this backend or another (a Smart-demoted
	// object rewritten hot, R13-10) — is retired inside the flip: removed by
	// the collector after its grace, never while a row names it.
	var versionID string
	if a.db != nil {
		// The pointer flip (commitBlobWrite): the head row names the new
		// address, the version row is written in the same transaction, the
		// displaced blob is retired (or kept by its version), and the
		// previous row's size is captured under its lock — a concurrent
		// DELETE cannot double-release.
		flip, dbErr := commitBlobWrite(r.Context(), a.db, blobWrite{TenantID: t.ID, Bucket: bucket, Key: artifact,
			Bill: a.bill,
			Version: &versionWrite{Size: metadataSize, ETag: etag, ContentType: contentType, Backend: backendName,
				Blob: versionBlobRef{StorageKey: blob, Enc: objectEncryption{Algorithm: encryptionAlgorithm, KeyVersion: encryptionKeyVersion, SSECKeyMD5: ssecKeyMD5}}},
			Upsert: func(tx *sql.Tx) error {
				// body_md5 stays NULL: a PUT's ETag is the body's MD5 (A.4).
				_, execErr := tx.ExecContext(r.Context(), `
				INSERT INTO object_head_cache
					(tenant_id, bucket, object_key, size_bytes, etag, content_type, backend_name, metadata, encryption_algorithm, content_disposition, content_encoding, content_language, cache_control, http_expires, website_redirect_location, floor, is_chunked, tags, encryption_key_version, storage_key, body_md5, ssec_key_md5, updated_at)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, FALSE, $17, $18, $19, NULL, NULLIF($20, ''), NOW())
				ON CONFLICT (tenant_id, bucket, object_key) DO UPDATE SET
					size_bytes            = EXCLUDED.size_bytes,
					etag                  = EXCLUDED.etag,
					content_type          = EXCLUDED.content_type,
					backend_name          = EXCLUDED.backend_name,
					metadata              = EXCLUDED.metadata,
					encryption_algorithm  = EXCLUDED.encryption_algorithm,
					encryption_key_version = EXCLUDED.encryption_key_version,
					content_disposition   = EXCLUDED.content_disposition,
					content_encoding      = EXCLUDED.content_encoding,
					content_language      = EXCLUDED.content_language,
					cache_control         = EXCLUDED.cache_control,
					http_expires          = EXCLUDED.http_expires,
					website_redirect_location = EXCLUDED.website_redirect_location,
					floor                 = EXCLUDED.floor,
					is_chunked            = EXCLUDED.is_chunked,
					tags                  = EXCLUDED.tags,
					storage_key           = EXCLUDED.storage_key,
					body_md5              = EXCLUDED.body_md5,
					ssec_key_md5          = EXCLUDED.ssec_key_md5,
					updated_at            = NOW()
			`, t.ID, bucket, artifact, metadataSize, etag, contentType, backendName, metaJSON, encryptionAlgorithm, contentDisposition, contentEncoding, contentLanguage, echoHdrs.CacheControl, echoHdrs.Expires, echoHdrs.WebsiteRedirect, usage.FloorOf(resolvedStorageClass), tagsJSON, encryptionKeyVersion, blob, ssecKeyMD5)
				return execErr
			}})
		versionID = flip.VersionID
		if dbErr != nil {
			// HEAD serves exclusively from object_head_cache — returning 200
			// without the row means every subsequent HEAD/GET 404s and the
			// bytes are never billed. Nothing changed for the key: the
			// previous object is intact, the new blob is an orphan queued
			// for the collector, and the client's retry writes afresh.
			retireOrphanWrite(r.Context(), a.db, a.logger, t.ID, bucket, blob, backendName)
			if errors.Is(dbErr, errQuotaRace) {
				// A concurrent write changed what this overwrite displaces:
				// the net reservation no longer fits (Part B). Refused,
				// nothing committed.
				WriteS3ErrorWithContext(w, ErrQuotaExceeded, r.URL.Path, generateRequestID(),
					WithSuggestion("Upgrade at "+billingURL()))
				return
			}
			a.logger.Error("failed to cache object metadata — failing PUT",
				zap.Error(dbErr),
				zap.String("tenant_id", t.ID),
				zap.String("bucket", bucket),
				zap.String("object", artifact))
			WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
			return
		}
		a.billSettled = true
	}

	applyObjectLockOnPut(r.Context(), a.db, t.ID, bucket, artifact, r)

	w.Header().Set("ETag", fmt.Sprintf(`"%s"`, etag))
	w.Header().Set("x-amz-request-id", generateRequestID())
	// Object size on the PutObject response (newer AWS surface; SDKs expose
	// it as PutObjectOutput.Size and test suites assert it).
	w.Header().Set("x-amz-object-size", strconv.FormatInt(metadataSize, 10))
	if crypto.IsSSEC(encryptionAlgorithm) {
		w.Header().Set("x-amz-server-side-encryption-customer-algorithm", "AES256")
	} else if encryptionAlgorithm != "" {
		w.Header().Set("x-amz-server-side-encryption", "AES256")
	}
	if versionID != "" {
		w.Header().Set("x-amz-version-id", versionID)
	}
	w.WriteHeader(http.StatusOK)

	a.logger.Info("artifact stored",
		zap.String("tenant_id", t.ID),
		zap.String("s3.bucket", bucket),
		zap.String("s3.object", object),
		zap.String("backend", backendName),
		zap.String("etag", etag),
		zap.String("version_id", versionID),
		zap.Bool("aws_chunked", chunked),
		zap.Bool("encrypted", encryptionAlgorithm != ""),
		zap.Int64("size", metadataSize))

	a.notifySvc.Fire(t.ID, bucket, "s3:ObjectCreated:Put", object, size, etag)
	emitEvent(r.Context(), a.db, a.logger, "object.created", t.ID, map[string]interface{}{
		"bucket": bucket, "key": object, "size": size, "etag": etag,
	})
}

// chunkingEnabled resolves the `chunking` feature flag for a tenant
// (tenant override → global row → default true). A nil flag service —
// tests and callers predating 1.13 — behaves as flag-on. Flag off routes
// PUTs down the plain whole-object path; GETs of already-chunked objects
// are unaffected (manifests are self-describing).
func (a *S3ToEngine) chunkingEnabled(tenantID string) bool {
	if a.flags == nil {
		return true
	}
	return a.flags.Enabled(flagChunking, tenantID)
}

// handleChunkedPut is the PUT's side of the chunked write: the request's
// attributes and ETag (the whole-stream MD5, checked against Content-MD5)
// into the reader-driven core (storeChunkedObject, chunked_write.go), then
// Object Lock, the response and s3:ObjectCreated:Put. Returns nil on success
// (response already written) or an error HandlePut answers.
func (a *S3ToEngine) handleChunkedPut(
	r *http.Request, w http.ResponseWriter,
	t *tenant.Tenant,
	bucket, artifact string,
	metadataSize int64,
	hashingBody io.Reader,
	hasher hash.Hash,
	wantMD5 string,
) error {
	ctx := r.Context()
	// Validated by HandlePut before the body was read.
	attrs, err := objectAttrsFromRequest(r)
	if err != nil {
		return fmt.Errorf("request attributes: %w", err)
	}
	declared := int64(-1)
	if putSizeDeclared(r) {
		declared = metadataSize
	}
	res, err := a.storeChunkedObject(ctx, chunkedWrite{
		TenantID: t.ID, Bucket: bucket, Key: artifact, Bill: a.bill,
		Body: hashingBody, DeclaredSize: declared, Attrs: attrs,
		ETag: func() (string, error) {
			etag := fmt.Sprintf("%x", hasher.Sum(nil))
			if wantMD5 != "" && etag != wantMD5 {
				return "", fmt.Errorf("content-md5 %s, computed %s: %w", wantMD5, etag, errBadDigest)
			}
			return etag, nil
		},
	})
	if err != nil {
		return err
	}
	a.billSettled = true

	applyObjectLockOnPut(ctx, a.db, t.ID, bucket, artifact, r)

	w.Header().Set("ETag", fmt.Sprintf(`"%s"`, res.ETag))
	w.Header().Set("x-amz-request-id", generateRequestID())
	w.Header().Set("x-amz-object-size", strconv.FormatInt(res.Size, 10))
	if res.Encrypted {
		w.Header().Set("x-amz-server-side-encryption", "AES256") // as HEAD and GET say (R8-15)
	}
	if res.Flip.VersionID != "" {
		w.Header().Set("x-amz-version-id", res.Flip.VersionID)
	}
	w.WriteHeader(http.StatusOK)

	a.notifySvc.Fire(t.ID, bucket, "s3:ObjectCreated:Put", artifact, res.Size, res.ETag)
	emitEvent(ctx, a.db, a.logger, "object.created", t.ID, map[string]interface{}{
		"bucket": bucket, "key": artifact, "size": res.Size, "etag": res.ETag, "chunked": true,
	})
	return nil
}

// chunkStoreResult is what a chunk store took a reference on: the blob this
// writer stored, or the row a concurrent first-store made ready (R8-02: the
// loser's own encoding is discarded — chunk_claim.go).
type chunkStoreResult struct {
	backend        string  // backend holding the blob (the row's when reused)
	ciphertextHash *string // SHA-256 of the blob that is actually stored, nil if unencrypted
	keyVersion     *int    // the tenant key version that blob is sealed under, nil if unencrypted
	reused         bool    // an existing row was referenced; nothing of this writer's was kept
}

// errChunkDecrypt marks a chunk that was read but did not decrypt.
var errChunkDecrypt = errors.New("chunk decryption failed")

// truncatedReason is the vaultaire_chunked_get_truncated_total reason of a
// chunk failure.
func truncatedReason(err error) string {
	switch {
	case errors.Is(err, errChunkIntegrity):
		return truncatedIntegrity
	case errors.Is(err, errChunkDecrypt):
		return truncatedDecrypt
	default:
		return truncatedFetch
	}
}

// errChunkIntegrity signals that a fetched chunk's bytes did not hash to its
// expected plaintext hash. The object exists but is corrupt — this is distinct
// from a missing/unresolvable manifest (which falls through to NoSuchKey).
var errChunkIntegrity = errors.New("chunk integrity verification failed")

// chunkDesc is a resolved chunk location + its byte position within the object.
type chunkDesc struct {
	scope          string // dedup scope of the index row
	storageKey     string
	backendID      string
	plaintextHash  string
	offset         int64  // byte offset of this chunk within the object
	size           int64  // chunk size in bytes (plaintext)
	compressed     bool   // true if chunk is stored compressed (needs decompression on read)
	encrypted      bool   // true if chunk is stored encrypted (needs decryption on read)
	ciphertextHash string // SHA-256 of encrypted blob for integrity verification
	keyVersion     int    // tenant key version the blob is sealed under (the row's, else the ref's)
}

// fetchAndVerifyChunk reads one chunk from the global container into a bounded
// buffer (≤ max chunk size, ~16 MB) and verifies its SHA-256 matches the
// expected plaintext hash before returning it. Verifying before the bytes are
// written guarantees corrupt data is never served, and the per-chunk read keeps
// peak memory at one chunk regardless of object size.
//
// Pipeline order: fetch → decrypt → decompress → verify.
func (a *S3ToEngine) fetchAndVerifyChunk(ctx context.Context, d chunkDesc, tenantID string) ([]byte, error) {
	// The one address of the chunk on the backend its index row names
	// (chunk_store.go); blobs written before WP-R8-7 are found at their old
	// address until the chunk move has run.
	rdr, err := a.chunks().get(ctx, chunkAddr{scope: d.scope, hash: d.plaintextHash, backend: d.backendID, key: d.storageKey})
	if err != nil && isObjectMissingErr(err) {
		// The plan was resolved when the GET began; the blob may have moved
		// since — a re-encrypt swap whose old address was deleted after its
		// grace, a chunk move — while this stream was still on its plan (a
		// long or paced download). One read of the row past the cache: a
		// row that names another blob is read there and opened with the
		// version it records (same plaintext hash, so the check below is
		// unchanged); an unchanged row is a real miss (WP-R8-1 A2). Never on
		// an error: "could not ask" is not "not there".
		if moved, ok := a.rereadChunk(ctx, d); ok {
			chunkedGetRereads.WithLabelValues(rereadMoved).Inc()
			d = moved
			rdr, err = a.chunks().get(ctx, chunkAddr{scope: d.scope, hash: d.plaintextHash, backend: d.backendID, key: d.storageKey})
		} else {
			chunkedGetRereads.WithLabelValues(rereadMissing).Inc()
		}
	}
	if err != nil {
		return nil, fmt.Errorf("fetch chunk %s: %w", d.plaintextHash[:16], err)
	}
	defer func() { _ = rdr.Close() }()

	// One allocation of the chunk's recorded size (+ the read slack
	// bytes.Buffer wants): io.ReadAll grows from 512 bytes, copying a 2 MiB
	// chunk ~12 times and leaving ~4 MiB of garbage per chunk. An encrypted
	// blob is the size plus the nonce and tag: without them the slack left
	// after the last byte is 484, under MinRead, and the buffer regrew once.
	var buf bytes.Buffer
	if d.size > 0 && d.size <= 64<<20 {
		n := int(d.size) + bytes.MinRead
		if d.encrypted {
			n += crypto.ChunkEncryptionOverhead
		}
		buf.Grow(n)
	}
	if _, err := buf.ReadFrom(rdr); err != nil {
		return nil, fmt.Errorf("read chunk %s: %w", d.plaintextHash[:16], err)
	}
	data := buf.Bytes()

	if d.encrypted && a.chunkEncSvc != nil {
		data, err = a.chunkEncSvc.DecryptChunkData(ctx, tenantID, d.keyVersion, d.plaintextHash, data, d.ciphertextHash)
		if err != nil {
			return nil, fmt.Errorf("%w: chunk %s: %w", errChunkDecrypt, d.plaintextHash[:16], err)
		}
	}

	if d.compressed {
		data, err = crypto.DecompressBuffer(data)
		if err != nil {
			return nil, fmt.Errorf("decompress chunk %s: %w", d.plaintextHash[:16], err)
		}
	}

	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != d.plaintextHash {
		return nil, fmt.Errorf("%w: chunk %s (%d bytes)", errChunkIntegrity, d.plaintextHash[:16], len(data))
	}
	return data, nil
}

// rereadChunk reads the chunk's index row past the GCI cache and returns the
// desc it describes now, ok only when the row names another blob than d
// (address, backend, key version or ciphertext). A row gone or unchanged, or
// a read that failed, is not ok: the caller's miss stands.
func (a *S3ToEngine) rereadChunk(ctx context.Context, d chunkDesc) (chunkDesc, bool) {
	if a.gci == nil {
		return d, false
	}
	res, err := a.gci.LookupChunkFresh(ctx, d.scope, d.plaintextHash)
	if err != nil {
		a.logger.Warn("chunked GET: re-read of a missing chunk's row failed", zap.String("hash", shortHash(d.plaintextHash)), zap.Error(err))
		return d, false
	}
	if res == nil || !res.Exists || res.Entry == nil {
		return d, false
	}
	e := res.Entry
	n := d
	n.storageKey = e.StorageKey
	if n.storageKey == "" {
		n.storageKey = "_chunks/" + d.plaintextHash
	}
	n.backendID = e.BackendID
	if e.CiphertextHash != nil {
		n.ciphertextHash = *e.CiphertextHash
	}
	if e.EncryptionKeyVersion != nil {
		n.keyVersion = *e.EncryptionKeyVersion
	}
	n.encrypted = e.Encrypted
	n.compressed = e.CompressionAlgo != nil
	n.size = e.SizeBytes
	changed := n.storageKey != d.storageKey || n.backendID != d.backendID ||
		n.keyVersion != d.keyVersion || n.ciphertextHash != d.ciphertextHash
	return n, changed
}

// handleChunkedGet serves a chunked object by streaming its content-defined
// chunks one at a time, in chunk_index order, directly to the response writer.
// Each chunk is read into a bounded buffer (~16 MB max) and integrity-verified
// before its bytes are written, so peak memory is one chunk — not the whole
// object — and corrupt data is never served. Range requests touch only the
// chunks that overlap the requested byte range (located via chunk_offset).
//
// Error contract: the manifest is fully resolved (all chunk index entries
// present) and every backend of the plan is one that can be asked (registered,
// breaker not open) BEFORE any byte is written, so either failure returns an
// error HandleGet answers (503 for a backend that cannot be asked, else 500).
// Once the first chunk is committed the status is fixed; a later
// fetch/integrity/decrypt failure CUTS the connection (abortResponse →
// http.ErrAbortHandler, counted in vaultaire_chunked_get_truncated_total)
// rather than serving bad bytes or ending a short body cleanly. A corrupt
// FIRST chunk yields a clean 500 (handled here — never a fallthrough to 404).
//
// Per-chunk convergent encryption (AES256-CE) is handled via fetchAndVerifyChunk
// when chunkEncSvc is set. SSE-C/SSE-S3 whole-object encryption remains mutually
// exclusive with chunking for objects that don't use the per-chunk path.
func (a *S3ToEngine) handleChunkedGet(
	w http.ResponseWriter, r *http.Request,
	t *tenant.Tenant,
	bucket, artifact, manifest string,
	oh objectHeaders, // the object's headers (HandleGet built them from its row)
) error {
	ctx := r.Context()
	cachedSize := oh.Size

	container := t.NamespaceContainer(bucket)

	// The manifest the row names (WP-VM-1): its address, or the key for one
	// written before 092. A row and its manifest are read in two statements:
	// a manifest an overwrite displaced meanwhile is retired, not released,
	// so it is still there.
	refs, err := a.gci.GetObjectChunks(ctx, t.ID, bucket, manifest)
	if err != nil {
		return fmt.Errorf("get object chunks: %w", err)
	}
	if len(refs) == 0 {
		return fmt.Errorf("no chunk references for %s/%s (manifest %s)", bucket, artifact, manifest)
	}

	// Preflight: resolve every chunk's location from the index without reading
	// data — one query per dedup scope. A missing index entry means the
	// manifest is unresolvable — return an error so HandleGet answers 500
	// before any byte is written.
	descs, err := a.resolveChunkDescs(ctx, refs)
	if err != nil {
		return err
	}

	// Build the byte plan: which chunks to read and the (skip, take) slice within
	// each (chunkPlan). Full GET takes every chunk whole; a range takes only
	// overlapping chunks, trimmed to the requested bounds.
	var (
		plan    []chunkSlice
		rng     *httpRange
		isRange bool
	)
	if rh := r.Header.Get("Range"); rh != "" && !errors.Is(rangeParseErr(rh, cachedSize), errMultiRange) {
		parsed, parseErr := parseRangeHeader(rh, cachedSize)
		if parseErr != nil {
			writeRangeNotSatisfiable(w, cachedSize)
			return nil
		}
		rng = parsed
		isRange = true
		plan = chunkPlan(descs, rng.start, rng.end)
	} else {
		plan = chunkPlan(descs, 0, -1)
	}

	// Every backend the plan reads from must be one we can ask NOW, before
	// the status line: a dead backend under chunk 7 used to commit a 200 and
	// stop the body short (Stage 1a Part A). Unregistered or breaker open =
	// ErrAllBackendsUnavailable, which HandleGet answers 503 + Retry-After.
	// The breaker is observed, never charged, and no backend is asked — a
	// HEAD per chunk would double the requests of every read.
	if ca, ok := a.engine.(interface{ CanAsk(string) error }); ok {
		seen := map[string]bool{}
		for _, p := range plan {
			b := p.desc.backendID
			if b == "" || seen[b] {
				continue
			}
			seen[b] = true
			if err := ca.CanAsk(b); err != nil {
				return fmt.Errorf("chunk %s: %w", shortHash(p.desc.plaintextHash), err)
			}
		}
	}

	// The object's headers are HEAD's on a 200 and a 206 (object_headers.go):
	// R2 Sippy reads a chunked object above 199 MiB as several ranged GETs
	// and compares their ETags, and a 206 without one failed every pull of a
	// chunked object with "upstream ETag changed during read" (2026-10-05).
	write200Headers := func() {
		w.Header().Set("x-amz-request-id", generateRequestID())
		oh.write(w, headersWhole, nil)
	}
	write206Headers := func() {
		w.Header().Set("x-amz-request-id", generateRequestID())
		oh.write(w, headersRange, rng)
		w.WriteHeader(http.StatusPartialContent)
	}

	// Stream the plan through a bounded, hedged read-ahead (internal/
	// parfetch): up to CHUNK_GET_WINDOW_BYTES of chunks — at most
	// CHUNK_GET_PREFETCH of them — are fetched and verified while earlier
	// ones stream to the client; a chunk still not done after the hedge
	// delay is requested again and the first verified copy wins (the
	// content address makes either copy valid). The writer consumes
	// strictly in plan order, so ordering, verification (inside
	// fetchAndVerifyChunk, before any byte is written) and the error
	// contract below are those of the sequential loop. A chunk holds its
	// full size from fetch start until the next one is taken.
	var planTotal int64
	for _, p := range plan {
		planTotal += p.take
	}
	streamStart := time.Now()
	stream := a.startChunkStream(ctx, t.ID, plan)
	defer stream.Close()

	// The first chunk is fetched + verified BEFORE headers are committed, so
	// a corrupt first chunk produces a clean 500. After that the status is
	// fixed; a failure aborts the body without serving bad bytes.
	headersWritten := false
	var written int64
	var streamErr error
	defer func() {
		observeLargeGet("chunked", streamErr, streamErr == nil && headersWritten && written == planTotal, written, streamStart)
	}()
	for _, p := range plan {
		slice, ferr := stream.Next()
		if ferr != nil {
			streamErr = ferr
			if !headersWritten {
				if errors.Is(ferr, errChunkIntegrity) {
					a.logger.Error("chunk integrity verification failed",
						zap.Error(ferr),
						zap.String("bucket", bucket),
						zap.String("artifact", artifact),
						zap.String("chunk", p.desc.plaintextHash))
					WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
					return nil
				}
				// Unresolved/unavailable before any byte is written — the
				// caller turns this into a clean 500 (never a fallthrough
				// to the plain path, which could hold a stale blob).
				return ferr
			}
			if ctx.Err() != nil {
				// The client went away: nothing to cut, nothing to count.
				return nil
			}
			// The status line is out: the only honest end is a cut. A
			// client that counts bytes would see a short body, but one that
			// has no Content-Length to count against (HTTP/2 through a
			// proxy, a re-chunking hop) would take a clean end for the
			// object.
			reason := truncatedReason(ferr)
			chunkedGetTruncated.WithLabelValues(reason).Inc()
			a.logger.Error("chunk failure mid-stream; cutting the connection",
				zap.Error(ferr),
				zap.String("reason", reason),
				zap.String("bucket", bucket),
				zap.String("artifact", artifact),
				zap.Int64("written", written))
			abortResponse(ctx)
			return nil
		}

		if !headersWritten {
			if isRange {
				write206Headers()
			} else {
				write200Headers()
			}
			headersWritten = true
		}

		n, werr := w.Write(slice)
		written += int64(n)
		if werr != nil { // the client went away: an abort, not a failed read
			a.logger.Error("failed to stream chunk to client",
				zap.Error(werr),
				zap.String("container", container),
				zap.String("artifact", artifact))
			return nil
		}
	}

	// Defensive: a zero-length object/plan still gets a valid response.
	if !headersWritten {
		write200Headers()
		w.WriteHeader(http.StatusOK)
	}

	a.logger.Info("chunked artifact retrieved",
		zap.String("s3.bucket", bucket),
		zap.String("s3.object", artifact),
		zap.String("engine.container", container),
		zap.Int("chunks", len(plan)),
		zap.Int64("bytes", written))

	emitEvent(ctx, a.db, a.logger, "object.downloaded", t.ID, map[string]interface{}{
		"bucket": bucket, "key": artifact, "size": written, "chunked": true,
	})

	return nil
}

// HandleDelete processes S3 DELETE requests
func (a *S3ToEngine) HandleDelete(w http.ResponseWriter, r *http.Request, bucket, object string) {
	t, err := tenant.FromContext(r.Context())
	if err != nil {
		a.logger.Warn("no tenant in context", zap.Error(err))
		WriteS3Error(w, ErrAccessDenied, r.URL.Path, generateRequestID())
		return
	}

	container := t.NamespaceContainer(bucket)
	reqVersionID := r.URL.Query().Get("versionId")
	vStatus := getBucketVersioningStatus(r.Context(), a.db, t.ID, bucket)

	// Object Lock is checked before EVERY delete flavour. AWS lets a delete
	// marker land on a retained key because every version keeps its bytes;
	// our versions are metadata-only (one blob per key, WP-R2-1), so a
	// marker here unbilled and hid the retained object and the next PUT
	// overwrote it (R2-01). Refusing the marker is the documented deviation.
	if lockErr := checkObjectLock(r.Context(), a.db, t.ID, bucket, object, isObjectLockBypass(r)); lockErr != nil {
		writeObjectLockRefusal(w, r, a.logger, lockErr)
		return
	}

	// ?versionId=null of a key with no version row names its current
	// object (written before versioning was enabled): a permanent delete,
	// the plain path below.
	if a.db != nil && (vStatus == "Enabled" || vStatus == "Suspended") && reqVersionID != "" &&
		!isUnanchoredNullVersion(r.Context(), a.db, t.ID, bucket, object, reqVersionID) {
		// A version that kept its bytes (WP-R2-1) has its blob queued in
		// retired_blobs in the same transaction as the row delete; deleting
		// the current version re-points the head row to the newly-latest
		// version (or drops it); the bill follows (PR C2).
		found, err := a.objectDeleteAftermath().deleteVersionReleasing(r.Context(), t.ID, bucket, object, reqVersionID)
		if err != nil {
			a.logger.Error("delete version failed", zap.Error(err))
			WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
			return
		}
		if !found {
			WriteS3Error(w, ErrNoSuchVersion, r.URL.Path, generateRequestID())
			return
		}

		w.Header().Set("x-amz-version-id", reqVersionID)
		w.WriteHeader(http.StatusNoContent)
		a.notifySvc.Fire(t.ID, bucket, "s3:ObjectRemoved:Delete", object, 0, "")
		emitEvent(r.Context(), a.db, a.logger, "object.deleted", t.ID, map[string]interface{}{
			"bucket": bucket, "key": object,
		})
		return
	}

	if a.db != nil && vStatus == "Enabled" && reqVersionID == "" {
		markerID, err := a.objectDeleteAftermath().deleteBehindMarker(r.Context(), t.ID, bucket, object)
		if err != nil {
			// Never drop the head row without its marker: a pre-versioning
			// object is anchored as the null version in the marker's
			// transaction, and without it the delete would lose its bytes.
			a.logger.Error("delete marker failed", zap.Error(err), zap.String("bucket", bucket), zap.String("object", object))
			WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
			return
		}

		w.Header().Set("x-amz-version-id", markerID)
		w.Header().Set("x-amz-delete-marker", "true")
		w.WriteHeader(http.StatusNoContent)
		a.notifySvc.Fire(t.ID, bucket, "s3:ObjectRemoved:Delete", object, 0, "")
		emitEvent(r.Context(), a.db, a.logger, "object.deleted", t.ID, map[string]interface{}{
			"bucket": bucket, "key": object,
		})
		return
	}

	if a.db != nil && vStatus == "Suspended" && reqVersionID == "" {
		// R61-5: the null version is replaced by a null delete marker (as
		// AWS); its bytes are retired and released, never deleted here.
		aft := a.objectDeleteAftermath()
		gone, err := aft.deleteSuspended(r.Context(), t.ID, bucket, object)
		if err != nil {
			a.logger.Error("delete (Suspended) failed", zap.Error(err), zap.String("bucket", bucket), zap.String("object", object))
			WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
			return
		}
		if gone {
			pctx, cancel := postCommit(r.Context())
			aft.afterObjectGone(pctx, t.ID, bucket, object)
			cancel()
		}
		w.Header().Set("x-amz-version-id", "null")
		w.Header().Set("x-amz-delete-marker", "true")
		w.WriteHeader(http.StatusNoContent)
		a.notifySvc.Fire(t.ID, bucket, "s3:ObjectRemoved:Delete", object, 0, "")
		emitEvent(r.Context(), a.db, a.logger, "object.deleted", t.ID, map[string]interface{}{
			"bucket": bucket, "key": object,
		})
		return
	}

	// Chunked objects: decrement chunk ref counts via GCI instead of
	// deleting from the backend. Actual chunk data stays until GC (Phase 8.7).
	// backend_name is the routing truth (the same column GET hints from):
	// without the hint, a DELETE after a restart went to the primary alone,
	// was answered "not found", and the bytes stayed on the real backend
	// forever while the head row — and the customer's bill — went away (R6-05).
	var isChunked bool
	var recordedBackend, storageKey string
	var read headRead
	if a.db != nil {
		// A failed read here must not be guessed away: treating a chunked
		// object as whole sends it to the backend delete, which "succeeds" as
		// an idempotent miss, and the head row goes while the manifest and
		// its GCI references leak forever (R8-07).
		rowErr := a.db.QueryRowContext(r.Context(),
			`SELECT is_chunked, COALESCE(backend_name, ''), COALESCE(storage_key, '') FROM object_head_cache WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3`,
			t.ID, bucket, object).Scan(&isChunked, &recordedBackend, &storageKey)
		if rowErr != nil && !errors.Is(rowErr, sql.ErrNoRows) {
			a.logger.Error("head cache read before delete failed", zap.Error(rowErr),
				zap.String("bucket", bucket), zap.String("object", object))
			WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
			return
		}
		read = headRead{found: rowErr == nil, storageKey: storageKey}
	} else {
		read.found = true // no database: the row delete is the old unconditional one
	}

	// The blob the row names (WP-R2-1); kept when a version row keeps it
	// (a Suspended bucket's plain DELETE of a key whose current bytes are
	// also an Enabled-era version).
	blob := blobArtifact(object, storageKey)
	keptByVersion := false
	if storageKey != "" && a.db != nil {
		named, nErr := versionKeepsBlob(r.Context(), a.db, t.ID, bucket, object, storageKey)
		if nErr != nil {
			a.logger.Error("delete: could not tell whether a version keeps the blob", zap.Error(nErr),
				zap.String("bucket", bucket), zap.String("object", object))
			WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
			return
		}
		keptByVersion = named
	}

	if isChunked && a.gci == nil {
		// A chunked row's bytes are its chunks: without the content index
		// this process can release none of them, and a whole-blob delete of
		// <key> on the primary would be a guess.
		a.logger.Error("delete: a chunked object on a process with no content index", zap.String("bucket", bucket), zap.String("object", object))
		WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
		return
	}
	switch {
	case isChunked || keptByVersion:
		// Nothing to delete on the backend: the chunks stay until dedup GC
		// (the manifest is released together with the head row below, in one
		// transaction, R8-08), and a version's bytes stay with the version.
	case !read.found:
		// No row names any bytes for this key (WP-R6-1: the row is the one
		// authority): nothing to delete on any backend. A blob no row names
		// is the collector's or the erasure sweep's, never a guess on the
		// primary (Stage 1a Prompt 6 Part 0).
	default:
		// Every copy the tables know of, the recorded one first (WP-R6-1:
		// a delete that left a second copy answered 204 — R6-24).
		dctx := r.Context()
		if recordedBackend != "" {
			noteRecordedBackend(a.engine, a.logger, "delete", recordedBackend)
			dctx = engine.WithRecordedBackend(dctx, recordedBackend)
		}
		copies, cErr := knownBlobCopies(r.Context(), a.db, t.ID, bucket, object, container, storageKey, recordedBackend)
		if cErr != nil {
			a.logger.Error("delete: the known copies could not be read", zap.Error(cErr),
				zap.String("bucket", bucket), zap.String("object", object))
			WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
			return
		}
		dctx = engine.WithKnownCopies(dctx, copies...)
		if err := a.engine.Delete(dctx, container, blob); err != nil {
			if errors.Is(err, engine.ErrAllBackendsUnavailable) {
				// The backend that holds the bytes is unreachable: the
				// client retries; the head row must NOT be removed (a
				// "miss" verdict from a fallback is not a verdict, R6-02).
				a.logger.Warn("delete: the object's backend is unavailable — 503, the row is kept",
					zap.String("tenant_id", t.ID), zap.String("bucket", bucket), zap.String("key", object),
					zap.String("backend", recordedBackend), zap.Error(err))
				w.Header().Set("Retry-After", "30")
				WriteS3Error(w, ErrServiceUnavailable, r.URL.Path, generateRequestID())
				return
			}
			if !isObjectMissingErr(err) {
				a.logger.Error("delete failed",
					zap.String("container", container),
					zap.String("artifact", object),
					zap.Error(err))
				WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
				return
			}
			// Missing on the backend: fall through so a drifted head-cache
			// row is still removed (and its bytes released) — S3 DELETE is
			// idempotent either way.
		}
	}

	// Everything after the bytes — the head row (billing), the lock row, the
	// Smart second copy, the parity shards, the notification — is the one
	// aftermath DeleteObjects runs too (object_delete_shared.go).
	if err := a.objectDeleteAftermath().settle(r.Context(), t.ID, bucket, object, isChunked, read); err != nil {
		// The row (and a chunked object's manifest) is still there: the
		// client retries — a backend miss, then the row. Answering 204 here
		// would leave a live (or phantom) object.
		a.logger.Error("delete bookkeeping failed",
			zap.Error(err),
			zap.String("tenant_id", t.ID),
			zap.String("bucket", bucket),
			zap.String("object", object))
		WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// errRegionDriverUnavailable: the bucket is pinned to a region this process
// has no driver for (the region's key pair is not configured, or the driver
// failed at boot). Writing to the primary instead would break the residency
// promise silently, so the PUT is refused (503 + Retry-After).
var errRegionDriverUnavailable = errors.New("region backend unavailable")

// bucketRegionDriver returns the engine driver name for a bucket's region:
// "" when the bucket is in the default region (served by the primary through
// the engine) or when the bucket row cannot be read; the `idrive-<region>`
// name when that driver is registered; errRegionDriverUnavailable when the
// bucket names a non-default region with no registered driver (WP-R7-1).
func bucketRegionDriver(ctx context.Context, db *sql.DB, eng engine.Engine, tenantID, bucket string) (string, error) {
	if db == nil {
		return "", nil
	}
	var region string
	err := db.QueryRowContext(ctx,
		"SELECT region FROM buckets WHERE tenant_id = $1 AND name = $2",
		tenantID, bucket).Scan(&region)
	if err != nil || region == "" || region == drivers.IDriveDefaultRegion(os.Getenv) {
		return "", nil
	}
	driverName := "idrive-" + region
	if ce, ok := eng.(*engine.CoreEngine); ok {
		if _, exists := ce.GetDriver(driverName); exists {
			return driverName, nil
		}
	}
	return "", fmt.Errorf("%w: bucket %s is pinned to %s and no %s driver is registered", errRegionDriverUnavailable, bucket, region, driverName)
}

var tierPreferenceToStorageClass = map[string]string{
	"performance": "STANDARD",
	"standard":    "STANDARD",
	"archive":     "GLACIER",
	"resilient":   "RESILIENT", // → lyve (engine/storage_class.go)
	// → the Sync.com WebDAV bridge, and only for a tenant with the
	// sync_backend flag (resolvePutStorageClass); operator-set only.
	"sync": "SYNC",
}

func bucketTierStorageClass(ctx context.Context, db *sql.DB, tenantID, bucket string) string {
	if db == nil {
		return ""
	}
	var pref string
	err := db.QueryRowContext(ctx,
		"SELECT tier_preference FROM buckets WHERE tenant_id = $1 AND name = $2",
		tenantID, bucket).Scan(&pref)
	if err != nil || pref == "auto" || pref == "" {
		return ""
	}
	return tierPreferenceToStorageClass[pref]
}

// publicBucketStorageClass returns "PUBLIC" when the bucket is public-read and
// an r2 driver is registered on the engine, else "". R2's role of record is
// public buckets / CDN origin only (SMART_TIER_DESIGN.md, 2026-09-19): $0
// egress behind the Cloudflare-proxied CDN host (internal/hosts). Without an r2
// driver (dev, CI, R2 outage at boot) public objects land on the primary
// exactly as before.
func publicBucketStorageClass(ctx context.Context, db *sql.DB, eng engine.Engine, tenantID, bucket string) string {
	if db == nil || eng == nil {
		return ""
	}
	ce, ok := eng.(*engine.CoreEngine)
	if !ok || ce == nil {
		return ""
	}
	if _, exists := ce.GetDriver("r2"); !exists {
		return ""
	}
	var visibility string
	err := db.QueryRowContext(ctx,
		"SELECT visibility FROM buckets WHERE tenant_id = $1 AND name = $2",
		tenantID, bucket).Scan(&visibility)
	if err != nil || visibility != "public-read" {
		return ""
	}
	return "PUBLIC"
}

// resolvePutStorageClass is the single placement resolution for PUT and
// multipart-complete: explicit x-amz-storage-class header → bucket
// tier_preference → PUBLIC for public-read buckets → "" (engine primary).
// Hot tiers (standard/performance both map to STANDARD) do not override the
// public role; cold/resilient tiers keep their placement promise even on a
// public bucket.
//
// The header is client input (R2-07). Only the classes we sell are honoured
// from it — STANDARD, GLACIER, DEEP_ARCHIVE (case-insensitive like AWS);
// internal names (PUBLIC → r2, RESILIENT → lyve), legacy AWS classes
// (REDUCED_REDUNDANCY — unmapped since WP-R7-5; it used to place on the
// hub's disk) and anything unknown fall back to the bucket's own resolution. A cold/resilient bucket tier is
// a placement promise: a header can make an archive object colder
// (DEEP_ARCHIVE) but never hotter, and a resilient bucket never moves.
//
// A `sync` bucket tier resolves to SYNC (→ the Sync.com WebDAV bridge) only
// when allowSync says the tenant has the `sync_backend` flag; otherwise the
// bucket places as an `auto` one (Sync's terms: no customer data without its
// written consent). nil allowSync = never.
func resolvePutStorageClass(ctx context.Context, db *sql.DB, eng engine.Engine, allowSync func(tenantID string) bool, tenantID, bucket, header string) string {
	requested := clientStorageClass(header)
	class := bucketTierStorageClass(ctx, db, tenantID, bucket)
	if class == "SYNC" && (allowSync == nil || !allowSync(tenantID)) {
		class = ""
	}
	if class != "" && class != "STANDARD" {
		if class == "GLACIER" && requested == "DEEP_ARCHIVE" {
			return requested
		}
		return class
	}
	if pub := publicBucketStorageClass(ctx, db, eng, tenantID, bucket); pub != "" {
		if requested == "GLACIER" || requested == "DEEP_ARCHIVE" {
			return requested
		}
		return pub
	}
	if requested != "" {
		return requested
	}
	return class
}

// clientStorageClass maps an x-amz-storage-class request header onto the
// classes a client may choose; everything else is "" (no preference).
func clientStorageClass(header string) string {
	switch strings.ToUpper(strings.TrimSpace(header)) {
	case "STANDARD":
		return "STANDARD"
	case "GLACIER":
		return "GLACIER"
	case "DEEP_ARCHIVE":
		return "DEEP_ARCHIVE"
	}
	return ""
}

// storageClassDisablesChunking reports classes whose objects must be stored
// WHOLE on their backend: chunk blobs always land on the engine's primary
// (the GCI `_global` container is shared across tenants and tiers), which
// would silently break the tier's placement promise — and PUBLIC objects
// must be addressable as one R2 key for the CDN / direct-serve path.
func storageClassDisablesChunking(class string) bool {
	switch class {
	case "RESILIENT", "GLACIER", "DEEP_ARCHIVE", "PUBLIC", "SYNC":
		return true
	}
	return false
}

// bucketSSEEnabled reports whether the bucket requires SSE-S3. A database
// error is an error, never "not encrypted" (the old isBucketSSEEnabled read
// it as false and the body was stored plaintext — WP-R8-1 follow-up A1):
// false, nil only when the bucket has no row or its row says no.
func bucketSSEEnabled(ctx context.Context, db *sql.DB, tenantID, bucket string) (bool, error) {
	s, err := bucketEncryptionSettings(ctx, db, tenantID, bucket)
	return s.SSE, err
}

// bucketEncryption is what a bucket asks of a write: SSE-S3 at rest
// (sse_enabled) and/or a customer key on every write (ssec_required, 096 —
// WP-SSEC-1 B.5).
type bucketEncryption struct {
	SSE, SSECRequired bool
}

// bucketEncryptionSettings reads the bucket's two encryption flags in one
// query. No row = neither; a read that fails is an error, never "off".
func bucketEncryptionSettings(ctx context.Context, db *sql.DB, tenantID, bucket string) (bucketEncryption, error) {
	if db == nil {
		return bucketEncryption{}, nil
	}
	var s bucketEncryption
	err := db.QueryRowContext(ctx,
		"SELECT sse_enabled, ssec_required FROM buckets WHERE tenant_id = $1 AND name = $2",
		tenantID, bucket).Scan(&s.SSE, &s.SSECRequired)
	if errors.Is(err, sql.ErrNoRows) {
		return bucketEncryption{}, nil
	}
	if err != nil {
		return bucketEncryption{}, fmt.Errorf("read sse_enabled of %s: %w", bucket, err)
	}
	return s, nil
}

// HandleList processes S3 LIST requests
func (a *S3ToEngine) HandleList(w http.ResponseWriter, r *http.Request, bucket, prefix string) {
	t, err := tenant.FromContext(r.Context())
	if err != nil {
		a.logger.Warn("no tenant in context", zap.Error(err))
		WriteS3Error(w, ErrAccessDenied, r.URL.Path, generateRequestID())
		return
	}

	container := t.NamespaceContainer(bucket)

	artifacts, err := a.engine.List(r.Context(), container, prefix)
	if err != nil {
		a.logger.Error("list failed",
			zap.String("container", container),
			zap.Error(err))
		WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
		return
	}

	w.Header().Set("Content-Type", "application/xml")

	if _, err := w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?>`)); err != nil {
		a.logger.Error("failed to write XML header", zap.Error(err))
		return
	}
	if _, err := w.Write([]byte(`<ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/">`)); err != nil {
		a.logger.Error("failed to write response", zap.Error(err))
		return
	}
	if _, err := fmt.Fprintf(w, "<Name>%s</Name>", bucket); err != nil { // #nosec G705 — S3 XML protocol output, bucket names are validated
		a.logger.Error("failed to write bucket name", zap.Error(err))
		return
	}

	for _, artifact := range artifacts {
		if _, err := w.Write([]byte("<Contents>")); err != nil {
			a.logger.Error("failed to write contents tag", zap.Error(err))
			return
		}
		if _, err := fmt.Fprintf(w, "<Key>%s</Key>", artifact.Key); err != nil {
			a.logger.Error("failed to write key", zap.Error(err))
			return
		}
		if _, err := fmt.Fprintf(w, "<Size>%d</Size>", artifact.Size); err != nil {
			a.logger.Error("failed to write size", zap.Error(err))
			return
		}
		if _, err := fmt.Fprintf(w, "<LastModified>%s</LastModified>",
			time.Now().UTC().Format("2006-01-02T15:04:05.000Z")); err != nil {
			a.logger.Error("failed to write last modified", zap.Error(err))
			return
		}
		if _, err := w.Write([]byte("<StorageClass>STANDARD</StorageClass>")); err != nil {
			a.logger.Error("failed to write storage class", zap.Error(err))
			return
		}
		if _, err := w.Write([]byte("</Contents>")); err != nil {
			a.logger.Error("failed to write contents closing tag", zap.Error(err))
			return
		}
	}

	if _, err := w.Write([]byte("</ListBucketResult>")); err != nil {
		a.logger.Error("failed to write closing tag", zap.Error(err))
		return
	}
}
