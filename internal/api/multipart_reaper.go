package api

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"time"

	"go.uber.org/zap"
)

// MultipartReaper closes the multipart disk-fill DoS (WP-10-minimal, H-1).
//
// Part data streams to /tmp/vaultaire-multipart/{uploadID}/ on local disk and
// is unbilled until CompleteMultipartUpload (quota reserves at complete,
// WP-1). Before the reaper existed, an initiated-but-never-completed upload
// kept its part files forever: a malicious or crashed client could fill the
// production disk at zero quota cost, and completed/aborted rows accumulated
// without bound.
//
// Two passes per cycle:
//   - Abort: active uploads whose LAST ACTIVITY (upload creation or newest
//     part) is older than AbandonAge are marked aborted and their temp dirs
//     removed. Activity-based, not initiation-based, so a slow-but-live
//     client pushing parts for days is never killed mid-upload.
//   - Purge: completed/aborted rows older than TerminalRetention are deleted
//     (part rows cascade via FK), removing any leftover temp dir first —
//     this also re-sweeps orphan part files written by a request that raced
//     the abort.
type MultipartReaper struct {
	db                *sql.DB
	logger            *zap.Logger
	AbandonAge        time.Duration
	TerminalRetention time.Duration
}

// MultipartReapResult holds the outcome of a single reap cycle.
type MultipartReapResult struct {
	Aborted int `json:"aborted"`
	Purged  int `json:"purged"`
}

func NewMultipartReaper(db *sql.DB, logger *zap.Logger) *MultipartReaper {
	if db == nil {
		return nil
	}
	return &MultipartReaper{
		db:                db,
		logger:            logger,
		AbandonAge:        48 * time.Hour,
		TerminalRetention: 7 * 24 * time.Hour,
	}
}

// Start runs one immediate reap (a restart after downtime should clean up
// right away, not an hour later) and then reaps hourly until ctx is done.
func (m *MultipartReaper) Start(ctx context.Context) {
	if m == nil {
		return
	}
	go func() {
		m.runAndLog(ctx)
		ticker := time.NewTicker(1 * time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				m.runAndLog(ctx)
			}
		}
	}()
}

func (m *MultipartReaper) runAndLog(ctx context.Context) {
	result, err := m.RunOnce(ctx)
	if err != nil {
		m.logger.Error("multipart reaper failed", zap.Error(err))
		return
	}
	if result.Aborted > 0 || result.Purged > 0 {
		m.logger.Info("multipart reaper completed",
			zap.Int("aborted", result.Aborted),
			zap.Int("purged", result.Purged))
	}
}

// RunOnce performs a single reap cycle.
func (m *MultipartReaper) RunOnce(ctx context.Context) (MultipartReapResult, error) {
	var result MultipartReapResult

	aborted, err := m.abortAbandoned(ctx)
	if err != nil {
		return result, fmt.Errorf("abort abandoned uploads: %w", err)
	}
	result.Aborted = aborted

	purged, err := m.purgeTerminal(ctx)
	if err != nil {
		return result, fmt.Errorf("purge terminal uploads: %w", err)
	}
	result.Purged = purged

	return result, nil
}

func (m *MultipartReaper) abortAbandoned(ctx context.Context) (int, error) {
	graceSecs := int(m.AbandonAge.Seconds())
	rows, err := m.db.QueryContext(ctx, `
		SELECT u.upload_id
		FROM multipart_uploads u
		LEFT JOIN multipart_parts p ON p.upload_id = u.upload_id
		WHERE u.status = 'active'
		GROUP BY u.upload_id, u.created_at
		HAVING GREATEST(u.created_at, COALESCE(MAX(p.created_at), u.created_at))
		       < NOW() - make_interval(secs => $1)
	`, graceSecs)
	if err != nil {
		return 0, fmt.Errorf("select abandoned: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return 0, fmt.Errorf("scan abandoned: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("iterate abandoned: %w", err)
	}

	var aborted int
	for _, id := range ids {
		// Conditional: a concurrent CompleteMultipartUpload may have flipped
		// the status since the scan — never remove a completed upload's state.
		res, err := m.db.ExecContext(ctx, `
			UPDATE multipart_uploads SET status = 'aborted'
			WHERE upload_id = $1 AND status = 'active'
		`, id)
		if err != nil {
			m.logger.Error("abort abandoned upload", zap.String("upload_id", id), zap.Error(err))
			continue
		}
		if n, _ := res.RowsAffected(); n == 0 {
			continue
		}
		if err := os.RemoveAll(multipartDir(id)); err != nil {
			// Dir removal failing leaks disk, not correctness; the terminal
			// purge retries it before deleting the row.
			m.logger.Warn("remove abandoned upload dir", zap.String("upload_id", id), zap.Error(err))
		}
		aborted++
		m.logger.Info("aborted abandoned multipart upload", zap.String("upload_id", id))
	}
	return aborted, nil
}

func (m *MultipartReaper) purgeTerminal(ctx context.Context) (int, error) {
	retentionSecs := int(m.TerminalRetention.Seconds())
	rows, err := m.db.QueryContext(ctx, `
		SELECT upload_id FROM multipart_uploads
		WHERE status IN ('completed', 'aborted')
		  AND created_at < NOW() - make_interval(secs => $1)
	`, retentionSecs)
	if err != nil {
		return 0, fmt.Errorf("select terminal: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return 0, fmt.Errorf("scan terminal: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("iterate terminal: %w", err)
	}

	var purged int
	for _, id := range ids {
		// Temp dir first (complete/abort already removed it; this re-sweeps
		// orphans), then the row — part rows cascade via FK.
		if err := os.RemoveAll(multipartDir(id)); err != nil {
			m.logger.Warn("remove terminal upload dir", zap.String("upload_id", id), zap.Error(err))
		}
		if _, err := m.db.ExecContext(ctx, `
			DELETE FROM multipart_uploads WHERE upload_id = $1
		`, id); err != nil {
			m.logger.Error("purge terminal upload", zap.String("upload_id", id), zap.Error(err))
			continue
		}
		purged++
	}
	return purged, nil
}
