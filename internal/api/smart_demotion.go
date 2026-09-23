package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/FairForge/vaultaire/internal/common"
	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/lib/pq"
	"go.uber.org/zap"
)

// Smart-tier demotion job (Phase 5.15.8).
//
// The Smart/Standard tier sells quota at a price that only works if ~85% of a
// tenant's bytes live on tape. This job enforces that: per tenant, at most
// HotFraction × quota stays on the hot backend. Two triggers, batched daily:
//
//  1. idle       — last_accessed older than IdleAfter → demote
//  2. over budget — hot bytes > budget → demote least-recently-used until under
//
// never touching objects younger than MinAge (upload settling; avoids
// demote-then-restore churn on fresh data). Writes always land hot; this job
// is the only thing that moves data down.
//
// Routing truth is object_head_cache.backend_name (GET/HEAD/restore all route
// on it), so a demotion is: copy hot→cold, then an etag-guarded conditional
// flip of that column. 0 rows updated = the object changed under us → remove
// the cold copy, skip. The hot copy is NOT deleted here: it is reclaimed on a
// later run after HotGrace, under a row lock, only if the row still routes
// cold with the same etag. Within the grace window a promotion is a routing
// flip with no data movement (read-path hook = follow-up PR).
//
// Scope v1: whole (non-chunked) objects, non-versioned buckets, buckets not
// pinned via tier_preference='performance'. Flag-gated per tenant/globally
// (smart_demotion, default OFF). No overage billing.

// flagChecker is the slice of *flags.Service the job needs.
type flagChecker interface {
	Enabled(key, tenantID string) bool
}

// SmartDemotionRunner is the daily demotion + hot-reclaim job.
type SmartDemotionRunner struct {
	db     *sql.DB
	eng    *engine.CoreEngine
	flags  flagChecker
	logger *zap.Logger

	HotBackend  string
	ColdBackend string
	Tiers       []string
	HotFraction float64
	IdleAfter   time.Duration
	MinAge      time.Duration
	HotGrace    time.Duration
	// MaxObjectsPerTenant bounds one tenant's candidates per run; MaxBytesPerRun
	// bounds the whole run (SLC transit: every demoted byte crosses the box).
	MaxObjectsPerTenant int
	MaxBytesPerRun      int64
	Interval            time.Duration

	// Promoter, when set, runs pending copy-backs (read-time promotion,
	// PR B) at the start of every run.
	Promoter *SmartPromoter

	now        func() time.Time
	beforeFlip func(bucket, key string) // test hook: runs after the cold copy, before the routing flip
}

// SmartDemotionResult is one run's outcome.
type SmartDemotionResult struct {
	DryRun         bool                  `json:"dry_run"`
	TenantsScanned int                   `json:"tenants_scanned"`
	Candidates     int                   `json:"candidates"`
	Demoted        int                   `json:"demoted"`
	Skipped        int                   `json:"skipped"`
	BytesDemoted   int64                 `json:"bytes_demoted"`
	HotReclaimed   int                   `json:"hot_reclaimed"`
	Promoted       int                   `json:"promoted"`
	Errors         []string              `json:"errors,omitempty"`
	Tenants        []TenantDemotionStats `json:"tenants,omitempty"`
}

// TenantDemotionStats is the per-tenant breakdown.
type TenantDemotionStats struct {
	TenantID       string `json:"tenant_id"`
	QuotaBytes     int64  `json:"quota_bytes"`
	HotBudgetBytes int64  `json:"hot_budget_bytes"`
	HotBytesBefore int64  `json:"hot_bytes_before"`
	HotBytesAfter  int64  `json:"hot_bytes_after"`
	IdleCandidates int    `json:"idle_candidates"`
	LRUCandidates  int    `json:"lru_candidates"`
	Demoted        int    `json:"demoted"`
}

type demotionCandidate struct {
	bucket, key, etag string
	size              int64
	reason            string
}

