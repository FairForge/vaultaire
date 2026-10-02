package api

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
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
	// bounds the bytes demoted per 24 hours (SLC transit: every demoted byte
	// crosses the box). It is a DAILY budget, not a per-run one: a run that
	// a deploy interrupts is run again by the next boot's catch-up, and the
	// second run only gets what the first left of the budget.
	MaxObjectsPerTenant int
	MaxBytesPerRun      int64
	// JobName is the job_runs name (tests use their own: the table is shared).
	JobName string

	// Promoter, when set, runs pending copy-backs (read-time promotion,
	// PR B) at the start of every run.
	Promoter *SmartPromoter

	now        func() time.Time
	beforeFlip func(bucket, key string) // test hook: runs after the cold copy, before the routing flip
	// scopeTenant, when set, confines the ledger passes (reclaim, drift, the
	// day's byte budget) to one tenant. Tests only: the ledger is one table
	// shared with every other test on the database, and a run that settles
	// another fixture's rows with THIS fixture's drivers closes rows it knows
	// nothing about.
	scopeTenant string

	beforeHotDelete  func(bucket, key string) // test hook: runs inside the reclaim tx, after the row locks, before the first stale copy is deleted
	afterStaleDelete func(bucket, key string) // test hook: runs after the reclaim committed, before its check for a write the deletes destroyed
}

// SmartDemotionResult is one run's outcome.
type SmartDemotionResult struct {
	DryRun bool `json:"dry_run"`
	// BytesDemotedBefore is what the ledger shows as demoted in the 24 h
	// before this run started: it counts against MaxBytesPerRun.
	BytesDemotedBefore int64                 `json:"bytes_demoted_before,omitempty"`
	TenantsScanned     int                   `json:"tenants_scanned"`
	Candidates         int                   `json:"candidates"`
	Demoted            int                   `json:"demoted"`
	Skipped            int                   `json:"skipped"`
	BytesDemoted       int64                 `json:"bytes_demoted"`
	HotReclaimed       int                   `json:"hot_reclaimed"`
	Promoted           int                   `json:"promoted"`
	Errors             []string              `json:"errors,omitempty"`
	Tenants            []TenantDemotionStats `json:"tenants,omitempty"`
}

// TenantDemotionStats is the per-tenant breakdown.
type TenantDemotionStats struct {
	TenantID       string `json:"tenant_id"`
	QuotaBytes     int64  `json:"quota_bytes"`
	PinHotBytes    int64  `json:"pin_hot_bytes"` // the paid pin-hot add-on, added to the budget
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
		JobName:             smartDemotionJobName,
		now:                 time.Now,
	}
}

// smartDemotionJobName is the job's name in job_runs and on the metrics.
const smartDemotionJobName = "smart_demotion"

