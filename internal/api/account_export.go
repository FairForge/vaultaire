package api

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/FairForge/vaultaire/internal/audit"
	"github.com/FairForge/vaultaire/internal/auth"
	"github.com/FairForge/vaultaire/internal/common"
	"github.com/FairForge/vaultaire/internal/crypto"
	"github.com/FairForge/vaultaire/internal/drivers"
	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/FairForge/vaultaire/internal/tenant"
	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"
)

// The asynchronous GDPR export (WP-R10-3b, with WP-R12-1 / R12-17 / R10-27).
//
// Before this file three entry points built three exports, each in memory:
// the management API marshalled the whole JSON and returned it inline (no
// artifact, nothing re-downloadable — account_exports.file_path and
// expires_at had never been set since migration 038), the dashboard had its
// own collector that loaded every object_head_cache row of the tenant into a
// []map and MarshalIndent'ed it (one click by a large tenant = a memory spike
// on the hub), and the compliance route answered 401 to everyone.
//
// One service now. Request records a 'pending' row (one in flight per user,
// enforced by a partial unique index — never read-then-insert). The
// account_export job (every minute, under the scheduler's lock) claims the
// oldest pending row and renders it: a json.Encoder writes the export
// section by section (account_export_sections.go) into the body the
// generatedObjectWriter spools — the customer write path: placement, head
// row, version ledger, quota — as an object of the tenant's system bucket
// `_exports`. The object query pages by keyset in COLLATE "C" order (R4's
// listing), a few thousand rows a page; nothing holds more than one page.
// The row turns 'completed' only after the writer returned: a deploy
// mid-render leaves it 'pending' (claimed_at stale), never 'completed' with a
// truncated object — the head row's etag is the proof of a whole one. Three
// failed attempts make it 'failed'. The download is a presigned GET on the
// tenant's primary pair (one hour, VAULTAIRE_ENDPOINT as base), minted per
// request and never logged; after 7 days the retention job deletes the
// object through the customer delete path and the row says 'expired' (410).
//
// The export is a GDPR right: an account over quota still gets it. The
// writer is given an explicit allowance for this one object; the bytes are
// accounted (never hidden) and the completion audit row says over_quota.

const (
	exportJobName     = "account_export"
	exportTTL         = 7 * 24 * time.Hour
	exportLinkTTL     = time.Hour
	exportMaxAttempts = 3
	// exportStaleClaim: a claim older than this belongs to a process that
	// died mid-render; the row is picked up again.
	exportStaleClaim = 15 * time.Minute
	// exportMaxPerRun bounds one run of the job.
	exportMaxPerRun       = 50
	exportDefaultPageSize = 2000
	exportContentType     = "application/json"
)

var (
	// ErrExportInFlight: the user already has a pending export.
	ErrExportInFlight = errors.New("an export is already being prepared")
	// ErrExportNotFound: no such export for this user (never "someone
	// else's" — existence is not confirmed).
	ErrExportNotFound = errors.New("export not found")
	// ErrExportExpired: the object has been (or is due to be) removed.
	ErrExportExpired = errors.New("export expired")
	// ErrExportNotReady: the export has no object yet.
	ErrExportNotReady = errors.New("export not ready")
)

var accountExportsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "vaultaire_account_exports_total",
	Help: "GDPR exports by result: completed, retried (an attempt failed and the row stays pending), failed (no attempts left), expired (object removed after 7 days).",
}, []string{"result"})

// ExportStatus is one account_exports row as the owner may see it.
type ExportStatus struct {
	ID          string
	TenantID    string
	Status      string // pending | completed | failed | expired
	SizeBytes   int64
	ETag        string
	ObjectKey   string
	CreatedAt   time.Time
	CompletedAt time.Time
	ExpiresAt   time.Time
	Error       string
	Attempts    int
	// Expired: the status is 'expired', or 'completed' past expires_at (the
	// retention job has not run yet). No URL is minted either way.
	Expired bool
}

