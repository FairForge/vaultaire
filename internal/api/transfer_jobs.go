package api

// WP-XFER-1 — transfer in (Stage 1a Prompt 59 PR A; docs/reviews/WP-STRIPE-11.md,
// docs/API.md § Transfers). A tenant gives a source (an S3-compatible
// endpoint, region, bucket, prefix and a key it made) and a destination
// bucket of its own; the `transfer` job (every minute) lists the source from
// the job's cursor into transfer_items, 1,000 keys a page, and copies each
// item THROUGH THE S3 API'S OWN HANDLERS — handlePutObject for a body under
// transferMultipartMin, the multipart handlers (initiate, a part per
// transferPartBytes, complete; abort on failure) above it — on a request
// carrying the tenant as an authenticated S3 request does. So placement
// (the ONE decision), the quota reservation, Object Lock, the version rows,
// the floor and the events are a PUT's, never a copy beside it.
//
//   - Idempotent on (source ETag, size): an item whose destination head row
//     has that ETag and size — or a destination written by an earlier
//     transfer of this source object, still at the ETag that copy gave it —
//     is `skipped`.
//   - Ranged GETs of transferRangeBytes, each pinned with If-Match to the
//     listing's ETag: a source object changed under the copy fails the item,
//     never splices bytes. A body under the multipart size whose ETag is an
//     MD5 is sent with its Content-MD5, so the PUT path checks the bytes.
//   - Metered as ingest on the destination's quota (the PUT's reservation);
//     a refusal for quota pauses the job (paused_reason quota, the 403's
//     code) until POST …/resume.
//   - A source 5xx / timeout is retried transferItemAttempts times, then the
//     item is `failed` and the job goes on; a source 401/403 pauses the job
//     (source_denied).
//   - Two rate caps, each one token bucket: the job's rate_bytes_per_sec
//     (default and ceiling TRANSFER_RATE_MAX) and the process's
//     TRANSFER_ORIGIN_SHARE (Step 0 answer 3: a transferred byte crosses SLC
//     twice).
//   - The source key (D-XFER-1): wrapped under the tenant key
//     (SealCredential) when the server has a master key, else the plaintext
//     column; NULLED with the access key id when the job completes, fails or
//     is cancelled. Never logged, never exported.
//   - The last item: `completed`, the key nulled, a `transfer.completed`
//     event (the counters, never the key) to the tenant's webhook endpoints.
//   - One job a tenant at a time: a session advisory lock per tenant.
//
// A vault-floor destination is 501 (D-XFER-2: Geyser's cloudSync is pinned
// to AWS), a Private (ssec_required) one 409: the job holds no customer key.

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"
	"golang.org/x/time/rate"

	"github.com/FairForge/vaultaire/internal/common"
	"github.com/FairForge/vaultaire/internal/tenant"
)

const (
	transferJobName      = "transfer"
	transferListPage     = 1000
	transferMaxParts     = 10000
	transferItemAttempts = 3
	transferRunTime      = 50 * time.Second
	transferItemBatch    = 100
	// The knobs' defaults (Step 0 answer 3).
	defaultTransferRateMax     = 25 << 20
	defaultTransferOriginShare = 50 << 20
	// transferBurst bounds what one wait of a token bucket may take.
	transferBurst = 1 << 20
)

// The copy's sizes (variables: a test copies a multipart object of a few
// MiB): ranged GETs of transferRangeBytes; a body of transferMultipartMin or
// more goes as a multipart upload of transferPartBytes parts.
var (
	transferRangeBytes   int64 = 16 << 20
	transferMultipartMin int64 = 64 << 20
	transferPartBytes    int64 = 64 << 20
)

// The job states (migration 127's CHECK).
const (
	transferPending   = "pending"
	transferRunning   = "running"
	transferPaused    = "paused"
	transferCompleted = "completed"
	transferFailed    = "failed"
	transferCancelled = "cancelled"
)

// The item outcomes.
const (
	itemPending = "pending"
	itemCopied  = "copied"
	itemSkipped = "skipped"
	itemFailed  = "failed"
)

var (
	transferItems = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "vaultaire_transfer_items_total",
		Help: "Source objects the transfer job settled, by outcome (copied, skipped, failed).",
	}, []string{"outcome"})
	transferBytes = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "vaultaire_transfer_bytes_total",
		Help: "Bytes the transfer job copied from tenants' sources into their buckets.",
	})
)

// transferConfig is the knobs: TRANSFER_RATE_MAX (a job's default and
// ceiling) and TRANSFER_ORIGIN_SHARE (every job on the process), sizes per
// second ("25MiB", "25MiB/s", "26214400").
type transferConfig struct {
	rateMax     int64
	originShare int64
}

