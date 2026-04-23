package api

import (
	"bufio"
	"context"
	"crypto/md5" // #nosec G501 — S3 spec requires MD5 for ETags
	"database/sql"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/FairForge/vaultaire/internal/tenant"
	"go.uber.org/zap"
)

// S3ToEngine adapts S3 requests to engine operations.
type S3ToEngine struct {
	engine engine.Engine
	db     *sql.DB
	logger *zap.Logger
}

// NewS3ToEngine creates a new adapter
func NewS3ToEngine(e engine.Engine, db *sql.DB, logger *zap.Logger) *S3ToEngine {
	return &S3ToEngine{
		engine: e,
		db:     db,
		logger: logger,
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
			WriteS3Error(w, ErrNoSuchKey, r.URL.Path, generateRequestID())
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
			WriteS3Error(w, ErrNoSuchKey, r.URL.Path, generateRequestID())
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
	var cacheHit bool
	if a.db != nil {
		err := a.db.QueryRowContext(r.Context(), `
			SELECT content_type, size_bytes, etag, updated_at
			FROM object_head_cache
			WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3`,
			t.ID, bucket, artifact).Scan(&cachedContentType, &cachedSize, &cachedETag, &cachedUpdatedAt)
		if err == nil {
			cacheHit = true
		}
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

	reader, err := a.engine.Get(r.Context(), container, artifact)
	if err != nil {
		if strings.Contains(err.Error(), "no such file or directory") ||
			strings.Contains(err.Error(), "not found") {
			WriteS3Error(w, ErrNoSuchKey, r.URL.Path, generateRequestID())
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

	rangeHeader := r.Header.Get("Range")
	if rangeHeader != "" && cacheHit && cachedSize > 0 {
		rng, parseErr := parseRangeHeader(rangeHeader, cachedSize)
		if parseErr != nil {
			writeRangeNotSatisfiable(w, cachedSize)
			return
		}
		w.Header().Set("x-amz-request-id", generateRequestID())
		if w.Header().Get("x-amz-version-id") == "" {
			w.Header().Set("x-amz-version-id", "null")
		}
		if err := serveRange(w, reader, rng, cachedSize, contentType); err != nil {
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
	}

	written, err := io.Copy(w, reader)
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

	if r.Header.Get("If-Match") != "" && a.db != nil {
		var currentETag string
		err := a.db.QueryRowContext(r.Context(), `
			SELECT etag FROM object_head_cache
			WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3`,
			t.ID, bucket, artifact).Scan(&currentETag)
		if err == nil && checkIfMatch(r, currentETag) {
			w.WriteHeader(http.StatusPreconditionFailed)
			return
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

	backendName, err := a.engine.Put(r.Context(), container, artifact, hashingBody)
	if err != nil {
		a.logger.Error("engine put failed",
			zap.Error(err),
			zap.String("container", container),
			zap.String("artifact", artifact))
		WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
		return
	}

	etag := fmt.Sprintf("%x", hasher.Sum(nil))

	contentType := r.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	if a.db != nil {
		_, dbErr := a.db.ExecContext(r.Context(), `
			INSERT INTO object_head_cache
				(tenant_id, bucket, object_key, size_bytes, etag, content_type, backend_name, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, NOW())
			ON CONFLICT (tenant_id, bucket, object_key) DO UPDATE SET
				size_bytes   = EXCLUDED.size_bytes,
				etag         = EXCLUDED.etag,
				content_type = EXCLUDED.content_type,
				backend_name = EXCLUDED.backend_name,
				updated_at   = NOW()
		`, t.ID, bucket, artifact, size, etag, contentType, backendName)
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
			t.ID, bucket, artifact, versionID, size, etag, contentType, backendName)
	}

	w.Header().Set("ETag", fmt.Sprintf(`"%s"`, etag))
	w.Header().Set("x-amz-request-id", generateRequestID())
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
		zap.Int64("size", size))
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
		return
	}

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

	if a.db != nil {
		_, _ = a.db.ExecContext(r.Context(), `
			DELETE FROM object_head_cache
			WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3
		`, t.ID, bucket, object)
	}

	w.WriteHeader(http.StatusNoContent)
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