// AccountExportService is the one export service behind the management API
// and the dashboard.
type AccountExportService struct {
	db     *sql.DB
	eng    engine.Engine
	writer *generatedObjectWriter
	quota  QuotaManager
	gci    *crypto.GlobalContentIndex
	logger *zap.Logger
	// endpoint is the public S3 base URL the presigned link is built on.
	endpoint string

	// JobName is the job_runs name (tests use their own: the table is shared).
	JobName string
	// PageSize is the keyset page of the object, version, lock, event and
	// audit queries.
	PageSize int
	// MaxRunTime bounds one run of the job.
	MaxRunTime time.Duration
	// SSEDefault: new system buckets are sse_enabled (the server's SSE is
	// configured), as createBucketRegistry does for customer buckets.
	SSEDefault bool

	now func() time.Time
	// onlyUserID scopes the claim and the purge to one user's rows (tests:
	// the table is shared with other fixtures of this package).
	onlyUserID string
	// beforeWrite runs before the object write (tests inject failures).
	beforeWrite func() error
}

// NewAccountExportService builds the service; nil without a database or an
// engine (the handlers then answer 503).
func NewAccountExportService(db *sql.DB, eng engine.Engine, quota QuotaManager, gci *crypto.GlobalContentIndex, endpoint string, logger *zap.Logger) *AccountExportService {
	if db == nil || eng == nil {
		return nil
	}
	if logger == nil {
		logger = zap.NewNop()
	}
	return &AccountExportService{
		db: db, eng: eng, quota: quota, gci: gci, logger: logger, endpoint: strings.TrimSuffix(endpoint, "/"),
		writer:     newGeneratedObjectWriter(db, eng, quota, gci, logger),
		JobName:    exportJobName,
		PageSize:   exportDefaultPageSize,
		MaxRunTime: 10 * time.Minute,
		now:        time.Now,
	}
}

// Request records a pending export and returns its id. ErrExportInFlight
// when the user already has one pending.
func (s *AccountExportService) Request(ctx context.Context, userID, tenantID string) (string, error) {
	if s == nil {
		return "", errors.New("export service not configured")
	}
	if userID == "" || tenantID == "" {
		return "", errors.New("export: user and tenant are required")
	}
	var id string
	err := s.db.QueryRowContext(ctx,
		`INSERT INTO account_exports (user_id, tenant_id, status, format) VALUES ($1::uuid, $2, 'pending', 'json') RETURNING id`,
		userID, tenantID).Scan(&id)
	var pqErr *pq.Error
	if errors.As(err, &pqErr) && pqErr.Code == "23505" {
		return "", ErrExportInFlight
	}
	if err != nil {
		return "", fmt.Errorf("record export request: %w", err)
	}
	return id, nil
}

const exportStatusSelect = `
	SELECT id, tenant_id, status, COALESCE(file_size_bytes, 0), COALESCE(etag, ''), COALESCE(file_path, ''),
	       created_at, completed_at, expires_at, COALESCE(error_message, ''), attempts
	  FROM account_exports`

func (s *AccountExportService) scanStatus(row interface{ Scan(...any) error }) (*ExportStatus, error) {
	var st ExportStatus
	var completed, expires sql.NullTime
	var path string
	if err := row.Scan(&st.ID, &st.TenantID, &st.Status, &st.SizeBytes, &st.ETag, &path,
		&st.CreatedAt, &completed, &expires, &st.Error, &st.Attempts); err != nil {
		return nil, err
	}
	if completed.Valid {
		st.CompletedAt = completed.Time
	}
	if expires.Valid {
		st.ExpiresAt = expires.Time
	}
	if i := strings.IndexByte(path, '/'); i >= 0 {
		st.ObjectKey = path[i+1:]
	}
	st.Expired = st.Status == "expired" || (st.Status == "completed" && expires.Valid && !expires.Time.After(s.now()))
	return &st, nil
}