// NewSmartDemotionRunner builds the runner; nil when there is no DB or engine.
func NewSmartDemotionRunner(db *sql.DB, eng *engine.CoreEngine, fl flagChecker, logger *zap.Logger) *SmartDemotionRunner {
	if db == nil || eng == nil {
		return nil
	}
	return &SmartDemotionRunner{
		db: db, eng: eng, flags: fl, logger: logger,
		HotBackend:          "idrive",
		ColdBackend:         "geyser",
		Tiers:               []string{"standard"},
		HotFraction:         0.15,
		IdleAfter:           14 * 24 * time.Hour,
		MinAge:              3 * 24 * time.Hour,
		HotGrace:            24 * time.Hour,
		MaxObjectsPerTenant: 1000,
		MaxBytesPerRun:      500 << 30, // 500 GiB
		Interval:            24 * time.Hour,
		now:                 time.Now,
	}
}

// Start runs RunOnce on the interval until ctx is cancelled.
func (r *SmartDemotionRunner) Start(ctx context.Context) {
	if r == nil {
		return
	}
	go func() {
		ticker := time.NewTicker(r.Interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				res, err := r.RunOnce(ctx, false)
				if err != nil {
					r.logger.Error("smart demotion run failed", zap.Error(err))
					continue
				}
				r.logger.Info("smart demotion run completed",
					zap.Int("tenants", res.TenantsScanned), zap.Int("candidates", res.Candidates),
					zap.Int("demoted", res.Demoted), zap.Int("skipped", res.Skipped),
					zap.Int64("bytes_demoted", res.BytesDemoted), zap.Int("hot_reclaimed", res.HotReclaimed),
					zap.Strings("errors", res.Errors))
			}
		}
	}()
}

// RunOnce performs one cycle: reclaim hot copies past grace, then demote.
// dryRun reports candidates without moving anything or writing the ledger.
func (r *SmartDemotionRunner) RunOnce(ctx context.Context, dryRun bool) (SmartDemotionResult, error) {
	res := SmartDemotionResult{DryRun: dryRun}
	if r == nil {
		return res, errors.New("smart demotion: runner not configured")
	}
	hot, ok := r.eng.GetDriver(r.HotBackend)
	if !ok {
		return res, fmt.Errorf("smart demotion: hot backend %q not registered", r.HotBackend)
	}
	cold, ok := r.eng.GetDriver(r.ColdBackend)
	if !ok {
		return res, fmt.Errorf("smart demotion: cold backend %q not registered", r.ColdBackend)
	}

	if !dryRun {
		if r.Promoter != nil {
			n, errs := r.Promoter.PromotePending(ctx)
			res.Promoted = n
			res.Errors = append(res.Errors, errs...)
		}
		n, errs := r.reclaimHotCopies(ctx, hot)
		res.HotReclaimed = n
		res.Errors = append(res.Errors, errs...)
	}

	rows, err := r.db.QueryContext(ctx,
		`SELECT tenant_id, storage_limit_bytes FROM tenant_quotas WHERE tier = ANY($1) ORDER BY tenant_id`,
		pq.Array(r.Tiers))
	if err != nil {
		return res, fmt.Errorf("smart demotion: list tenants: %w", err)
	}
	type tenantRow struct {
		id    string
		quota int64
	}
	var tenants []tenantRow
	for rows.Next() {
		var t tenantRow
		if err := rows.Scan(&t.id, &t.quota); err != nil {
			_ = rows.Close()
			return res, fmt.Errorf("smart demotion: scan tenant: %w", err)
		}
		tenants = append(tenants, t)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return res, fmt.Errorf("smart demotion: iterate tenants: %w", err)
	}

	for _, t := range tenants {
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		if r.flags == nil || !r.flags.Enabled(flagSmartDemotion, t.id) {
			continue
		}
		res.TenantsScanned++
		stats, cands, err := r.planTenant(ctx, t.id, t.quota)
		if err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("%s: plan: %v", t.id, err))
			continue
		}
		res.Candidates += len(cands)
		for _, c := range cands {
			if res.BytesDemoted >= r.MaxBytesPerRun {
				break
			}
			if dryRun {
				continue
			}
			moved, err := r.demote(ctx, hot, cold, t.id, c)
			switch {
			case err != nil:
				res.Errors = append(res.Errors, fmt.Sprintf("%s %s/%s: %v", t.id, c.bucket, c.key, err))
			case !moved:
				res.Skipped++
			default:
				res.Demoted++
				stats.Demoted++
				res.BytesDemoted += c.size
				stats.HotBytesAfter -= c.size
			}
		}
		res.Tenants = append(res.Tenants, stats)
	}
	return res, nil
}

