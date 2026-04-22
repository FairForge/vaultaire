package api

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"
)

func (s *Server) handleCDNRequest(w http.ResponseWriter, r *http.Request) {
	slug := chi.URLParam(r, "slug")
	bucket := chi.URLParam(r, "bucket")
	key := strings.TrimPrefix(chi.URLParam(r, "*"), "/")

	if slug == "" || bucket == "" || key == "" {
		http.NotFound(w, r)
		return
	}

	if s.db == nil {
		http.NotFound(w, r)
		return
	}

	ctx := r.Context()

	var tenantID string
	err := s.db.QueryRowContext(ctx,
		"SELECT id FROM tenants WHERE slug = $1", slug).Scan(&tenantID)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	var visibility, corsOrigins string
	var cacheMaxAgeSecs int
	err = s.db.QueryRowContext(ctx, `
		SELECT visibility, cors_origins, cache_max_age_secs
		FROM buckets WHERE tenant_id = $1 AND name = $2`,
		tenantID, bucket).Scan(&visibility, &corsOrigins, &cacheMaxAgeSecs)
	if err != nil || visibility != "public-read" {
		http.NotFound(w, r)
		return
	}

	if s.cdnRateLimiter != nil && !s.cdnRateLimiter.Allow("cdn:"+slug+":"+bucket) {
		http.NotFound(w, r)
		return
	}

	var sizeBytes int64
	var etag, contentType string
	var updatedAt time.Time
	err = s.db.QueryRowContext(ctx, `
		SELECT size_bytes, etag, content_type, updated_at
		FROM object_head_cache
		WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3`,
		tenantID, bucket, key).Scan(&sizeBytes, &etag, &contentType, &updatedAt)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	cacheControl := fmt.Sprintf("public, max-age=%d, stale-while-revalidate=600", cacheMaxAgeSecs)

	if code := evaluateConditionalGET(r, etag, updatedAt); code == http.StatusNotModified {
		writeNotModified(w, etag, updatedAt, cacheControl)
		return
	} else if code == http.StatusPreconditionFailed {
		w.WriteHeader(http.StatusPreconditionFailed)
		return
	}

	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Length", fmt.Sprintf("%d", sizeBytes))
	w.Header().Set("ETag", fmt.Sprintf(`"%s"`, etag))
	w.Header().Set("Cache-Control", cacheControl)
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Robots-Tag", "noindex, nofollow")
	if !updatedAt.IsZero() {
		w.Header().Set("Last-Modified", updatedAt.UTC().Format(http.TimeFormat))
	}

	if corsOrigins != "" {
		w.Header().Set("Access-Control-Allow-Origin", corsOrigins)
	}

	if r.Method == http.MethodHead {
		return
	}

	container := fmt.Sprintf("%s_%s", tenantID, bucket)
	reader, err := s.engine.Get(ctx, container, key)
	if err != nil {
		s.logger.Error("cdn engine.Get failed",
			zap.String("container", container),
			zap.String("key", key),
			zap.Error(err))
		http.NotFound(w, r)
		return
	}
	defer func() { _ = reader.Close() }()

	rangeHeader := r.Header.Get("Range")
	if rangeHeader != "" && sizeBytes > 0 {
		rng, parseErr := parseRangeHeader(rangeHeader, sizeBytes)
		if parseErr != nil {
			writeRangeNotSatisfiable(w, sizeBytes)
			return
		}
		if err := serveRange(w, reader, rng, sizeBytes, contentType); err != nil {
			s.logger.Error("cdn range serve failed",
				zap.String("key", key),
				zap.Error(err))
			return
		}
		if s.bandwidthTracker != nil {
			s.bandwidthTracker.Record(ctx, tenantID, 0, rng.length)
		}
		return
	}

	written, err := io.Copy(w, reader)
	if err != nil {
		s.logger.Error("cdn stream failed",
			zap.String("key", key),
			zap.Error(err))
		return
	}

	if s.bandwidthTracker != nil {
		s.bandwidthTracker.Record(ctx, tenantID, 0, written)
	}

	s.logger.Debug("cdn served",
		zap.String("slug", slug),
		zap.String("bucket", bucket),
		zap.String("key", key),
		zap.Int64("bytes", written))
}