// Get returns one of the user's exports. ErrExportNotFound for any id that
// is not theirs.
func (s *AccountExportService) Get(ctx context.Context, exportID, userID string) (*ExportStatus, error) {
	if s == nil {
		return nil, errors.New("export service not configured")
	}
	if _, err := uuid.Parse(exportID); err != nil {
		return nil, ErrExportNotFound
	}
	if _, err := uuid.Parse(userID); err != nil {
		return nil, ErrExportNotFound
	}
	st, err := s.scanStatus(s.db.QueryRowContext(ctx, exportStatusSelect+` WHERE id = $1::uuid AND user_id = $2::uuid`, exportID, userID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrExportNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("read export: %w", err)
	}
	return st, nil
}

// Latest returns the user's most recent export (nil when none).
func (s *AccountExportService) Latest(ctx context.Context, userID string) (*ExportStatus, error) {
	if s == nil {
		return nil, errors.New("export service not configured")
	}
	if _, err := uuid.Parse(userID); err != nil {
		return nil, nil
	}
	st, err := s.scanStatus(s.db.QueryRowContext(ctx, exportStatusSelect+` WHERE user_id = $1::uuid ORDER BY created_at DESC LIMIT 1`, userID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read latest export: %w", err)
	}
	return st, nil
}

// DownloadURL mints a presigned GET of the export object on the tenant's
// primary pair, valid exportLinkTTL. The URL carries the key id and a
// signature, never the secret; it is for the signed-in owner's eyes and is
// not logged. A rotated primary pair kills every URL minted before it.
func (s *AccountExportService) DownloadURL(ctx context.Context, exportID, userID string) (string, time.Time, error) {
	st, err := s.Get(ctx, exportID, userID)
	if err != nil {
		return "", time.Time{}, err
	}
	switch {
	case st.Expired:
		return "", time.Time{}, ErrExportExpired
	case st.Status != "completed" || st.ObjectKey == "":
		return "", time.Time{}, ErrExportNotReady
	}
	accessKey, secretKey, err := auth.PrimaryPair(ctx, s.db, st.TenantID)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("read tenant credentials: %w", err)
	}
	if accessKey == "" || secretKey == "" {
		return "", time.Time{}, errors.New("the tenant has no primary credentials")
	}
	link, expires := generatePresignedS3URL(s.endpoint, accessKey, secretKey, tenant.ExportsBucket, st.ObjectKey, "GET", int(exportLinkTTL.Seconds()))
	return link, expires, nil
}

// --- the job ----------------------------------------------------------------

// spec: an interval job — every minute, under the scheduler's lock, a run
// claims and renders pending exports one at a time. A run FAILS only when
// the claim itself cannot be made (the table unreadable); an export whose
// render failed is a note (and a retry at the next run).
func (s *AccountExportService) spec() jobSpec {
	return jobSpec{
		Name: s.JobName, Every: time.Minute, BootDelay: 15 * time.Second, MaxRunTime: s.MaxRunTime,
		Run: func(ctx context.Context) (jobReport, error) {
			res, err := s.RunOnce(ctx)
			return jobReport{Rows: int64(res.Completed), Note: strings.Join(res.Errors, "; ")}, err
		},
	}
}

// ExportRunResult is one run's outcome.
type ExportRunResult struct {
	Completed int
	Retried   int
	Failed    int
	Errors    []string
}

// claimed is one row the run holds.
type claimed struct {
	id, userID, tenantID string
	attempts             int
}

// claimNext takes the oldest pending row that has attempts left and no live
// claim, bumping attempts and claimed_at in the same statement. FOR UPDATE
// SKIP LOCKED: two runs (two processes, a trigger beside the scheduler)
// never take the same row.
func (s *AccountExportService) claimNext(ctx context.Context, tried []string) (*claimed, error) {
	var c claimed
	err := s.db.QueryRowContext(ctx, `
		UPDATE account_exports
		   SET attempts = attempts + 1, claimed_at = NOW()
		 WHERE id = (SELECT id FROM account_exports
		              WHERE status = 'pending' AND attempts < $1
		                AND (claimed_at IS NULL OR claimed_at < NOW() - $2::interval)
		                AND ($3 = '' OR user_id::text = $3)
		                AND NOT (id::text = ANY($4::text[]))
		              ORDER BY created_at
		              LIMIT 1 FOR UPDATE SKIP LOCKED)
		RETURNING id, user_id::text, tenant_id, attempts`,
		exportMaxAttempts, exportStaleClaim.String(), s.onlyUserID, pq.Array(tried)).Scan(&c.id, &c.userID, &c.tenantID, &c.attempts)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("claim export: %w", err)
	}
	return &c, nil
}

