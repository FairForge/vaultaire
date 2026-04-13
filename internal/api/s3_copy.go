package api

import (
	"crypto/md5" // #nosec G501 — S3 spec requires MD5 for ETags
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/FairForge/vaultaire/internal/tenant"
	"go.uber.org/zap"
)

// CopyObjectResult is the XML response for a successful CopyObject.
type CopyObjectResult struct {
	XMLName      xml.Name `xml:"CopyObjectResult"`
	ETag         string   `xml:"ETag"`
	LastModified string   `xml:"LastModified"`
}

// handleCopyObject handles S3 CopyObject requests.
//
// S3 spec: PUT /dest-bucket/dest-key with x-amz-copy-source header.
// The source object is streamed through an io.Pipe so the entire object
// is never buffered in memory. The head cache is updated for the new object.
func (s *Server) handleCopyObject(w http.ResponseWriter, r *http.Request, req *S3Request) {
	t, err := tenant.FromContext(r.Context())
	if err != nil || t == nil {
		WriteS3Error(w, ErrAccessDenied, r.URL.Path, generateRequestID())
		return
	}

	// Parse the x-amz-copy-source header: /bucket/key or bucket/key
	copySource := r.Header.Get("x-amz-copy-source")
	srcBucket, srcKey, err := parseCopySource(copySource)
	if err != nil {
		s.logger.Warn("invalid x-amz-copy-source",
			zap.String("copy_source", copySource),
			zap.Error(err))
		WriteS3Error(w, ErrInvalidRequest, r.URL.Path, generateRequestID())
		return
	}

	destBucket := req.Bucket
	destKey := req.Object

	srcContainer := t.NamespaceContainer(srcBucket)
	destContainer := t.NamespaceContainer(destBucket)

	s.logger.Debug("CopyObject",
		zap.String("tenant_id", t.ID),
		zap.String("src_bucket", srcBucket),
		zap.String("src_key", srcKey),
		zap.String("dest_bucket", destBucket),
		zap.String("dest_key", destKey),
		zap.String("src_container", srcContainer),
		zap.String("dest_container", destContainer))

	// Self-copy (same bucket + key) is a metadata-only operation in S3.
	// With local filesystem backends, Put truncates before Get completes,
	// so we handle this as a no-op: verify source exists, return its ETag.
	if srcBucket == destBucket && srcKey == destKey {
		s.handleSelfCopy(w, r, t, srcBucket, srcKey, srcContainer)
		return
	}

	// Read source object.
	reader, err := s.engine.Get(r.Context(), srcContainer, srcKey)
	if err != nil {
		if strings.Contains(err.Error(), "no such file or directory") ||
			strings.Contains(err.Error(), "not found") {
			WriteS3Error(w, ErrNoSuchKey, r.URL.Path, generateRequestID())
		} else {
			s.logger.Error("copy: source get failed",
				zap.Error(err),
				zap.String("container", srcContainer),
				zap.String("key", srcKey))
			WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
		}
		return
	}
	defer func() { _ = reader.Close() }()

	// Stream source through MD5 hasher into destination — no buffering.
	hasher := md5.New() // #nosec G401 — S3 spec requires MD5 for ETags
	tee := io.TeeReader(reader, hasher)

	backendName, err := s.engine.Put(r.Context(), destContainer, destKey, tee)
	if err != nil {
		s.logger.Error("copy: dest put failed",
			zap.Error(err),
			zap.String("container", destContainer),
			zap.String("key", destKey))
		WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
		return
	}

	etag := fmt.Sprintf("%x", hasher.Sum(nil))
	now := time.Now().UTC()

	// Update object_head_cache for the copied object.
	if s.db != nil {
		// Look up source metadata for size and content type.
		var sizeBytes int64
		var contentType string
		err := s.db.QueryRowContext(r.Context(), `
			SELECT size_bytes, content_type
			FROM object_head_cache
			WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3
		`, t.ID, srcBucket, srcKey).Scan(&sizeBytes, &contentType)
		if err != nil {
			// Fallback: we don't know size/content-type from source cache.
			sizeBytes = 0
			contentType = "application/octet-stream"
		}

		_, dbErr := s.db.ExecContext(r.Context(), `
			INSERT INTO object_head_cache
				(tenant_id, bucket, object_key, size_bytes, etag, content_type, backend_name, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
			ON CONFLICT (tenant_id, bucket, object_key) DO UPDATE SET
				size_bytes   = EXCLUDED.size_bytes,
				etag         = EXCLUDED.etag,
				content_type = EXCLUDED.content_type,
				backend_name = EXCLUDED.backend_name,
				updated_at   = EXCLUDED.updated_at
		`, t.ID, destBucket, destKey, sizeBytes, etag, contentType, backendName, now)
		if dbErr != nil {
			s.logger.Error("copy: failed to cache object metadata",
				zap.Error(dbErr),
				zap.String("tenant_id", t.ID),
				zap.String("bucket", destBucket),
				zap.String("key", destKey))
		}
	}

	// Return CopyObjectResult XML.
	result := CopyObjectResult{
		ETag:         fmt.Sprintf(`"%s"`, etag),
		LastModified: now.Format("2006-01-02T15:04:05.000Z"),
	}

	xmlData, err := xml.MarshalIndent(result, "", "  ")
	if err != nil {
		s.logger.Error("copy: XML marshal failed", zap.Error(err))
		WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
		return
	}

	w.Header().Set("Content-Type", "application/xml")
	w.Header().Set("x-amz-request-id", generateRequestID())
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(xml.Header))
	_, _ = w.Write(xmlData)

	s.logger.Info("object copied",
		zap.String("tenant_id", t.ID),
		zap.String("src", srcBucket+"/"+srcKey),
		zap.String("dest", destBucket+"/"+destKey),
		zap.String("backend", backendName),
		zap.String("etag", etag))
}

