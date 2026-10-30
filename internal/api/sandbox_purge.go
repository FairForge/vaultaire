package api

// The sandbox's purge (WP-DX-1, Stage 1a Prompt 50): test data is kept 7
// days. POST /api/v1/manage/sandbox/reset (the parent's call) empties the
// sandbox at once — every object's bytes off `local`, every object,
// version, upload, lock and bucket row, its counter back to 0; its keys
// stay. The daily sandbox_purge job (10:30 UTC) removes every sandbox
// object whose last write is older than 7 days, and its versions of that
// age. A sandbox's bytes are on `local` only (placeOn), so the purge asks
// `local` alone; Object Lock is not honoured in test mode (test data).

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"time"

	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/auth"
	"github.com/FairForge/vaultaire/internal/common"
	"github.com/FairForge/vaultaire/internal/engine"
)

const (
	sandboxPurgeJobName = "sandbox_purge"
	// sandboxRetention is how long test data is kept.
	sandboxRetention = 7 * 24 * time.Hour
)

// SandboxPurgeResult is one purge (a reset's answer, a run's job_runs.result).
type SandboxPurgeResult struct {
	Sandboxes       int      `json:"sandboxes"`
	ObjectsDeleted  int      `json:"objects_deleted"`
	VersionsDeleted int      `json:"versions_deleted"`
	UploadsAborted  int      `json:"uploads_aborted"`
	BucketsDeleted  int      `json:"buckets_deleted"`
	Errors          []string `json:"errors,omitempty"`
}

func (r *SandboxPurgeResult) add(o SandboxPurgeResult) {
	r.ObjectsDeleted += o.ObjectsDeleted
	r.VersionsDeleted += o.VersionsDeleted
	r.UploadsAborted += o.UploadsAborted
	r.BucketsDeleted += o.BucketsDeleted
	r.Errors = append(r.Errors, o.Errors...)
}

// purgeSandbox empties sandboxID — wholly (before nil: the reset) or of
// what was last written before before (the nightly purge). A blob that
// cannot be deleted keeps its row (the next run asks again).
func (s *Server) purgeSandbox(ctx context.Context, sandboxID string, before *time.Time) (SandboxPurgeResult, error) {
	return purgeSandbox(ctx, s.db, s.engine, sandboxID, before)
}