// RunOnce renders every claimable export, one at a time, up to
// exportMaxPerRun; a row is attempted once per run (a failed one waits for
// the next run). Locking and job_runs are the scheduler's.
func (s *AccountExportService) RunOnce(ctx context.Context) (ExportRunResult, error) {
	var res ExportRunResult
	if s == nil {
		return res, errors.New("export: service not configured")
	}
	tried := []string{}
	for i := 0; i < exportMaxPerRun; i++ {
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		c, err := s.claimNext(ctx, tried)
		if err != nil {
			return res, err
		}
		if c == nil {
			return res, nil
		}
		tried = append(tried, c.id)
		log := s.logger.With(zap.String("export_id", c.id), zap.String("tenant_id", c.tenantID), zap.Int("attempt", c.attempts))
		if err := s.renderOne(ctx, c, log); err != nil {
			if c.attempts >= exportMaxAttempts {
				res.Failed++
				accountExportsTotal.WithLabelValues("failed").Inc()
				s.mark(ctx, c.id, `UPDATE account_exports SET status = 'failed', error_message = $2, claimed_at = NULL WHERE id = $1::uuid AND status = 'pending'`, err.Error())
				log.Error("account export: failed, no attempts left", zap.Error(err))
			} else {
				res.Retried++
				accountExportsTotal.WithLabelValues("retried").Inc()
				// The claim is released at once: the next run retries. (A
				// claim left in place is for a process that died mid-render.)
				s.mark(ctx, c.id, `UPDATE account_exports SET error_message = $2, claimed_at = NULL WHERE id = $1::uuid AND status = 'pending'`, err.Error())
				log.Warn("account export: attempt failed, will retry", zap.Error(err))
			}
			res.Errors = append(res.Errors, fmt.Sprintf("export %s (attempt %d): %v", c.id, c.attempts, err))
			continue
		}
		res.Completed++
		accountExportsTotal.WithLabelValues("completed").Inc()
	}
	return res, nil
}

// mark writes a row update on a short context of its own: the run's context
// may be the reason the render failed.
func (s *AccountExportService) mark(ctx context.Context, id, stmt string, args ...any) {
	mctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if _, err := s.db.ExecContext(mctx, stmt, append([]any{id}, args...)...); err != nil {
		s.logger.Error("account export: could not update the row", zap.String("export_id", id), zap.Error(err))
	}
}

// exportObjectKey is the export's key in the system bucket.
func exportObjectKey(id string) string { return "account-export-" + id + ".json" }