// planTenant computes the budget and selects candidates: idle objects first
// (oldest access first), then LRU extras only while still over budget.
func (r *SmartDemotionRunner) planTenant(ctx context.Context, tenantID string, quota int64) (TenantDemotionStats, []demotionCandidate, error) {
	stats := TenantDemotionStats{TenantID: tenantID, QuotaBytes: quota, HotBudgetBytes: int64(float64(quota) * r.HotFraction)}
	if err := r.db.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(size_bytes),0) FROM object_head_cache WHERE tenant_id=$1 AND backend_name=$2`,
		tenantID, r.HotBackend).Scan(&stats.HotBytesBefore); err != nil {
		return stats, nil, fmt.Errorf("hot bytes: %w", err)
	}
	stats.HotBytesAfter = stats.HotBytesBefore

	now := r.now()
	idleCutoff := now.Add(-r.IdleAfter)
	ageCutoff := now.Add(-r.MinAge)

	// Eligible = whole objects on the hot backend, old enough, in buckets
	// that are neither versioned nor pinned hot.
	const eligible = `
		FROM object_head_cache o
		WHERE o.tenant_id = $1 AND o.backend_name = $2 AND NOT o.is_chunked AND o.size_bytes > 0
		  AND o.created_at < $3
		  AND NOT EXISTS (SELECT 1 FROM buckets b WHERE b.tenant_id = o.tenant_id AND b.name = o.bucket
		                    AND (COALESCE(b.versioning_status,'') IN ('Enabled','Suspended')
		                         OR COALESCE(b.tier_preference,'') = 'performance'))`

	var cands []demotionCandidate
	idleRows, err := r.db.QueryContext(ctx,
		`SELECT o.bucket, o.object_key, o.etag, o.size_bytes `+eligible+
			` AND o.last_accessed < $4 ORDER BY o.last_accessed ASC LIMIT $5`,
		tenantID, r.HotBackend, ageCutoff, idleCutoff, r.MaxObjectsPerTenant)
	if err != nil {
		return stats, nil, fmt.Errorf("idle candidates: %w", err)
	}
	var idleBytes int64
	for idleRows.Next() {
		var c demotionCandidate
		if err := idleRows.Scan(&c.bucket, &c.key, &c.etag, &c.size); err != nil {
			_ = idleRows.Close()
			return stats, nil, fmt.Errorf("scan idle: %w", err)
		}
		c.reason = "idle"
		cands = append(cands, c)
		idleBytes += c.size
	}
	_ = idleRows.Close()
	if err := idleRows.Err(); err != nil {
		return stats, nil, fmt.Errorf("iterate idle: %w", err)
	}
	stats.IdleCandidates = len(cands)

	remaining := stats.HotBytesBefore - idleBytes - stats.HotBudgetBytes
	if remaining > 0 && len(cands) < r.MaxObjectsPerTenant {
		lruRows, err := r.db.QueryContext(ctx,
			`SELECT o.bucket, o.object_key, o.etag, o.size_bytes `+eligible+
				` AND o.last_accessed >= $4 ORDER BY o.last_accessed ASC LIMIT $5`,
			tenantID, r.HotBackend, ageCutoff, idleCutoff, r.MaxObjectsPerTenant-len(cands))
		if err != nil {
			return stats, nil, fmt.Errorf("lru candidates: %w", err)
		}
		for lruRows.Next() && remaining > 0 {
			var c demotionCandidate
			if err := lruRows.Scan(&c.bucket, &c.key, &c.etag, &c.size); err != nil {
				_ = lruRows.Close()
				return stats, nil, fmt.Errorf("scan lru: %w", err)
			}
			c.reason = "over_budget"
			cands = append(cands, c)
			remaining -= c.size
			stats.LRUCandidates++
		}
		_ = lruRows.Close()
		if err := lruRows.Err(); err != nil {
			return stats, nil, fmt.Errorf("iterate lru: %w", err)
		}
	}
	return stats, cands, nil
}

// demote copies one object hot→cold and flips routing under an etag guard.
// Returns (false, nil) when the object changed during the copy.
func (r *SmartDemotionRunner) demote(ctx context.Context, hot, cold engine.Driver, tenantID string, c demotionCandidate) (bool, error) {
	tctx := common.WithTenantID(ctx, tenantID) // drivers key blobs by tenant from ctx
	container := tenantID + "_" + c.bucket     // engine container = tenant.NamespaceContainer(bucket)

	rc, err := hot.Get(tctx, container, c.key)
	if err != nil {
		return false, fmt.Errorf("read hot: %w", err)
	}
	putErr := cold.Put(tctx, container, c.key, rc, engine.WithContentLength(c.size))
	_ = rc.Close()
	if putErr != nil {
		return false, fmt.Errorf("write cold: %w", putErr)
	}

	if r.beforeFlip != nil {
		r.beforeFlip(c.bucket, c.key)
	}

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	upd, err := tx.ExecContext(ctx,
		`UPDATE object_head_cache SET backend_name = $1
		 WHERE tenant_id = $2 AND bucket = $3 AND object_key = $4
		   AND backend_name = $5 AND etag = $6 AND NOT is_chunked`,
		r.ColdBackend, tenantID, c.bucket, c.key, r.HotBackend, c.etag)
	if err != nil {
		return false, fmt.Errorf("flip routing: %w", err)
	}
	n, _ := upd.RowsAffected()
	if n == 0 {
		// Changed under us. Our cold copy is stale garbage UNLESS the new
		// version itself landed on the cold backend at this key (a PUT with
		// an archive storage class) — then the blob there is theirs.
		var cur string
		qerr := r.db.QueryRowContext(ctx, `SELECT COALESCE(backend_name,'') FROM object_head_cache WHERE tenant_id=$1 AND bucket=$2 AND object_key=$3`, tenantID, c.bucket, c.key).Scan(&cur)
		if qerr == nil && cur != r.ColdBackend {
			if delErr := cold.Delete(tctx, container, c.key); delErr != nil {
				r.logger.Warn("smart demotion: stale cold copy not removed",
					zap.String("tenant", tenantID), zap.String("bucket", c.bucket), zap.String("key", c.key), zap.Error(delErr))
			}
		}
		return false, nil
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO smart_demotions (tenant_id, bucket, object_key, etag, size_bytes, hot_backend, cold_backend, reason, demoted_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
		 ON CONFLICT (tenant_id, bucket, object_key) DO UPDATE SET
		   etag = EXCLUDED.etag, size_bytes = EXCLUDED.size_bytes, hot_backend = EXCLUDED.hot_backend,
		   cold_backend = EXCLUDED.cold_backend, reason = EXCLUDED.reason, demoted_at = EXCLUDED.demoted_at,
		   hot_deleted_at = NULL, hot_outcome = ''`,
		tenantID, c.bucket, c.key, c.etag, c.size, r.HotBackend, r.ColdBackend, c.reason, r.now()); err != nil {
		return false, fmt.Errorf("ledger: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit: %w", err)
	}

	// Keep the engine's in-memory routing map consistent for this process;
	// other processes/restarts read backend_name from the head cache on GET.
	r.eng.HintBackend(container, c.key, r.ColdBackend)
	r.logger.Info("smart demotion: object demoted",
		zap.String("tenant", tenantID), zap.String("bucket", c.bucket), zap.String("key", c.key),
		zap.Int64("bytes", c.size), zap.String("reason", c.reason))
	return true, nil
}