// spec is the job's schedule: daily at 06:30 UTC (night in the Americas — the
// job moves bytes across the box), catch-up five minutes after boot. It used
// to be a 24 h ticker with no run at boot (Review R13-07): it never fired on
// a box that is redeployed several times a day.
//
// What makes a run fail: the tenant list could not be read, or a backend is
// no longer registered — nothing was moved; it is retried at the next hourly
// check. What does NOT: an object that could not be moved, reclaimed or
// promoted, or a tenant whose plan failed — those are counted in the note
// and are tomorrow's candidates again; the run ceiling (the rest is
// tomorrow's). A run that fails for one object must not repeat every hour:
// every repeat re-reads every tenant and, with MaxBytesPerRun, would turn a
// daily budget into an hourly one (the budget is also enforced over 24 h).
func (r *SmartDemotionRunner) spec() jobSpec {
	return jobSpec{
		Name: r.JobName, Hour: 6, Minute: 30,
		BootDelay: 5 * time.Minute, MaxRunTime: 6 * time.Hour,
		Run: func(ctx context.Context) (jobReport, error) {
			res, err := r.RunOnce(ctx, false)
			r.logger.Info("smart demotion run",
				zap.Int("tenants", res.TenantsScanned), zap.Int("candidates", res.Candidates),
				zap.Int("demoted", res.Demoted), zap.Int("skipped", res.Skipped),
				zap.Int64("bytes_demoted", res.BytesDemoted), zap.Int("hot_reclaimed", res.HotReclaimed),
				zap.Int("promoted", res.Promoted), zap.Strings("errors", res.Errors), zap.Error(err))
			rep := jobReport{Rows: int64(res.Demoted + res.HotReclaimed + res.Promoted)}
			var notes []string
			if errors.Is(err, context.DeadlineExceeded) {
				// The ceiling, not a failure: what is left is tomorrow's.
				notes = append(notes, "stopped at the run ceiling")
				err = nil
			}
			if n := len(res.Errors); n > 0 {
				notes = append(notes, fmt.Sprintf("%d item(s) failed, first: %s", n, res.Errors[0]))
			}
			rep.Note = strings.Join(notes, "; ")
			return rep, err
		},
	}
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
		n, errs := r.reclaimHotCopies(ctx)
		res.HotReclaimed = n
		res.Errors = append(res.Errors, errs...)
		if ctx.Err() != nil {
			// Stopping (a deploy) or at the ceiling: what the two passes
			// above did not reach is the next run's work, and the run's
			// result is the context's error — not "the day's bytes could
			// not be read", which is what the next query would report.
			return res, ctx.Err()
		}
	}

	// The byte budget is per 24 hours, whatever number of runs it takes: the
	// ledger says what the last day's runs already moved (a row whose object
	// was promoted since is gone from it — an undercount, never an overcount).
	if err := r.db.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(size_bytes), 0) FROM smart_demotions WHERE demoted_at > $1 AND ($2 = '' OR tenant_id = $2)`,
		r.now().Add(-24*time.Hour), r.scopeTenant).Scan(&res.BytesDemotedBefore); err != nil {
		return res, fmt.Errorf("smart demotion: read the day's demoted bytes: %w", err)
	}

	// The hot budget is a share of the DOWNSTAIRS quota (the attic is tape by
	// definition) plus whatever the tenant pays to pin hot (Review R10-07):
	// a house tenant's downstairs is its `standard` floor row; a tenant
	// without floor rows keeps the single total.
	rows, err := r.db.QueryContext(ctx,
		`SELECT tq.tenant_id,
		        COALESCE((SELECT f.storage_limit_bytes FROM tenant_floor_quotas f
		                   WHERE f.tenant_id = tq.tenant_id AND f.floor = 'standard'), tq.storage_limit_bytes),
		        tq.pin_hot_bytes
		   FROM tenant_quotas tq WHERE tq.tier = ANY($1) ORDER BY tq.tenant_id`,
		pq.Array(r.Tiers))
	if err != nil {
		return res, fmt.Errorf("smart demotion: list tenants: %w", err)
	}
	type tenantRow struct {
		id     string
		quota  int64
		pinHot int64
	}
	var tenants []tenantRow
	for rows.Next() {
		var t tenantRow
		if err := rows.Scan(&t.id, &t.quota, &t.pinHot); err != nil {
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
		stats, cands, err := r.planTenant(ctx, t.id, t.quota, t.pinHot)
		if err != nil && ctx.Err() != nil {
			return res, ctx.Err()
		}
		if err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("%s: plan: %v", t.id, err))
			continue
		}
		res.Candidates += len(cands)
		for _, c := range cands {
			if ctx.Err() != nil {
				// Stopping (a deploy) or at the ceiling: the rest is not a
				// list of failed objects, it is the next run's work.
				return res, ctx.Err()
			}
			if res.BytesDemotedBefore+res.BytesDemoted >= r.MaxBytesPerRun {
				break
			}
			if dryRun {
				continue
			}
			moved, err := r.demote(ctx, hot, cold, t.id, c)
			switch {
			case err != nil && ctx.Err() != nil:
				// The move was cut by the stop, it did not fail.
				return res, ctx.Err()
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
func (r *SmartDemotionRunner) planTenant(ctx context.Context, tenantID string, quota, pinHot int64) (TenantDemotionStats, []demotionCandidate, error) {
	stats := TenantDemotionStats{TenantID: tenantID, QuotaBytes: quota, PinHotBytes: pinHot,
		HotBudgetBytes: int64(float64(quota)*r.HotFraction) + pinHot}
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
	// that are neither versioned nor pinned hot. "Old enough" counts from the
	// last WRITE (updated_at is set by the write upserts only): an overwrite
	// keeps the row's created_at and last_accessed, so a key rewritten today
	// used to look a month old and idle and its fresh bytes went to tape the
	// next morning — the churn MinAge exists to prevent (WP-R13-2).
	const eligible = `
		FROM object_head_cache o
		WHERE o.tenant_id = $1 AND o.backend_name = $2 AND NOT o.is_chunked AND o.size_bytes > 0
		  AND o.created_at < $3 AND o.updated_at < $3
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
		// an archive storage class) — then the blob there is theirs. An
		// object DELETED during the copy has no row at all: the copy is
		// garbage too (it used to stay on tape, WP-R13-2).
		var cur string
		qerr := r.db.QueryRowContext(ctx, `SELECT COALESCE(backend_name,'') FROM object_head_cache WHERE tenant_id=$1 AND bucket=$2 AND object_key=$3`, tenantID, c.bucket, c.key).Scan(&cur)
		if errors.Is(qerr, sql.ErrNoRows) || (qerr == nil && cur != r.ColdBackend) {
			if delErr := cold.Delete(tctx, container, c.key); delErr != nil && !isObjectMissingErr(delErr) {
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
		   hot_deleted_at = NULL, hot_outcome = '', promote_requested_at = NULL, restore_requested_at = NULL`,
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

// reclaimHotCopies settles the ledger rows that owe a delete
// (ledgerSettler.settle with the grace elapsed), in two passes:
//
//   - open rows demoted longer than HotGrace ago: the hot copy of an object
//     that is still as the demotion left it is deleted; for an object
//     rewritten, moved or deleted since, the copies the key no longer routes
//     to are deleted — the cold copy an overwrite left on tape (R13-10) as
//     well as an orphaned hot one — and the copy it does route to is never
//     touched;
//   - reclaimed rows whose object is no longer the demoted one (no head row,
//     another backend, another etag, chunked): the write or delete that
//     changed it removes the cold copy itself, at once; this pass is what
//     retries the ones that failed or that a restart cut short.
//
// A cancelled run stops between two rows: the rows it did not reach are the
// next run's work, never a list of failures.
func (r *SmartDemotionRunner) reclaimHotCopies(ctx context.Context) (int, []string) {
	cutoff := r.now().Add(-r.HotGrace)
	reclaimed, errs := r.settlePass(ctx, "reclaim",
		`SELECT tenant_id, bucket, object_key FROM smart_demotions
		 WHERE hot_deleted_at IS NULL AND demoted_at < $1 AND ($2 = '' OR tenant_id = $2)
		 ORDER BY demoted_at ASC LIMIT $3`, cutoff)
	if ctx.Err() != nil {
		return reclaimed, errs
	}
	_, driftErrs := r.settlePass(ctx, "reclaim (changed since)",
		`SELECT d.tenant_id, d.bucket, d.object_key FROM smart_demotions d
		 LEFT JOIN object_head_cache o
		   ON o.tenant_id = d.tenant_id AND o.bucket = d.bucket AND o.object_key = d.object_key
		 WHERE d.hot_deleted_at IS NOT NULL AND d.hot_outcome = 'deleted' AND d.demoted_at < $1
		   AND ($2 = '' OR d.tenant_id = $2)
		   AND (o.object_key IS NULL OR o.is_chunked
		        OR COALESCE(o.backend_name,'') <> d.cold_backend OR COALESCE(o.etag,'') <> d.etag)
		 ORDER BY d.tenant_id, d.bucket, d.object_key LIMIT $3`, cutoff)
	return reclaimed, append(errs, driftErrs...)
}

// settlePass settles every row a listing returns, in batches until one comes
// back short (Review R13-20: a single LIMIT capped reclaims at 1000 per run
// for ALL tenants while demotion moves 1000 per tenant, so hot copies — paid
// storage — piled up past the grace). A row whose settle failed is selected
// again by the next batch; the seen set stops that within one run.
func (r *SmartDemotionRunner) settlePass(ctx context.Context, what, listing string, cutoff time.Time) (int, []string) {
	var errs []string
	type pending struct{ tenant, bucket, key string }
	reclaimed := 0
	settler := r.settler()
	seen := map[string]bool{}
	for ctx.Err() == nil {
		rows, err := r.db.QueryContext(ctx, listing, cutoff, r.scopeTenant, r.MaxObjectsPerTenant)
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			return reclaimed, append(errs, fmt.Sprintf("%s: list: %v", what, err))
		}
		var todo []pending
		for rows.Next() {
			var p pending
			if err := rows.Scan(&p.tenant, &p.bucket, &p.key); err != nil {
				_ = rows.Close()
				return reclaimed, append(errs, fmt.Sprintf("%s: scan: %v", what, err))
			}
			if id := p.tenant + "/" + p.bucket + "/" + p.key; !seen[id] {
				seen[id] = true
				todo = append(todo, p)
			}
		}
		iterErr := rows.Err()
		_ = rows.Close()
		if iterErr != nil {
			if ctx.Err() != nil {
				break
			}
			return reclaimed, append(errs, fmt.Sprintf("%s: iterate: %v", what, iterErr))
		}
		if len(todo) == 0 {
			break
		}
		fetched := len(todo)
		for _, p := range todo {
			if ctx.Err() != nil {
				return reclaimed, errs
			}
			outcome, err := settler.settle(ctx, lostWriteReclaim, p.tenant, p.bucket, p.key, true)
			if err != nil {
				if ctx.Err() != nil {
					// Cut short by the stop itself: the row is untouched or
					// rolled back, and still owed for the next run.
					return reclaimed, errs
				}
				errs = append(errs, fmt.Sprintf("%s %s %s/%s: %v", what, p.tenant, p.bucket, p.key, err))
				continue
			}
			if outcome == ledgerDeleted || outcome == ledgerObjectGone {
				reclaimed++
			}
		}
		if fetched < r.MaxObjectsPerTenant {
			break
		}
	}
	return reclaimed, errs
}

// settler is the ledger settler with this runner's clock and test hooks.
func (r *SmartDemotionRunner) settler() *ledgerSettler {
	return &ledgerSettler{db: r.db, eng: r.eng, logger: r.logger, now: r.now,
		beforeDelete: r.beforeHotDelete, afterDelete: r.afterStaleDelete}
}