// purgeSandbox is the purge itself (the reset, the job and the deletion
// runner's stage before the row erase).
func purgeSandbox(ctx context.Context, db *sql.DB, eng engine.Engine, sandboxID string, before *time.Time) (SandboxPurgeResult, error) {
	var res SandboxPurgeResult
	if !isSandbox(ctx, db, sandboxID) {
		return res, fmt.Errorf("purge %s: not a sandbox", sandboxID) // never a live tenant's data
	}
	ge, _ := eng.(interface {
		GetDriver(string) (engine.Driver, bool)
	})
	if ge == nil {
		return res, fmt.Errorf("purge %s: no engine", sandboxID)
	}
	local, ok := ge.GetDriver(sandboxBackend)
	if !ok {
		return res, fmt.Errorf("purge %s: no %s driver", sandboxID, sandboxBackend)
	}
	dctx := common.WithTenantID(ctx, sandboxID)
	var cutoff any
	if before != nil {
		cutoff = *before
	}
	delBlob := func(bucket, key, storageKey string) bool {
		container, addr := objectBlobAddress(sandboxID, bucket, key, storageKey) // the row's resolved address
		if err := local.Delete(dctx, container, addr); err != nil && !isObjectMissingErr(err) {
			res.Errors = append(res.Errors, fmt.Sprintf("delete %s/%s: %v", bucket, key, err))
			return false
		}
		return true
	}

	type obj struct{ bucket, key, storageKey, version string }
	collect := func(q string) ([]obj, error) {
		rows, err := db.QueryContext(ctx, q, sandboxID, cutoff)
		if err != nil {
			return nil, err
		}
		defer func() { _ = rows.Close() }()
		var out []obj
		for rows.Next() {
			var o obj
			if err := rows.Scan(&o.bucket, &o.key, &o.storageKey, &o.version); err != nil {
				return nil, err
			}
			out = append(out, o)
		}
		return out, rows.Err()
	}

	heads, err := collect(`SELECT bucket, object_key, COALESCE(storage_key, ''), '' FROM object_head_cache
		WHERE tenant_id = $1 AND ($2::timestamptz IS NULL OR updated_at < $2)`)
	if err != nil {
		return res, fmt.Errorf("purge %s: heads: %w", sandboxID, err)
	}
	for _, o := range heads {
		if !delBlob(o.bucket, o.key, o.storageKey) {
			continue
		}
		if _, err := db.ExecContext(ctx, `DELETE FROM object_head_cache WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3`,
			sandboxID, o.bucket, o.key); err != nil {
			return res, fmt.Errorf("purge %s: head row: %w", sandboxID, err)
		}
		res.ObjectsDeleted++
	}

	versions, err := collect(`SELECT bucket, object_key, COALESCE(storage_key, ''), version_id FROM object_versions
		WHERE tenant_id = $1 AND ($2::timestamptz IS NULL OR created_at < $2)`)
	if err != nil {
		return res, fmt.Errorf("purge %s: versions: %w", sandboxID, err)
	}
	for _, o := range versions {
		var named bool
		if err := db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM object_head_cache WHERE tenant_id = $1 AND bucket = $2
			AND object_key = $3 AND COALESCE(storage_key, object_key) = $4)`, sandboxID, o.bucket, o.key, o.storageKey).Scan(&named); err != nil {
			return res, fmt.Errorf("purge %s: version naming: %w", sandboxID, err)
		}
		if o.storageKey != "" && !named && !delBlob(o.bucket, o.key, o.storageKey) {
			continue
		}
		if _, err := db.ExecContext(ctx, `DELETE FROM object_versions WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3 AND version_id = $4`,
			sandboxID, o.bucket, o.key, o.version); err != nil {
			return res, fmt.Errorf("purge %s: version row: %w", sandboxID, err)
		}
		res.VersionsDeleted++
	}

	// Active uploads are aborted (the multipart reaper removes their staged
	// parts and their rows, as for any aborted upload).
	out, err := db.ExecContext(ctx, `UPDATE multipart_uploads SET status = 'aborted'
		WHERE tenant_id = $1 AND status = 'active' AND ($2::timestamptz IS NULL OR created_at < $2)`, sandboxID, cutoff)
	if err != nil {
		return res, fmt.Errorf("purge %s: uploads: %w", sandboxID, err)
	}
	n, _ := out.RowsAffected()
	res.UploadsAborted = int(n)

	if before == nil && len(res.Errors) == 0 {
		if _, err := db.ExecContext(ctx, `DELETE FROM object_locks WHERE tenant_id = $1`, sandboxID); err != nil {
			return res, fmt.Errorf("purge %s: locks: %w", sandboxID, err)
		}
		out, err := db.ExecContext(ctx, `DELETE FROM buckets WHERE tenant_id = $1`, sandboxID)
		if err != nil {
			return res, fmt.Errorf("purge %s: buckets: %w", sandboxID, err)
		}
		n, _ := out.RowsAffected()
		res.BucketsDeleted = int(n)
	}
	// The sandbox's counter is what remains (its own cap, never a bill). A
	// sandbox is its own pool's root: its pool counter moves with it (RP82-3).
	if _, err := db.ExecContext(ctx, `UPDATE tenant_quotas SET
		storage_used_bytes = (SELECT COALESCE(SUM(size_bytes), 0) FROM object_head_cache WHERE tenant_id = $1),
		pool_used_bytes = pool_used_bytes - storage_used_bytes + (SELECT COALESCE(SUM(size_bytes), 0) FROM object_head_cache WHERE tenant_id = $1),
		updated_at = NOW()
		WHERE tenant_id = $1`, sandboxID); err != nil {
		return res, fmt.Errorf("purge %s: counter: %w", sandboxID, err)
	}
	return res, nil
}

// handleMgmtSandboxReset is POST /api/v1/manage/sandbox/reset.
func (s *Server) handleMgmtSandboxReset(w http.ResponseWriter, r *http.Request) {
	tenantID, _ := r.Context().Value(tenantIDKey).(string)
	if tenantID == "" || s.db == nil {
		writeManagementError(w, ErrTypeAuthentication, "missing_tenant", "tenant not found in token", "")
		return
	}
	resp := map[string]any{"object": "sandbox_reset", "objects_deleted": 0, "versions_deleted": 0, "uploads_aborted": 0, "buckets_deleted": 0,
		"request_id": getRequestID(w)}
	sandbox, err := auth.SandboxOf(r.Context(), s.db, tenantID)
	if err != nil {
		s.logger.Error("sandbox reset: the sandbox could not be read", zap.Error(err))
		writeManagementError(w, ErrTypeAPI, "internal_error", "failed to reset the sandbox", "")
		return
	}
	if sandbox != "" {
		res, err := s.purgeSandbox(r.Context(), sandbox, nil)
		if err != nil || len(res.Errors) > 0 {
			s.logger.Error("sandbox reset failed", zap.Error(err), zap.Strings("errors", res.Errors), zap.String("tenant_id", tenantID))
			writeManagementError(w, ErrTypeAPI, "internal_error", "the sandbox was not fully reset; retry", "")
			return
		}
		resp["objects_deleted"], resp["versions_deleted"] = res.ObjectsDeleted, res.VersionsDeleted
		resp["uploads_aborted"], resp["buckets_deleted"] = res.UploadsAborted, res.BucketsDeleted
	}
	writeJSON(w, http.StatusOK, resp)
}

// SandboxPurgeJob is the daily sandbox_purge job.
type SandboxPurgeJob struct {
	s          *Server
	JobName    string
	onlyTenant string // tests: one sandbox
	now        func() time.Time
}

func newSandboxPurgeJob(s *Server) *SandboxPurgeJob {
	return &SandboxPurgeJob{s: s, JobName: sandboxPurgeJobName, now: time.Now}
}

func (j *SandboxPurgeJob) spec() jobSpec {
	return jobSpec{Name: j.JobName, Hour: 10, Minute: 30, BootDelay: 9 * time.Minute, MaxRunTime: 30 * time.Minute,
		Run: func(ctx context.Context) (jobReport, error) {
			res, err := j.RunOnce(ctx)
			note := ""
			if len(res.Errors) > 0 {
				note = fmt.Sprintf("%d deletes failed (first: %s); retried next run", len(res.Errors), res.Errors[0])
			}
			return jobReport{Rows: int64(res.ObjectsDeleted + res.VersionsDeleted), Result: res, Note: note}, err
		}}
}

// RunOnce purges every sandbox of what is older than sandboxRetention.
func (j *SandboxPurgeJob) RunOnce(ctx context.Context) (SandboxPurgeResult, error) {
	var res SandboxPurgeResult
	rows, err := j.s.db.QueryContext(ctx, `SELECT id FROM tenants WHERE sandbox_of IS NOT NULL AND ($1 = '' OR id = $1)`, j.onlyTenant)
	if err != nil {
		return res, fmt.Errorf("sandbox purge: %w", err)
	}
	ids, err := scanStrings(rows)
	if err != nil {
		return res, fmt.Errorf("sandbox purge: %w", err)
	}
	cutoff := j.now().Add(-sandboxRetention)
	for _, id := range ids {
		one, err := j.s.purgeSandbox(ctx, id, &cutoff)
		res.add(one)
		res.Sandboxes++
		if err != nil {
			res.Errors = append(res.Errors, err.Error())
		}
	}
	return res, nil
}

// scanStrings reads one string column.
func scanStrings(rows *sql.Rows) ([]string, error) {
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}