// handleSelfCopy handles the special case where source and destination are
// the same object. In S3 this is a metadata-refresh no-op. We verify the
// source exists and return its existing ETag.
func (s *Server) handleSelfCopy(w http.ResponseWriter, r *http.Request, t *tenant.Tenant, bucket, key, container string) {
	// Verify the source object exists by reading and computing ETag.
	reader, err := s.engine.Get(r.Context(), container, key)
	if err != nil {
		if strings.Contains(err.Error(), "no such file or directory") ||
			strings.Contains(err.Error(), "not found") {
			WriteS3Error(w, ErrNoSuchKey, r.URL.Path, generateRequestID())
		} else {
			s.logger.Error("self-copy: get failed", zap.Error(err))
			WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
		}
		return
	}
	defer func() { _ = reader.Close() }()

	hasher := md5.New() // #nosec G401 — S3 spec requires MD5 for ETags
	if _, err := io.Copy(hasher, reader); err != nil {
		s.logger.Error("self-copy: read failed", zap.Error(err))
		WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
		return
	}

	etag := fmt.Sprintf("%x", hasher.Sum(nil))
	now := time.Now().UTC()

	result := CopyObjectResult{
		ETag:         fmt.Sprintf(`"%s"`, etag),
		LastModified: now.Format("2006-01-02T15:04:05.000Z"),
	}

	xmlData, err := xml.MarshalIndent(result, "", "  ")
	if err != nil {
		WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
		return
	}

	w.Header().Set("Content-Type", "application/xml")
	w.Header().Set("x-amz-request-id", generateRequestID())
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(xml.Header))
	_, _ = w.Write(xmlData)

	s.logger.Info("self-copy (no-op)",
		zap.String("tenant_id", t.ID),
		zap.String("bucket", bucket),
		zap.String("key", key),
		zap.String("etag", etag))
}

// parseCopySource parses the x-amz-copy-source header value.
// Accepts formats: /bucket/key, bucket/key, /bucket/key?versionId=xxx
// Returns source bucket and key. URL-encoded keys are not decoded (not yet needed).
func parseCopySource(source string) (bucket, key string, err error) {
	if source == "" {
		return "", "", fmt.Errorf("empty copy source")
	}

	// Strip leading slash.
	source = strings.TrimPrefix(source, "/")

	// Strip query string (e.g. ?versionId=...).
	if idx := strings.IndexByte(source, '?'); idx >= 0 {
		source = source[:idx]
	}

	parts := strings.SplitN(source, "/", 2)
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("invalid copy source format: %q", source)
	}

	return parts[0], parts[1], nil
}