// renderOne writes the export object and completes the row.
func (s *AccountExportService) renderOne(ctx context.Context, c *claimed, log *zap.Logger) error {
	if err := s.ensureSystemBucket(ctx, c.tenantID); err != nil {
		return err
	}
	if s.beforeWrite != nil {
		if err := s.beforeWrite(); err != nil {
			return err
		}
	}
	key := exportObjectKey(c.id)
	tctx := common.WithTenantID(ctx, c.tenantID)
	delivered, err := s.writer.writeOpts(tctx, c.tenantID, tenant.ExportsBucket, key, exportContentType, func(w io.Writer) error {
		return renderExport(ctx, s.db, w, exportSubject{UserID: c.userID, TenantID: c.tenantID, ExportID: c.id, Now: s.now()}, s.PageSize)
	}, generatedWriteOptions{AllowOverQuota: true})
	if err != nil {
		return err
	}
	// The row is 'completed' only now, with the writer's size and etag; a
	// render cut short never gets here.
	res, err := s.db.ExecContext(context.WithoutCancel(ctx), `
		UPDATE account_exports
		   SET status = 'completed', file_path = $2, file_size_bytes = $3, etag = $4,
		       completed_at = NOW(), expires_at = NOW() + $5::interval, error_message = NULL, claimed_at = NULL
		 WHERE id = $1::uuid AND status = 'pending'`,
		c.id, tenant.ExportsBucket+"/"+key, delivered.Size, delivered.ETag, exportTTL.String())
	if err != nil {
		return fmt.Errorf("complete export row: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// The row went away under us (an erasure between claim and write):
		// the object must not outlive it.
		log.Warn("account export: row gone before completion; removing the object")
		s.deleteExportObject(context.WithoutCancel(ctx), c.tenantID, key)
		return errors.New("export row disappeared during the render")
	}
	audit.Record(context.WithoutCancel(ctx), s.db, audit.Entry{UserID: c.userID, TenantID: c.tenantID, EventType: "account",
		Action: "account.export_completed", Resource: "export:" + c.id,
		Metadata: map[string]any{"bytes": delivered.Size, "etag": delivered.ETag, "attempts": c.attempts,
			"over_quota": delivered.OverQuota, "bucket": tenant.ExportsBucket, "key": key, "expires_in": exportTTL.String()}})
	log.Info("account export completed", zap.Int64("bytes", delivered.Size), zap.Bool("over_quota", delivered.OverQuota))
	return nil
}

// ensureSystemBucket creates the tenant's `_exports` registry row (the
// generatedObjectWriter refuses a bucket with no row — WP-R12-10): private,
// never versioned, never lock-enabled, in the default region.
func (s *AccountExportService) ensureSystemBucket(ctx context.Context, tenantID string) error {
	region := drivers.IDriveDefaultRegion(os.Getenv)
	residency := "us"
	if drivers.IsEURegion(region) {
		residency = "eu"
	}
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO buckets (tenant_id, name, visibility, sse_enabled, region, data_residency, versioning_status, object_lock_enabled, metadata)
		VALUES ($1, $2, 'private', $3, $4, $5, 'disabled', FALSE, '{"system": true, "purpose": "account exports"}'::jsonb)
		ON CONFLICT (tenant_id, name) DO NOTHING`,
		tenantID, tenant.ExportsBucket, s.SSEDefault, region, residency); err != nil {
		return fmt.Errorf("create system bucket: %w", err)
	}
	return nil
}

// deleteExportObject removes one export object the way DeleteObject does:
// the blob on its recorded backend (a miss is fine), then the head row, then
// the quota. The caller decides what the row says.
func (s *AccountExportService) deleteExportObject(ctx context.Context, tenantID, key string) {
	t := &tenant.Tenant{ID: tenantID}
	container := t.NamespaceContainer(tenant.ExportsBucket)
	tctx := common.WithTenantID(ctx, tenantID)
	var backend string
	err := s.db.QueryRowContext(tctx, `SELECT COALESCE(backend_name, '') FROM object_head_cache WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3`,
		tenantID, tenant.ExportsBucket, key).Scan(&backend)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		s.logger.Error("account export: read head row", zap.String("tenant_id", tenantID), zap.Error(err))
	}
	if backend != "" {
		if ce, ok := s.eng.(*engine.CoreEngine); ok {
			ce.HintBackend(container, key, backend)
		}
	}
	if err := s.eng.Delete(tctx, container, key); err != nil && !isObjectMissingErr(err) {
		s.logger.Error("account export: delete object", zap.String("tenant_id", tenantID), zap.String("key", key), zap.Error(err))
	}
	row, found, err := deleteHeadRowReleasing(tctx, s.db, manifestReleaser(s.gci), tenantID, tenant.ExportsBucket, key)
	if err != nil {
		s.logger.Error("account export: delete head row", zap.String("tenant_id", tenantID), zap.Error(err))
		return
	}
	if found && s.quota != nil && row.Size > 0 {
		if err := releaseQuotaOn(tctx, s.quota, tenantID, row.Floor, row.Size); err != nil {
			s.logger.Error("account export: quota release", zap.String("tenant_id", tenantID), zap.Error(err))
		}
	}
}

// --- expiry (the retention job's step) ------------------------------------

// PurgeExpired deletes the object of every completed export past expires_at
// and marks the row 'expired'. The row stays (the audit trail of the
// request). A delete that fails leaves the row 'completed' for the next run.
func (s *AccountExportService) PurgeExpired(ctx context.Context) (int64, error) {
	if s == nil {
		return 0, nil
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, tenant_id, COALESCE(file_path, '')
		  FROM account_exports
		 WHERE status = 'completed' AND expires_at IS NOT NULL AND expires_at < NOW()
		   AND ($1 = '' OR user_id::text = $1)
		 ORDER BY expires_at
		 LIMIT 1000`, s.onlyUserID)
	if err != nil {
		return 0, fmt.Errorf("list expired exports: %w", err)
	}
	type due struct{ id, tenantID, path string }
	var dues []due
	for rows.Next() {
		var d due
		if err := rows.Scan(&d.id, &d.tenantID, &d.path); err != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("scan expired export: %w", err)
		}
		dues = append(dues, d)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	var n int64
	for _, d := range dues {
		if ctx.Err() != nil {
			return n, ctx.Err()
		}
		if i := strings.IndexByte(d.path, '/'); i >= 0 {
			s.deleteExportObject(ctx, d.tenantID, d.path[i+1:])
		}
		res, err := s.db.ExecContext(ctx, `UPDATE account_exports SET status = 'expired' WHERE id = $1::uuid AND status = 'completed'`, d.id)
		if err != nil {
			return n, fmt.Errorf("mark export %s expired: %w", d.id, err)
		}
		if k, _ := res.RowsAffected(); k > 0 {
			n++
			accountExportsTotal.WithLabelValues("expired").Inc()
		}
	}
	return n, nil
}
