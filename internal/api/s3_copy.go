package api

import (
	"crypto/md5" // #nosec G501 — S3 spec requires MD5 for ETags
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
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

// countingReader wraps an io.Reader and tracks the total bytes read.
// Used during CopyObject so the destination size can be persisted from the
// authoritative byte count rather than relying on the source's head_cache row
// (which can be missing or stale for objects written before the cache existed).
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// resolveCopyContentType picks the content-type for a CopyObject destination.
//
// Per S3 spec: when x-amz-metadata-directive is "REPLACE" the request's own
// Content-Type header wins; when "COPY" (the default) the source object's
// content-type is preserved. Either way we fall back to
// application/octet-stream if the chosen source is empty.
func resolveCopyContentType(directive, requestCT, sourceCT string) string {
	if strings.EqualFold(directive, "REPLACE") {
		if requestCT != "" {
			return requestCT
		}
		return "application/octet-stream"
	}
	if sourceCT != "" {
		return sourceCT
	}
	return "application/octet-stream"
}

// handleCopyObject handles S3 CopyObject requests.
//
// S3 spec: PUT /dest-bucket/dest-key with x-amz-copy-source header. The source
// is streamed through a TeeReader into the destination — never buffered in
// memory. The byte count from the wrapping countingReader is used as the
// authoritative size for the destination's head_cache row.
//
// x-amz-metadata-directive selects whether to preserve source metadata
// (default, "COPY") or take it from the request ("REPLACE"). Self-copy is now
// handled by the same Get→Put streaming path because LocalDriver.Put is atomic
// (writes via temp+rename) and no longer truncates the source mid-read.
func (s *Server) handleCopyObject(w http.ResponseWriter, r *http.Request, req *S3Request) {
	t, err := tenant.FromContext(r.Context())
	if err != nil || t == nil {
		WriteS3Error(w, ErrAccessDenied, r.URL.Path, generateRequestID())
		return
	}

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

	directive := r.Header.Get("x-amz-metadata-directive")

	s.logger.Debug("CopyObject",
		zap.String("tenant_id", t.ID),
		zap.String("src_bucket", srcBucket),
		zap.String("src_key", srcKey),
		zap.String("dest_bucket", destBucket),
		zap.String("dest_key", destKey),
		zap.String("directive", directive))

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

	// Stream source → MD5 hasher → destination, tallying bytes as we go so
	// the persisted size never depends on the source cache row being present.
	counter := &countingReader{r: reader}
	hasher := md5.New() // #nosec G401 — S3 spec requires MD5 for ETags
	tee := io.TeeReader(counter, hasher)

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
		// Look up source content-type (for COPY directive). Size is now
		// authoritative from counter.n — no fallback path needed.
		var sourceCT string
		_ = s.db.QueryRowContext(r.Context(), `
			SELECT content_type
			FROM object_head_cache
			WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3
		`, t.ID, srcBucket, srcKey).Scan(&sourceCT)

		contentType := resolveCopyContentType(directive, r.Header.Get("Content-Type"), sourceCT)

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
		`, t.ID, destBucket, destKey, counter.n, etag, contentType, backendName, now)
		if dbErr != nil {
			s.logger.Error("copy: failed to cache object metadata",
				zap.Error(dbErr),
				zap.String("tenant_id", t.ID),
				zap.String("bucket", destBucket),
				zap.String("key", destKey))
		}
	}

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
		zap.String("directive", directive),
		zap.Int64("size", counter.n),
		zap.String("etag", etag))
}

// parseCopySource parses the x-amz-copy-source header value.
//
// Accepts: /bucket/key, bucket/key, /bucket/key?versionId=xxx. The key portion
// is percent-decoded per AWS spec, so x-amz-copy-source: /b/foo%20bar resolves
// to source key "foo bar" (and %2F resolves to a literal '/' inside the key).
func parseCopySource(source string) (bucket, key string, err error) {
	if source == "" {
		return "", "", fmt.Errorf("empty copy source")
	}

	source = strings.TrimPrefix(source, "/")

	if idx := strings.IndexByte(source, '?'); idx >= 0 {
		source = source[:idx]
	}

	parts := strings.SplitN(source, "/", 2)
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("invalid copy source format: %q", source)
	}

	decodedKey, err := url.PathUnescape(parts[1])
	if err != nil {
		return "", "", fmt.Errorf("invalid percent-encoding in copy source key: %w", err)
	}

	return parts[0], decodedKey, nil
}
