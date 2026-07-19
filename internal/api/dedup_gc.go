package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
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
}

// DedupGCResult holds the outcome of a single GC run.
type DedupGCResult struct {
	Reconciled     int   `json:"reconciled"`
	Deleted        int   `json:"deleted"`
	BytesReclaimed int64 `json:"bytes_reclaimed"`
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
	}
}

// StartDedupGC runs a background goroutine that triggers RunOnce daily.
func (g *DedupGCRunner) StartDedupGC(ctx context.Context) {
	if g == nil {
		return
	}
	go func() {
		ticker := time.NewTicker(24 * time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				result, err := g.RunOnce(ctx)
				if err != nil {
					g.logger.Error("dedup gc failed", zap.Error(err))
					continue
				}
				g.logger.Info("dedup gc completed",
					zap.Int("reconciled", result.Reconciled),
					zap.Int("deleted", result.Deleted),
					zap.Int64("bytes_reclaimed", result.BytesReclaimed))
			}
		}
	}()
}

// RunOnce performs a single GC cycle: reconcile then sweep.
func (g *DedupGCRunner) RunOnce(ctx context.Context) (DedupGCResult, error) {
	var result DedupGCResult

	reconciled, err := g.reconcile(ctx)
	if err != nil {
		return result, fmt.Errorf("reconcile: %w", err)
	}
	result.Reconciled = reconciled

	deleted, reclaimed, err := g.sweep(ctx)
	if err != nil {
		return result, fmt.Errorf("sweep: %w", err)
	}
	result.Deleted = deleted
	result.BytesReclaimed = reclaimed

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
func (g *DedupGCRunner) sweep(ctx context.Context) (int, int64, error) {
	graceSecs := int(g.GracePeriod.Seconds())
	rows, err := g.db.QueryContext(ctx, `
		SELECT dedup_scope, plaintext_hash, backend_id, storage_key, size_bytes
		FROM global_content_index
		WHERE marked_for_deletion = TRUE
		  AND ref_count = 0
		  AND marked_at < NOW() - make_interval(secs => $1)
	`, graceSecs)
	if err != nil {
		return 0, 0, fmt.Errorf("sweep query: %w", err)
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
			return 0, 0, fmt.Errorf("scan candidate: %w", err)
		}
		candidates = append(candidates, c)
	}
	if err := rows.Err(); err != nil {
		return 0, 0, fmt.Errorf("iterate candidates: %w", err)
	}

	var deleted int
	var reclaimed int64
	for _, c := range candidates {
		ok, err := g.sweepOne(ctx, c.scope, c.hash, c.backendID, c.key)
		if err != nil {
			g.logger.Error("sweep chunk",
				zap.String("hash", c.hash), zap.Error(err))
			continue
		}
		if ok {
			deleted++
			reclaimed += c.size
		}
	}

	return deleted, reclaimed, nil
}

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
func (g *DedupGCRunner) sweepOne(ctx context.Context, scope, hash, backendID, key string) (bool, error) {
	conn, err := g.db.Conn(ctx)
	if err != nil {
		return false, fmt.Errorf("acquire sweep conn: %w", err)
	}
	defer func() { _ = conn.Close() }()

	if _, err := conn.ExecContext(ctx,
		`SELECT pg_advisory_lock(hashtext($1), hashtext($2))`, scope, hash); err != nil {
		return false, fmt.Errorf("advisory lock: %w", err)
	}
	defer func() {
		_, _ = conn.ExecContext(ctx,
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
		return false, fmt.Errorf("delete gci row: %w", err)
	}
	affected, _ := res.RowsAffected()
	if affected == 0 {
		// Re-referenced since the candidate scan — nothing to do.
		return false, nil
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
		return false, nil
	}

	return true, nil
}

func (s *Server) handleDedupGCTrigger(w http.ResponseWriter, r *http.Request) {
	if s.dedupGCRunner == nil {
		http.Error(w, "dedup gc not available", http.StatusServiceUnavailable)
		return
	}
	result, err := s.dedupGCRunner.RunOnce(r.Context())
	if err != nil {
		s.logger.Error("manual dedup gc failed", zap.Error(err))
		http.Error(w, "gc failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(result)
}
