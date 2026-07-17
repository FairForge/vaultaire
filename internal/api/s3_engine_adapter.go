package api

import (
	"bufio"
	"bytes"
	"context"
	"crypto/md5"    // #nosec G501 — S3 spec requires MD5 for ETags
	"crypto/sha256" // chunk integrity verification on read
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/FairForge/vaultaire/internal/crypto"
	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/FairForge/vaultaire/internal/tenant"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// chunkContainer is the shared, tenant-independent container that stores
// deduplicated content-defined chunks. Dedup is global — identical content is
// stored once across all tenants — so chunks must live outside any tenant's
// namespace, otherwise an object that dedups against a chunk first written by a
// different tenant or bucket would have no reachable copy.
//
// Isolation is preserved at the manifest layer: a tenant can only reach a chunk
// through its own tenant_chunk_refs rows (queried by tenant_id in
// GetObjectChunks). The container itself is not addressable via the S3 API,
// since every S3 request is routed through the tenant namespace
// ("tenant/{id}/{bucket}"), never the bare "_global" prefix.
const chunkContainer = "_global"

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
}

// NewS3ToEngine creates a new adapter
func NewS3ToEngine(e engine.Engine, db *sql.DB, logger *zap.Logger) *S3ToEngine {
	return &S3ToEngine{
		engine:    e,
		db:        db,
		logger:    logger,
		notifySvc: NewNotificationDispatcher(db, logger),
	}
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
		var isDeleteMarker bool
		var vETag, vContentType string
		var vSize int64
		var vCreatedAt time.Time
		err := a.db.QueryRowContext(r.Context(), `
			SELECT is_delete_marker, etag, content_type, size_bytes, created_at
			FROM object_versions
			WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3 AND version_id = $4`,
			t.ID, bucket, artifact, reqVersionID).Scan(&isDeleteMarker, &vETag, &vContentType, &vSize, &vCreatedAt)
		if err != nil {
			WriteS3Error(w, ErrNoSuchVersion, r.URL.Path, generateRequestID())
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
	var cachedIsChunked bool
	var cacheHit bool
	if a.db != nil {
		err := a.db.QueryRowContext(r.Context(), `
			SELECT content_type, size_bytes, etag, updated_at, COALESCE(metadata, '{}'), COALESCE(backend_name, ''), COALESCE(encryption_algorithm, ''), COALESCE(tags, '{}'), COALESCE(content_disposition, ''), is_chunked
			FROM object_head_cache
			WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3`,
			t.ID, bucket, artifact).Scan(&cachedContentType, &cachedSize, &cachedETag, &cachedUpdatedAt, &cachedMetadata, &cachedBackendName, &cachedEncAlgo, &cachedTags, &cachedContentDisposition, &cachedIsChunked)
		if err == nil {
			cacheHit = true
		}
	}

	// Seed the engine's in-memory routing map so GET goes directly to the
	// correct backend instead of failing over from primary on restart.
	if cacheHit && cachedBackendName != "" {
		if ce, ok := a.engine.(*engine.CoreEngine); ok {
			ce.HintBackend(container, artifact, cachedBackendName)
		}
	}

	if cacheHit && a.db != nil {
		go func() {
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

	// Chunked objects are reassembled from their individual chunk storage keys
	// rather than fetched as a single artifact. On any failure we fall through
	// to the normal path (which will surface NoSuchKey, since no whole-object
	// blob exists at this key). The full object is buffered before any response
	// bytes are written, so fallthrough is always safe.
	if cacheHit && cachedIsChunked && a.gci != nil {
		chunkErr := a.handleChunkedGet(w, r, t, bucket, artifact,
			cachedSize, cachedETag, cachedContentType, cachedUpdatedAt,
			cachedMetadata, cachedTags, cachedContentDisposition, cachedBackendName)
		if chunkErr == nil {
			return
		}
		a.logger.Warn("chunked get failed, falling through to normal path",
			zap.Error(chunkErr),
			zap.String("bucket", bucket),
			zap.String("artifact", artifact))
	}

	reader, err := a.engine.Get(r.Context(), container, artifact)
	if err != nil {
		if errors.Is(err, engine.ErrAllBackendsUnavailable) {
			w.Header().Set("Retry-After", "30")
			WriteS3Error(w, ErrServiceUnavailable, r.URL.Path, generateRequestID())
		} else if strings.Contains(err.Error(), "no such file or directory") ||
			strings.Contains(err.Error(), "not found") {
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
	} else if cachedEncAlgo != "" && a.sseService != nil {
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

	rangeHeader := r.Header.Get("Range")
	if rangeHeader != "" && cacheHit && cachedSize > 0 {
		rng, parseErr := parseRangeHeader(rangeHeader, cachedSize)
		if parseErr != nil {
			writeRangeNotSatisfiable(w, cachedSize)
			return
		}

		// Use backend-native range GET when available (avoids downloading the
		// full object and discarding prefix bytes — 10-50× faster for large files).
		rangeReader := io.Reader(dataReader)
		if ce, ok := a.engine.(*engine.CoreEngine); ok {
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
	w.Header().Set("Cache-Control", "private, no-cache")
	w.Header().Set("x-amz-storage-class", engine.BackendToStorageClass(cachedBackendName))
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
	if decoded := r.Header.Get("x-amz-decoded-content-length"); decoded != "" {
		if n, err := strconv.ParseInt(decoded, 10, 64); err == nil {
			size = n
		}
	}
	if size < 0 {
		size = 0
	}

	if a.db != nil {
		var existingETag string
		existsErr := a.db.QueryRowContext(r.Context(), `
			SELECT etag FROM object_head_cache
			WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3`,
			t.ID, bucket, artifact).Scan(&existingETag)
		if existsErr == nil {
			if lockErr := checkObjectLock(r.Context(), a.db, t.ID, bucket, artifact, isObjectLockBypass(r)); lockErr != nil {
				WriteS3Error(w, ErrObjectLocked, r.URL.Path, generateRequestID())
				return
			}
			if r.Header.Get("If-Match") != "" && checkIfMatch(r, existingETag) {
				w.WriteHeader(http.StatusPreconditionFailed)
				return
			}
		}
	}

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
	var body io.Reader = r.Body
	if chunked {
		body = newAWSChunkedReader(r.Body)
	}

	hasher := md5.New() // #nosec G401 — S3 spec requires MD5 for ETags
	hashingBody := io.TeeReader(body, hasher)

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
	_, tenantUUIDErr := uuid.Parse(t.ID)
	willChunkEncrypt := a.gci != nil && a.chunkEncSvc != nil &&
		metadataSize > chunkThreshold && tenantUUIDErr == nil

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
	// When chunkEncSvc is set, per-chunk convergent encryption handles oversize
	// objects via the chunked path — no 256 MiB limit applies.
	if a.sseService != nil && a.chunkEncSvc == nil && encryptionAlgorithm == "" && metadataSize > crypto.MaxEncryptableSize &&
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
	chunkEncryptionAvailable := a.chunkEncSvc != nil
	if a.gci != nil && metadataSize > chunkThreshold && (encryptionAlgorithm == "" || chunkEncryptionAvailable) {
		if tenantUUID, parseErr := uuid.Parse(t.ID); parseErr == nil {
			chunkErr := a.handleChunkedPut(r, w, t, tenantUUID, bucket, artifact, metadataSize, hashingBody, hasher)
			if chunkErr == nil {
				return
			}
			a.logger.Error("chunked upload failed",
				zap.Error(chunkErr),
				zap.String("bucket", bucket),
				zap.String("key", artifact))
			if errors.Is(chunkErr, engine.ErrAllBackendsUnavailable) {
				WriteS3Error(w, ErrServiceUnavailable, r.URL.Path, generateRequestID())
			} else {
				WriteS3Error(w, bodyReadErrorCode(chunkErr), r.URL.Path, generateRequestID())
			}
			return
		}
	}

	storageClass := r.Header.Get("x-amz-storage-class")
	if storageClass == "" {
		storageClass = bucketTierStorageClass(r.Context(), a.db, t.ID, bucket)
	}
	putOpts := []engine.PutOption{engine.WithContentLength(size)}
	if storageClass != "" {
		putOpts = append(putOpts, engine.WithStorageClass(storageClass))
	}

	// Region-aware routing: if the bucket has a non-default region and
	// a region-specific driver is registered, route directly to it.
	var backendName string
	regionDriver := bucketRegionDriver(r.Context(), a.db, a.engine, t.ID, bucket)
	if regionDriver != "" {
		if ce, ok := a.engine.(*engine.CoreEngine); ok {
			if drv, exists := ce.GetDriver(regionDriver); exists {
				putErr := drv.Put(r.Context(), container, artifact, hashingBody, putOpts...)
				if putErr != nil {
					a.logger.Error("region driver put failed",
						zap.Error(putErr),
						zap.String("driver", regionDriver))
					WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
					return
				}
				backendName = regionDriver
				ce.HintBackend(container, artifact, regionDriver)
			}
		}
	}
	if backendName == "" {
		var putErr error
		backendName, putErr = a.engine.Put(r.Context(), container, artifact, hashingBody, putOpts...)
		err = putErr
	}
	if err != nil {
		switch {
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

	etag := fmt.Sprintf("%x", hasher.Sum(nil))

	contentType := r.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	contentDisposition := sanitizeContentDisposition(r.Header.Get("Content-Disposition"))

	userMeta := extractS3Metadata(r)
	if err := validateMetadata(userMeta); err != nil {
		WriteS3Error(w, ErrInvalidRequest, r.URL.Path, generateRequestID())
		return
	}
	metaJSON, _ := json.Marshal(userMeta)

	if a.db != nil {
		_, dbErr := a.db.ExecContext(r.Context(), `
			INSERT INTO object_head_cache
				(tenant_id, bucket, object_key, size_bytes, etag, content_type, backend_name, metadata, encryption_algorithm, content_disposition, is_chunked, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, FALSE, NOW())
			ON CONFLICT (tenant_id, bucket, object_key) DO UPDATE SET
				size_bytes            = EXCLUDED.size_bytes,
				etag                  = EXCLUDED.etag,
				content_type          = EXCLUDED.content_type,
				backend_name          = EXCLUDED.backend_name,
				metadata              = EXCLUDED.metadata,
				encryption_algorithm  = EXCLUDED.encryption_algorithm,
				content_disposition   = EXCLUDED.content_disposition,
				is_chunked            = EXCLUDED.is_chunked,
				updated_at            = NOW()
		`, t.ID, bucket, artifact, metadataSize, etag, contentType, backendName, metaJSON, encryptionAlgorithm, contentDisposition)
		if dbErr != nil {
			a.logger.Error("failed to cache object metadata",
				zap.Error(dbErr),
				zap.String("tenant_id", t.ID),
				zap.String("bucket", bucket),
				zap.String("object", artifact))
		}
	}

	versionID := ""
	vStatus := getBucketVersioningStatus(r.Context(), a.db, t.ID, bucket)
	if a.db != nil && (vStatus == "Enabled" || vStatus == "Suspended") {
		if vStatus == "Enabled" {
			versionID = generateVersionID()
		} else {
			versionID = "null"
		}

		_, _ = a.db.ExecContext(r.Context(), `
			UPDATE object_versions SET is_latest = FALSE
			WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3 AND is_latest = TRUE`,
			t.ID, bucket, artifact)

		_, _ = a.db.ExecContext(r.Context(), `
			INSERT INTO object_versions
				(tenant_id, bucket, object_key, version_id, size_bytes, etag, content_type, is_latest, is_delete_marker, backend_name)
			VALUES ($1, $2, $3, $4, $5, $6, $7, TRUE, FALSE, $8)
			ON CONFLICT (tenant_id, bucket, object_key, version_id) DO UPDATE SET
				size_bytes = EXCLUDED.size_bytes, etag = EXCLUDED.etag,
				content_type = EXCLUDED.content_type, is_latest = TRUE,
				is_delete_marker = FALSE, backend_name = EXCLUDED.backend_name`,
			t.ID, bucket, artifact, versionID, metadataSize, etag, contentType, backendName)
	}

	applyObjectLockOnPut(r.Context(), a.db, t.ID, bucket, artifact, r)

	w.Header().Set("ETag", fmt.Sprintf(`"%s"`, etag))
	w.Header().Set("x-amz-request-id", generateRequestID())
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

// handleChunkedPut splits a large object into content-defined chunks,
// deduplicates via the Global Content Index, and stores each unique chunk
// individually. Returns nil on success (response already written) or an
// error to signal fallback to the normal path.
func (a *S3ToEngine) handleChunkedPut(
	r *http.Request, w http.ResponseWriter,
	t *tenant.Tenant, tenantUUID uuid.UUID,
	bucket, artifact string,
	metadataSize int64,
	hashingBody io.Reader,
	hasher hash.Hash,
) error {
	ctx := r.Context()

	chunker, err := crypto.DefaultFastCDCChunker()
	if err != nil {
		return fmt.Errorf("create chunker: %w", err)
	}

	chunkCh, err := chunker.ChunkContext(ctx, hashingBody)
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
		dedupScope = t.ID
	}

	newRefs := make([]crypto.TenantChunkRef, 0, 16)
	for result := range chunkCh {
		if result.Err != nil {
			return fmt.Errorf("chunking stream: %w", result.Err)
		}

		chunk := &result.Chunk
		measuredSize += int64(chunk.Size)
		chunkCount++

		lookup, lookupErr := a.gci.LookupChunk(ctx, dedupScope, chunk.Hash)
		if lookupErr != nil {
			return fmt.Errorf("lookup chunk %s: %w", chunk.Hash[:16], lookupErr)
		}

		// Encrypted chunks are namespaced by tenant in the object store too, so
		// two tenants' ciphertexts for the same plaintext never collide.
		storageKey := "_chunks/" + chunk.Hash
		if encrypting {
			storageKey = "_chunks/" + t.ID + "/" + chunk.Hash
		}
		var ciphertextHash string

		if lookup.IsNewChunk {
			storeData := chunk.Data
			var compressedSize *int64
			var compressionAlgo *string
			var encrypted bool
			var encryptionAlgo *string

			if crypto.ShouldCompress(chunk.Data, contentType) {
				compressed, compErr := crypto.CompressBuffer(chunk.Data)
				if compErr == nil && len(compressed) < chunk.Size {
					storeData = compressed
					cs := int64(len(compressed))
					compressedSize = &cs
					algo := "zstd"
					compressionAlgo = &algo
				}
			}

			if a.chunkEncSvc != nil {
				ct, ctHash, encErr := a.chunkEncSvc.EncryptChunkData(t.ID, chunk.Hash, storeData)
				if encErr != nil {
					return fmt.Errorf("encrypt chunk %s: %w", chunk.Hash[:16], encErr)
				}
				storeData = ct
				ciphertextHash = ctHash
				encrypted = true
				algo := "AES256-CE"
				encryptionAlgo = &algo
			}

			storedBytes := int64(len(storeData))
			chunkOpts := []engine.PutOption{engine.WithContentLength(storedBytes)}
			bn, putErr := a.engine.Put(ctx, chunkContainer, storageKey, bytes.NewReader(storeData), chunkOpts...)
			if putErr != nil {
				return fmt.Errorf("store chunk %s: %w", chunk.Hash[:16], putErr)
			}
			if backendName == "chunked" {
				backendName = bn
			}

			var entryCiphertextHash *string
			if ciphertextHash != "" {
				entryCiphertextHash = &ciphertextHash
			}
			if insertErr := a.gci.InsertChunk(ctx, &crypto.GCIEntry{
				DedupScope:      dedupScope,
				PlaintextHash:   chunk.Hash,
				BackendID:       bn,
				StorageKey:      storageKey,
				SizeBytes:       int64(chunk.Size),
				CompressedSize:  compressedSize,
				CompressionAlgo: compressionAlgo,
				Encrypted:       encrypted,
				EncryptionAlgo:  encryptionAlgo,
				CiphertextHash:  entryCiphertextHash,
				RefCount:        1,
			}); insertErr != nil {
				return fmt.Errorf("insert chunk index %s: %w", chunk.Hash[:16], insertErr)
			}
			physicalSize += storedBytes

		} else {
			if incErr := a.gci.IncrementRef(ctx, dedupScope, chunk.Hash); incErr != nil {
				return fmt.Errorf("increment ref %s: %w", chunk.Hash[:16], incErr)
			}
		}

		// The ciphertext hash describes the blob that is actually stored, so on
		// a dedup hit it is copied from the index row — never recomputed. The
		// stored blob's compression was decided by the FIRST upload's
		// Content-Type; recomputing under the current request's Content-Type
		// (or a different zstd version) can hash a blob that was never stored,
		// making the object fail its integrity check on every GET.
		var refCiphertextHash *string
		if ciphertextHash != "" {
			refCiphertextHash = &ciphertextHash
		} else if lookup.Entry != nil && lookup.Entry.CiphertextHash != nil {
			refCiphertextHash = lookup.Entry.CiphertextHash
		}

		newRefs = append(newRefs, crypto.TenantChunkRef{
			TenantID:             tenantUUID,
			BucketName:           bucket,
			ObjectKey:            artifact,
			ChunkIndex:           chunk.Index,
			ChunkOffset:          chunk.Offset,
			PlaintextHash:        chunk.Hash,
			DedupScope:           dedupScope,
			EncryptionKeyVersion: 1,
			CiphertextHash:       refCiphertextHash,
		})
	}

	if physicalSize == 0 {
		physicalSize = measuredSize
	}
	dedupRatio := float32(1.0)
	if physicalSize > 0 {
		dedupRatio = float32(measuredSize) / float32(physicalSize)
	}

	// Atomically install the new manifest and release any previous version's
	// chunk references (single transaction — no stale refs, no ref leak).
	if metaErr := a.gci.ReplaceObjectManifest(ctx, tenantUUID, bucket, artifact, newRefs, &crypto.ObjectMeta{
		TenantID:     tenantUUID,
		BucketName:   bucket,
		ObjectKey:    artifact,
		TotalSize:    measuredSize,
		ChunkCount:   chunkCount,
		ContentType:  &contentType,
		LogicalSize:  measuredSize,
		PhysicalSize: &physicalSize,
		DedupRatio:   &dedupRatio,
	}); metaErr != nil {
		return fmt.Errorf("replace object manifest: %w", metaErr)
	}

	etag := fmt.Sprintf("%x", hasher.Sum(nil))

	contentDisposition := sanitizeContentDisposition(r.Header.Get("Content-Disposition"))
	userMeta := extractS3Metadata(r)
	_ = validateMetadata(userMeta)
	metaJSON, _ := json.Marshal(userMeta)

	chunkEncAlgo := ""
	if a.chunkEncSvc != nil {
		chunkEncAlgo = "AES256-CE"
	}

	if a.db != nil {
		_, _ = a.db.ExecContext(ctx, `
			INSERT INTO object_head_cache
				(tenant_id, bucket, object_key, size_bytes, etag, content_type, backend_name, metadata, encryption_algorithm, content_disposition, is_chunked, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, TRUE, NOW())
			ON CONFLICT (tenant_id, bucket, object_key) DO UPDATE SET
				size_bytes            = EXCLUDED.size_bytes,
				etag                  = EXCLUDED.etag,
				content_type          = EXCLUDED.content_type,
				backend_name          = EXCLUDED.backend_name,
				metadata              = EXCLUDED.metadata,
				encryption_algorithm  = EXCLUDED.encryption_algorithm,
				content_disposition   = EXCLUDED.content_disposition,
				is_chunked            = EXCLUDED.is_chunked,
				updated_at            = NOW()
		`, t.ID, bucket, artifact, measuredSize, etag, contentType, backendName, metaJSON, chunkEncAlgo, contentDisposition)
	}

	versionID := ""
	vStatus := getBucketVersioningStatus(ctx, a.db, t.ID, bucket)
	if a.db != nil && (vStatus == "Enabled" || vStatus == "Suspended") {
		if vStatus == "Enabled" {
			versionID = generateVersionID()
		} else {
			versionID = "null"
		}
		_, _ = a.db.ExecContext(ctx, `
			UPDATE object_versions SET is_latest = FALSE
			WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3 AND is_latest = TRUE`,
			t.ID, bucket, artifact)
		_, _ = a.db.ExecContext(ctx, `
			INSERT INTO object_versions
				(tenant_id, bucket, object_key, version_id, size_bytes, etag, content_type, is_latest, is_delete_marker, backend_name)
			VALUES ($1, $2, $3, $4, $5, $6, $7, TRUE, FALSE, $8)
			ON CONFLICT (tenant_id, bucket, object_key, version_id) DO UPDATE SET
				size_bytes = EXCLUDED.size_bytes, etag = EXCLUDED.etag,
				content_type = EXCLUDED.content_type, is_latest = TRUE,
				is_delete_marker = FALSE, backend_name = EXCLUDED.backend_name`,
			t.ID, bucket, artifact, versionID, measuredSize, etag, contentType, backendName)
	}

	applyObjectLockOnPut(ctx, a.db, t.ID, bucket, artifact, r)

	w.Header().Set("ETag", fmt.Sprintf(`"%s"`, etag))
	w.Header().Set("x-amz-request-id", generateRequestID())
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

// errChunkIntegrity signals that a fetched chunk's bytes did not hash to its
// expected plaintext hash. The object exists but is corrupt — this is distinct
// from a missing/unresolvable manifest (which falls through to NoSuchKey).
var errChunkIntegrity = errors.New("chunk integrity verification failed")

// chunkDesc is a resolved chunk location + its byte position within the object.
type chunkDesc struct {
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
	// Hint the backend that holds this chunk so retrieval is deterministic after
	// a restart (when the engine's in-memory routing map is cold).
	if d.backendID != "" {
		if ce, ok := a.engine.(*engine.CoreEngine); ok {
			ce.HintBackend(chunkContainer, d.storageKey, d.backendID)
		}
	}

	rdr, err := a.engine.Get(ctx, chunkContainer, d.storageKey)
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
	cachedBackendName string,
) error {
	ctx := r.Context()

	tenantUUID, err := uuid.Parse(t.ID)
	if err != nil {
		return fmt.Errorf("parse tenant UUID: %w", err)
	}

	container := t.NamespaceContainer(bucket)

	refs, err := a.gci.GetObjectChunks(ctx, tenantUUID, bucket, artifact)
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
	if rh := r.Header.Get("Range"); rh != "" && cachedSize > 0 {
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
		w.Header().Set("Cache-Control", "private, no-cache")
		w.Header().Set("x-amz-storage-class", engine.BackendToStorageClass(cachedBackendName))
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

	// Stream the plan. The first chunk is fetched + verified BEFORE headers are
	// committed, so a corrupt first chunk produces a clean 500. After that the
	// status is fixed; a failure aborts the body without serving bad bytes.
	headersWritten := false
	var written int64
	for _, p := range plan {
		data, ferr := a.fetchAndVerifyChunk(ctx, p.desc, t.ID)
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
				// Unresolved/unavailable before any byte is written — fall through.
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

		_, _ = a.db.ExecContext(r.Context(), `
			DELETE FROM object_head_cache
			WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3`,
			t.ID, bucket, object)

		w.Header().Set("x-amz-version-id", markerID)
		w.Header().Set("x-amz-delete-marker", "true")
		w.WriteHeader(http.StatusNoContent)
		a.notifySvc.Fire(t.ID, bucket, "s3:ObjectRemoved:Delete", object, 0, "")
		emitEvent(r.Context(), a.db, a.logger, "object.deleted", t.ID, map[string]interface{}{
			"bucket": bucket, "key": object,
		})
		return
	}

	if lockErr := checkObjectLock(r.Context(), a.db, t.ID, bucket, object, isObjectLockBypass(r)); lockErr != nil {
		WriteS3Error(w, ErrObjectLocked, r.URL.Path, generateRequestID())
		return
	}

	// Chunked objects: decrement chunk ref counts via GCI instead of
	// deleting from the backend. Actual chunk data stays until GC (Phase 8.7).
	var isChunked bool
	if a.db != nil {
		_ = a.db.QueryRowContext(r.Context(),
			`SELECT is_chunked FROM object_head_cache WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3`,
			t.ID, bucket, object).Scan(&isChunked)
	}

	if isChunked && a.gci != nil {
		if tenantUUID, parseErr := uuid.Parse(t.ID); parseErr == nil {
			if delErr := a.gci.DeleteObjectChunks(r.Context(), tenantUUID, bucket, object); delErr != nil {
				a.logger.Error("chunked delete failed",
					zap.Error(delErr),
					zap.String("tenant_id", t.ID),
					zap.String("bucket", bucket),
					zap.String("object", object))
				WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
				return
			}
		}
	} else {
		if err := a.engine.Delete(r.Context(), container, object); err != nil {
			if strings.Contains(err.Error(), "no such file or directory") ||
				strings.Contains(err.Error(), "not found") {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			a.logger.Error("delete failed",
				zap.String("container", container),
				zap.String("artifact", object),
				zap.Error(err))
			WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
			return
		}
	}

	if a.db != nil {
		_, _ = a.db.ExecContext(r.Context(), `
			DELETE FROM object_head_cache
			WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3
		`, t.ID, bucket, object)
	}

	w.WriteHeader(http.StatusNoContent)
	a.notifySvc.Fire(t.ID, bucket, "s3:ObjectRemoved:Delete", object, 0, "")
	emitEvent(r.Context(), a.db, a.logger, "object.deleted", t.ID, map[string]interface{}{
		"bucket": bucket, "key": object,
	})
}

// bucketRegionDriver returns the engine driver name for a bucket's region.
// Returns "" if the bucket uses the default region or if no region-specific
// driver is registered (non-iDrive backends).
func bucketRegionDriver(ctx context.Context, db *sql.DB, eng engine.Engine, tenantID, bucket string) string {
	if db == nil {
		return ""
	}
	var region string
	err := db.QueryRowContext(ctx,
		"SELECT region FROM buckets WHERE tenant_id = $1 AND name = $2",
		tenantID, bucket).Scan(&region)
	if err != nil || region == "" || region == "us-west-1" {
		return ""
	}
	driverName := "idrive-" + region
	ce, ok := eng.(*engine.CoreEngine)
	if !ok {
		return ""
	}
	if _, exists := ce.GetDriver(driverName); exists {
		return driverName
	}
	return ""
}

var tierPreferenceToStorageClass = map[string]string{
	"performance": "STANDARD",
	"standard":    "STANDARD",
	"archive":     "GLACIER",
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