func transferConfigFromEnv(getenv func(string) string, logger *zap.Logger) transferConfig {
	c := transferConfig{rateMax: defaultTransferRateMax, originShare: defaultTransferOriginShare}
	knob := func(name string, dst *int64) {
		raw := strings.TrimSpace(getenv(name))
		if raw == "" {
			return
		}
		n, err := parseByteSize(strings.TrimSuffix(raw, "/s"))
		if err != nil || n < 1<<20 || n > 10<<30 {
			logger.Warn(name+" rejected (a size per second, 1MiB–10GiB); default kept", zap.String("value", raw), zap.Int64("default", *dst))
			return
		}
		*dst = n
	}
	knob("TRANSFER_RATE_MAX", &c.rateMax)
	knob("TRANSFER_ORIGIN_SHARE", &c.originShare)
	return c
}

// transferRuntime is the process's state of the feature: the knobs and the
// origin's one token bucket.
type transferRuntime struct {
	cfg    transferConfig
	origin *rate.Limiter
}

func newTransferRuntime(cfg transferConfig) *transferRuntime {
	return &transferRuntime{cfg: cfg, origin: rate.NewLimiter(rate.Limit(cfg.originShare), transferBurst)}
}

// transfers is the server's runtime (lazily the defaults: tests).
func (s *Server) transfers() *transferRuntime {
	s.transferOnce.Do(func() {
		if s.transfer == nil {
			s.transfer = newTransferRuntime(transferConfig{rateMax: defaultTransferRateMax, originShare: defaultTransferOriginShare})
		}
	})
	return s.transfer
}

// transferRow is a transfer_jobs row.
type transferRow struct {
	ID, TenantID, Bucket                string
	Endpoint, Region, SrcBucket, Prefix string
	State, PausedReason, PausedCode     string
	Cursor                              string
	ListedAll                           bool
	Listed, Copied, Skipped, Failed     int64
	Bytes, Rate                         int64
	LastError                           string
	CreatedAt, UpdatedAt                time.Time
	StartedAt, FinishedAt               *time.Time
	accessKey, secretPlain              sql.NullString
	secretWrapped                       []byte
	secretVersion                       sql.NullInt64
}

const transferColumns = `id, tenant_id, bucket, source_endpoint, source_region, source_bucket, source_prefix, state, paused_reason, paused_error,
	list_cursor, listed_all, listed, copied, skipped, failed, bytes_copied, rate_bytes_per_sec, last_error, created_at, updated_at, started_at, finished_at,
	access_key_id, secret_key, secret_wrapped, secret_key_version`

