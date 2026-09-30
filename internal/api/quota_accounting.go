package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/FairForge/vaultaire/internal/audit"
	"net/http"
	"time"

	"github.com/FairForge/vaultaire/internal/crypto"
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
//
// Floors (066, dashboard plan Phase 1): every write resolves its storage
// class first and is reserved on the floor that class maps to (attic for
// GLACIER/DEEP_ARCHIVE, downstairs otherwise — usage.FloorOf); the floor is
// written on the head-cache row so deletes and overwrites release the same
// floor they were charged on. A QuotaManager that does not implement
// floorQuotaManager (the nil manager, test stubs) is charged on the total
// only, exactly as before.

// floorQuotaManager is the per-floor slice of usage.QuotaManager.
type floorQuotaManager interface {
	CheckAndReserveFloor(ctx context.Context, tenantID, floor string, bytes int64) (bool, error)
	ReleaseFloor(ctx context.Context, tenantID, floor string, bytes int64) error
}

// reserveQuota reserves n bytes for tenantID on floor, per floor when the
// manager supports it.
func reserveQuota(ctx context.Context, qm QuotaManager, tenantID, floor string, n int64) (bool, error) {
	if fq, ok := qm.(floorQuotaManager); ok {
		return fq.CheckAndReserveFloor(ctx, tenantID, floor, n)
	}
	return qm.CheckAndReserve(ctx, tenantID, n)
}

// releaseQuotaOn releases n bytes for tenantID on floor (negative n
// force-accounts), per floor when the manager supports it.
func releaseQuotaOn(ctx context.Context, qm QuotaManager, tenantID, floor string, n int64) error {
	if fq, ok := qm.(floorQuotaManager); ok {
		return fq.ReleaseFloor(ctx, tenantID, floor, n)
	}
	return qm.ReleaseQuota(ctx, tenantID, n)
}

// quotaCtx returns a short-lived context detached from the request so quota
// bookkeeping still completes when the client has already disconnected.
func quotaCtx(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Second)
}

// displacedRow is what a head-cache upsert found under the key it replaced:
// the billing record's size and floor (zero/"" when the key was new).
type displacedRow struct {
	Size    int64
	Floor   string
	Backend string // backend_name of the displaced row ("" when new/unknown) — the hint for its stale blob (R8-20)
}

// settlePutQuota reconciles a successful write's up-front reservation
// (reserved; 0 when the size was unknown at reservation time) against the
// logical size actually recorded in object_head_cache (actual) on the
// write's floor, then releases any object the write overwrote on ITS floor
// (which may differ: a bucket's tier can change between two writes).
func (s *Server) settlePutQuota(ctx context.Context, tenantID, floor string, reserved, actual int64, old displacedRow) {
	qm := s.quotaManager
	if qm == nil || tenantID == "" {
		return
	}
	switch {
	case actual < reserved:
		if err := releaseQuotaOn(ctx, qm, tenantID, floor, reserved-actual); err != nil {
			s.logger.Error("quota settle: release over-reservation failed",
				zap.Error(err), zap.String("tenant_id", tenantID))
		}
	case actual > reserved:
		ok, err := reserveQuota(ctx, qm, tenantID, floor, actual-reserved)
		if err != nil {
			s.logger.Error("quota settle: reserve shortfall failed",
				zap.Error(err), zap.String("tenant_id", tenantID))
		} else if !ok {
			// The bytes are already durably stored; account them anyway so
			// billing reflects reality — the tenant simply sits over limit
			// until they delete or upgrade. A negative delta adds
			// unconditionally.
			if err := releaseQuotaOn(ctx, qm, tenantID, floor, -(actual - reserved)); err != nil {
				s.logger.Error("quota settle: force-account shortfall failed",
					zap.Error(err), zap.String("tenant_id", tenantID))
			}
		}
	}
	if old.Size > 0 {
		if err := releaseQuotaOn(ctx, qm, tenantID, old.Floor, old.Size); err != nil {
			s.logger.Error("quota settle: release overwritten object failed",
				zap.Error(err), zap.String("tenant_id", tenantID))
		}
	}
}

// releaseQuota releases n reserved bytes on floor, logging (never failing
// the request) on error.
func (s *Server) releaseQuota(ctx context.Context, tenantID, floor string, n int64) {
	if s.quotaManager == nil || tenantID == "" || n <= 0 {
		return
	}
	if err := releaseQuotaOn(ctx, s.quotaManager, tenantID, floor, n); err != nil {
		s.logger.Error("quota release failed",
			zap.Error(err), zap.String("tenant_id", tenantID), zap.Int64("bytes", n))
	}
}

// releaseQuotaForDelete releases a deleted object's logical bytes on the
// floor its head row recorded, on a context detached from the request —
// the delete has already committed, so the bookkeeping must complete even
// if the client disconnects.
func (a *S3ToEngine) releaseQuotaForDelete(r *http.Request, tenantID string, row displacedRow) {
	if a.quota == nil || row.Size <= 0 {
		return
	}
	ctx, cancel := quotaCtx(r)
	defer cancel()
	if err := releaseQuotaOn(ctx, a.quota, tenantID, row.Floor, row.Size); err != nil {
		a.logger.Error("quota release after delete failed",
			zap.Error(err), zap.String("tenant_id", tenantID), zap.Int64("bytes", row.Size))
	}
}