// reclaimHotCopies deletes hot copies of objects demoted longer than HotGrace
// ago, under the head-cache row lock and only while the row still routes to
// the cold backend with the demoted etag. An object rewritten or moved since
// keeps its hot bytes (they are live data); an object deleted since has an
// orphaned hot copy that is removed.
func (r *SmartDemotionRunner) reclaimHotCopies(ctx context.Context, hot engine.Driver) (int, []string) {
	var errs []string
	cutoff := r.now().Add(-r.HotGrace)
	rows, err := r.db.QueryContext(ctx,
		`SELECT tenant_id, bucket, object_key, etag, cold_backend FROM smart_demotions
		 WHERE hot_deleted_at IS NULL AND demoted_at < $1 ORDER BY demoted_at ASC LIMIT $2`,
		cutoff, r.MaxObjectsPerTenant)
	if err != nil {
		return 0, []string{fmt.Sprintf("reclaim: list: %v", err)}
	}
	type pending struct{ tenant, bucket, key, etag, cold string }
	var todo []pending
	for rows.Next() {
		var p pending
		if err := rows.Scan(&p.tenant, &p.bucket, &p.key, &p.etag, &p.cold); err != nil {
			_ = rows.Close()
			return 0, []string{fmt.Sprintf("reclaim: scan: %v", err)}
		}
		todo = append(todo, p)
	}
	_ = rows.Close()

	reclaimed := 0
	for _, p := range todo {
		outcome, err := r.reclaimOne(ctx, hot, p.tenant, p.bucket, p.key, p.etag, p.cold)
		if err != nil {
			errs = append(errs, fmt.Sprintf("reclaim %s %s/%s: %v", p.tenant, p.bucket, p.key, err))
			continue
		}
		if outcome == "deleted" || outcome == "object_gone" {
			reclaimed++
		}
	}
	return reclaimed, errs
}

