package api

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"go.uber.org/zap"
)

// WP-1 quota accounting.
//
// Invariant: tenant_quotas.storage_used_bytes == SUM(object_head_cache.size_bytes)
// per tenant, in LOGICAL bytes (chunked/deduplicated objects bill their
// logical size, not physical stored bytes).
//
// The API layer is the single reservation site: it reserves the declared
// size up front (atomic check-and-reserve inside QuotaManager), settles to
// the logical size actually recorded after a successful write, releases the
// reservation on failure, and releases the previous object's size on
// overwrite. Delete paths release the head-cache size they remove.

// quotaCtx returns a short-lived context detached from the request so quota
// bookkeeping still completes when the client has already disconnected.
func quotaCtx(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Second)
}

// settlePutQuota reconciles a successful write's up-front reservation
// (reserved; 0 when the size was unknown at reservation time) against the
// logical size actually recorded in object_head_cache (actual), then
// releases the size of any object the write overwrote (oldSize).
func (s *Server) settlePutQuota(ctx context.Context, tenantID string, reserved, actual, oldSize int64) {
	qm := s.quotaManager
	if qm == nil || tenantID == "" {
		return
	}
	switch {
	case actual < reserved:
		if err := qm.ReleaseQuota(ctx, tenantID, reserved-actual); err != nil {
			s.logger.Error("quota settle: release over-reservation failed",
				zap.Error(err), zap.String("tenant_id", tenantID))
		}
	case actual > reserved:
		ok, err := qm.CheckAndReserve(ctx, tenantID, actual-reserved)
		if err != nil {
			s.logger.Error("quota settle: reserve shortfall failed",
				zap.Error(err), zap.String("tenant_id", tenantID))
		} else if !ok {
			// The bytes are already durably stored; account them anyway so
			// billing reflects reality — the tenant simply sits over limit
			// until they delete or upgrade. ReleaseQuota with a negative
			// delta adds unconditionally.
			if err := qm.ReleaseQuota(ctx, tenantID, -(actual - reserved)); err != nil {
				s.logger.Error("quota settle: force-account shortfall failed",
					zap.Error(err), zap.String("tenant_id", tenantID))
			}
		}
	}
	if oldSize > 0 {
		if err := qm.ReleaseQuota(ctx, tenantID, oldSize); err != nil {
			s.logger.Error("quota settle: release overwritten object failed",
				zap.Error(err), zap.String("tenant_id", tenantID))
		}
	}
}

// releaseQuota releases n reserved bytes, logging (never failing the
// request) on error.
func (s *Server) releaseQuota(ctx context.Context, tenantID string, n int64) {
	if s.quotaManager == nil || tenantID == "" || n <= 0 {
		return
	}
	if err := s.quotaManager.ReleaseQuota(ctx, tenantID, n); err != nil {
		s.logger.Error("quota release failed",
			zap.Error(err), zap.String("tenant_id", tenantID), zap.Int64("bytes", n))
	}
}

// releaseQuotaForDelete releases a deleted object's logical bytes on a
// context detached from the request — the delete has already committed, so
// the bookkeeping must complete even if the client disconnects.
func (a *S3ToEngine) releaseQuotaForDelete(r *http.Request, tenantID string, size int64) {
	if a.quota == nil || size <= 0 {
		return
	}
	ctx, cancel := quotaCtx(r)
	defer cancel()
	if err := a.quota.ReleaseQuota(ctx, tenantID, size); err != nil {
		a.logger.Error("quota release after delete failed",
			zap.Error(err), zap.String("tenant_id", tenantID), zap.Int64("bytes", size))
	}
}

// headCacheSize returns the logical size recorded for an object, or 0 if the
// object does not exist (or the lookup fails — callers treat that as "no
// previous object", which under-releases rather than over-releases).
func (s *Server) headCacheSize(ctx context.Context, tenantID, bucket, key string) int64 {
	if s.db == nil {
		return 0
	}
	var size int64
	_ = s.db.QueryRowContext(ctx, `
		SELECT size_bytes FROM object_head_cache
		WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3`,
		tenantID, bucket, key).Scan(&size)
	return size
}

// storageReconciler is implemented by usage.QuotaManager; the nil quota
// manager (no database) does not implement it.
type storageReconciler interface {
	ReconcileStorageUsage(ctx context.Context) (int64, error)
}

// handleQuotaReconcile rewrites every tenant's storage_used_bytes from the
// object_head_cache sum (admin-only; Gate C runs this once before enabling
// metered billing).
func (s *Server) handleQuotaReconcile(w http.ResponseWriter, r *http.Request) {
	rec, ok := s.quotaManager.(storageReconciler)
	if !ok {
		http.Error(w, "quota reconciliation not available", http.StatusServiceUnavailable)
		return
	}
	n, err := rec.ReconcileStorageUsage(r.Context())
	if err != nil {
		s.logger.Error("quota reconciliation failed", zap.Error(err))
		http.Error(w, "reconcile failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	s.logger.Info("quota reconciliation complete", zap.Int64("tenants_updated", n))
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"object":          "quota_reconcile",
		"tenants_updated": n,
	})
}
