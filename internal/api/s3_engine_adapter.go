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
	"github.com/FairForge/vaultaire/internal/parfetch"
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
	// object's logical bytes (WP-1). PUT reservation/settlement lives in the
	// Server layer, which reads putLogicalBytes and displacedBytes after a
	// successful HandlePut: the logical size recorded in object_head_cache,
	// and the previous row's size captured atomically by the upsert (0 when
	// the PUT created the key).
	quota           QuotaManager
	putLogicalBytes int64
	displaced       displacedRow

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

	// If the current chunk is exhausted, read the next chunk header.
	for a.chunkLeft == 0 {
		// Read the chunk size line: "<hex-size>[;chunk-extension]\r\n"
		line, err := a.r.ReadString('\n')
		if err != nil {
			return 0, err
		}
		line = strings.TrimRight(line, "\r\n")

		// Strip chunk extensions (e.g. ";chunk-signature=...")
		if idx := strings.IndexByte(line, ';'); idx >= 0 {
			line = line[:idx]
		}
		line = strings.TrimSpace(line)

		size, err := strconv.ParseInt(line, 16, 64)
		if err != nil {
			return 0, fmt.Errorf("aws-chunked: invalid chunk size %q: %w", line, err)
		}

		if size == 0 {
			// Terminal chunk — drain trailing headers and signal EOF.
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

	// When a chunk is fully consumed, read and discard the trailing CRLF.
	if a.chunkLeft == 0 {
		_, _ = a.r.ReadString('\n')
	}

	return n, err
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
		err := a.db.QueryRowContext(r.Context(), `
			SELECT is_delete_marker, is_latest, etag, content_type, size_bytes, created_at,
			       COALESCE(storage_key, ''), COALESCE(backend_name, ''), COALESCE(encryption_algorithm, '')
			FROM object_versions
			WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3 AND version_id = $4`,
			t.ID, bucket, artifact, reqVersionID).Scan(&isDeleteMarker, &isLatest, &vETag, &vContentType, &vSize, &vCreatedAt,
			&vStorageKey, &vBackend, &vEncAlgo)
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
				storageKey: vStorageKey, backend: vBackend, encAlgo: vEncAlgo}
		}
		if !isDeleteMarker && !isLatest && version == nil && !a.versionBytesIntact(r.Context(), t.ID, bucket, artifact, reqVersionID) {
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
			w.Header().Set("x-amz-version-id", reqVersionID)
			w.Header().Set("x-amz-delete-marker", "true")
			reqID := generateRequestID()
			if suggestion := keySuggestion(r.Context(), a.db, t.ID, bucket, artifact); suggestion != "" {
				WriteS3ErrorWithContext(w, ErrNoSuchKey, r.URL.Path, reqID, WithSuggestion(suggestion))
			} else {
				WriteS3Error(w, ErrNoSuchKey, r.URL.Path, reqID)
			}
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
	var cacheHit bool
	if a.db != nil {
		err := a.db.QueryRowContext(r.Context(), `
			SELECT content_type, size_bytes, etag, updated_at, COALESCE(metadata, '{}'), COALESCE(backend_name, ''), COALESCE(encryption_algorithm, ''), COALESCE(tags, '{}'), COALESCE(content_disposition, ''), COALESCE(content_encoding, ''), COALESCE(content_language, ''), COALESCE(cache_control, ''), COALESCE(http_expires, ''), COALESCE(website_redirect_location, ''), is_chunked, floor, COALESCE(storage_key, '')
			FROM object_head_cache
			WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3`,
			t.ID, bucket, artifact).Scan(&cachedContentType, &cachedSize, &cachedETag, &cachedUpdatedAt, &cachedMetadata, &cachedBackendName, &cachedEncAlgo, &cachedTags, &cachedContentDisposition, &cachedContentEncoding, &cachedContentLanguage, &cachedEcho.CacheControl, &cachedEcho.Expires, &cachedEcho.WebsiteRedirect, &cachedIsChunked, &cachedFloor, &cachedStorageKey)
		if err == nil {
			cacheHit = true
		}
	}
	if version != nil {
		// The version's own row describes its bytes; the head row's user
		// metadata, tags and echo headers belong to the current object and
		// are not sent with an older version's body (no per-version copy of
		// them exists). The floor stays the key's.
		cacheHit = true
		cachedSize, cachedETag, cachedContentType, cachedUpdatedAt = version.size, version.etag, version.contentType, version.createdAt
		cachedBackendName, cachedEncAlgo, cachedStorageKey, cachedIsChunked = version.backend, version.encAlgo, version.storageKey, false
		cachedMetadata, cachedTags = []byte("{}"), []byte("{}")
		cachedContentDisposition, cachedContentEncoding, cachedContentLanguage, cachedEcho = "", "", "", putEchoHeaders{}
	}
	// Where the object's bytes are (WP-R2-1): the row's own address, <key>
	// when it records none.
	_, blob := objectBlobAddress(t.ID, bucket, artifact, cachedStorageKey)

	// Smart-tier read-time promotion: a demoted object being read comes
	// back hot (routing flip inside the grace window, async copy-back after).
	if cacheHit && version == nil && a.smartPromoter != nil && cachedBackendName == a.smartPromoter.ColdBackend && !cachedIsChunked {
		cachedBackendName = a.smartPromoter.OnRead(r.Context(), t.ID, bucket, artifact, cachedETag)
	}

	// Seed the engine's in-memory routing map so GET goes directly to the
	// correct backend instead of failing over from primary on restart.
	if cacheHit && cachedBackendName != "" {
		if !cachedIsChunked { // a chunked row's name is a sentinel, not a route
			noteRecordedBackend(a.engine, a.logger, "get", cachedBackendName)
		}
		if ce, ok := a.engine.(*engine.CoreEngine); ok {
			ce.HintBackend(container, blob, cachedBackendName)
		}
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

	if cacheHit {
		if code := evaluateConditionalGET(r, cachedETag, cachedUpdatedAt); code == http.StatusNotModified {
			writeNotModified(w, cachedETag, cachedUpdatedAt, "private, no-cache")
			return
		} else if code == http.StatusPreconditionFailed {
			w.WriteHeader(http.StatusPreconditionFailed)
			return
		}
	}

	contentType := cachedContentType
	if contentType == "" {
		contentType = a.detectContentType(artifact)
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
		chunkErr := a.handleChunkedGet(w, r, t, bucket, artifact,
			cachedSize, cachedETag, cachedContentType, cachedUpdatedAt,
			cachedMetadata, cachedTags, cachedContentDisposition, cachedContentEncoding, cachedContentLanguage, cachedEcho, storageClass)
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
	}
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
				reader, nativeRange = pr, rng
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
	}
	if reader == nil && err == nil {
		reader, err = a.engine.Get(gctx, container, blob)
	}
	// A vault object whose backend failed — an error, not found through the
	// failover chain, an open breaker — is rebuilt from its parity copy when
	// there is one (WP-VAULT-1). On tape (ErrArchived) is the object's state,
	// not a failure: the restore semantics stay.
	if err != nil && cacheHit && cachedFloor == usage.FloorVault && a.vaultParity != nil && !errors.Is(err, engine.ErrArchived) {
		off, n := int64(0), cachedSize
		if wantRange != nil {
			off, n = wantRange.start, wantRange.length
		}
		pr, perr := a.vaultParity.Open(r.Context(), t.ID, bucket, artifact, blob, cachedETag, cachedSize, off, n)
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
	if cachedEncAlgo == crypto.SSECAlgorithm {
		if !crypto.HasSSECHeaders(r) {
			WriteS3ErrorWithContext(w, ErrAccessDenied, r.URL.Path, generateRequestID(),
				WithSuggestion("This object was encrypted with SSE-C. Provide the encryption key."))
			return
		}
		ssecKey, parseErr := crypto.ParseSSECHeaders(r)
		if parseErr != nil {
			WriteS3ErrorWithContext(w, ErrInvalidRequest, r.URL.Path, generateRequestID(),
				WithSuggestion(parseErr.Error()))
			return
		}

		encBytes, readErr := io.ReadAll(reader)
		if readErr != nil {
			a.logger.Error("failed to read SSE-C encrypted object", zap.Error(readErr))
			WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
			return
		}
		plaintext, decErr := crypto.SSECDecrypt(ssecKey, encBytes)
		for i := range ssecKey {
			ssecKey[i] = 0
		}
		if decErr != nil {
			if errors.Is(decErr, crypto.ErrSSECKeyMismatch) {
				WriteS3ErrorWithContext(w, ErrAccessDenied, r.URL.Path, generateRequestID(),
					WithSuggestion("The provided encryption key does not match."))
				return
			}
			a.logger.Error("SSE-C decryption failed", zap.Error(decErr))
			WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
			return
		}
		dataReader = bytes.NewReader(plaintext)
		w.Header().Set("x-amz-server-side-encryption-customer-algorithm", "AES256")
	} else if cachedEncAlgo != "" {
		if a.sseService == nil {
			// The row says the blob is SSE-S3 ciphertext and this process has
			// no master key (unset, mistyped, rotated on one node). Serving
			// the raw blob as a 200 hands the client garbage cut to the
			// plaintext Content-Length (R8-04) — fail closed instead.
			a.logger.Error("encrypted object read with no SSE service: ENCRYPTION_MASTER_KEY is not configured on this process",
				zap.String("bucket", bucket), zap.String("object", artifact), zap.String("algorithm", cachedEncAlgo))
			WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
			return
		}
		encBytes, readErr := io.ReadAll(reader)
		if readErr != nil {
			a.logger.Error("failed to read encrypted object", zap.Error(readErr))
			WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
			return
		}
		plaintext, decErr := a.sseService.DecryptBytes(r.Context(), t.ID, encBytes)
		if decErr != nil {
			a.logger.Error("SSE-S3 decryption failed", zap.Error(decErr))
			WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
			return
		}
		dataReader = bytes.NewReader(plaintext)
		w.Header().Set("x-amz-server-side-encryption", "AES256")
	}

	// Content-Disposition: ?response-content-disposition overrides the stored
	// value (applies to both presigned and plain authenticated GET, since it's
	// part of the signed request). Set before the range branch so both 200 and
	// 206 responses carry it.
	disposition := r.URL.Query().Get("response-content-disposition")
	if disposition == "" {
		disposition = cachedContentDisposition
	}
	if disposition = sanitizeContentDisposition(disposition); disposition != "" {
		w.Header().Set("Content-Disposition", disposition)
	}
	if cachedContentEncoding != "" {
		w.Header().Set("Content-Encoding", cachedContentEncoding)
	}
	if cachedContentLanguage != "" {
		w.Header().Set("Content-Language", cachedContentLanguage)
	}
	setEchoHeaders(w.Header(), cachedEcho.CacheControl, cachedEcho.Expires, cachedEcho.WebsiteRedirect)

	// The object's identity headers are the same on a 200 and a 206: S3 sends
	// ETag, Last-Modified, the storage class and the user metadata on ranged
	// responses too, and multi-range downloaders (aws-cli, rclone, R2 Sippy on
	// objects >199 MiB) compare the ETag across their ranged GETs — a 206
	// without one read as "upstream ETag changed during read" (2026-10-04).
	setIdentityHeaders := func() {
		w.Header().Set("x-amz-request-id", generateRequestID())
		if w.Header().Get("x-amz-version-id") == "" {
			w.Header().Set("x-amz-version-id", "null")
		}
		w.Header().Set("Accept-Ranges", "bytes")
		if w.Header().Get("Cache-Control") == "" {
			w.Header().Set("Cache-Control", "private, no-cache")
		}
		w.Header().Set("x-amz-storage-class", storageClass)
		if cacheHit {
			if cachedETag != "" {
				w.Header().Set("ETag", fmt.Sprintf(`"%s"`, cachedETag))
			}
			if !cachedUpdatedAt.IsZero() {
				w.Header().Set("Last-Modified", cachedUpdatedAt.UTC().Format(http.TimeFormat))
			}
			setS3MetadataHeaders(w, cachedMetadata)
			if n := tagCount(cachedTags); n > 0 {
				w.Header().Set("x-amz-tagging-count", strconv.Itoa(n))
			}
		}
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
				setIdentityHeaders()
				w.Header().Set("Content-Type", contentType)
				w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", rng.start, rng.end, cachedSize))
				w.Header().Set("Content-Length", strconv.FormatInt(rng.length, 10))
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
		setIdentityHeaders()
		if err := serveRange(w, rangeReader, rng, cachedSize, contentType); err != nil {
			a.logger.Error("range serve failed",
				zap.Error(err),
				zap.String("container", container),
				zap.String("artifact", artifact))
			cutUnlessClientGone(r.Context())
		}
		return
	}

	w.Header().Set("Content-Type", contentType)
	// The stored per-object Cache-Control (set earlier from head cache) wins;
	// "private, no-cache" is only the default for objects without one.
	setIdentityHeaders()
	if cacheHit && cachedSize > 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(cachedSize, 10))
	}

	written, err := io.Copy(w, dataReader)
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

	bodyCounter := &countingReader{r: body}
	hasher := md5.New() // #nosec G401 — S3 spec requires MD5 for ETags
	hashingBody := io.TeeReader(bodyCounter, hasher)

	metadataSize := size
	var encryptionAlgorithm string
	var encryptionKeyVersion *int // SSE-S3: the tenant key version the blob is sealed under (WP-R8-1)

	// A large object takes the chunked path, which — when per-chunk convergent
	// encryption is available — encrypts each chunk itself. Whole-object SSE-S3
	// must be skipped for such objects: otherwise the object is encrypted twice
	// (SSE-S3 then per-chunk) and GET, which only peels the per-chunk layer,
	// returns SSE ciphertext (silent corruption). SSE-S3 is also non-determin-
	// istic (random KEM ciphertext + nonce), which would defeat the determin-
	// istic chunk dedup. Objects >256 MiB already skip SSE-S3 via the size cap;
	// this closes the 64–256 MiB band. (WP-7)
	chunkThreshold := a.chunkingThreshold
	if chunkThreshold <= 0 {
		chunkThreshold = 64 * 1024 * 1024 // 64 MB default
	}
	// Versioned buckets keep the plain path: tenant_chunk_refs has no
	// version_id, so a chunked overwrite destroys the previous version's
	// manifest while object_versions still advertises it as retrievable —
	// silent version data loss. Until manifests are version-aware (post-launch
	// WP), versioning-enabled/suspended buckets store whole objects exactly as
	// they did before chunking existed. Must match the gate below so SSE-S3
	// still applies where chunking is skipped.
	chunkingDisabledByVersioning := false
	if a.db != nil {
		vs := getBucketVersioningStatus(r.Context(), a.db, t.ID, bucket)
		chunkingDisabledByVersioning = vs == "Enabled" || vs == "Suspended"
	}
	// Resolve the storage class once: explicit x-amz-storage-class header wins,
	// then the bucket's tier_preference. Needed this early because a RESILIENT
	// or archive-class (GLACIER/DEEP_ARCHIVE) resolution must keep the object
	// on the PLAIN path — chunk blobs always land on the engine's primary
	// backend (the GCI's `_global` container is shared across tenants and
	// tiers), so a chunked object would silently break the tier's placement
	// promise. For archive that meant >64 MB objects got hot-tier COGS on
	// iDrive while sold as "on tape". Whole objects on Lyve/Geyser are fine
	// (Lyve multiparts at 214 MB/s, Geyser ingests 227 MB/s sustained).
	// Deliberate trade: resilient/archive objects skip dedup.
	// Public-read buckets resolve to PUBLIC (→ the R2 public-bucket/CDN
	// backend) when no cold/resilient tier or explicit header says otherwise;
	// PUBLIC objects stay whole too, so the CDN path can address them as one
	// R2 key.
	resolvedStorageClass := a.storageClass
	if !a.storageClassResolved {
		resolvedStorageClass = resolvePutStorageClass(r.Context(), a.db, a.engine, syncPlacementGate(a.flags), t.ID, bucket,
			r.Header.Get("x-amz-storage-class"))
	}
	chunkingDisabledByTier := storageClassDisablesChunking(resolvedStorageClass)
	// A bucket pinned to a region promises where its bytes live (WP-R7-1):
	// the plain path places through the region's driver or refuses the
	// write, never the primary. A chunk blob has one address on the engine's
	// primary (WP-R8-7) and a dedup hit may reference a chunk any tenant
	// uploaded anywhere, so a chunked object cannot keep that promise — and
	// with no driver for the region the plain path refused the PUT while the
	// chunked path accepted it. A pinned bucket stores whole objects. Asked
	// only where it can matter (one bucket read per large PUT).
	chunkingDisabledByRegion := false
	if a.gci != nil && a.db != nil && metadataSize > chunkThreshold {
		if name, rErr := bucketRegionDriver(r.Context(), a.db, a.engine, t.ID, bucket); name != "" || rErr != nil {
			chunkingDisabledByRegion = true
		}
	}
	// Mirrors the chunked-path gate below exactly — including the `chunking`
	// kill-switch. When this said "the chunk path will encrypt it" but the
	// flag then kept the object out of the chunk path, an SSE bucket's large
	// object was stored as plaintext (R8-03).
	// The flag is read ONCE per request: a refresh between two reads could
	// skip SSE here and then keep the object out of the chunk path below.
	chunkingOn := a.chunkingEnabled(t.ID)
	willChunkEncrypt := a.gci != nil && a.chunkEncSvc != nil &&
		metadataSize > chunkThreshold && !chunkingDisabledByVersioning && !chunkingDisabledByTier &&
		!chunkingDisabledByRegion && chunkingOn

	// Whether the bucket requires encryption, read once and strictly: a
	// database error used to read as "not encrypted" and the body was
	// stored plaintext. A process with no SSE service (no master key on this
	// slot) refuses a write into an sse_enabled bucket instead of storing
	// it plaintext — R8-04's rule for the read, on the write side (WP-R8-1
	// follow-up A1). SSE-C is the customer's own key: not affected.
	bucketSSE, sseErr := bucketSSEEnabled(r.Context(), a.db, t.ID, bucket)
	if sseErr != nil {
		a.logger.Error("PUT: bucket encryption setting unreadable", zap.Error(sseErr), zap.String("bucket", bucket))
		WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
		return
	}
	if bucketSSE && a.sseService == nil && !crypto.HasSSECHeaders(r) {
		a.logger.Error("PUT into an encrypted bucket refused: ENCRYPTION_MASTER_KEY is not configured on this process",
			zap.String("bucket", bucket), zap.String("object", artifact))
		w.Header().Set("Retry-After", "30")
		WriteS3Error(w, ErrServiceUnavailable, r.URL.Path, generateRequestID())
		return
	}

	if crypto.HasSSECHeaders(r) {
		if r.Header.Get("x-amz-server-side-encryption") != "" {
			WriteS3ErrorWithContext(w, ErrInvalidRequest, r.URL.Path, generateRequestID(),
				WithSuggestion("Cannot use SSE-S3 and SSE-C simultaneously."))
			return
		}
		if size <= 0 || size > crypto.MaxEncryptableSize {
			WriteS3Error(w, ErrEntityTooLarge, r.URL.Path, generateRequestID())
			return
		}

		ssecKey, parseErr := crypto.ParseSSECHeaders(r)
		if parseErr != nil {
			WriteS3ErrorWithContext(w, ErrInvalidRequest, r.URL.Path, generateRequestID(),
				WithSuggestion(parseErr.Error()))
			return
		}

		plaintext, readErr := io.ReadAll(hashingBody)
		if readErr != nil {
			a.logger.Error("failed to read plaintext for SSE-C encryption", zap.Error(readErr))
			WriteS3Error(w, bodyReadErrorCode(readErr), r.URL.Path, generateRequestID())
			return
		}
		if sizeDeclared && int64(len(plaintext)) != metadataSize {
			WriteS3Error(w, ErrIncompleteBody, r.URL.Path, generateRequestID())
			return
		}
		if wantMD5 != "" && fmt.Sprintf("%x", hasher.Sum(nil)) != wantMD5 {
			WriteS3Error(w, ErrBadDigest, r.URL.Path, generateRequestID())
			return
		}

		ciphertext, encErr := crypto.SSECEncrypt(ssecKey, plaintext)
		if encErr != nil {
			a.logger.Error("SSE-C encryption failed", zap.Error(encErr))
			WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
			return
		}

		for i := range ssecKey {
			ssecKey[i] = 0
		}

		hashingBody = bytes.NewReader(ciphertext)
		size = int64(len(ciphertext))
		encryptionAlgorithm = crypto.SSECAlgorithm
	} else {
		shouldEncrypt := a.sseService != nil && !willChunkEncrypt && size > 0 && size <= crypto.MaxEncryptableSize &&
			(r.Header.Get("x-amz-server-side-encryption") == "AES256" || bucketSSE)

		if shouldEncrypt {
			if _, err := a.sseService.Current(r.Context(), t.ID); err != nil {
				a.logger.Error("failed to ensure tenant encryption key", zap.Error(err))
				WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
				return
			}

			plaintext, readErr := io.ReadAll(hashingBody)
			if readErr != nil {
				a.logger.Error("failed to read plaintext for encryption", zap.Error(readErr))
				WriteS3Error(w, bodyReadErrorCode(readErr), r.URL.Path, generateRequestID())
				return
			}
			if sizeDeclared && int64(len(plaintext)) != metadataSize {
				WriteS3Error(w, ErrIncompleteBody, r.URL.Path, generateRequestID())
				return
			}
			if wantMD5 != "" && fmt.Sprintf("%x", hasher.Sum(nil)) != wantMD5 {
				WriteS3Error(w, ErrBadDigest, r.URL.Path, generateRequestID())
				return
			}

			ciphertext, keyVersion, encErr := a.sseService.EncryptBytes(r.Context(), t.ID, plaintext)
			if encErr != nil {
				a.logger.Error("SSE-S3 encryption failed", zap.Error(encErr))
				WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
				return
			}
			encryptionKeyVersion = &keyVersion

			hashingBody = bytes.NewReader(ciphertext)
			size = int64(len(ciphertext))
			encryptionAlgorithm = crypto.SSEAlgorithm
		}
	}

	// Encryption-required guard: SSE-S3 encrypts the whole object in memory and
	// is capped at crypto.MaxEncryptableSize. When encryption is required (an
	// explicit x-amz-server-side-encryption header, or a bucket that defaults to
	// SSE) but the object exceeds that cap, SSE was skipped above
	// (encryptionAlgorithm == ""). Silently storing such an object as plaintext
	// would violate the bucket's encryption guarantee, so reject it instead.
	// (SSE-C already rejects oversize objects in its own branch above.)
	// When the object WILL take the chunked path with per-chunk convergent
	// encryption, no 256 MiB limit applies; any other reason it is not going
	// to be encrypted (no chunk service, chunking flag off, versioned bucket,
	// whole-object tier) means it must be refused here (R8-03).
	if a.sseService != nil && !willChunkEncrypt && encryptionAlgorithm == "" && metadataSize > crypto.MaxEncryptableSize &&
		(r.Header.Get("x-amz-server-side-encryption") == "AES256" || bucketSSE) {
		WriteS3ErrorWithContext(w, ErrEntityTooLarge, r.URL.Path, generateRequestID(),
			WithSuggestion("Objects larger than 256 MiB cannot yet be server-side encrypted. Upload without encryption, or split the object."))
		return
	}

	// Chunked upload path: objects above the threshold are split into
	// content-defined chunks and deduplicated via the GCI. When chunkEncSvc
	// is set, per-chunk convergent encryption is applied (Phase 10) —
	// chunking and encryption are no longer mutually exclusive. (chunkThreshold
	// was computed above, where it also gates SSE-S3 skip.)
	// Only UNENCRYPTED bodies may enter the chunked path. At this point
	// encryptionAlgorithm != "" means the body is already ciphertext:
	// SSE-C always (encrypted above with the customer's key — chunking it
	// would chunk ciphertext, then stamp AES256-CE over the SSE-C marker and
	// serve raw ciphertext on GET with no key check), or SSE-S3 when the
	// per-chunk service wasn't available. Per-chunk encryption (AES256-CE)
	// happens INSIDE handleChunkedPut on plaintext; whole-object SSE-S3 was
	// deliberately skipped above (willChunkEncrypt) for bodies heading here.
	if a.gci != nil && metadataSize > chunkThreshold && !chunkingDisabledByVersioning &&
		!chunkingDisabledByTier && !chunkingDisabledByRegion && encryptionAlgorithm == "" && chunkingOn {
		{
			// WP-C: no uuid.Parse gate — tenant IDs are strings ("tenant-<hex>"
			// from registration). The old gate silently skipped chunking for
			// every real tenant.
			chunkErr := a.handleChunkedPut(r, w, t, t.ID, bucket, artifact, metadataSize, hashingBody, hasher, wantMD5)
			if chunkErr == nil {
				return
			}
			a.logger.Error("chunked upload failed",
				zap.Error(chunkErr),
				zap.String("bucket", bucket),
				zap.String("key", artifact))
			switch {
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
		case errors.Is(err, engine.ErrCallerAborted):
			// The client's body broke off or trickled below the minimum
			// rate: its request, never the backend's (Prompt 2b.2 C1).
			a.logger.Info("put body not received",
				zap.String("container", container), zap.String("artifact", artifact), zap.Error(err))
			WriteS3Error(w, bodyReadErrorCode(err), r.URL.Path, generateRequestID())
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
	a.putLogicalBytes = metadataSize

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
		flip, dbErr := commitBlobWrite(r.Context(), a.db, manifestReleaser(a.gci), blobWrite{TenantID: t.ID, Bucket: bucket, Key: artifact,
			Version: &versionWrite{Size: metadataSize, ETag: etag, ContentType: contentType, Backend: backendName,
				Blob: versionBlobRef{StorageKey: blob, Enc: objectEncryption{Algorithm: encryptionAlgorithm, KeyVersion: encryptionKeyVersion}}},
			Upsert: func(tx *sql.Tx) error {
				_, execErr := tx.ExecContext(r.Context(), `
				INSERT INTO object_head_cache
					(tenant_id, bucket, object_key, size_bytes, etag, content_type, backend_name, metadata, encryption_algorithm, content_disposition, content_encoding, content_language, cache_control, http_expires, website_redirect_location, floor, is_chunked, tags, encryption_key_version, storage_key, updated_at)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, FALSE, $17, $18, $19, NOW())
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
					updated_at            = NOW()
			`, t.ID, bucket, artifact, metadataSize, etag, contentType, backendName, metaJSON, encryptionAlgorithm, contentDisposition, contentEncoding, contentLanguage, echoHdrs.CacheControl, echoHdrs.Expires, echoHdrs.WebsiteRedirect, usage.FloorOf(resolvedStorageClass), tagsJSON, encryptionKeyVersion, blob)
				return execErr
			}})
		a.displaced = flip.Displaced
		versionID = flip.VersionID
		if dbErr != nil {
			// HEAD serves exclusively from object_head_cache — returning 200
			// without the row means every subsequent HEAD/GET 404s and the
			// bytes are never billed. Nothing changed for the key: the
			// previous object is intact, the new blob is an orphan queued
			// for the collector, and the client's retry writes afresh.
			a.displaced = displacedRow{}
			a.putLogicalBytes = 0
			retireOrphanWrite(r.Context(), a.db, a.logger, t.ID, bucket, blob, backendName)
			a.logger.Error("failed to cache object metadata — failing PUT",
				zap.Error(dbErr),
				zap.String("tenant_id", t.ID),
				zap.String("bucket", bucket),
				zap.String("object", artifact))
			WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
			return
		}
	}

	applyObjectLockOnPut(r.Context(), a.db, t.ID, bucket, artifact, r)

	w.Header().Set("ETag", fmt.Sprintf(`"%s"`, etag))
	w.Header().Set("x-amz-request-id", generateRequestID())
	// Object size on the PutObject response (newer AWS surface; SDKs expose
	// it as PutObjectOutput.Size and test suites assert it).
	w.Header().Set("x-amz-object-size", strconv.FormatInt(metadataSize, 10))
	if encryptionAlgorithm == crypto.SSECAlgorithm {
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

// handleChunkedPut splits a large object into content-defined chunks,
// deduplicates via the Global Content Index, and stores each unique chunk
// individually. Returns nil on success (response already written) or an
// error to signal fallback to the normal path.
func (a *S3ToEngine) handleChunkedPut(
	r *http.Request, w http.ResponseWriter,
	t *tenant.Tenant, tenantID string,
	bucket, artifact string,
	metadataSize int64,
	hashingBody io.Reader,
	hasher hash.Hash,
	wantMD5 string,
) error {
	// Tag set for the head row (validated in HandlePut before the body was
	// read; re-parsed here because the chunked path builds its own row).
	chunkTags, _ := parseTaggingHeader(r.Header.Get("x-amz-tagging"))
	tagsJSON, _ := json.Marshal(chunkTags)
	ctx := r.Context()

	// pctx cancels the chunker, the store workers, and their DB work as one
	// unit: the first error anywhere stops the whole upload promptly.
	pctx, cancelStores := context.WithCancel(ctx)
	defer cancelStores()

	chunker, err := crypto.DefaultChunker()
	if err != nil {
		return fmt.Errorf("create chunker: %w", err)
	}

	chunkCh, err := chunker.ChunkContext(pctx, hashingBody)
	if err != nil {
		return fmt.Errorf("start streaming chunker: %w", err)
	}

	var physicalSize int64
	var measuredSize int64
	var chunkCount int
	backendName := "chunked"

	contentType := r.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	// Dedup scope: encrypted chunks are namespaced to the tenant (their
	// convergent key is per-tenant, so identical plaintext yields distinct
	// ciphertext — cross-tenant dedup would hand one tenant another's
	// undecryptable bytes). Unencrypted chunks stay globally shared.
	encrypting := a.chunkEncSvc != nil
	dedupScope := crypto.GlobalDedupScope
	if encrypting {
		// The tenant ID becomes the dedup-scope partition, so it must not
		// be the shared scope's name. The S3 front door refuses reserved
		// ids (handleS3Request); this is the same rule where it is
		// load-bearing.
		if engine.IsReservedTenantID(t.ID) {
			return fmt.Errorf("tenant ID is reserved: it collides with the shared dedup scope")
		}
		dedupScope = t.ID
	}

	// New-chunk stores fan out to a bounded worker pool — the store step is a
	// full HTTPS round-trip to the backend, and running stores one at a time
	// capped engine-path uploads at chunk_size ÷ round-trip (~19 MB/s to
	// iDrive). This dispatcher loop stays sequential for every dedup
	// decision; only compress → encrypt → storeChunkLocked runs on workers.
	pool := newChunkStorePool(pctx, a, cancelStores,
		tenantID, t.ID, bucket, artifact, dedupScope, contentType, encrypting)

	// Every chunk processed takes one GCI reference (fresh insert at
	// ref_count 1, ON CONFLICT increment, or IncrementRef on a dedup hit). If
	// the PUT aborts before the manifest install commits, those references are
	// orphaned — nothing ever releases them, so shared chunks become
	// unsweepable forever (F10). Compensate on every error path; after a
	// successful install the manifest owns the references. Decrements run on a
	// cancellation-immune context: the abort may BE a cancellation. This defer
	// only reads pool state after join() — every return below joins first.
	manifestInstalled := false
	defer func() {
		if manifestInstalled {
			return
		}
		compCtx := context.WithoutCancel(ctx)
		for _, ref := range pool.compensationRefs() {
			if _, decErr := a.gci.DecrementRef(compCtx, ref.scope, ref.hash); decErr != nil {
				a.logger.Warn("compensating ref decrement failed after aborted chunked PUT (reconcile will heal)",
					zap.String("hash", ref.hash), zap.Error(decErr))
			}
		}
	}()

	for result := range chunkCh {
		if result.Err != nil {
			pool.fail(fmt.Errorf("chunking stream: %w", result.Err))
			break
		}

		chunk := result.Chunk
		measuredSize += int64(chunk.Size)
		chunkCount++

		if pool.failed() {
			break
		}

		// A duplicate of a hash that is being stored right now waits for that
		// store instead of racing it into a second store of the same blob.
		if pool.noteDuplicate(&chunk) {
			continue
		}

		lookup, lookupErr := a.gci.LookupChunk(pctx, dedupScope, chunk.Hash)
		if lookupErr != nil {
			pool.fail(fmt.Errorf("lookup chunk %s: %w", chunk.Hash[:16], lookupErr))
			break
		}

		mustStore := lookup.IsNewChunk
		if !mustStore {
			rows, incErr := a.gci.IncrementRef(pctx, dedupScope, chunk.Hash)
			if incErr != nil {
				pool.fail(fmt.Errorf("increment ref %s: %w", chunk.Hash[:16], incErr))
				break
			}
			if rows == 0 {
				// The chunk vanished between lookup and increment — GC swept
				// it (the lookup was likely served from a stale cache entry).
				// Treat it as new: re-store the data and re-insert the row.
				// Proceeding without this installs a manifest pointing at
				// deleted data (WP-6).
				a.logger.Warn("dedup hit on vanished chunk — re-storing",
					zap.String("hash", chunk.Hash[:16]),
					zap.String("scope", dedupScope))
				mustStore = true
				lookup.Entry = nil
			}
		}

		if !mustStore {
			// The ciphertext hash describes the blob that is actually stored,
			// so on a dedup hit it is copied from the index row — never
			// recomputed. The stored blob's compression was decided by the
			// FIRST upload's Content-Type; recomputing under the current
			// request's Content-Type (or a different zstd version) can hash a
			// blob that was never stored, making the object fail its
			// integrity check on every GET.
			// The key version likewise (WP-R8-1): a dedup hit after a
			// rotation points at a blob sealed under the OLD version, and
			// the ref says so until the re-encrypt job moves the blob.
			var refCiphertextHash *string
			var refKeyVersion *int
			if lookup.Entry != nil {
				refCiphertextHash = lookup.Entry.CiphertextHash
				refKeyVersion = lookup.Entry.EncryptionKeyVersion
			}
			pool.addRef(chunk.Index, chunk.Offset, chunk.Hash, refCiphertextHash, refKeyVersion)
			continue
		}

		pool.submit(chunk)
	}

	if joinErr := pool.join(); joinErr != nil {
		return joinErr
	}
	physicalSize = pool.physicalSize
	if pool.backendName != "" {
		backendName = pool.backendName
	}
	newRefs := pool.sortedRefs()

	// Reject declared-vs-measured mismatch before installing the manifest:
	// stored chunks without refs are swept by dedup GC, but a manifest with
	// a client-invented logical size would poison billing permanently.
	if putSizeDeclared(r) && measuredSize != metadataSize {
		return fmt.Errorf("declared %d, measured %d: %w",
			metadataSize, measuredSize, errDecodedLengthMismatch)
	}

	// physicalSize stays truthful: 0 means fully deduplicated — this upload
	// added no new physical bytes. (It was previously forced to measuredSize,
	// which reported a perfect dedup as "no dedup" and poisoned any COGS math
	// built on physical_size.) dedup_ratio 0 is the fully-deduped sentinel.
	dedupRatio := float32(0)
	if physicalSize > 0 {
		dedupRatio = float32(measuredSize) / float32(physicalSize)
	}

	etag := fmt.Sprintf("%x", hasher.Sum(nil))
	if wantMD5 != "" && etag != wantMD5 {
		// Before the manifest install: the stored chunks are ref-released by
		// the deferred compensator and swept by GC.
		return fmt.Errorf("content-md5 %s, computed %s: %w", wantMD5, etag, errBadDigest)
	}
	contentDisposition := sanitizeContentDisposition(r.Header.Get("Content-Disposition"))
	contentEncoding := requestContentEncoding(r)
	contentLanguage := requestContentLanguage(r)
	echoHdrs := requestEchoHeaders(r)
	userMeta := extractS3Metadata(r) // validated by HandlePut before the body was read
	metaJSON, _ := json.Marshal(userMeta)

	chunkEncAlgo := ""
	if a.chunkEncSvc != nil {
		chunkEncAlgo = "AES256-CE"
	}

	if a.db == nil {
		return fmt.Errorf("chunked path requires a database")
	}

	// Manifest swap and head-cache upsert commit in ONE transaction. Split
	// across two, a failed upsert left the old manifest already destroyed
	// while the head cache still advertised the old ETag/size — split-brain
	// with no compensating action. atomicHeadUpsert locks the head row first,
	// so concurrent overwrites of the same key serialize before touching the
	// manifest (consistent lock order: head row → manifest tables).
	// The version row is written in the same transaction (the flip,
	// WP-R2-1): a chunked object keeps no whole blob, so its version names
	// none — but a Suspended bucket's null-version rewrite must release and
	// retire the retained bytes it replaces exactly as a whole write does.
	flip, dbErr := commitBlobWrite(ctx, a.db, nil, blobWrite{TenantID: t.ID, Bucket: bucket, Key: artifact,
		Version: &versionWrite{Size: measuredSize, ETag: etag, ContentType: contentType, Backend: backendName},
		Upsert: func(tx *sql.Tx) error {
			if metaErr := a.gci.ReplaceObjectManifestTx(ctx, tx, tenantID, bucket, artifact, newRefs, &crypto.ObjectMeta{
				TenantID:       tenantID,
				BucketName:     bucket,
				ObjectKey:      artifact,
				TotalSize:      measuredSize,
				ChunkCount:     chunkCount,
				ContentType:    &contentType,
				LogicalSize:    measuredSize,
				PhysicalSize:   &physicalSize,
				DedupRatio:     &dedupRatio,
				PipelineConfig: chunkedPipeline(chunker, encrypting),
			}); metaErr != nil {
				return fmt.Errorf("replace object manifest: %w", metaErr)
			}
			_, execErr := tx.ExecContext(ctx, `
			INSERT INTO object_head_cache
				(tenant_id, bucket, object_key, size_bytes, etag, content_type, backend_name, metadata, encryption_algorithm, content_disposition, content_encoding, content_language, cache_control, http_expires, website_redirect_location, floor, is_chunked, tags, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $17, TRUE, $16, NOW())
			ON CONFLICT (tenant_id, bucket, object_key) DO UPDATE SET
				size_bytes            = EXCLUDED.size_bytes,
				etag                  = EXCLUDED.etag,
				content_type          = EXCLUDED.content_type,
				backend_name          = EXCLUDED.backend_name,
				metadata              = EXCLUDED.metadata,
				encryption_algorithm  = EXCLUDED.encryption_algorithm,
				encryption_key_version = NULL, -- versions live on the chunks
				content_disposition   = EXCLUDED.content_disposition,
				content_encoding      = EXCLUDED.content_encoding,
				content_language      = EXCLUDED.content_language,
				cache_control         = EXCLUDED.cache_control,
				http_expires          = EXCLUDED.http_expires,
				website_redirect_location = EXCLUDED.website_redirect_location,
				floor                 = EXCLUDED.floor,
				is_chunked            = EXCLUDED.is_chunked,
				tags                  = EXCLUDED.tags,
				storage_key           = NULL, -- a chunked object has no whole blob
				updated_at            = NOW()
		`, t.ID, bucket, artifact, measuredSize, etag, contentType, backendName, metaJSON, chunkEncAlgo, contentDisposition, contentEncoding, contentLanguage, echoHdrs.CacheControl, echoHdrs.Expires, echoHdrs.WebsiteRedirect, tagsJSON, chunkedObjectFloor)
			return execErr
		}})
	a.displaced = flip.Displaced
	if dbErr != nil {
		// Manifest swap and head upsert rolled back together: the previous
		// version is fully intact (old manifest, old head row) and the retry
		// re-runs the whole swap. This request's ref increments are released
		// by the deferred compensator (F10); chunk blobs whose refs drop to
		// zero are reclaimed by dedup GC.
		a.displaced = displacedRow{}
		a.putLogicalBytes = 0
		return fmt.Errorf("install chunked object %s/%s: %w", bucket, artifact, dbErr)
	}
	manifestInstalled = true
	a.putLogicalBytes = measuredSize

	// A whole-object blob this key held before (every pre-WP-C large object,
	// or a plain-path overwrite; at <key> or at its address) was retired in
	// the install's transaction (the flip, WP-R2-1): nothing can serve it,
	// and the collector removes it after its grace.

	versionID := flip.VersionID

	applyObjectLockOnPut(ctx, a.db, t.ID, bucket, artifact, r)

	w.Header().Set("ETag", fmt.Sprintf(`"%s"`, etag))
	w.Header().Set("x-amz-request-id", generateRequestID())
	w.Header().Set("x-amz-object-size", strconv.FormatInt(measuredSize, 10))
	if versionID != "" {
		w.Header().Set("x-amz-version-id", versionID)
	}
	w.WriteHeader(http.StatusOK)

	a.logger.Info("chunked artifact stored (streaming)",
		zap.String("tenant_id", t.ID),
		zap.String("s3.bucket", bucket),
		zap.String("s3.object", artifact),
		zap.String("backend", backendName),
		zap.String("etag", etag),
		zap.Int("chunks", chunkCount),
		zap.Float32("dedup_ratio", dedupRatio),
		zap.Int64("size", measuredSize))

	a.notifySvc.Fire(t.ID, bucket, "s3:ObjectCreated:Put", artifact, measuredSize, etag)
	emitEvent(ctx, a.db, a.logger, "object.created", t.ID, map[string]interface{}{
		"bucket": bucket, "key": artifact, "size": measuredSize, "etag": etag, "chunked": true,
	})

	return nil
}

// storeChunkLocked stores a chunk's blob and inserts its GCI row while holding
// pg_advisory_xact_lock(hashtext(scope), hashtext(hash)) — the same lock the
// dedup GC sweep takes around its row-delete + blob-delete (WP-6). Without it
// the delete-vs-reref race interleaves: sweep deletes the row, this path
// re-stores blob + row, sweep then deletes the blob out from under the live
// row. The lock serializes the two so the sweep either finishes first (this
// path stores fresh) or sees the re-inserted row's ref_count and skips.
//
// The row is re-checked UNDER the lock (R8-02): two requests that both missed
// the lookup for a new chunk both encode it — possibly differently, since the
// compression decision follows each request's Content-Type — and serialize
// here. Without the re-check the loser overwrote the winner's blob while its
// INSERT ... ON CONFLICT merely bumped the count, leaving the row's
// compression flag and ciphertext hash describing bytes that were no longer
// there: every object sharing the chunk then failed its integrity check on
// every GET. The loser now takes a reference on the winner's row and reports
// the winner's backend and ciphertext hash; its own encoding is discarded.
type chunkStoreResult struct {
	backend        string  // backend holding the blob (the row's when reused)
	ciphertextHash *string // SHA-256 of the blob that is actually stored, nil if unencrypted
	keyVersion     *int    // the tenant key version that blob is sealed under, nil if unencrypted
	reused         bool    // an existing row was found under the lock; nothing was written
}

func (a *S3ToEngine) storeChunkLocked(ctx context.Context, scope, storageKey string, storeData []byte, entry *crypto.GCIEntry) (chunkStoreResult, error) {
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return chunkStoreResult{}, fmt.Errorf("begin chunk store tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx,
		`SELECT pg_advisory_xact_lock(hashtext($1), hashtext($2))`, scope, entry.PlaintextHash); err != nil {
		return chunkStoreResult{}, fmt.Errorf("advisory lock: %w", err)
	}

	var rowBackend string
	var rowCiphertextHash sql.NullString
	var rowKeyVersion sql.NullInt32
	err = tx.QueryRowContext(ctx, `
		SELECT backend_id, ciphertext_hash, encryption_key_version FROM global_content_index
		WHERE dedup_scope = $1 AND plaintext_hash = $2
		FOR UPDATE`, scope, entry.PlaintextHash).Scan(&rowBackend, &rowCiphertextHash, &rowKeyVersion)
	switch {
	case err == nil:
		rows, incErr := a.gci.IncrementRefTx(ctx, tx, scope, entry.PlaintextHash)
		if incErr != nil {
			return chunkStoreResult{}, fmt.Errorf("reference existing chunk: %w", incErr)
		}
		if rows != 1 {
			return chunkStoreResult{}, fmt.Errorf("reference existing chunk: %d rows updated under lock", rows)
		}
		if err := tx.Commit(); err != nil {
			return chunkStoreResult{}, fmt.Errorf("commit chunk reference: %w", err)
		}
		res := chunkStoreResult{backend: rowBackend, reused: true}
		if rowCiphertextHash.Valid {
			h := rowCiphertextHash.String
			res.ciphertextHash = &h
		}
		if rowKeyVersion.Valid {
			v := int(rowKeyVersion.Int32)
			res.keyVersion = &v
		}
		return res, nil
	case errors.Is(err, sql.ErrNoRows):
		// New chunk: store it below.
	default:
		return chunkStoreResult{}, fmt.Errorf("check chunk index under lock: %w", err)
	}

	bn, putErr := a.chunks().put(ctx, storageKey, storeData)
	if putErr != nil {
		return chunkStoreResult{}, putErr
	}
	entry.BackendID = bn

	if insertErr := a.gci.InsertChunkTx(ctx, tx, entry); insertErr != nil {
		return chunkStoreResult{}, fmt.Errorf("insert chunk index: %w", insertErr)
	}
	if err := tx.Commit(); err != nil {
		return chunkStoreResult{}, fmt.Errorf("commit chunk store: %w", err)
	}
	return chunkStoreResult{backend: bn, ciphertextHash: entry.CiphertextHash, keyVersion: entry.EncryptionKeyVersion}, nil
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
	bucket, artifact string,
	cachedSize int64,
	cachedETag string,
	cachedContentType string,
	cachedUpdatedAt time.Time,
	cachedMetadata []byte,
	cachedTags []byte,
	cachedContentDisposition string,
	cachedContentEncoding string,
	cachedContentLanguage string,
	cachedEcho putEchoHeaders,
	storageClass string,
) error {
	ctx := r.Context()

	container := t.NamespaceContainer(bucket)

	refs, err := a.gci.GetObjectChunks(ctx, t.ID, bucket, artifact)
	if err != nil {
		return fmt.Errorf("get object chunks: %w", err)
	}
	if len(refs) == 0 {
		return fmt.Errorf("no chunk references for %s/%s", bucket, artifact)
	}

	// Preflight: resolve every chunk's location from the index without reading
	// data — one query per dedup scope. A missing index entry means the
	// manifest is unresolvable — return an error so HandleGet answers 500
	// before any byte is written.
	descs, err := a.resolveChunkDescs(ctx, refs)
	if err != nil {
		return err
	}

	contentType := cachedContentType
	if contentType == "" {
		contentType = a.detectContentType(artifact)
	}

	// Content-Disposition: ?response-content-disposition overrides the stored
	// value (part of the signed request). Header mutations before the first
	// WriteHeader are buffered, so this is safe to set up front.
	disposition := r.URL.Query().Get("response-content-disposition")
	if disposition == "" {
		disposition = cachedContentDisposition
	}
	if disposition = sanitizeContentDisposition(disposition); disposition != "" {
		w.Header().Set("Content-Disposition", disposition)
	}
	if cachedContentEncoding != "" {
		w.Header().Set("Content-Encoding", cachedContentEncoding)
	}
	if cachedContentLanguage != "" {
		w.Header().Set("Content-Language", cachedContentLanguage)
	}
	setEchoHeaders(w.Header(), cachedEcho.CacheControl, cachedEcho.Expires, cachedEcho.WebsiteRedirect)

	// Build the byte plan: which chunks to read and the (skip, take) slice within
	// each. Full GET takes every chunk whole; a range takes only overlapping
	// chunks, trimmed to the requested bounds.
	type chunkSlice struct {
		desc chunkDesc
		skip int64
		take int64
	}
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
		for _, d := range descs {
			chunkStart := d.offset
			chunkEnd := d.offset + d.size - 1
			if chunkEnd < rng.start {
				continue
			}
			if chunkStart > rng.end {
				break
			}
			skip := int64(0)
			if rng.start > chunkStart {
				skip = rng.start - chunkStart
			}
			takeEnd := chunkEnd
			if rng.end < takeEnd {
				takeEnd = rng.end
			}
			plan = append(plan, chunkSlice{desc: d, skip: skip, take: takeEnd - (chunkStart + skip) + 1})
		}
	} else {
		for _, d := range descs {
			plan = append(plan, chunkSlice{desc: d, skip: 0, take: d.size})
		}
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

	// The object's identity headers are the same on a 200 and a 206 — the
	// same rule the plain path follows (setIdentityHeaders in HandleGet):
	// R2 Sippy reads a chunked object above 199 MiB as several ranged GETs
	// and compares their ETags, and a 206 without one failed every pull of a
	// chunked object with "upstream ETag changed during read" (2026-10-05).
	setIdentityHeaders := func() {
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("x-amz-request-id", generateRequestID())
		if w.Header().Get("x-amz-version-id") == "" {
			w.Header().Set("x-amz-version-id", "null")
		}
		w.Header().Set("Accept-Ranges", "bytes")
		// The stored per-object Cache-Control (set earlier from head cache) wins;
		// "private, no-cache" is only the default for objects without one.
		if w.Header().Get("Cache-Control") == "" {
			w.Header().Set("Cache-Control", "private, no-cache")
		}
		w.Header().Set("x-amz-storage-class", storageClass)
		if cachedETag != "" {
			w.Header().Set("ETag", fmt.Sprintf(`"%s"`, cachedETag))
		}
		if !cachedUpdatedAt.IsZero() {
			w.Header().Set("Last-Modified", cachedUpdatedAt.UTC().Format(http.TimeFormat))
		}
		setS3MetadataHeaders(w, cachedMetadata)
		if n := tagCount(cachedTags); n > 0 {
			w.Header().Set("x-amz-tagging-count", strconv.Itoa(n))
		}
	}
	write200Headers := func() {
		setIdentityHeaders()
		if cachedSize > 0 {
			w.Header().Set("Content-Length", strconv.FormatInt(cachedSize, 10))
		}
	}
	write206Headers := func() {
		setIdentityHeaders()
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", rng.start, rng.end, cachedSize))
		w.Header().Set("Content-Length", strconv.FormatInt(rng.length, 10))
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
	sizes := make([]int64, len(plan))
	var planTotal int64
	for i, p := range plan {
		sizes[i] = p.desc.size
		planTotal += p.take
	}
	streamStart := time.Now()
	stream := parfetch.Start(ctx, a.chunkStreamConfig(), sizes, func(fctx context.Context, i int) ([]byte, error) {
		data, ferr := a.fetchAndVerifyChunk(fctx, plan[i].desc, t.ID)
		if ferr != nil {
			return nil, ferr
		}
		p := plan[i]
		if p.skip != 0 || p.take != int64(len(data)) {
			end := p.skip + p.take
			if end > int64(len(data)) {
				end = int64(len(data))
			}
			data = data[p.skip:end]
		}
		return data, nil
	})
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

	// Chunked objects: decrement chunk ref counts via GCI instead of
	// deleting from the backend. Actual chunk data stays until GC (Phase 8.7).
	// backend_name is the routing truth (the same column GET hints from):
	// without the hint, a DELETE after a restart went to the primary alone,
	// was answered "not found", and the bytes stayed on the real backend
	// forever while the head row — and the customer's bill — went away (R6-05).
	var isChunked bool
	var recordedBackend, storageKey string
	if a.db != nil {
		// A failed read here must not be guessed away: treating a chunked
		// object as whole sends it to the backend delete, which "succeeds" as
		// an idempotent miss, and the head row goes while the manifest and
		// its GCI references leak forever (R8-07).
		if rowErr := a.db.QueryRowContext(r.Context(),
			`SELECT is_chunked, COALESCE(backend_name, ''), COALESCE(storage_key, '') FROM object_head_cache WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3`,
			t.ID, bucket, object).Scan(&isChunked, &recordedBackend, &storageKey); rowErr != nil && !errors.Is(rowErr, sql.ErrNoRows) {
			a.logger.Error("head cache read before delete failed", zap.Error(rowErr),
				zap.String("bucket", bucket), zap.String("object", object))
			WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
			return
		}
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

	if (isChunked && a.gci != nil) || keptByVersion {
		// Nothing to delete on the backend: the chunks stay until dedup GC
		// (the manifest is released together with the head row below, in one
		// transaction, R8-08), and a version's bytes stay with the version.
	} else {
		if recordedBackend != "" {
			noteRecordedBackend(a.engine, a.logger, "delete", recordedBackend)
			if ce, ok := a.engine.(*engine.CoreEngine); ok {
				ce.HintBackend(container, blob, recordedBackend)
			}
		}
		if err := a.engine.Delete(r.Context(), container, blob); err != nil {
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
	if err := a.objectDeleteAftermath().settle(r.Context(), t.ID, bucket, object, isChunked); err != nil {
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
	if db == nil {
		return false, nil
	}
	var enabled bool
	err := db.QueryRowContext(ctx,
		"SELECT sse_enabled FROM buckets WHERE tenant_id = $1 AND name = $2",
		tenantID, bucket).Scan(&enabled)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read sse_enabled of %s: %w", bucket, err)
	}
	return enabled, nil
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
