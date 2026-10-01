package api

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/FairForge/vaultaire/internal/common"
	"github.com/FairForge/vaultaire/internal/engine"
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

	// A suspended tenant serves nothing on the CDN either (Review R14-02,
	// R4-19): the S3 path already refuses it (isTenantSuspended), and the
	// unauthenticated public path is the one an abuse suspension exists for.
	// Same 404 as an unknown slug, so suspension is not an oracle.
	var tenantID string
	err := s.db.QueryRowContext(ctx,
		"SELECT id FROM tenants WHERE slug = $1 AND suspended_at IS NULL", slug).Scan(&tenantID)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	var visibility, corsOrigins string
	var cacheMaxAgeSecs int
	var forceDownload bool
	err = s.db.QueryRowContext(ctx, `
		SELECT visibility, cors_origins, cache_max_age_secs, COALESCE(cdn_force_download, FALSE)
		FROM buckets WHERE tenant_id = $1 AND name = $2`,
		tenantID, bucket).Scan(&visibility, &corsOrigins, &cacheMaxAgeSecs, &forceDownload)
	if err != nil || visibility != "public-read" {
		http.NotFound(w, r)
		return
	}

	if r.Method == http.MethodOptions {
		handleCDNPreflight(w, r, corsOrigins)
		return
	}

	if s.cdnRateLimiter != nil && !s.cdnRateLimiter.Allow("cdn:"+slug+":"+bucket) {
		http.NotFound(w, r)
		return
	}

	if s.cdnAnalytics != nil {
		if _, _, exceeded := s.cdnAnalytics.CheckBudget(ctx, tenantID, bucket); exceeded {
			w.Header().Set("Retry-After", "3600")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":"bandwidth budget exceeded for this bucket"}`))
			return
		}
	}

	var sizeBytes int64
	var etag, contentType string
	var updatedAt time.Time
	var contentDisposition string
	var backendName string
	err = s.db.QueryRowContext(ctx, `
		SELECT size_bytes, etag, content_type, updated_at, COALESCE(content_disposition, ''), COALESCE(backend_name, '')
		FROM object_head_cache
		WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3`,
		tenantID, bucket, key).Scan(&sizeBytes, &etag, &contentType, &updatedAt, &contentDisposition, &backendName)
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

	// A GET sends the object: count its bytes on the tenant's month counter
	// as they are written and, past the egress allowance, pace them through
	// the SAME token bucket the tenant's S3 downloads use (WP-R10-9). The
	// public bucket's readers spend the owner's allowance.
	if r.Method == http.MethodGet && s.egress != nil {
		cw := &countingResponseWriter{ResponseWriter: w, span: s.egress.span(ctx, tenantID)}
		gw, admitted := s.egress.admit(cw, r, tenantID, egressSurfaceCDN)
		if !admitted {
			// The stream guard: the tenant is being paced and already has
			// EGRESS_THROTTLE_MAX_STREAMS paced CDN responses open.
			w.Header().Set("Retry-After", egressRetryAfter)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":"this account has used its monthly egress allowance and too many rate-limited downloads are in progress; retry shortly"}`))
			return
		}
		// Recorded when the handler returns, however it returns: a reader
		// that hangs up mid-body used to cost the tenant nothing.
		defer func() {
			gw.close()
			if s.bandwidthTracker != nil {
				s.bandwidthTracker.recordResponse(tenantID, common.BackendUsed(ctx), 0, cw)
			}
		}()
		w = gw
	}

	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Disposition", cdnContentDisposition(forceDownload, contentDisposition, contentType, key))
	w.Header().Set("Content-Length", fmt.Sprintf("%d", sizeBytes))
	w.Header().Set("ETag", fmt.Sprintf(`"%s"`, etag))
	w.Header().Set("Cache-Control", cacheControl)
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Robots-Tag", "noindex, nofollow")
	if !updatedAt.IsZero() {
		w.Header().Set("Last-Modified", updatedAt.UTC().Format(http.TimeFormat))
	}

	setCORSHeaders(w, r, corsOrigins)

	if r.Method == http.MethodHead {
		return
	}

	container := fmt.Sprintf("%s_%s", tenantID, bucket)
	// Tenant-scoped backends (iDrive, Geyser) build their storage keys from
	// the context tenant, not the container name. Serving without it made
	// every driver look under tenant "default" — public objects on those
	// backends 404'd even though the bytes existed (2026-07-31).
	ctx = common.WithTenantID(ctx, tenantID)
	// Seed the engine's routing map like the S3 GET path does, so the fetch
	// goes straight to the backend that holds the object instead of walking
	// the failover chain after a restart.
	if backendName != "" && s.engine != nil {
		s.engine.HintBackend(container, key, backendName)
	}
	// Backend-attribution slot: the engine records which backend served the
	// bytes so CDN egress lands in backend_bandwidth_daily too.
	ctx, _ = common.WithBackendNote(ctx)
	reader, err := s.engine.Get(ctx, container, key)
	if err != nil {
		s.logger.Error("cdn engine.Get failed",
			zap.String("container", container),
			zap.String("key", key),
			zap.Error(err))
		switch {
		case errors.Is(err, engine.ErrAllBackendsUnavailable):
			// The backend holding the bytes is unreachable: clients retry a
			// 503, they treat a 404 as a deletion (R6-25).
			w.Header().Set("Retry-After", "30")
			http.Error(w, "storage backend temporarily unavailable", http.StatusServiceUnavailable)
		case isObjectMissingErr(err):
			http.NotFound(w, r)
		default:
			http.Error(w, "internal error", http.StatusInternalServerError)
		}
		return
	}
	defer func() { _ = reader.Close() }()

	rangeHeader := r.Header.Get("Range")
	if rangeHeader != "" && errors.Is(rangeParseErr(rangeHeader, sizeBytes), errMultiRange) {
		rangeHeader = "" // served whole (RFC 9110 §14.2)
	}
	if rangeHeader != "" {
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
		if s.bandwidthTracker != nil && s.egress == nil {
			s.bandwidthTracker.RecordWithBackend(ctx, tenantID, common.BackendUsed(ctx), 0, rng.length)
		}
		if s.cdnAnalytics != nil {
			s.cdnAnalytics.Record(ctx, tenantID, bucket, key, rng.length,
				r.Header.Get("CF-IPCountry"), r.Referer())
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

	if s.bandwidthTracker != nil && s.egress == nil {
		s.bandwidthTracker.RecordWithBackend(ctx, tenantID, common.BackendUsed(ctx), 0, written)
	}
	if s.cdnAnalytics != nil {
		s.cdnAnalytics.Record(ctx, tenantID, bucket, key, written,
			r.Header.Get("CF-IPCountry"), r.Referer())
	}

	s.logger.Debug("cdn served",
		zap.String("slug", slug),
		zap.String("bucket", bucket),
		zap.String("key", key),
		zap.Int64("bytes", written))
}
