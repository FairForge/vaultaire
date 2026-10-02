package api

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/FairForge/vaultaire/internal/crypto"
	"github.com/FairForge/vaultaire/internal/engine"
	"go.uber.org/zap"
)

// DedupGCRunner reclaims storage from orphaned deduplicated chunks.
// Phase A reconciles ref counts against actual tenant_chunk_refs rows.
// Phase B deletes chunks that have been marked_for_deletion past the grace period.
type DedupGCRunner struct {
	db          *sql.DB
	eng         *engine.CoreEngine
	gci         *crypto.GlobalContentIndex
	logger      *zap.Logger
	GracePeriod time.Duration
	// JobName is the job_runs name (tests use their own: the table is shared).
	JobName string
}

// dedupGCJobName is the job's name in job_runs and on the metrics.
const dedupGCJobName = "dedup_gc"

// DedupGCResult holds the outcome of a single GC run.
type DedupGCResult struct {
	Reconciled     int   `json:"reconciled"`
	Deleted        int   `json:"deleted"`
	BytesReclaimed int64 `json:"bytes_reclaimed"`
	// Failed counts sweep candidates that could not be swept (lock or row
	// delete error, or a blob the backend would not delete — leaked, never
	// corrupt). They stay candidates and the next run takes them.
	Failed int `json:"failed,omitempty"`
}

// NewDedupGCRunner builds the GC runner. gci must be the SAME instance the PUT
// path dedups through — the sweep invalidates its in-memory cache before
// deleting rows, and a runner wired to a different (or nil) GCI leaves stale
// cache entries pointing at deleted chunks (WP-6).
func NewDedupGCRunner(db *sql.DB, eng *engine.CoreEngine, gci *crypto.GlobalContentIndex, logger *zap.Logger) *DedupGCRunner {
	if db == nil || eng == nil {
		return nil
	}
	return &DedupGCRunner{
		db:          db,
		eng:         eng,
		gci:         gci,
		logger:      logger,
		GracePeriod: 7 * 24 * time.Hour,
		JobName:     dedupGCJobName,
	}
}

// spec is the job's schedule: daily at 02:30 UTC, catch-up three minutes
// after boot. It used to be a 24 h ticker with no run at boot (Review
// R13-07): on a box redeployed several times a day the tick never came and
// the GC ran only when an admin posted the trigger.
//
// What makes a run fail: the reconcile statement or the candidate scan
// failed — nothing was swept; it is retried at the next hourly check (both
// are single statements). A chunk that could not be swept does NOT fail the
// run: it is counted (`failed`), written to job_runs as a note, and stays a
// candidate for tomorrow's run — one stuck blob must not make the GC repeat
// every hour.
func (g *DedupGCRunner) spec() jobSpec {
	return jobSpec{
		Name: g.JobName, Hour: 2, Minute: 30,
		BootDelay: 3 * time.Minute, MaxRunTime: 2 * time.Hour,
		Run: func(ctx context.Context) (jobReport, error) {
			res, err := g.RunOnce(ctx)
			g.logger.Info("dedup gc run", zap.Int("reconciled", res.Reconciled), zap.Int("deleted", res.Deleted),
				zap.Int64("bytes_reclaimed", res.BytesReclaimed), zap.Int("failed", res.Failed), zap.Error(err))
			rep := jobReport{Rows: int64(res.Deleted)}
			if res.Failed > 0 {
				rep.Note = fmt.Sprintf("%d chunk(s) not swept (see the log); they stay candidates", res.Failed)
			}
			return rep, err
		},
	}
}

// RunOnce performs a single GC cycle: reconcile then sweep.
func (g *DedupGCRunner) RunOnce(ctx context.Context) (DedupGCResult, error) {
	var result DedupGCResult

	reconciled, err := g.reconcile(ctx)
	if err != nil {
		return result, fmt.Errorf("reconcile: %w", err)
	}
	result.Reconciled = reconciled

	deleted, failed, reclaimed, err := g.sweep(ctx)
	result.Deleted = deleted
	result.Failed = failed
	result.BytesReclaimed = reclaimed
	if err != nil {
		return result, fmt.Errorf("sweep: %w", err)
	}

	return result, nil
}