// atomicHeadUpsert captures the size and floor of an existing
// object_head_cache row (locking it), runs the caller's upsert in the same
// transaction, and returns the displaced row — zero when the key did not
// exist. Holding the row lock across the upsert means a concurrent
// DELETE ... RETURNING (or another writer) serializes against it, so the
// same bytes can never be released twice.
//
// A single-statement CTE (WITH old AS (SELECT ... FOR UPDATE) INSERT ...
// RETURNING (SELECT FROM old)) does NOT work here: the CTE is pulled lazily
// during RETURNING, after the upsert has already self-updated the row, so
// the FOR UPDATE scan finds no visible tuple and reports no previous size.
func atomicHeadUpsert(ctx context.Context, db *sql.DB, tenantID, bucket, key string,
	upsert func(tx *sql.Tx) error) (displacedRow, error) {
	return atomicHeadUpsertReleasing(ctx, db, nil, tenantID, bucket, key, upsert)
}

// chunkManifestReleaser is the slice of GlobalContentIndex the head-upsert
// path needs; an interface so quota_accounting stays mock-testable.
type chunkManifestReleaser interface {
	DeleteObjectChunksTx(ctx context.Context, tx *sql.Tx, tenantID, bucket, key string) error
}

// manifestReleaser adapts a possibly-nil *crypto.GlobalContentIndex to the
// interface without producing a typed-nil (which would panic on call).
func manifestReleaser(g *crypto.GlobalContentIndex) chunkManifestReleaser {
	if g == nil {
		return nil
	}
	return g
}

// atomicHeadUpsertReleasing is atomicHeadUpsert for writers that store a
// NON-chunked row: when the displaced row was a chunked object, its manifest
// is released in the same transaction. Without this, a plain PUT / copy /
// multipart-complete overwriting a chunked object flips (or worse, fails to
// flip) is_chunked while the old tenant_chunk_refs and their GCI refcounts
// leak forever — and a row left is_chunked=TRUE over a plain blob makes GET
// serve the OLD object's bytes from the stale manifest.
func atomicHeadUpsertReleasing(ctx context.Context, db *sql.DB, gci chunkManifestReleaser,
	tenantID, bucket, key string, upsert func(tx *sql.Tx) error) (displacedRow, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return displacedRow{}, fmt.Errorf("begin head-cache upsert: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var displaced displacedRow
	var displacedChunked bool
	err = tx.QueryRowContext(ctx, `
		SELECT size_bytes, floor, COALESCE(backend_name, ''), is_chunked FROM object_head_cache
		WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3
		FOR UPDATE`,
		tenantID, bucket, key).Scan(&displaced.Size, &displaced.Floor, &displaced.Backend, &displacedChunked)
	if err != nil && err != sql.ErrNoRows {
		return displacedRow{}, fmt.Errorf("lock head-cache row: %w", err)
	}

	if displacedChunked && gci != nil {
		if relErr := gci.DeleteObjectChunksTx(ctx, tx, tenantID, bucket, key); relErr != nil {
			return displacedRow{}, fmt.Errorf("release displaced chunk manifest: %w", relErr)
		}
	}

	if err := upsert(tx); err != nil {
		return displacedRow{}, fmt.Errorf("upsert head-cache row: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return displacedRow{}, fmt.Errorf("commit head-cache upsert: %w", err)
	}
	return displaced, nil
}

// deleteHeadRowReleasing removes an object's head-cache row (the billing
// record, DELETE ... RETURNING so the bytes are released exactly once) and,
// when the row was a chunked object, releases its manifest — tenant_chunk_refs,
// GCI ref counts, object_metadata — in the SAME transaction. Split across two
// (manifest first, head row second) a crash between them left a head row that
// said is_chunked with no manifest behind it: HEAD 200, GET 500, still billed
// (R8-08); the delete-marker branch never released the manifest at all
// (R8-07/R2-16). Lock order matches atomicHeadUpsertReleasing: head row, then
// manifest rows.
//
// Returns found=false (no error) when there was no row: S3 DELETE is
// idempotent and there is nothing to release.
func deleteHeadRowReleasing(ctx context.Context, db *sql.DB, gci chunkManifestReleaser,
	tenantID, bucket, key string) (row displacedRow, found bool, err error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return displacedRow{}, false, fmt.Errorf("begin head-cache delete: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var chunked bool
	err = tx.QueryRowContext(ctx, `
		DELETE FROM object_head_cache
		WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3
		RETURNING size_bytes, floor, COALESCE(backend_name, ''), is_chunked`,
		tenantID, bucket, key).Scan(&row.Size, &row.Floor, &row.Backend, &chunked)
	if errors.Is(err, sql.ErrNoRows) {
		return displacedRow{}, false, nil
	}
	if err != nil {
		return displacedRow{}, false, fmt.Errorf("delete head-cache row: %w", err)
	}
	if chunked && gci != nil {
		if relErr := gci.DeleteObjectChunksTx(ctx, tx, tenantID, bucket, key); relErr != nil {
			return displacedRow{}, false, fmt.Errorf("release chunk manifest: %w", relErr)
		}
	}
	if err := tx.Commit(); err != nil {
		return displacedRow{}, false, fmt.Errorf("commit head-cache delete: %w", err)
	}
	return row, true, nil
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
	actor, _ := r.Context().Value(userIDKey).(string)
	audit.Record(r.Context(), s.db, audit.Entry{UserID: actor, EventType: "admin", Action: "admin.quota_reconcile", Error: err,
		Metadata: map[string]any{"tenants_updated": n}})
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
