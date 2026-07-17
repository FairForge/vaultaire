package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/FairForge/vaultaire/internal/engine"
	"go.uber.org/zap"
)

// DedupGCRunner reclaims storage from orphaned deduplicated chunks.
// Phase A reconciles ref counts against actual tenant_chunk_refs rows.
// Phase B deletes chunks that have been marked_for_deletion past the grace period.
type DedupGCRunner struct {
	db          *sql.DB
	eng         *engine.CoreEngine
	logger      *zap.Logger
	GracePeriod time.Duration
}

// DedupGCResult holds the outcome of a single GC run.
type DedupGCResult struct {
	Reconciled     int   `json:"reconciled"`
	Deleted        int   `json:"deleted"`
	BytesReclaimed int64 `json:"bytes_reclaimed"`
}

func NewDedupGCRunner(db *sql.DB, eng *engine.CoreEngine, logger *zap.Logger) *DedupGCRunner {
	if db == nil || eng == nil {
		return nil
	}
	return &DedupGCRunner{
		db:          db,
		eng:         eng,
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
		res, err := g.db.ExecContext(ctx, `
			DELETE FROM global_content_index
			WHERE dedup_scope = $1
			  AND plaintext_hash = $2
			  AND ref_count = 0
			  AND marked_for_deletion = TRUE
		`, c.scope, c.hash)
		if err != nil {
			g.logger.Error("delete gci row",
				zap.String("hash", c.hash), zap.Error(err))
			continue
		}
		affected, _ := res.RowsAffected()
		if affected == 0 {
			continue
		}

		g.eng.HintBackend(chunkContainer, c.key, c.backendID)
		if err := g.eng.Delete(ctx, chunkContainer, c.key); err != nil {
			g.logger.Error("delete chunk data (leaked, not corrupt)",
				zap.String("hash", c.hash),
				zap.String("key", c.key),
				zap.Error(err))
			continue
		}

		deleted++
		reclaimed += c.size
	}

	return deleted, reclaimed, nil
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