// reconcile corrects ref_count drift by counting actual tenant_chunk_refs.
// Only touches chunks whose last_accessed_at is older than the grace period
// to avoid corrupting in-flight streaming uploads.
func (g *DedupGCRunner) reconcile(ctx context.Context) (int, error) {
	graceSecs := int(g.GracePeriod.Seconds())
	res, err := g.db.ExecContext(ctx, `
		WITH actual AS (
			SELECT dedup_scope, plaintext_hash, COUNT(*) AS cnt
			FROM tenant_chunk_refs
			GROUP BY dedup_scope, plaintext_hash
		)
		UPDATE global_content_index g
		SET ref_count = COALESCE(a.cnt, 0),
		    marked_for_deletion = (COALESCE(a.cnt, 0) = 0),
		    marked_at = CASE
		        WHEN COALESCE(a.cnt, 0) = 0 AND NOT g.marked_for_deletion THEN NOW()
		        WHEN COALESCE(a.cnt, 0) > 0 THEN NULL
		        ELSE g.marked_at
		    END
		FROM global_content_index g2
		LEFT JOIN actual a
		       ON g2.dedup_scope = a.dedup_scope AND g2.plaintext_hash = a.plaintext_hash
		WHERE g.dedup_scope = g2.dedup_scope AND g.plaintext_hash = g2.plaintext_hash
		  AND g.last_accessed_at < NOW() - make_interval(secs => $1)
		  AND g.ref_count <> COALESCE(a.cnt, 0)
	`, graceSecs)
	if err != nil {
		return 0, fmt.Errorf("reconcile query: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// sweep deletes chunks that have been ref_count=0 and marked_for_deletion past
// the grace period. Uses conditional DELETE to avoid racing with concurrent re-refs.
func (g *DedupGCRunner) sweep(ctx context.Context) (int, int, int64, error) {
	graceSecs := int(g.GracePeriod.Seconds())
	rows, err := g.db.QueryContext(ctx, `
		SELECT dedup_scope, plaintext_hash, backend_id, storage_key, size_bytes
		FROM global_content_index
		WHERE marked_for_deletion = TRUE
		  AND ref_count = 0
		  AND marked_at < NOW() - make_interval(secs => $1)
	`, graceSecs)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("sweep query: %w", err)
	}
	defer func() { _ = rows.Close() }()

	type candidate struct {
		scope     string
		hash      string
		backendID string
		key       string
		size      int64
	}
	var candidates []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.scope, &c.hash, &c.backendID, &c.key, &c.size); err != nil {
			return 0, 0, 0, fmt.Errorf("scan candidate: %w", err)
		}
		candidates = append(candidates, c)
	}
	if err := rows.Err(); err != nil {
		return 0, 0, 0, fmt.Errorf("iterate candidates: %w", err)
	}

	var deleted, failed int
	var reclaimed int64
	for _, c := range candidates {
		if ctx.Err() != nil {
			// Run deadline or shutdown: what is left stays a candidate.
			return deleted, failed, reclaimed, ctx.Err()
		}
		outcome, err := g.sweepOne(ctx, c.scope, c.hash, c.backendID, c.key)
		if err != nil {
			g.logger.Error("sweep chunk",
				zap.String("hash", c.hash), zap.Error(err))
			failed++
			continue
		}
		switch outcome {
		case sweepDeleted:
			deleted++
			reclaimed += c.size
		case sweepBlobLeaked:
			failed++
		}
	}

	return deleted, failed, reclaimed, nil
}

// sweepOutcome is what happened to one sweep candidate.
type sweepOutcome int

const (
	// sweepKept: re-referenced since the scan — nothing to do.
	sweepKept sweepOutcome = iota
	// sweepDeleted: row and blob are gone.
	sweepDeleted
	// sweepBlobLeaked: the row is gone and the backend refused the blob
	// delete — a leaked blob, never a corrupt object.
	sweepBlobLeaked
)

// sweepOne deletes a single candidate's GCI row and its backing blob while
// holding an advisory lock keyed on (scope, hash) — the same lock the chunked
// PUT path takes (as pg_advisory_xact_lock) before storing a chunk (WP-6).
// Without it, the delete-vs-reref race loses data: sweep deletes the row, a
// concurrent PUT re-inserts it and re-stores the blob, then sweep deletes the
// blob out from under the live row.
//
// A SESSION-level lock on a dedicated connection (not a transaction lock) so
// the row delete can commit BEFORE the blob delete while the lock is still
// held: with a single tx, a commit failure after the blob delete would leave a
// live row pointing at deleted data. Ordering here is row-commit → blob delete
// → unlock; every failure mode leaks at most a blob, never corrupts a manifest.
// The connection close releases the lock even on a crash.
//
// The cache entry is invalidated before the row delete (a stale entry makes a
// later PUT dedup-hit a chunk that no longer exists) and again after the blob
// delete (a concurrent LookupChunk may re-cache the row in the window before
// the delete commits).
func (g *DedupGCRunner) sweepOne(ctx context.Context, scope, hash, backendID, key string) (sweepOutcome, error) {
	conn, err := g.db.Conn(ctx)
	if err != nil {
		return sweepKept, fmt.Errorf("acquire sweep conn: %w", err)
	}
	defer func() { _ = conn.Close() }()

	if _, err := conn.ExecContext(ctx,
		`SELECT pg_advisory_lock(hashtext($1), hashtext($2))`, scope, hash); err != nil {
		return sweepKept, fmt.Errorf("advisory lock: %w", err)
	}
	defer func() {
		_, _ = conn.ExecContext(context.WithoutCancel(ctx),
			`SELECT pg_advisory_unlock(hashtext($1), hashtext($2))`, scope, hash)
	}()

	if g.gci != nil {
		g.gci.InvalidateCache(scope, hash)
	}

	res, err := conn.ExecContext(ctx, `
		DELETE FROM global_content_index
		WHERE dedup_scope = $1
		  AND plaintext_hash = $2
		  AND ref_count = 0
		  AND marked_for_deletion = TRUE
	`, scope, hash)
	if err != nil {
		return sweepKept, fmt.Errorf("delete gci row: %w", err)
	}
	affected, _ := res.RowsAffected()
	if affected == 0 {
		// Re-referenced since the candidate scan — nothing to do.
		return sweepKept, nil
	}

	if g.gci != nil {
		g.gci.InvalidateCache(scope, hash)
	}

	// Blob delete happens under the same lock, after the row delete committed:
	// a concurrent PUT re-storing this chunk blocks on the lock, then finds no
	// row and stores fresh data. A failed blob delete leaks the blob (same as
	// before WP-6), never corrupts a live object.
	g.eng.HintBackend(chunkContainer, key, backendID)
	if err := g.eng.Delete(ctx, chunkContainer, key); err != nil {
		g.logger.Error("delete chunk data (leaked, not corrupt)",
			zap.String("hash", hash),
			zap.String("key", key),
			zap.Error(err))
		return sweepBlobLeaked, nil
	}

	return sweepDeleted, nil
}
