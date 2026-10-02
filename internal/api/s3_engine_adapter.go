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

	// chunkGetPrefetch bounds how many chunks a chunked GET fetches ahead of
	// the write cursor (default defaultChunkGetPrefetch; env
	// CHUNK_GET_PREFETCH via the Server; 1 = sequential fetches).
	chunkGetPrefetch int

	// smartPromoter brings Smart-demoted objects back hot on read (5.15.8
	// PR B). Nil = no promotion (tests, callers that never set it).
	smartPromoter *SmartPromoter

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

	if reqVersionID != "" && a.db != nil {
		var isDeleteMarker, isLatest bool
		var vETag, vContentType string
		var vSize int64
		var vCreatedAt time.Time
		err := a.db.QueryRowContext(r.Context(), `
			SELECT is_delete_marker, is_latest, etag, content_type, size_bytes, created_at
			FROM object_versions
			WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3 AND version_id = $4`,
			t.ID, bucket, artifact, reqVersionID).Scan(&isDeleteMarker, &isLatest, &vETag, &vContentType, &vSize, &vCreatedAt)
		if err != nil {
			WriteS3Error(w, ErrNoSuchVersion, r.URL.Path, generateRequestID())
			return
		}
		if !isDeleteMarker && !isLatest && !a.versionBytesIntact(r.Context(), t.ID, bucket, artifact, reqVersionID) {
			// R2-03: versions are metadata-only — every PUT overwrites the
			// one blob at this key — so the bytes of a non-current version
			// no longer exist. Answering 200 with the CURRENT object's bytes
			// under the requested version id (the previous behaviour) made
			// restore tools restore the wrong data. Fail loudly until
			// WP-R2-1 keeps one blob per version. The one non-current
			// version whose bytes ARE intact — the newest live version
			// behind a delete marker (nothing has overwritten it) — is
			// still served, so an accidental delete stays recoverable.
			w.Header().Set("x-amz-version-id", reqVersionID)
			WriteS3ErrorWithContext(w, ErrNotImplemented, r.URL.Path, generateRequestID(),
				WithSuggestion("Only the current version of an object can be retrieved on this service; non-current versions are listed but their data is not retained."))
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
	var cacheHit bool
	if a.db != nil {
		err := a.db.QueryRowContext(r.Context(), `
			SELECT content_type, size_bytes, etag, updated_at, COALESCE(metadata, '{}'), COALESCE(backend_name, ''), COALESCE(encryption_algorithm, ''), COALESCE(tags, '{}'), COALESCE(content_disposition, ''), COALESCE(content_encoding, ''), COALESCE(content_language, ''), COALESCE(cache_control, ''), COALESCE(http_expires, ''), COALESCE(website_redirect_location, ''), is_chunked, floor
			FROM object_head_cache
			WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3`,
			t.ID, bucket, artifact).Scan(&cachedContentType, &cachedSize, &cachedETag, &cachedUpdatedAt, &cachedMetadata, &cachedBackendName, &cachedEncAlgo, &cachedTags, &cachedContentDisposition, &cachedContentEncoding, &cachedContentLanguage, &cachedEcho.CacheControl, &cachedEcho.Expires, &cachedEcho.WebsiteRedirect, &cachedIsChunked, &cachedFloor)
		if err == nil {
			cacheHit = true
		}
	}

	// Smart-tier read-time promotion: a demoted object being read comes
	// back hot (routing flip inside the grace window, async copy-back after).
	if cacheHit && a.smartPromoter != nil && cachedBackendName == a.smartPromoter.ColdBackend && !cachedIsChunked {
		cachedBackendName = a.smartPromoter.OnRead(r.Context(), t.ID, bucket, artifact, cachedETag)
	}

	// Seed the engine's in-memory routing map so GET goes directly to the
	// correct backend instead of failing over from primary on restart.
	if cacheHit && cachedBackendName != "" {
		if ce, ok := a.engine.(*engine.CoreEngine); ok {
			ce.HintBackend(container, artifact, cachedBackendName)
		}
	}
	// The class this response reports: the floor decides, not the backend
	// the bytes are read from (WP-R13-1) — the same value HEAD and the
	// listings give.
	storageClass := engine.CustomerStorageClass(cachedFloor, cachedBackendName)

	if cacheHit && a.db != nil {
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
	// preflight resolves every chunk before any byte is written, so the 500
	// is always clean.
	if cacheHit && cachedIsChunked && a.gci != nil {
		chunkErr := a.handleChunkedGet(w, r, t, bucket, artifact,
			cachedSize, cachedETag, cachedContentType, cachedUpdatedAt,
			cachedMetadata, cachedTags, cachedContentDisposition, cachedContentEncoding, cachedContentLanguage, cachedEcho, storageClass)
		if chunkErr != nil {
			a.logger.Error("chunked get failed",
				zap.Error(chunkErr),
				zap.String("bucket", bucket),
				zap.String("artifact", artifact))
			WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
		}
		return
	}

	reader, err := a.engine.Get(r.Context(), container, artifact)
	if err != nil {
		if errors.Is(err, engine.ErrAllBackendsUnavailable) {
			w.Header().Set("Retry-After", "30")
			WriteS3Error(w, ErrServiceUnavailable, r.URL.Path, generateRequestID())
		} else if errors.Is(err, engine.ErrArchived) {
			// The bytes are on tape. A downstairs object is restored on the
			// reader's behalf and the answer is a retryable 503 (Smart
			// customers never issue restores themselves); an attic object
			// answers Glacier's 403 InvalidObjectState.
			writeArchivedRead(w, r, a.smartPromoter, t.ID, bucket, artifact, cachedFloor,
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
		if ce, ok := a.engine.(*engine.CoreEngine); ok && cachedEncAlgo == "" {
			if rr, rangeErr := ce.GetRange(r.Context(), container, artifact, rng.start, rng.length); rangeErr == nil {
				defer func() { _ = rr.Close() }()
				rangeReader = rr
				// rangeReader already positioned at rng.start — write headers and copy directly
				w.Header().Set("Content-Type", contentType)
				w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", rng.start, rng.end, cachedSize))
				w.Header().Set("Content-Length", strconv.FormatInt(rng.length, 10))
				w.Header().Set("Accept-Ranges", "bytes")
				w.Header().Set("x-amz-request-id", generateRequestID())
				if w.Header().Get("x-amz-version-id") == "" {
					w.Header().Set("x-amz-version-id", "null")
				}
				w.WriteHeader(http.StatusPartialContent)
				_, _ = io.CopyN(w, rangeReader, rng.length)
				return
			}
		}

		// Fallback: serveRange with full-object reader (old path)
		w.Header().Set("x-amz-request-id", generateRequestID())
		if w.Header().Get("x-amz-version-id") == "" {
			w.Header().Set("x-amz-version-id", "null")
		}
		if err := serveRange(w, rangeReader, rng, cachedSize, contentType); err != nil {
			a.logger.Error("range serve failed",
				zap.Error(err),
				zap.String("container", container),
				zap.String("artifact", artifact))
		}
		return
	}

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
	if cacheHit {
		if cachedSize > 0 {
			w.Header().Set("Content-Length", strconv.FormatInt(cachedSize, 10))
		}
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

	written, err := io.Copy(w, dataReader)
	if err != nil {
		a.logger.Error("failed to stream artifact",
			zap.Error(err),
			zap.String("container", container),
			zap.String("artifact", artifact))
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

	if a.db != nil {
		// The lock lives in object_locks, not on the head row: a retained
		// key whose head row is gone (delete marker, crash between deletes)
		// must still refuse the overwrite — the write below replaces the
		// backend bytes in place (R2-01).
		if lockErr := checkObjectLock(r.Context(), a.db, t.ID, bucket, artifact, isObjectLockBypass(r)); lockErr != nil {
			WriteS3ErrorWithContext(w, ErrAccessDenied, r.URL.Path, generateRequestID(),
				WithSuggestion(lockDeniedHint(r)))
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
		resolvedStorageClass = resolvePutStorageClass(r.Context(), a.db, a.engine, t.ID, bucket,
			r.Header.Get("x-amz-storage-class"))
	}
	chunkingDisabledByTier := storageClassDisablesChunking(resolvedStorageClass)
	// Mirrors the chunked-path gate below exactly — including the `chunking`
	// kill-switch. When this said "the chunk path will encrypt it" but the
	// flag then kept the object out of the chunk path, an SSE bucket's large
	// object was stored as plaintext (R8-03).
	// The flag is read ONCE per request: a refresh between two reads could
	// skip SSE here and then keep the object out of the chunk path below.
	chunkingOn := a.chunkingEnabled(t.ID)
	willChunkEncrypt := a.gci != nil && a.chunkEncSvc != nil &&
		metadataSize > chunkThreshold && !chunkingDisabledByVersioning && !chunkingDisabledByTier &&
		chunkingOn

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
			(r.Header.Get("x-amz-server-side-encryption") == "AES256" ||
				isBucketSSEEnabled(r.Context(), a.db, t.ID, bucket))

		if shouldEncrypt {
			if err := a.sseService.EnsureTenantKey(r.Context(), t.ID); err != nil {
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

			ciphertext, encErr := a.sseService.EncryptBytes(r.Context(), t.ID, plaintext)
			if encErr != nil {
				a.logger.Error("SSE-S3 encryption failed", zap.Error(encErr))
				WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
				return
			}

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
		(r.Header.Get("x-amz-server-side-encryption") == "AES256" ||
			isBucketSSEEnabled(r.Context(), a.db, t.ID, bucket)) {
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
		!chunkingDisabledByTier && encryptionAlgorithm == "" && chunkingOn {
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

	// Placement (region driver for a pinned bucket, else the engine) is the
	// shared helper so multipart complete and CopyObject cannot drift from
	// it again (R3-08).
	var backendName string
	backendName, err = placeObject(r.Context(), a.db, a.engine, t.ID, bucket, container, artifact, hashingBody, putOpts...)
	if err != nil {
		switch {
		case errors.Is(err, errRegionDriverUnavailable):
			// Never fall through to the primary: the bucket promised a region.
			a.logger.Error("region-pinned bucket has no driver — PUT refused",
				zap.String("bucket", bucket), zap.Error(err))
			w.Header().Set("Retry-After", "300")
			WriteS3ErrorWithContext(w, ErrServiceUnavailable, r.URL.Path, generateRequestID(),
				WithSuggestion("This bucket's region is not enabled on this deployment."))
		case errors.Is(err, engine.ErrAllBackendsUnavailable):
			w.Header().Set("Retry-After", "30")
			WriteS3Error(w, ErrServiceUnavailable, r.URL.Path, generateRequestID())
		case errors.Is(err, engine.ErrQuotaExceeded):
			// Quota exhaustion is a client condition, never a 500.
			WriteS3ErrorWithContext(w, ErrQuotaExceeded, r.URL.Path, generateRequestID(),
				WithSuggestion("Storage quota exceeded. Upgrade at https://stored.ge/dashboard/billing"))
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
	// what arrived.
	if !sizeDeclared {
		metadataSize = bodyCounter.n
	} else if bodyCounter.n != metadataSize {
		a.logger.Warn("PUT body length mismatch",
			zap.String("tenant_id", t.ID),
			zap.Bool("aws_chunked", chunked),
			zap.Int64("declared", metadataSize),
			zap.Int64("measured", bodyCounter.n))
		WriteS3Error(w, ErrIncompleteBody, r.URL.Path, generateRequestID())
		return
	}

	etag := fmt.Sprintf("%x", hasher.Sum(nil))
	if wantMD5 != "" && etag != wantMD5 {
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

	if a.db != nil {
		// atomicHeadUpsert locks the previous row and returns its size in
		// the same transaction as the upsert, so the overwritten bytes are
		// captured atomically — a concurrent DELETE cannot double-release.
		displaced, dbErr := atomicHeadUpsertReleasing(r.Context(), a.db, manifestReleaser(a.gci), t.ID, bucket, artifact, func(tx *sql.Tx) error {
			_, execErr := tx.ExecContext(r.Context(), `
				INSERT INTO object_head_cache
					(tenant_id, bucket, object_key, size_bytes, etag, content_type, backend_name, metadata, encryption_algorithm, content_disposition, content_encoding, content_language, cache_control, http_expires, website_redirect_location, floor, is_chunked, tags, updated_at)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, FALSE, $17, NOW())
				ON CONFLICT (tenant_id, bucket, object_key) DO UPDATE SET
					size_bytes            = EXCLUDED.size_bytes,
					etag                  = EXCLUDED.etag,
					content_type          = EXCLUDED.content_type,
					backend_name          = EXCLUDED.backend_name,
					metadata              = EXCLUDED.metadata,
					encryption_algorithm  = EXCLUDED.encryption_algorithm,
					content_disposition   = EXCLUDED.content_disposition,
					content_encoding      = EXCLUDED.content_encoding,
					content_language      = EXCLUDED.content_language,
					cache_control         = EXCLUDED.cache_control,
					http_expires          = EXCLUDED.http_expires,
					website_redirect_location = EXCLUDED.website_redirect_location,
					floor                 = EXCLUDED.floor,
					is_chunked            = EXCLUDED.is_chunked,
					tags                  = EXCLUDED.tags,
					updated_at            = NOW()
			`, t.ID, bucket, artifact, metadataSize, etag, contentType, backendName, metaJSON, encryptionAlgorithm, contentDisposition, contentEncoding, contentLanguage, echoHdrs.CacheControl, echoHdrs.Expires, echoHdrs.WebsiteRedirect, usage.FloorOf(resolvedStorageClass), tagsJSON)
			return execErr
		})
		a.displaced = displaced
		if dbErr != nil {
			// HEAD serves exclusively from object_head_cache — returning 200
			// without the row means every subsequent HEAD/GET 404s and the
			// bytes are never billed. The blob is already durable, so the
			// client's retry is safe and idempotent (upsert). Fail loudly.
			a.displaced = displacedRow{}
			a.putLogicalBytes = 0
			a.logger.Error("failed to cache object metadata — failing PUT",
				zap.Error(dbErr),
				zap.String("tenant_id", t.ID),
				zap.String("bucket", bucket),
				zap.String("object", artifact))
			WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
			return
		}
	}

	// The row this PUT replaced may have routed to ANOTHER backend — a
	// Smart-demoted object rewritten hot, a class or visibility change, a
	// failover. Its blob there has nothing pointing at it any more: remove
	// it (R13-10 — it stayed on tape forever; the chunked path already did
	// this). Same backend = overwritten in place, nothing to do.
	dropDisplacedBlob(r.Context(), a.db, a.engine, a.logger, lostWriteOverwrite,
		t.ID, bucket, container, artifact, a.displaced, backendName)

	versionID := recordObjectVersion(r.Context(), a.db, t.ID, bucket, artifact, metadataSize, etag, contentType, backendName)

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
			var refCiphertextHash *string
			if lookup.Entry != nil && lookup.Entry.CiphertextHash != nil {
				refCiphertextHash = lookup.Entry.CiphertextHash
			}
			pool.addRef(chunk.Index, chunk.Offset, chunk.Hash, refCiphertextHash)
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
	displaced, dbErr := atomicHeadUpsert(ctx, a.db, t.ID, bucket, artifact, func(tx *sql.Tx) error {
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
				content_disposition   = EXCLUDED.content_disposition,
				content_encoding      = EXCLUDED.content_encoding,
				content_language      = EXCLUDED.content_language,
				cache_control         = EXCLUDED.cache_control,
				http_expires          = EXCLUDED.http_expires,
				website_redirect_location = EXCLUDED.website_redirect_location,
				floor                 = EXCLUDED.floor,
				is_chunked            = EXCLUDED.is_chunked,
				tags                  = EXCLUDED.tags,
				updated_at            = NOW()
		`, t.ID, bucket, artifact, measuredSize, etag, contentType, backendName, metaJSON, chunkEncAlgo, contentDisposition, contentEncoding, contentLanguage, echoHdrs.CacheControl, echoHdrs.Expires, echoHdrs.WebsiteRedirect, tagsJSON, chunkedObjectFloor)
		return execErr
	})
	a.displaced = displaced
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

	// A whole-object blob may exist at this key from before the object was
	// chunked (every pre-WP-C large object, or a plain-path overwrite). It is
	// now stale — remove it so nothing can ever serve those bytes. Best-effort:
	// the chunked GET path no longer falls through to the plain path, so a
	// failed delete costs orphaned disk, not wrong data.
	// The displaced row's backend_name is where that blob lives (R8-20): hint
	// it, or a blob that landed off-primary is looked for in the wrong place.
	if displaced.Backend != "" {
		if ce, ok := a.engine.(*engine.CoreEngine); ok {
			ce.HintBackend(t.NamespaceContainer(bucket), artifact, displaced.Backend)
		}
	}
	if blobErr := a.engine.Delete(ctx, t.NamespaceContainer(bucket), artifact); blobErr != nil &&
		!isObjectMissingErr(blobErr) {
		a.logger.Warn("stale whole-object blob delete failed after chunked PUT",
			zap.Error(blobErr), zap.String("bucket", bucket), zap.String("object", artifact))
	}

	versionID := recordObjectVersion(ctx, a.db, t.ID, bucket, artifact, measuredSize, etag, contentType, backendName)

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
	err = tx.QueryRowContext(ctx, `
		SELECT backend_id, ciphertext_hash FROM global_content_index
		WHERE dedup_scope = $1 AND plaintext_hash = $2
		FOR UPDATE`, scope, entry.PlaintextHash).Scan(&rowBackend, &rowCiphertextHash)
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
	return chunkStoreResult{backend: bn, ciphertextHash: entry.CiphertextHash}, nil
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
	if err != nil {
		return nil, fmt.Errorf("fetch chunk %s: %w", d.plaintextHash[:16], err)
	}
	defer func() { _ = rdr.Close() }()

	data, err := io.ReadAll(rdr)
	if err != nil {
		return nil, fmt.Errorf("read chunk %s: %w", d.plaintextHash[:16], err)
	}

	if d.encrypted && a.chunkEncSvc != nil {
		data, err = a.chunkEncSvc.DecryptChunkData(tenantID, d.plaintextHash, data, d.ciphertextHash)
		if err != nil {
			return nil, fmt.Errorf("decrypt chunk %s: %w", d.plaintextHash[:16], err)
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

// handleChunkedGet serves a chunked object by streaming its content-defined
// chunks one at a time, in chunk_index order, directly to the response writer.
// Each chunk is read into a bounded buffer (~16 MB max) and integrity-verified
// before its bytes are written, so peak memory is one chunk — not the whole
// object — and corrupt data is never served. Range requests touch only the
// chunks that overlap the requested byte range (located via chunk_offset).
//
// Fallthrough contract: the manifest is fully resolved (all chunk index entries
// present) BEFORE any byte is written, so a resolution failure returns an error
// and the caller falls through to the normal GET path. Once the first chunk is
// committed the status is fixed; a later integrity/fetch failure aborts the body
// (a short read the client detects) rather than serving bad bytes. A corrupt
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
	// data. A missing index entry means the manifest is unresolvable — return an
	// error so HandleGet falls through (→ NoSuchKey) before any byte is written.
	descs := make([]chunkDesc, len(refs))
	for i, ref := range refs {
		scope := ref.DedupScope
		if scope == "" {
			scope = crypto.GlobalDedupScope
		}
		lookup, lookupErr := a.gci.LookupChunk(ctx, scope, ref.PlaintextHash)
		if lookupErr != nil {
			return fmt.Errorf("lookup chunk %s: %w", ref.PlaintextHash[:16], lookupErr)
		}
		if lookup == nil || lookup.Entry == nil {
			return fmt.Errorf("chunk %s missing from index", ref.PlaintextHash[:16])
		}
		storageKey := lookup.Entry.StorageKey
		if storageKey == "" {
			storageKey = "_chunks/" + ref.PlaintextHash
		}
		// The GCI row's ciphertext hash is authoritative (it was computed from
		// the blob actually stored); per-ref copies are a fallback for rows
		// written before the hash lived on the index.
		var ctHash string
		if lookup.Entry.CiphertextHash != nil {
			ctHash = *lookup.Entry.CiphertextHash
		} else if ref.CiphertextHash != nil {
			ctHash = *ref.CiphertextHash
		}
		descs[i] = chunkDesc{
			scope:          scope,
			storageKey:     storageKey,
			backendID:      lookup.Entry.BackendID,
			plaintextHash:  ref.PlaintextHash,
			offset:         ref.ChunkOffset,
			size:           lookup.Entry.SizeBytes,
			compressed:     lookup.Entry.CompressionAlgo != nil,
			encrypted:      lookup.Entry.Encrypted,
			ciphertextHash: ctHash,
		}
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

	write200Headers := func() {
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
		if cachedSize > 0 {
			w.Header().Set("Content-Length", strconv.FormatInt(cachedSize, 10))
		}
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
	write206Headers := func() {
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", rng.start, rng.end, cachedSize))
		w.Header().Set("Content-Length", strconv.FormatInt(rng.length, 10))
		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("x-amz-request-id", generateRequestID())
		if w.Header().Get("x-amz-version-id") == "" {
			w.Header().Set("x-amz-version-id", "null")
		}
		w.WriteHeader(http.StatusPartialContent)
	}

	// Stream the plan with bounded prefetch: while chunk i streams to the
	// client, up to `prefetch` later chunks are already being fetched and
	// verified — the sequential loop paid one full backend round-trip per
	// chunk, the same ceiling the parallel chunk-store pool removed on PUT.
	// A slot is held from fetch-start until the writer consumes the chunk,
	// so in-flight + fetched-but-unwritten buffers never exceed `prefetch`
	// chunks (≤16 MB each). Results arrive per-index on buffered channels;
	// the writer consumes strictly in index order, so ordering, verification
	// (inside fetchAndVerifyChunk, before any byte is written), and the
	// error contract below are identical to the sequential loop.
	prefetch := a.chunkGetPrefetch
	if prefetch < 1 {
		prefetch = 1
	}
	type fetchOut struct {
		data []byte
		err  error
	}
	fctx, cancelFetch := context.WithCancel(ctx)
	defer cancelFetch()
	results := make([]chan fetchOut, len(plan))
	for i := range results {
		results[i] = make(chan fetchOut, 1) // buffered: a cancelled writer never strands the fetcher
	}
	slots := make(chan struct{}, prefetch)
	go func() {
		for i := range plan {
			select {
			case slots <- struct{}{}:
			case <-fctx.Done():
				return
			}
			go func(i int) {
				data, ferr := a.fetchAndVerifyChunk(fctx, plan[i].desc, t.ID)
				results[i] <- fetchOut{data: data, err: ferr}
			}(i)
		}
	}()

	// The first chunk is fetched + verified BEFORE headers are committed, so
	// a corrupt first chunk produces a clean 500. After that the status is
	// fixed; a failure aborts the body without serving bad bytes.
	headersWritten := false
	var written int64
	for i, p := range plan {
		out := <-results[i]
		data, ferr := out.data, out.err
		<-slots
		if ferr != nil {
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
			a.logger.Error("chunk failure mid-stream; aborting response",
				zap.Error(ferr),
				zap.String("bucket", bucket),
				zap.String("artifact", artifact))
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

		slice := data
		if p.skip != 0 || p.take != int64(len(data)) {
			end := p.skip + p.take
			if end > int64(len(data)) {
				end = int64(len(data))
			}
			slice = data[p.skip:end]
		}
		n, werr := w.Write(slice)
		written += int64(n)
		if werr != nil {
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
		WriteS3ErrorWithContext(w, ErrAccessDenied, r.URL.Path, generateRequestID(),
			WithSuggestion(lockDeniedHint(r)))
		return
	}

	if a.db != nil && (vStatus == "Enabled" || vStatus == "Suspended") && reqVersionID != "" {
		result, err := a.db.ExecContext(r.Context(), `
			DELETE FROM object_versions
			WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3 AND version_id = $4`,
			t.ID, bucket, object, reqVersionID)
		if err != nil {
			a.logger.Error("delete version failed", zap.Error(err))
			WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
			return
		}
		rows, _ := result.RowsAffected()
		if rows == 0 {
			WriteS3Error(w, ErrNoSuchVersion, r.URL.Path, generateRequestID())
			return
		}

		var hasRemaining bool
		_ = a.db.QueryRowContext(r.Context(), `
			SELECT EXISTS(SELECT 1 FROM object_versions
			WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3)`,
			t.ID, bucket, object).Scan(&hasRemaining)

		if hasRemaining {
			_, _ = a.db.ExecContext(r.Context(), `
				UPDATE object_versions SET is_latest = TRUE
				WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3
				AND created_at = (
					SELECT MAX(created_at) FROM object_versions
					WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3
				)`, t.ID, bucket, object)
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
		markerID := generateVersionID()

		_, _ = a.db.ExecContext(r.Context(), `
			UPDATE object_versions SET is_latest = FALSE
			WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3 AND is_latest = TRUE`,
			t.ID, bucket, object)

		_, _ = a.db.ExecContext(r.Context(), `
			INSERT INTO object_versions
				(tenant_id, bucket, object_key, version_id, size_bytes, etag, content_type, is_latest, is_delete_marker)
			VALUES ($1, $2, $3, $4, 0, '', 'application/octet-stream', TRUE, TRUE)`,
			t.ID, bucket, object, markerID)

		// The head-cache row is the billing record (WP-1): DELETE...RETURNING
		// captures the removed size atomically, so a concurrent writer or
		// deleter can never cause the same bytes to be released twice. An
		// object chunked before versioning was enabled releases its manifest
		// in the same transaction (R8-07) — the marker branch used to drop the
		// head row and leak the manifest and its GCI references forever.
		marked, found, delErr := deleteHeadRowReleasing(r.Context(), a.db, manifestReleaser(a.gci), t.ID, bucket, object)
		if delErr != nil {
			a.logger.Error("delete-marker head cache delete failed", zap.Error(delErr),
				zap.String("bucket", bucket), zap.String("object", object))
			WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
			return
		}
		if found {
			a.releaseQuotaForDelete(r, t.ID, marked)
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
	var recordedBackend string
	if a.db != nil {
		// A failed read here must not be guessed away: treating a chunked
		// object as whole sends it to the backend delete, which "succeeds" as
		// an idempotent miss, and the head row goes while the manifest and
		// its GCI references leak forever (R8-07).
		if rowErr := a.db.QueryRowContext(r.Context(),
			`SELECT is_chunked, COALESCE(backend_name, '') FROM object_head_cache WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3`,
			t.ID, bucket, object).Scan(&isChunked, &recordedBackend); rowErr != nil && !errors.Is(rowErr, sql.ErrNoRows) {
			a.logger.Error("head cache read before delete failed", zap.Error(rowErr),
				zap.String("bucket", bucket), zap.String("object", object))
			WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
			return
		}
	}

	if isChunked && a.gci != nil {
		// Nothing to delete on the backend: the chunks stay until dedup GC.
		// The manifest is released together with the head row below, in one
		// transaction (R8-08).
	} else {
		if recordedBackend != "" {
			if ce, ok := a.engine.(*engine.CoreEngine); ok {
				ce.HintBackend(container, object, recordedBackend)
			}
		}
		if err := a.engine.Delete(r.Context(), container, object); err != nil {
			if errors.Is(err, engine.ErrAllBackendsUnavailable) {
				// The backend that holds the bytes is unreachable: the
				// client retries; the head row must NOT be removed (a
				// "miss" verdict from a fallback is not a verdict, R6-02).
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

	if a.db != nil {
		// DELETE...RETURNING releases exactly the bytes this request removed
		// (the row is the billing record — WP-1). Logical size for chunked;
		// a chunked object's manifest goes in the same transaction (R8-08).
		deleted, found, delErr := deleteHeadRowReleasing(r.Context(), a.db, manifestReleaser(a.gci), t.ID, bucket, object)
		switch {
		case delErr != nil && isChunked:
			// The manifest is still intact (rolled back with the row): the
			// client retries. Answering 204 here would leave a live object.
			a.logger.Error("chunked delete failed",
				zap.Error(delErr),
				zap.String("tenant_id", t.ID),
				zap.String("bucket", bucket),
				zap.String("object", object))
			WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
			return
		case delErr != nil:
			// The blob is already gone; the drifted row is the only loss.
			a.logger.Error("head cache delete failed", zap.Error(delErr))
		case found:
			a.releaseQuotaForDelete(r, t.ID, deleted)
		}
		// The retention (expired, governance-bypassed, or none) goes with the
		// object: the PUT-side lock check no longer needs a head row, so a
		// stale lock row would refuse the next upload to this key.
		if _, lockDelErr := a.db.ExecContext(r.Context(), `
			DELETE FROM object_locks WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3`,
			t.ID, bucket, object); lockDelErr != nil {
			a.logger.Error("object lock row delete failed", zap.Error(lockDelErr))
		}
		// A Smart-demoted object has a second copy (the hot one, until it is
		// reclaimed) that the delete above did not reach: remove it now and
		// settle the ledger, so no later job owes this key a delete (WP-R13-2).
		a.smartPromoter.OnDelete(r.Context(), t.ID, bucket, object)
	}

	w.WriteHeader(http.StatusNoContent)
	a.notifySvc.Fire(t.ID, bucket, "s3:ObjectRemoved:Delete", object, 0, "")
	emitEvent(r.Context(), a.db, a.logger, "object.deleted", t.ID, map[string]interface{}{
		"bucket": bucket, "key": object,
	})
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
// egress behind the Cloudflare-proxied cdn.stored.ge host. Without an r2
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
// internal names (PUBLIC → r2, RESILIENT → lyve), legacy AWS classes that
// would map onto hub disk (REDUCED_REDUNDANCY → local) and anything unknown
// fall back to the bucket's own resolution. A cold/resilient bucket tier is
// a placement promise: a header can make an archive object colder
// (DEEP_ARCHIVE) but never hotter, and a resilient bucket never moves.
func resolvePutStorageClass(ctx context.Context, db *sql.DB, eng engine.Engine, tenantID, bucket, header string) string {
	requested := clientStorageClass(header)
	class := bucketTierStorageClass(ctx, db, tenantID, bucket)
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
	case "RESILIENT", "GLACIER", "DEEP_ARCHIVE", "PUBLIC":
		return true
	}
	return false
}

func isBucketSSEEnabled(ctx context.Context, db *sql.DB, tenantID, bucket string) bool {
	if db == nil {
		return false
	}
	var enabled bool
	err := db.QueryRowContext(ctx,
		"SELECT sse_enabled FROM buckets WHERE tenant_id = $1 AND name = $2",
		tenantID, bucket).Scan(&enabled)
	return err == nil && enabled
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