func (r *SmartDemotionRunner) reclaimOne(ctx context.Context, hot engine.Driver, tenantID, bucket, key, etag, coldBackend string) (string, error) {
	tctx := common.WithTenantID(ctx, tenantID)
	container := tenantID + "_" + bucket

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var curBackend, curEtag string
	err = tx.QueryRowContext(ctx,
		`SELECT COALESCE(backend_name,''), COALESCE(etag,'') FROM object_head_cache
		 WHERE tenant_id=$1 AND bucket=$2 AND object_key=$3 FOR UPDATE`,
		tenantID, bucket, key).Scan(&curBackend, &curEtag)
	outcome := ""
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// Object deleted since demotion: the hot copy is an orphan.
		outcome = "object_gone"
	case err != nil:
		return "", fmt.Errorf("lock row: %w", err)
	case curBackend == coldBackend && curEtag == etag:
		outcome = "deleted"
	default:
		outcome = "kept_changed"
	}

	if outcome == "deleted" || outcome == "object_gone" {
		if delErr := hot.Delete(tctx, container, key); delErr != nil {
			// Leave the ledger open; retry next run. An orphan on the hot
			// backend costs money, never data.
			_, _ = tx.ExecContext(ctx, `UPDATE smart_demotions SET hot_outcome='delete_failed' WHERE tenant_id=$1 AND bucket=$2 AND object_key=$3`, tenantID, bucket, key)
			_ = tx.Commit()
			return "", fmt.Errorf("delete hot: %w", delErr)
		}
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE smart_demotions SET hot_deleted_at=$4, hot_outcome=$5 WHERE tenant_id=$1 AND bucket=$2 AND object_key=$3`,
		tenantID, bucket, key, r.now(), outcome); err != nil {
		return "", fmt.Errorf("close ledger: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("commit: %w", err)
	}
	return outcome, nil
}

// handleSmartDemotionTrigger runs one cycle on demand (admin). ?dry_run=true
// reports what would move without moving it — the flag-dark verification
// path before enabling the flag for a tenant.
func (s *Server) handleSmartDemotionTrigger(w http.ResponseWriter, r *http.Request) {
	if s.smartDemotion == nil {
		http.Error(w, "smart demotion not available", http.StatusServiceUnavailable)
		return
	}
	dryRun, _ := strconv.ParseBool(r.URL.Query().Get("dry_run"))
	res, err := s.smartDemotion.RunOnce(r.Context(), dryRun)
	if err != nil {
		s.logger.Error("manual smart demotion failed", zap.Error(err))
		http.Error(w, "smart demotion failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(res)
}