func scanTransfer(row interface{ Scan(...any) error }) (*transferRow, error) {
	var t transferRow
	err := row.Scan(&t.ID, &t.TenantID, &t.Bucket, &t.Endpoint, &t.Region, &t.SrcBucket, &t.Prefix, &t.State, &t.PausedReason, &t.PausedCode,
		&t.Cursor, &t.ListedAll, &t.Listed, &t.Copied, &t.Skipped, &t.Failed, &t.Bytes, &t.Rate, &t.LastError, &t.CreatedAt, &t.UpdatedAt,
		&t.StartedAt, &t.FinishedAt, &t.accessKey, &t.secretPlain, &t.secretWrapped, &t.secretVersion)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// transferEndSQL ends a job: the state, and the key nulled in the SAME
// statement (D-XFER-1).
const transferEndSQL = `UPDATE transfer_jobs SET state = $2, access_key_id = NULL, secret_key = NULL, secret_wrapped = NULL,
	secret_key_version = NULL, finished_at = NOW(), updated_at = NOW(), last_error = COALESCE(NULLIF($3, ''), last_error)
	WHERE id = $1 AND state NOT IN ('completed', 'failed', 'cancelled')`

// sealTransferSecret is what the row stores of the source secret: wrapped
// under the tenant key with a master key, else the plaintext.
func (s *Server) sealTransferSecret(ctx context.Context, tenantID, jobID, secret string) (plain sql.NullString, wrapped []byte, version sql.NullInt64, err error) {
	if s.tenantKeys == nil {
		return sql.NullString{String: secret, Valid: true}, nil, sql.NullInt64{}, nil
	}
	w, v, err := s.tenantKeys.SealCredential(ctx, tenantID, jobID, []byte(secret))
	if err != nil {
		return sql.NullString{}, nil, sql.NullInt64{}, fmt.Errorf("seal the source key: %w", err)
	}
	return sql.NullString{}, w, sql.NullInt64{Int64: int64(v), Valid: true}, nil
}

// openTransferSecret is the source secret of a row ("" when it is gone).
func (s *Server) openTransferSecret(ctx context.Context, t *transferRow) (string, error) {
	if len(t.secretWrapped) > 0 {
		if s.tenantKeys == nil || !t.secretVersion.Valid {
			return "", errors.New("the source key is wrapped and the server has no master key")
		}
		b, err := s.tenantKeys.OpenCredential(ctx, t.TenantID, t.ID, int(t.secretVersion.Int64), t.secretWrapped)
		if err != nil {
			return "", fmt.Errorf("open the source key: %w", err)
		}
		return string(b), nil
	}
	return t.secretPlain.String, nil
}

// --- the job ---

// TransferJob is the scheduler's job.
type TransferJob struct {
	s *Server
	// JobName is the job_runs name; onlyTenant limits a run (tests share the
	// database); runTime bounds a run's copying.
	JobName    string
	onlyTenant string
	runTime    time.Duration
	// stopAfter, when set, ends a run after that many settled items (a
	// test's restart).
	stopAfter int
}

func newTransferJob(s *Server) *TransferJob {
	if s.db == nil {
		return nil
	}
	for _, o := range []string{itemCopied, itemSkipped, itemFailed} {
		transferItems.WithLabelValues(o)
	}
	return &TransferJob{s: s, JobName: transferJobName, runTime: transferRunTime}
}

func (j *TransferJob) spec() jobSpec {
	return jobSpec{Name: j.JobName, Every: time.Minute, BootDelay: 3 * time.Minute, MaxRunTime: 58 * time.Second,
		Run: func(ctx context.Context) (jobReport, error) {
			n, err := j.RunOnce(ctx)
			return jobReport{Rows: int64(n)}, err
		}}
}

// RunOnce works the due jobs, oldest first, each under its tenant's lock,
// until the run's time is spent; the items it settled.
func (j *TransferJob) RunOnce(ctx context.Context) (int, error) {
	until := time.Now().Add(j.runTime)
	q := `SELECT id, tenant_id FROM transfer_jobs WHERE state IN ('pending', 'running')`
	args := []any{}
	if j.onlyTenant != "" {
		q += ` AND tenant_id = $1`
		args = append(args, j.onlyTenant)
	}
	rows, err := j.s.db.QueryContext(ctx, q+` ORDER BY created_at LIMIT 50`, args...)
	if err != nil {
		return 0, fmt.Errorf("transfer: the due jobs: %w", err)
	}
	type due struct{ id, tenant string }
	var list []due
	for rows.Next() {
		var d due
		if err := rows.Scan(&d.id, &d.tenant); err != nil {
			_ = rows.Close()
			return 0, err
		}
		list = append(list, d)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	settled := 0
	busy := map[string]bool{}
	for _, d := range list {
		if time.Now().After(until) || ctx.Err() != nil || busy[d.tenant] {
			continue
		}
		busy[d.tenant] = true // one job a tenant per run
		n, err := j.runTenantJob(ctx, d.id, d.tenant, until)
		settled += n
		if err != nil {
			j.s.log().Warn("transfer: a job's run failed", zap.String("job", d.id), zap.String("tenant_id", d.tenant), zap.Error(err))
		}
		if j.stopAfter > 0 && settled >= j.stopAfter {
			break
		}
	}
	return settled, nil
}

// runTenantJob takes the tenant's lock (a session lock on its own
// connection) and works the job.
func (j *TransferJob) runTenantJob(ctx context.Context, id, tenantID string, until time.Time) (int, error) {
	conn, err := j.s.db.Conn(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = conn.Close() }()
	var got bool
	if err := conn.QueryRowContext(ctx, `SELECT pg_try_advisory_lock(hashtext('transfer'), hashtext($1))`, tenantID).Scan(&got); err != nil {
		return 0, err
	}
	if !got {
		return 0, nil
	}
	defer func() {
		_, _ = conn.ExecContext(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock(hashtext('transfer'), hashtext($1))`, tenantID)
	}()
	return j.work(ctx, id, until)
}

func (j *TransferJob) load(ctx context.Context, id string) (*transferRow, error) {
	return scanTransfer(j.s.db.QueryRowContext(ctx, `SELECT `+transferColumns+` FROM transfer_jobs WHERE id = $1`, id))
}

// work lists and copies until the job is done, paused, or the run's time
// is spent.
func (j *TransferJob) work(ctx context.Context, id string, until time.Time) (int, error) {
	t, err := j.load(ctx, id)
	if err != nil {
		return 0, err
	}
	if t.State != transferPending && t.State != transferRunning {
		return 0, nil
	}
	if !t.accessKey.Valid {
		_, err := j.s.db.ExecContext(ctx, transferEndSQL, id, transferFailed, "the source key is gone")
		return 0, err
	}
	secret, err := j.s.openTransferSecret(ctx, t)
	if err != nil {
		_, _ = j.s.db.ExecContext(ctx, transferEndSQL, id, transferFailed, err.Error())
		return 0, nil
	}
	if _, err := j.s.db.ExecContext(ctx, `UPDATE transfer_jobs SET state = 'running', started_at = COALESCE(started_at, NOW()), updated_at = NOW()
		WHERE id = $1 AND state IN ('pending', 'running')`, id); err != nil {
		return 0, err
	}
	src := newTransferSource(t.Endpoint, t.Region, t.SrcBucket, t.Prefix, t.accessKey.String, secret)
	jobLimit := rate.NewLimiter(rate.Limit(t.Rate), transferBurst)
	settled := 0
	for time.Now().Before(until) && ctx.Err() == nil {
		if j.stopAfter > 0 && settled >= j.stopAfter {
			return settled, nil
		}
		// Keep a page of items ahead of the copy.
		var pending int
		if err := j.s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM transfer_items WHERE job_id = $1 AND state = 'pending'`, id).Scan(&pending); err != nil {
			return settled, err
		}
		if pending == 0 {
			t, err = j.load(ctx, id)
			if err != nil {
				return settled, err
			}
			if t.ListedAll {
				return settled, j.complete(ctx, t)
			}
			if stop, err := j.listPage(ctx, t, src); err != nil || stop {
				return settled, err
			}
			continue
		}
		items, err := j.pendingItems(ctx, id)
		if err != nil {
			return settled, err
		}
		for _, it := range items {
			if time.Now().After(until) || ctx.Err() != nil || (j.stopAfter > 0 && settled >= j.stopAfter) {
				return settled, nil
			}
			stop, err := j.copyItem(ctx, t, src, jobLimit, it)
			if err != nil {
				return settled, err
			}
			settled++
			if stop {
				return settled, nil
			}
		}
	}
	return settled, nil
}

// listPage lists one page after the cursor into transfer_items; stop = the
// job paused or ended.
func (j *TransferJob) listPage(ctx context.Context, t *transferRow, src *transferSource) (bool, error) {
	page, more, err := src.list(ctx, t.Cursor, transferListPage)
	if err != nil {
		class, code := classifySourceError(err)
		switch class {
		case sourceDenied:
			return true, j.pause(ctx, t.ID, "source_denied", code)
		case sourceGone, sourceUnreachable:
			_, err := j.s.db.ExecContext(ctx, transferEndSQL, t.ID, transferFailed, "the source listing: "+code)
			return true, err
		}
		_, _ = j.s.db.ExecContext(ctx, `UPDATE transfer_jobs SET last_error = $2, updated_at = NOW() WHERE id = $1`, t.ID, "the source listing: "+code)
		return true, nil // retried next run
	}
	tx, err := j.s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	cursor := t.Cursor
	for _, it := range page {
		if _, err := tx.ExecContext(ctx, `INSERT INTO transfer_items (job_id, tenant_id, source_key, source_etag, size) VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT DO NOTHING`, t.ID, t.TenantID, it.Key, it.ETag, it.Size); err != nil {
			return false, err
		}
		cursor = it.Key
	}
	if _, err := tx.ExecContext(ctx, `UPDATE transfer_jobs SET list_cursor = $2, listed = listed + $3, listed_all = $4, updated_at = NOW() WHERE id = $1`,
		t.ID, cursor, len(page), !more || len(page) == 0); err != nil {
		return false, err
	}
	return false, tx.Commit()
}

// transferItem is a pending transfer_items row.
type transferItem struct {
	Key, ETag string
	Size      int64
	Attempts  int
}

func (j *TransferJob) pendingItems(ctx context.Context, id string) ([]transferItem, error) {
	rows, err := j.s.db.QueryContext(ctx, `SELECT source_key, source_etag, size, attempts FROM transfer_items
		WHERE job_id = $1 AND state = 'pending' ORDER BY source_key LIMIT $2`, id, transferItemBatch)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []transferItem
	for rows.Next() {
		var it transferItem
		if err := rows.Scan(&it.Key, &it.ETag, &it.Size, &it.Attempts); err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// settle records an item's outcome and the job's counters in one
// transaction.
func (j *TransferJob) settle(ctx context.Context, t *transferRow, it transferItem, outcome, why, destETag string, bytes int64) error {
	tx, err := j.s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `UPDATE transfer_items SET state = $3, last_error = $4, dest_etag = NULLIF($5, ''), attempts = attempts + 1, updated_at = NOW()
		WHERE job_id = $1 AND source_key = $2`, t.ID, it.Key, outcome, why, destETag); err != nil {
		return err
	}
	col := map[string]string{itemCopied: "copied", itemSkipped: "skipped", itemFailed: "failed"}[outcome]
	if _, err := tx.ExecContext(ctx, `UPDATE transfer_jobs SET `+col+` = `+col+` + 1, bytes_copied = bytes_copied + $2, updated_at = NOW(),
		last_error = CASE WHEN $3 = '' THEN last_error ELSE $3 END WHERE id = $1`, t.ID, bytes, why); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	transferItems.WithLabelValues(outcome).Inc()
	if bytes > 0 {
		transferBytes.Add(float64(bytes))
	}
	return nil
}

// pause stops a job until POST …/resume.
func (j *TransferJob) pause(ctx context.Context, id, reason, code string) error {
	_, err := j.s.db.ExecContext(ctx, `UPDATE transfer_jobs SET state = 'paused', paused_reason = $2, paused_error = $3, updated_at = NOW()
		WHERE id = $1 AND state IN ('pending', 'running')`, id, reason, code)
	return err
}

// complete ends a job whose every item is settled, the key nulled, and
// sends the event.
func (j *TransferJob) complete(ctx context.Context, t *transferRow) error {
	res, err := j.s.db.ExecContext(ctx, transferEndSQL, t.ID, transferCompleted, "")
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil
	}
	t, err = j.load(ctx, t.ID)
	if err != nil {
		return err
	}
	emitEvent(ctx, j.s.db, j.s.log(), "transfer.completed", t.TenantID, map[string]any{
		"transfer_id": t.ID, "bucket": t.Bucket, "source_endpoint": t.Endpoint, "source_bucket": t.SrcBucket, "source_prefix": t.Prefix,
		"listed": t.Listed, "copied": t.Copied, "skipped": t.Skipped, "failed": t.Failed, "bytes": t.Bytes})
	return nil
}

// stillLive reports whether the job may still write (not cancelled,
// paused or ended under the copy).
func (j *TransferJob) stillLive(ctx context.Context, id string) bool {
	var st string
	if err := j.s.db.QueryRowContext(ctx, `SELECT state FROM transfer_jobs WHERE id = $1`, id).Scan(&st); err != nil {
		return false
	}
	return st == transferRunning
}

// alreadyThere reports whether the destination holds this source object:
// its head row has the source's ETag and size, or an earlier transfer of
// this tenant copied this source object (same key, ETag, size) into this
// key and the head row still has the ETag that copy gave it.
func (j *TransferJob) alreadyThere(ctx context.Context, t *transferRow, it transferItem) (bool, error) {
	var etag string
	var size int64
	err := j.s.db.QueryRowContext(ctx, `SELECT etag, size_bytes FROM object_head_cache WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3`,
		t.TenantID, t.Bucket, it.Key).Scan(&etag, &size)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if size != it.Size {
		return false, nil
	}
	if normETag(etag) == it.ETag {
		return true, nil
	}
	var n int
	err = j.s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM transfer_items i JOIN transfer_jobs jb ON jb.id = i.job_id
		WHERE jb.tenant_id = $1 AND jb.bucket = $2 AND jb.source_endpoint = $3 AND jb.source_bucket = $4
		  AND i.source_key = $5 AND i.source_etag = $6 AND i.size = $7 AND i.state = 'copied' AND i.dest_etag = $8`,
		t.TenantID, t.Bucket, t.Endpoint, t.SrcBucket, it.Key, it.ETag, it.Size, normETag(etag)).Scan(&n)
	return n > 0, err
}

// copyItem copies one item; stop = the job paused (the run moves on).
func (j *TransferJob) copyItem(ctx context.Context, t *transferRow, src *transferSource, jobLimit *rate.Limiter, it transferItem) (bool, error) {
	there, err := j.alreadyThere(ctx, t, it)
	if err != nil {
		return false, err
	}
	if there {
		return false, j.settle(ctx, t, it, itemSkipped, "", "", 0)
	}
	// A cancel or pause under the copy cuts it: the body stops, the write
	// commits nothing.
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		tick := time.NewTicker(2 * time.Second)
		defer tick.Stop()
		for {
			select {
			case <-cctx.Done():
				return
			case <-tick.C:
				if !j.stillLive(cctx, t.ID) {
					cancel()
					return
				}
			}
		}
	}()
	out := j.s.transferCopy(cctx, t, src, jobLimit, it)
	if ctx.Err() != nil {
		return true, nil // the run's end: the item stays pending
	}
	if cctx.Err() != nil {
		return true, nil // cancelled or paused: nothing written, nothing settled
	}
	switch {
	case out.err == nil:
		return false, j.settle(ctx, t, it, itemCopied, "", out.etag, it.Size)
	case out.quota:
		return true, j.pause(ctx, t.ID, "quota", out.code)
	case out.code == ErrNoSuchBucket:
		// The destination is gone: nothing more can land.
		_, err := j.s.db.ExecContext(ctx, transferEndSQL, t.ID, transferFailed, "the destination bucket is gone")
		return true, err
	case out.sourceDenied:
		return true, j.pause(ctx, t.ID, "source_denied", out.code)
	case out.permanent || it.Attempts+1 >= transferItemAttempts:
		return false, j.settle(ctx, t, it, itemFailed, out.err.Error(), "", 0)
	}
	_, err = j.s.db.ExecContext(ctx, `UPDATE transfer_items SET attempts = attempts + 1, last_error = $3, updated_at = NOW()
		WHERE job_id = $1 AND source_key = $2`, t.ID, it.Key, out.err.Error())
	return false, err
}

// copyOutcome is one copy's answer.
type copyOutcome struct {
	etag         string
	err          error
	quota        bool // the destination's quota refused it
	sourceDenied bool
	permanent    bool // retrying cannot help (a destination's 4xx, the source object gone)
	code         string
}

// transferCopy writes one source object into the destination through the
// S3 handlers: a PUT under transferMultipartMin, a multipart upload above.
func (s *Server) transferCopy(ctx context.Context, t *transferRow, src *transferSource, jobLimit *rate.Limiter, it transferItem) copyOutcome {
	ranges := &transferRanges{ctx: ctx, src: src, key: it.Key, etag: it.ETag, size: it.Size,
		limits: []*rate.Limiter{jobLimit, s.transfers().origin}}
	if it.Size < transferMultipartMin {
		attrs, err := ranges.open()
		if err != nil {
			return sourceOutcome(err)
		}
		hdr := attrs.header.Clone()
		if isMD5Hex(it.ETag) {
			sum, _ := hex.DecodeString(it.ETag)
			hdr.Set("Content-MD5", base64.StdEncoding.EncodeToString(sum))
		}
		rec := s.transferS3(ctx, t.TenantID, http.MethodPut, t.Bucket, it.Key, nil, hdr, ranges, it.Size)
		if ranges.err != nil {
			return sourceOutcome(ranges.err)
		}
		return destOutcome(rec)
	}
	return s.transferMultipart(ctx, t, ranges, it)
}

// transferMultipart is the multipart path: initiate (the object's
// attributes), a part per transferPartBytes (at least, so ≤ 10,000 parts),
// complete; any failure aborts the upload.
func (s *Server) transferMultipart(ctx context.Context, t *transferRow, ranges *transferRanges, it transferItem) copyOutcome {
	attrs, err := ranges.open()
	if err != nil {
		return sourceOutcome(err)
	}
	rec := s.transferS3(ctx, t.TenantID, http.MethodPost, t.Bucket, it.Key, url.Values{"uploads": {""}}, attrs.header, nil, 0)
	if rec.status != http.StatusOK {
		return destOutcome(rec)
	}
	var init struct {
		UploadID string `xml:"UploadId"`
	}
	if err := xml.Unmarshal(rec.body.Bytes(), &init); err != nil || init.UploadID == "" {
		return copyOutcome{err: fmt.Errorf("initiate answered no upload id"), permanent: true}
	}
	abort := func() {
		s.transferS3(context.WithoutCancel(ctx), t.TenantID, http.MethodDelete, t.Bucket, it.Key, url.Values{"uploadId": {init.UploadID}}, http.Header{}, nil, 0)
	}
	part := transferPartBytes
	if need := (it.Size + transferMaxParts - 1) / transferMaxParts; need > part {
		part = (need + (1 << 20) - 1) &^ ((1 << 20) - 1)
	}
	type done struct {
		XMLName    xml.Name `xml:"Part"`
		PartNumber int      `xml:"PartNumber"`
		ETag       string   `xml:"ETag"`
	}
	var parts []done
	for n, off := 1, int64(0); off < it.Size; n, off = n+1, off+part {
		size := min(part, it.Size-off)
		body := ranges.window(off, size)
		q := url.Values{"uploadId": {init.UploadID}, "partNumber": {strconv.Itoa(n)}}
		rec := s.transferS3(ctx, t.TenantID, http.MethodPut, t.Bucket, it.Key, q, http.Header{}, body, size)
		if ranges.err != nil {
			abort()
			return sourceOutcome(ranges.err)
		}
		if rec.status != http.StatusOK {
			abort()
			return destOutcome(rec)
		}
		parts = append(parts, done{PartNumber: n, ETag: rec.header.Get("ETag")})
	}
	var b bytes.Buffer
	b.WriteString("<CompleteMultipartUpload>")
	enc := xml.NewEncoder(&b)
	for _, p := range parts {
		if err := enc.Encode(p); err != nil {
			abort()
			return copyOutcome{err: err}
		}
	}
	b.WriteString("</CompleteMultipartUpload>")
	rec = s.transferS3(ctx, t.TenantID, http.MethodPost, t.Bucket, it.Key, url.Values{"uploadId": {init.UploadID}},
		http.Header{"Content-Type": {"application/xml"}}, bytes.NewReader(b.Bytes()), int64(b.Len()))
	if rec.status != http.StatusOK || bytes.Contains(rec.body.Bytes(), []byte("<Error>")) {
		abort()
		return destOutcome(rec)
	}
	var res struct {
		ETag string `xml:"ETag"`
	}
	_ = xml.Unmarshal(rec.body.Bytes(), &res)
	return copyOutcome{etag: normETag(res.ETag)}
}

// transferS3 is one S3 operation on the tenant's own bucket, through the
// handler a signed request of the tenant would reach — with the tenant on
// the context as handleS3Request puts it there.
func (s *Server) transferS3(ctx context.Context, tenantID, method, bucket, key string, q url.Values, hdr http.Header, body io.Reader, size int64) *relayRecorder {
	u := &url.URL{Path: "/" + bucket + "/" + key, RawQuery: q.Encode()}
	if q != nil {
		// "?uploads" carries no "=".
		u.RawQuery = strings.ReplaceAll(q.Encode(), "uploads=", "uploads")
	}
	if body == nil {
		body = http.NoBody
	}
	r, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	rec := newRelayRecorder()
	if err != nil {
		WriteS3Error(rec, ErrInternalError, u.Path, generateRequestID())
		return rec
	}
	r.Header = hdr.Clone()
	if r.Header == nil {
		r.Header = http.Header{}
	}
	r.ContentLength = size
	r.Header.Set("Content-Length", strconv.FormatInt(size, 10))
	r.Header.Set("User-Agent", "stored-transfer/1")
	tn := &tenant.Tenant{ID: tenantID, Namespace: fmt.Sprintf("tenant/%s/", tenantID), Plan: "starter", Status: "active"}
	r = r.WithContext(tenant.WithTenant(common.WithTenantID(ctx, tenantID), tn))
	if s.db != nil && isTenantSuspended(r.Context(), s.db, tenantID) {
		WriteS3Error(rec, ErrAccountSuspended, u.Path, generateRequestID())
		return rec
	}
	req, err := NewS3Parser(s.logger).ParseRequest(r)
	if err != nil {
		WriteS3Error(rec, ErrInvalidRequest, u.Path, generateRequestID())
		return rec
	}
	req.TenantID = tenantID
	switch req.Operation {
	case "PutObject":
		s.handlePutObject(rec, r, req)
	case "InitiateMultipartUpload":
		s.handleInitiateMultipartUpload(rec, r, req.Bucket, req.Object)
	case "UploadPart":
		s.handleUploadPart(rec, r, req.Bucket, req.Object)
	case "CompleteMultipartUpload":
		s.handleCompleteMultipartUpload(rec, r, req.Bucket, req.Object)
	case "AbortMultipartUpload":
		s.handleAbortMultipartUpload(rec, r, req.Bucket, req.Object)
	default:
		WriteS3Error(rec, ErrNotImplemented, u.Path, generateRequestID())
	}
	if rec.status == 0 {
		rec.status = http.StatusOK
	}
	return rec
}

// destOutcome reads a handler's answer.
func destOutcome(rec *relayRecorder) copyOutcome {
	if rec.status == http.StatusOK || rec.status == http.StatusNoContent {
		return copyOutcome{etag: normETag(rec.header.Get("ETag"))}
	}
	code := s3ErrorCodeOf(rec.body.Bytes())
	out := copyOutcome{code: code, err: fmt.Errorf("the destination answered %d %s", rec.status, code)}
	switch {
	case code == ErrQuotaExceeded || strings.Contains(code, "Quota"):
		out.quota = true
	case rec.status >= 400 && rec.status < 500 && rec.status != http.StatusRequestTimeout && code != "SlowDown":
		out.permanent = true
	}
	return out
}

// sourceOutcome reads a source failure.
func sourceOutcome(err error) copyOutcome {
	class, code := classifySourceError(err)
	out := copyOutcome{code: code, err: fmt.Errorf("the source: %s", code)}
	switch class {
	case sourceDenied:
		out.sourceDenied = true
	case sourceGone:
		out.permanent = true
		out.err = fmt.Errorf("the source object is gone or changed: %s", code)
	}
	return out
}

// s3ErrorCodeOf is an S3 error document's <Code> ("" when none).
func s3ErrorCodeOf(b []byte) string {
	var e struct {
		Code string `xml:"Code"`
	}
	if xml.Unmarshal(b, &e) != nil {
		return ""
	}
	return e.Code
}

// transferRanges reads one source object as ranged GETs of
// transferRangeBytes pinned to its ETag, paced by the token buckets; the
// first range is opened ahead to read the object's attributes. err is the
// source's failure (the destination's answer is then not the copy's).
type transferRanges struct {
	ctx    context.Context
	src    *transferSource
	key    string
	etag   string
	size   int64
	limits []*rate.Limiter

	off    int64 // the next byte to read
	end    int64 // the window's end (exclusive)
	cur    io.ReadCloser
	left   int64 // the current range's bytes not yet read
	first  io.ReadCloser
	firstN int64
	err    error
}

// open reads the first range's answer: the object's attributes.
func (r *transferRanges) open() (transferObjectAttrs, error) {
	n := min(transferRangeBytes, r.size)
	if n <= 0 {
		n = 1 // an empty object: one byte asked, none given
	}
	body, out, err := r.src.rangeGet(r.ctx, r.key, r.etag, 0, n)
	if err != nil {
		if r.size == 0 {
			// An empty object answers 416 to any range: its attributes are
			// unknown, its bytes are none.
			return transferObjectAttrs{header: http.Header{}}, nil
		}
		return transferObjectAttrs{}, err
	}
	r.first, r.firstN = body, n
	r.end = r.size
	return attrsOf(out), nil
}

// window makes the reader serve [off, off+n).
func (r *transferRanges) window(off, n int64) io.Reader {
	if r.cur != nil && r.off != off {
		_ = r.cur.Close()
		r.cur, r.left = nil, 0
	}
	r.off, r.end = off, off+n
	return r
}

func (r *transferRanges) Read(p []byte) (int, error) {
	if r.off >= r.end {
		return 0, io.EOF
	}
	if r.cur == nil {
		if r.first != nil && r.off == 0 {
			r.cur, r.left, r.first = r.first, r.firstN, nil
		} else {
			if r.first != nil {
				_ = r.first.Close()
				r.first = nil
			}
			n := min(transferRangeBytes, r.size-r.off)
			body, _, err := r.src.rangeGet(r.ctx, r.key, r.etag, r.off, n)
			if err != nil {
				r.err = err
				return 0, err
			}
			r.cur, r.left = body, n
		}
	}
	want := min(int64(len(p)), r.left, r.end-r.off, transferBurst)
	for _, l := range r.limits {
		if l != nil {
			if err := l.WaitN(r.ctx, int(want)); err != nil {
				return 0, err
			}
		}
	}
	n, err := r.cur.Read(p[:want])
	r.off += int64(n)
	r.left -= int64(n)
	if r.left == 0 || errors.Is(err, io.EOF) {
		_ = r.cur.Close()
		r.cur = nil
		if r.left > 0 && n == 0 {
			r.err = io.ErrUnexpectedEOF
			return 0, r.err
		}
		r.left = 0
		err = nil
	}
	if err != nil {
		r.err = err
	}
	return n, err
}

// transferJobsCollector is vaultaire_transfer_jobs{state}: the jobs by
// state, read from the table on scrape (every state a series).
type transferJobsCollector struct {
	db   *sql.DB
	desc *prometheus.Desc
}

func newTransferJobsCollector(db *sql.DB) *transferJobsCollector {
	return &transferJobsCollector{db: db, desc: prometheus.NewDesc("vaultaire_transfer_jobs",
		"Transfer jobs (WP-XFER-1) by state, from transfer_jobs.", []string{"state"}, nil)}
}

func (c *transferJobsCollector) Describe(ch chan<- *prometheus.Desc) { ch <- c.desc }

func (c *transferJobsCollector) Collect(ch chan<- prometheus.Metric) {
	counts := map[string]float64{transferPending: 0, transferRunning: 0, transferPaused: 0, transferCompleted: 0, transferFailed: 0, transferCancelled: 0}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	rows, err := c.db.QueryContext(ctx, `SELECT state, COUNT(*) FROM transfer_jobs GROUP BY state`)
	if err == nil {
		for rows.Next() {
			var st string
			var n float64
			if rows.Scan(&st, &n) == nil {
				counts[st] = n
			}
		}
		if rows.Err() != nil {
			counts = map[string]float64{}
		}
		_ = rows.Close()
	}
	for st, n := range counts {
		ch <- prometheus.MustNewConstMetric(c.desc, prometheus.GaugeValue, n, st)
	}
}
