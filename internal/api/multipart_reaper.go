package api

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"time"

	"github.com/lib/pq"
	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/engine"
)

// MultipartReaper closes the multipart disk-fill DoS (WP-10-minimal, H-1).
//
// Part data streams to <MULTIPART_STAGING_DIR>/{uploadID}/ on local disk and
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
	// engine aborts the backend half of an edge upload (WP-EDGE-1 Part A);
	// nil = such uploads are left for a process that has one.
	engine *engine.CoreEngine
}

// MultipartReapResult holds the outcome of a single reap cycle.
type MultipartReapResult struct {
	Aborted int `json:"aborted"`
	Purged  int `json:"purged"`
	// OrphanDirs: staging directories removed because no row names their
	// upload (sweepOrphanStaging).
	OrphanDirs int `json:"orphan_dirs"`
	// BackendAborted: backend multipart uploads of aborted edge uploads
	// the reaper aborted (an Abort the handler could not make, an
	// abandoned edge upload); BackendAbortFailed: left for the next pass.
	BackendAborted     int `json:"backend_aborted"`
	BackendAbortFailed int `json:"backend_abort_failed"`
}

// orphanStagingGrace is how old a staging directory with no upload row must
// be before the reaper removes it. A directory is created by UploadPart,
// after its upload's row exists, so one with no row is never a live
// upload's; the grace only spares a directory a request is creating right
// now for an upload whose row another request is deleting.
const orphanStagingGrace = time.Hour

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

// multipartReaperJobName is the job's name in job_runs and on the metrics.
const multipartReaperJobName = "multipart_reaper"

// spec is the job's schedule: a reap shortly after boot (a restart after
// downtime should clean up right away, not an hour later), then hourly. A
// run fails when a pass could not list its rows; a single upload that could
// not be aborted or purged is logged and the pass continues.
func (m *MultipartReaper) spec() jobSpec {
	return jobSpec{
		Name: multipartReaperJobName, Every: time.Hour, BootDelay: 10 * time.Second, MaxRunTime: 30 * time.Minute,
		Run: func(ctx context.Context) (jobReport, error) {
			result, err := m.RunOnce(ctx)
			return jobReport{Rows: int64(result.Aborted + result.Purged + result.OrphanDirs)}, err
		},
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

	orphans, err := m.sweepOrphanStaging(ctx)
	if err != nil {
		return result, fmt.Errorf("sweep orphan staging: %w", err)
	}
	result.OrphanDirs = orphans

	aborted, failed, err := m.abortEdgeBackendUploads(ctx)
	if err != nil {
		return result, fmt.Errorf("abort edge backend uploads: %w", err)
	}
	result.BackendAborted, result.BackendAbortFailed = aborted, failed

	return result, nil
}

// abortEdgeBackendUploads aborts the backend half of every ended edge
// upload whose backend call has not been stamped (WP-EDGE-1 Part A): an
// aborted row whose handler abort failed or that the reaper's own abandon
// pass ended, and (RP64-5) a completed row whose Complete stamp failed —
// AbortMultipart of a completed upload answers NoSuchUpload, which the
// driver maps to success, so the stamp follows and the purge can take the
// row. A backend that cannot be asked keeps the row for the next pass; the
// purge never deletes such a row (its backend upload would leak).
func (m *MultipartReaper) abortEdgeBackendUploads(ctx context.Context) (aborted, failed int, err error) {
	if m.engine == nil {
		return 0, 0, nil
	}
	rows, err := m.db.QueryContext(ctx, `
		SELECT upload_id, tenant_id, bucket, edge_backend, backend_upload_id, storage_key
		FROM multipart_uploads
		WHERE status IN ('aborted', 'completed') AND edge AND backend_upload_id IS NOT NULL AND backend_upload_ended_at IS NULL
		LIMIT 200`)
	if err != nil {
		return 0, 0, fmt.Errorf("select aborted edge uploads: %w", err)
	}
	type row struct{ id, tenant, bucket, backend, buid, sk string }
	var due []row
	for rows.Next() {
		var r row
		var backend, sk sql.NullString
		if err := rows.Scan(&r.id, &r.tenant, &r.bucket, &backend, &r.buid, &sk); err != nil {
			_ = rows.Close()
			return 0, 0, fmt.Errorf("scan aborted edge upload: %w", err)
		}
		r.backend, r.sk = backend.String, sk.String
		due = append(due, r)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return 0, 0, fmt.Errorf("iterate aborted edge uploads: %w", err)
	}
	for _, r := range due {
		up := edgeUpload{Edge: true, Backend: r.backend, BackendUploadID: r.buid, StorageKey: r.sk}
		if aErr := abortEdgeUploadOn(ctx, m.db, m.engine, m.logger, r.tenant, r.bucket, r.id, up); aErr != nil {
			failed++
			continue
		}
		aborted++
	}
	return aborted, failed, nil
}

// sweepOrphanStaging removes the staging directories whose upload has no
// row (Stage 1a Prompt 7, Part 0). The account erasure deletes an upload's
// row with the account: an upload created after its abort pass and before
// EraseRows — the customer's client still running — left its staged parts,
// plaintext, on the staging disk until a reboot, because the passes above
// only remove the directories of rows they read.
func (m *MultipartReaper) sweepOrphanStaging(ctx context.Context) (int, error) {
	root := multipartStagingDir()
	entries, err := os.ReadDir(root)
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("list %s: %w", root, err)
	}
	cutoff := time.Now().Add(-orphanStagingGrace)
	var ids []string
	for _, e := range entries {
		if !e.IsDir() || !validUploadID(e.Name()) {
			continue
		}
		info, err := e.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		ids = append(ids, e.Name())
	}
	if len(ids) == 0 {
		return 0, nil
	}
	rows, err := m.db.QueryContext(ctx, `SELECT upload_id FROM multipart_uploads WHERE upload_id = ANY($1)`, pq.Array(ids))
	if err != nil {
		return 0, fmt.Errorf("read upload rows: %w", err)
	}
	known := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("scan upload row: %w", err)
		}
		known[id] = true
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("read upload rows: %w", err)
	}
	removed := 0
	for _, id := range ids {
		if known[id] {
			continue
		}
		if err := os.RemoveAll(multipartDir(id)); err != nil {
			m.logger.Warn("remove orphan staging dir", zap.String("upload_id", id), zap.Error(err))
			continue
		}
		removed++
		m.logger.Info("removed a staging directory whose upload has no row", zap.String("upload_id", id))
	}
	return removed, nil
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
		// the status since the scan — never remove a completed upload's
		// state. What the upload holds is released with the abort, in one
		// transaction (WP-R10-8 Part C).
		ended, err := endUploadReleasing(ctx, m.db, id, "", "", "")
		if err != nil {
			m.logger.Error("abort abandoned upload", zap.String("upload_id", id), zap.Error(err))
			continue
		}
		if !ended {
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
		  AND NOT (edge AND backend_upload_id IS NOT NULL AND backend_upload_ended_at IS NULL)
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
