package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
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

// atomicHeadUpsert captures the size of an existing object_head_cache row
// (locking it), runs the caller's upsert in the same transaction, and
// returns the displaced size — 0 when the key did not exist. Holding the
// row lock across the upsert means a concurrent DELETE ... RETURNING (or
// another writer) serializes against it, so the same bytes can never be
// released twice.
//
// A single-statement CTE (WITH old AS (SELECT ... FOR UPDATE) INSERT ...
// RETURNING (SELECT FROM old)) does NOT work here: the CTE is pulled lazily
// during RETURNING, after the upsert has already self-updated the row, so
// the FOR UPDATE scan finds no visible tuple and reports no previous size.
func atomicHeadUpsert(ctx context.Context, db *sql.DB, tenantID, bucket, key string,
	upsert func(tx *sql.Tx) error) (int64, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin head-cache upsert: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var displaced int64
	err = tx.QueryRowContext(ctx, `
		SELECT size_bytes FROM object_head_cache
		WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3
		FOR UPDATE`,
		tenantID, bucket, key).Scan(&displaced)
	if err != nil && err != sql.ErrNoRows {
		return 0, fmt.Errorf("lock head-cache row: %w", err)
	}

	if err := upsert(tx); err != nil {
		return 0, fmt.Errorf("upsert head-cache row: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit head-cache upsert: %w", err)
	}
	return displaced, nil
}

// storageReconciler is implemented by usage.QuotaManager; the nil quota
// manager (no database) does not implement it.
type storageReconciler interface {
	ReconcileStorageUsage(ctx context.Context) (int64, error)
}

// handleQuotaReconcile rewrites every tenant's storage_used_bytes from the
// object_head_cache sum (admin-only; Gate C runs this once before enabling
// metered billing).
//
// Run only while writes are quiesced: an in-flight PUT's reservation is not
// yet reflected in object_head_cache, so reconciling during live traffic
// erases it and under-counts until the next reconcile.
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
