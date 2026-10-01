package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/FairForge/vaultaire/internal/account"
	"github.com/FairForge/vaultaire/internal/audit"
	"github.com/FairForge/vaultaire/internal/common"
	"github.com/FairForge/vaultaire/internal/crypto"
	dashauth "github.com/FairForge/vaultaire/internal/dashboard/auth"
	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/FairForge/vaultaire/internal/tenant"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"
)

// Account-deletion runner (WP-R10-3, decisions D-15 / D-16; R10-08, R9-23,
// R11-13, R12-22, R13's design row).
//
// A deletion request starts a 30-day grace period during which nothing
// changes (D-16: login, S3 and the APIs keep working so the customer can
// export, migrate and cancel). On the date this job erases the account, in
// the R13 shape (retention.go): daily at RunAt UTC with a catch-up at boot
// via job_runs, ONE pg_try_advisory_lock per run so the ticker and the
// admin trigger never overlap, batched work, metrics, an audit row.
//
// Per due tenant, in this order, every step idempotent and resumable:
//
//	a. Stripe — cancel the subscription immediately and stamp
//	   tenants.deletion_stripe_cancelled_at. A subscription id on the row is
//	   cancelled on every run, stamped or not (the call is idempotent; a
//	   stale stamp must never let an erase skip it). A Stripe error (or no Stripe
//	   client while a subscription id is set) defers THIS tenant to the next
//	   run; nothing of theirs is touched before billing is settled.
//	b. Objects — every object_head_cache row is deleted on its RECORDED
//	   backend (HintBackend + engine.Delete, the R6-05 rule); chunked rows
//	   release their manifest under the key lock in the same transaction as
//	   the head row (deleteHeadRowReleasing, R8) and never touch _global
//	   blobs (dedup GC sweeps them); active multipart uploads are aborted
//	   and their staging dirs removed; quota is released per object so a
//	   cancel mid-walk leaves an honest ledger. Object Lock does not protect
//	   against the owner's own erasure — the count is logged. A backend
//	   failure leaves the row; the tenant is deferred and the next run
//	   resumes where it stopped.
//	c. Rows — account.EraseRows: one transaction over every tenant/user
//	   table (the contract is tested against information_schema), users and
//	   tenants last, ledgers kept and scrubbed.
//	d. Sessions — dashboard sessions and STS tokens go with the rows; the
//	   in-process credential cache is evicted (auth.Evict) so the erased
//	   user cannot sign in from memory until the next restart.
//	e. Report — one account.erased audit row with the counts per stage.
//
// A cancel wins any time before stage c: the user row is re-read under FOR
// UPDATE before every batch and inside the erase transaction. Objects a
// batch already deleted are gone — the grace period is the time to change
// one's mind, the walk is not.

const (
	accountDeletionJob            = "account_deletion"
	accountDeletionLockKey  int64 = 0x6163636f // "acco"
	outcomeErased                 = "erased"
	outcomeDeferred               = "deferred"
	outcomeCancelled              = "cancelled"
	accountDeletionDueLimit       = 200
)

var (
	accountDeletionTenants = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "vaultaire_account_deletion_tenants_total",
		Help: "Tenants handled by the account-deletion runner, by result (erased, deferred, cancelled).",
	}, []string{"result"})
	accountDeletionObjects = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "vaultaire_account_deletion_objects_total",
		Help: "Objects handled by the account-deletion runner, by result (deleted, chunked_released, failed).",
	}, []string{"result"})
	accountDeletionLastRun = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "vaultaire_account_deletion_last_run_timestamp_seconds",
		Help: "Unix time of the last account-deletion run that deferred no tenant.",
	})
	accountDeletionRuns = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "vaultaire_account_deletion_runs_total",
		Help: "Account-deletion runs by outcome.",
	}, []string{"outcome"})
)

// subscriptionCanceller is the slice of billing.StripeService the runner
// needs; an interface so the test can count calls.
type subscriptionCanceller interface {
	CancelSubscription(ctx context.Context, tenantID string) error
}

// accountEvictor is auth.AuthService.Evict.
type accountEvictor interface {
	Evict(userID, tenantID string)
}

// AccountDeletionRunner erases accounts whose grace period has ended.
type AccountDeletionRunner struct {
	db      *sql.DB
	logger  *zap.Logger
	eng     engine.Engine
	gci     *crypto.GlobalContentIndex
	quota   QuotaManager
	account *account.Service

	// Stripe is nil when STRIPE_SECRET_KEY is not set: a tenant with a
	// subscription id is then deferred (never erased unbilled).
	Stripe subscriptionCanceller
	// Auth evicts the erased account from the in-process cache (nil-safe).
	Auth accountEvictor
	// Sessions revokes the user's dashboard sessions (nil-safe; the DB
	// store's rows are deleted by EraseRows anyway).
	Sessions dashauth.SessionStore

	BatchSize              int
	RunAtHour, RunAtMinute int
	MaxRunTime             time.Duration
	TenantDeadline         time.Duration
	BootDelay              time.Duration

	now         func() time.Time
	beforeBatch func() // test hook: runs before each object batch
	// onlyDue is a test hook: when set, RunOnce skips every due account it
	// rejects. Packages share one test database and seed their own past-due
	// accounts; a fixture's run must not erase them. Never set in production.
	onlyDue func(account.Due) bool
}

// TenantErasure is one tenant's outcome in a run.
type TenantErasure struct {
	TenantID         string           `json:"tenant_id"`
	UserID           string           `json:"user_id"`
	Outcome          string           `json:"outcome"`
	StripeCancelled  bool             `json:"stripe_cancelled"`
	ObjectsDeleted   int              `json:"objects_deleted"`
	ChunkedReleased  int              `json:"chunked_released"`
	LockedErased     int              `json:"locked_erased"`
	ObjectFailures   int              `json:"object_failures"`
	MultipartAborted int              `json:"multipart_aborted"`
	Rows             map[string]int64 `json:"rows,omitempty"`
	Error            string           `json:"error,omitempty"`
}

// AccountDeletionResult is one run's outcome.
type AccountDeletionResult struct {
	Tenants   []TenantErasure `json:"tenants"`
	Erased    int             `json:"erased"`
	Deferred  int             `json:"deferred"`
	Cancelled int             `json:"cancelled"`
	Duration  string          `json:"duration"`
	Errors    []string        `json:"errors,omitempty"`
}

// NewAccountDeletionRunner builds the runner; nil without a DB or engine.
func NewAccountDeletionRunner(db *sql.DB, logger *zap.Logger, eng engine.Engine, gci *crypto.GlobalContentIndex,
	quota QuotaManager, acct *account.Service) *AccountDeletionRunner {
	if db == nil || eng == nil || acct == nil {
		return nil
	}
	if logger == nil {
		logger = zap.NewNop()
	}
	return &AccountDeletionRunner{
		db: db, logger: logger, eng: eng, gci: gci, quota: quota, account: acct,
		BatchSize:      1000,
		RunAtHour:      4,
		RunAtMinute:    30,
		MaxRunTime:     6 * time.Hour,
		TenantDeadline: 2 * time.Hour,
		BootDelay:      2 * time.Minute,
		now:            time.Now,
	}
}

// Start runs the scheduler until ctx is done (retention.go's shape).
func (r *AccountDeletionRunner) Start(ctx context.Context) {
	if r == nil {
		return
	}
	go func() {
		select {
		case <-ctx.Done():
			return
		case <-time.After(r.BootDelay):
		}
		r.runIfDue(ctx)
		ticker := time.NewTicker(time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				r.runIfDue(ctx)
			}
		}
	}()
}

func (r *AccountDeletionRunner) lastScheduled(now time.Time) time.Time {
	now = now.UTC()
	at := time.Date(now.Year(), now.Month(), now.Day(), r.RunAtHour, r.RunAtMinute, 0, 0, time.UTC)
	if at.After(now) {
		at = at.Add(-24 * time.Hour)
	}
	return at
}

func (r *AccountDeletionRunner) due(ctx context.Context) (bool, error) {
	last, err := lastJobSuccess(ctx, r.db, accountDeletionJob)
	if err != nil {
		return false, err
	}
	return last.IsZero() || last.Before(r.lastScheduled(r.now())), nil
}

func (r *AccountDeletionRunner) runIfDue(ctx context.Context) {
	due, err := r.due(ctx)
	if err != nil {
		r.logger.Warn("account deletion: could not read job_runs", zap.Error(err))
		return
	}
	if !due {
		return
	}
	res, err := r.RunOnce(ctx)
	switch {
	case errors.Is(err, errJobAlreadyRunning):
		r.logger.Warn("account deletion: run skipped, another instance holds the lock")
	case err != nil:
		r.logger.Error("account deletion run finished with deferred tenants", zap.Error(err),
			zap.Int("erased", res.Erased), zap.Int("deferred", res.Deferred), zap.Int("cancelled", res.Cancelled))
	default:
		r.logger.Info("account deletion run completed", zap.Int("erased", res.Erased),
			zap.Int("cancelled", res.Cancelled), zap.String("duration", res.Duration))
	}
}

// RunOnce erases every due account under the advisory lock. A deferred
// tenant (Stripe or a backend refused) makes the run's outcome "error" so
// job_runs.last_success_at stays put and the hourly check retries; a
// cancelled tenant does not.
func (r *AccountDeletionRunner) RunOnce(ctx context.Context) (AccountDeletionResult, error) {
	res := AccountDeletionResult{}
	if r == nil {
		return res, errors.New("account deletion: runner not configured")
	}
	ctx, cancel := context.WithTimeout(ctx, r.MaxRunTime)
	defer cancel()

	conn, err := r.db.Conn(ctx)
	if err != nil {
		return res, fmt.Errorf("account deletion: acquire conn: %w", err)
	}
	defer func() { _ = conn.Close() }()
	var locked bool
	if err := conn.QueryRowContext(ctx, `SELECT pg_try_advisory_lock($1)`, accountDeletionLockKey).Scan(&locked); err != nil {
		return res, fmt.Errorf("account deletion: advisory lock: %w", err)
	}
	if !locked {
		return res, errJobAlreadyRunning
	}
	defer func() {
		_, _ = conn.ExecContext(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, accountDeletionLockKey)
	}()

	start := r.now()
	recordJobStart(ctx, r.db, accountDeletionJob, start)

	dues, err := r.account.ListDue(ctx, start, accountDeletionDueLimit)
	if err != nil {
		accountDeletionRuns.WithLabelValues("error").Inc()
		recordJobFinish(context.WithoutCancel(ctx), r.db, accountDeletionJob, r.now(), "error", err, 0)
		return res, fmt.Errorf("account deletion: %w", err)
	}
	for _, d := range dues {
		if r.onlyDue != nil && !r.onlyDue(d) {
			continue
		}
		if ctx.Err() != nil {
			res.Errors = append(res.Errors, "run deadline reached before tenant "+d.TenantID)
			break
		}
		te := r.eraseTenant(ctx, d)
		res.Tenants = append(res.Tenants, te)
		accountDeletionTenants.WithLabelValues(te.Outcome).Inc()
		switch te.Outcome {
		case outcomeErased:
			res.Erased++
		case outcomeCancelled:
			res.Cancelled++
		default:
			res.Deferred++
			res.Errors = append(res.Errors, fmt.Sprintf("tenant %s deferred: %s", d.TenantID, te.Error))
		}
	}
	res.Duration = r.now().Sub(start).String()

	outcome := "ok"
	var runErr error
	if len(res.Errors) > 0 {
		outcome = "error"
		runErr = errors.New(strings.Join(res.Errors, "; "))
	} else {
		accountDeletionLastRun.Set(float64(r.now().Unix()))
	}
	accountDeletionRuns.WithLabelValues(outcome).Inc()
	recordJobFinish(context.WithoutCancel(ctx), r.db, accountDeletionJob, r.now(), outcome, runErr, int64(res.Erased))
	return res, runErr
}

// eraseTenant runs stages a–e for one account. It never returns an error:
// the outcome says what happened and the next run resumes.
func (r *AccountDeletionRunner) eraseTenant(ctx context.Context, d account.Due) TenantErasure {
	te := TenantErasure{TenantID: d.TenantID, UserID: d.UserID}
	log := r.logger.With(zap.String("tenant_id", d.TenantID), zap.String("user_id", d.UserID),
		zap.Time("scheduled_at", d.ScheduledAt))
	ctx, cancel := context.WithTimeout(ctx, r.TenantDeadline)
	defer cancel()
	defer func() {
		// The user's own audit rows are deleted with the account, so the
		// erasure record is keyed by the tenant only.
		switch te.Outcome {
		case outcomeErased:
			audit.Record(ctx, r.db, audit.Entry{TenantID: d.TenantID, EventType: "account", Action: "account.erased",
				Severity: "warning", Metadata: r.auditMeta(ctx, te, d)})
		case outcomeCancelled:
			audit.Record(ctx, r.db, audit.Entry{UserID: d.UserID, TenantID: d.TenantID, EventType: "account", Action: "account.erasure_cancelled",
				Severity: "warning", Metadata: r.auditMeta(ctx, te, d)})
		}
	}()

	// The select was not under a lock: confirm under FOR UPDATE before
	// touching anything (a cancel between the select and here wins).
	still, err := r.account.StillDue(ctx, d.UserID, r.now())
	if err != nil {
		return r.defer_(te, log, "re-read user", err)
	}
	if !still {
		te.Outcome = outcomeCancelled
		log.Info("account deletion: cancelled before the walk")
		return te
	}

	// a. Stripe.
	if d.TenantID != "" {
		if err := r.cancelSubscription(ctx, d.TenantID, &te); err != nil {
			return r.defer_(te, log, "stripe", err)
		}
	}

	// b. Objects, multipart.
	if d.TenantID != "" {
		stop, err := r.walkObjects(ctx, d, &te, log)
		if err != nil {
			return r.defer_(te, log, "object walk", err)
		}
		if stop {
			te.Outcome = outcomeCancelled
			log.Warn("account deletion: cancelled mid-walk; objects already deleted are gone",
				zap.Int("objects_deleted", te.ObjectsDeleted), zap.Int("chunked_released", te.ChunkedReleased))
			return te
		}
		if err := r.abortMultipart(ctx, d.TenantID, &te); err != nil {
			return r.defer_(te, log, "multipart", err)
		}
		var remaining int
		if err := r.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM object_head_cache WHERE tenant_id = $1`, d.TenantID).Scan(&remaining); err != nil {
			return r.defer_(te, log, "count remaining objects", err)
		}
		if te.ObjectFailures > 0 || remaining > 0 {
			return r.defer_(te, log, "objects remain", fmt.Errorf("%d head rows left, %d backend failures — resuming next run", remaining, te.ObjectFailures))
		}
	}

	// c. Rows (one transaction; refuses when a cancel landed meanwhile).
	rows, err := r.account.EraseRows(ctx, d.UserID, d.TenantID, d.Email, r.now())
	if errors.Is(err, account.ErrNotPending) {
		te.Outcome = outcomeCancelled
		log.Warn("account deletion: cancelled before the row erase; objects already deleted are gone")
		return te
	}
	if err != nil {
		return r.defer_(te, log, "erase rows", err)
	}
	te.Rows = rows.Rows

	// d. Sessions and the in-process cache.
	if r.Sessions != nil {
		if err := r.Sessions.DeleteByUserID(context.WithoutCancel(ctx), d.UserID); err != nil {
			log.Error("account deletion: revoke sessions", zap.Error(err))
		}
	}
	if r.Auth != nil {
		r.Auth.Evict(d.UserID, d.TenantID)
	}

	// e. Report.
	te.Outcome = outcomeErased
	log.Info("account erased",
		zap.Bool("stripe_cancelled", te.StripeCancelled),
		zap.Int("objects_deleted", te.ObjectsDeleted), zap.Int("chunked_released", te.ChunkedReleased),
		zap.Int("locked_erased", te.LockedErased), zap.Int("multipart_aborted", te.MultipartAborted),
		zap.Int64("rows", rows.Total))
	return te
}

// auditMeta is the erasure record's body. The actor is the runner ("system")
// on a scheduled run; an admin trigger carries the admin in the context, so
// audit.Record stamps performed_by and the metadata says the run was manual.
func (r *AccountDeletionRunner) auditMeta(ctx context.Context, te TenantErasure, d account.Due) map[string]any {
	actor := "system"
	if audit.ActorFromContext(ctx) != "" {
		actor = "admin_trigger"
	}
	return map[string]any{
		"actor": actor, "user_id": d.UserID, "scheduled_at": d.ScheduledAt,
		"stripe_cancelled": te.StripeCancelled, "objects_deleted": te.ObjectsDeleted,
		"chunked_released": te.ChunkedReleased, "locked_erased": te.LockedErased,
		"multipart_aborted": te.MultipartAborted, "object_failures": te.ObjectFailures,
		"rows": te.Rows,
	}
}

func (r *AccountDeletionRunner) defer_(te TenantErasure, log *zap.Logger, stage string, err error) TenantErasure {
	te.Outcome = outcomeDeferred
	te.Error = stage + ": " + err.Error()
	log.Error("account deletion: tenant deferred to the next run", zap.String("stage", stage), zap.Error(err),
		zap.Int("objects_deleted", te.ObjectsDeleted), zap.Int("object_failures", te.ObjectFailures))
	return te
}

// cancelSubscription is stage a. Idempotent through CancelSubscription: a
// run that repeats it (a crash before the stamp, a deferred tenant whose
// webhook has not cleared the id yet) finds the subscription already
// cancelled at Stripe. The stamp records that this runner cancelled it.
func (r *AccountDeletionRunner) cancelSubscription(ctx context.Context, tenantID string, te *TenantErasure) error {
	var subID sql.NullString
	var stamped sql.NullTime
	err := r.db.QueryRowContext(ctx,
		`SELECT stripe_subscription_id, deletion_stripe_cancelled_at FROM tenants WHERE id = $1`, tenantID).Scan(&subID, &stamped)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read subscription: %w", err)
	}
	if !subID.Valid || subID.String == "" {
		// Nothing to bill: never subscribed, or the webhook cleared the id
		// after an earlier run's cancel (the stamp says which).
		te.StripeCancelled = stamped.Valid
		return nil
	}
	// A subscription id is on the row. The stamp is not proof that THIS one
	// is cancelled: a run that stamped and was then deferred or cancelled
	// leaves the stamp behind while the account lives on (D-16), and the
	// customer may have bought again since. CancelSubscription answers an
	// already-cancelled subscription with success, so ask every time.
	if r.Stripe == nil {
		return fmt.Errorf("subscription %s is set but STRIPE_SECRET_KEY is not configured — the account is not erased until billing can be cancelled", subID.String)
	}
	if err := r.Stripe.CancelSubscription(ctx, tenantID); err != nil {
		return fmt.Errorf("cancel subscription %s: %w", subID.String, err)
	}
	if _, err := r.db.ExecContext(ctx,
		`UPDATE tenants SET deletion_stripe_cancelled_at = NOW() WHERE id = $1`, tenantID); err != nil {
		return fmt.Errorf("stamp subscription cancel: %w", err)
	}
	te.StripeCancelled = true
	return nil
}

// walkObjects is stage b. Returns stop=true when a cancel landed between
// batches. Backend failures are counted, never fatal: the row stays.
func (r *AccountDeletionRunner) walkObjects(ctx context.Context, d account.Due, te *TenantErasure, log *zap.Logger) (bool, error) {
	if err := r.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM object_locks
		 WHERE tenant_id = $1 AND (legal_hold OR retain_until_date > NOW())`, d.TenantID).Scan(&te.LockedErased); err != nil {
		return false, fmt.Errorf("count locked objects: %w", err)
	}
	if te.LockedErased > 0 {
		log.Warn("account deletion: Object Lock does not survive the owner's erasure", zap.Int("locked_objects", te.LockedErased))
	}
	t := &tenant.Tenant{ID: d.TenantID}
	tctx := common.WithTenantID(ctx, d.TenantID)

	type headRow struct {
		bucket, key, backend string
		chunked              bool
	}
	var lastBucket, lastKey string
	for {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		if r.beforeBatch != nil {
			r.beforeBatch()
		}
		still, err := r.account.StillDue(ctx, d.UserID, r.now())
		if err != nil {
			return false, err
		}
		if !still {
			return true, nil
		}
		rows, err := r.db.QueryContext(ctx, `
			SELECT bucket, object_key, COALESCE(backend_name, ''), is_chunked
			  FROM object_head_cache
			 WHERE tenant_id = $1 AND (bucket, object_key) > ($2, $3)
			 ORDER BY bucket, object_key
			 LIMIT $4`, d.TenantID, lastBucket, lastKey, r.BatchSize)
		if err != nil {
			return false, fmt.Errorf("list objects: %w", err)
		}
		var batch []headRow
		for rows.Next() {
			var h headRow
			if err := rows.Scan(&h.bucket, &h.key, &h.backend, &h.chunked); err != nil {
				_ = rows.Close()
				return false, fmt.Errorf("scan object: %w", err)
			}
			batch = append(batch, h)
		}
		_ = rows.Close()
		if err := rows.Err(); err != nil {
			return false, fmt.Errorf("list objects: %w", err)
		}
		if len(batch) == 0 {
			return false, nil
		}
		for _, h := range batch {
			lastBucket, lastKey = h.bucket, h.key
			if err := r.deleteObject(tctx, t, h.bucket, h.key, h.backend, h.chunked, te); err != nil {
				te.ObjectFailures++
				accountDeletionObjects.WithLabelValues("failed").Inc()
				log.Error("account deletion: object left in place", zap.String("bucket", h.bucket),
					zap.String("key", h.key), zap.String("backend", h.backend), zap.Error(err))
			}
		}
		if len(batch) < r.BatchSize {
			return false, nil
		}
	}
}

// deleteObject removes one object the way S3 DELETE does (s3_engine_adapter):
// whole objects go from the recorded backend first, then the head row;
// chunked objects release their manifest with the head row and leave the
// _global chunks to dedup GC.
func (r *AccountDeletionRunner) deleteObject(ctx context.Context, t *tenant.Tenant, bucket, key, backend string, chunked bool, te *TenantErasure) error {
	container := t.NamespaceContainer(bucket)
	if !chunked {
		// A demoted object whose hot copy is not yet reclaimed has bytes on
		// TWO backends; the recorded one is the cold copy. Delete the hot
		// copy first (a failure keeps the row so the next run retries).
		var hot string
		err := r.db.QueryRowContext(ctx, `
			SELECT hot_backend FROM smart_demotions
			 WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3 AND hot_deleted_at IS NULL`,
			t.ID, bucket, key).Scan(&hot)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("read demotion ledger: %w", err)
		}
		if hot != "" && hot != backend {
			if err := r.deleteOnBackend(ctx, container, key, hot); err != nil {
				return fmt.Errorf("hot copy on %s: %w", hot, err)
			}
		}
		if err := r.deleteOnBackend(ctx, container, key, backend); err != nil {
			return err
		}
	}
	row, found, err := deleteHeadRowReleasing(ctx, r.db, manifestReleaser(r.gci), t.ID, bucket, key)
	if err != nil {
		return fmt.Errorf("head row: %w", err)
	}
	if chunked {
		te.ChunkedReleased++
		accountDeletionObjects.WithLabelValues("chunked_released").Inc()
	} else {
		te.ObjectsDeleted++
		accountDeletionObjects.WithLabelValues("deleted").Inc()
	}
	if found && r.quota != nil && row.Size > 0 {
		qctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err := releaseQuotaOn(qctx, r.quota, t.ID, row.Floor, row.Size); err != nil {
			r.logger.Error("account deletion: quota release", zap.String("tenant_id", t.ID), zap.Error(err))
		}
	}
	return nil
}

// deleteOnBackend deletes one blob where the head row says it is (the R6-05
// rule: hint first, or a post-restart delete goes to the primary alone). A
// miss is fine — the row drifted; anything else is the caller's failure.
func (r *AccountDeletionRunner) deleteOnBackend(ctx context.Context, container, key, backend string) error {
	if backend != "" {
		if ce, ok := r.eng.(*engine.CoreEngine); ok {
			ce.HintBackend(container, key, backend)
		}
	}
	if err := r.eng.Delete(ctx, container, key); err != nil && !isObjectMissingErr(err) {
		return fmt.Errorf("backend delete: %w", err)
	}
	return nil
}

// abortMultipart aborts the tenant's active uploads and removes every
// staging directory of theirs (the reaper would purge the rows in 7 days;
// EraseRows removes them now).
func (r *AccountDeletionRunner) abortMultipart(ctx context.Context, tenantID string, te *TenantErasure) error {
	rows, err := r.db.QueryContext(ctx, `SELECT upload_id, status FROM multipart_uploads WHERE tenant_id = $1`, tenantID)
	if err != nil {
		return fmt.Errorf("list uploads: %w", err)
	}
	type up struct{ id, status string }
	var ups []up
	for rows.Next() {
		var u up
		if err := rows.Scan(&u.id, &u.status); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan upload: %w", err)
		}
		ups = append(ups, u)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, u := range ups {
		if u.status == "active" {
			if _, err := r.db.ExecContext(ctx,
				`UPDATE multipart_uploads SET status = 'aborted' WHERE upload_id = $1 AND tenant_id = $2 AND status = 'active'`,
				u.id, tenantID); err != nil {
				return fmt.Errorf("abort upload %s: %w", u.id, err)
			}
			te.MultipartAborted++
		}
		if validUploadID(u.id) {
			if err := os.RemoveAll(multipartDir(u.id)); err != nil {
				return fmt.Errorf("remove staging dir of %s: %w", u.id, err)
			}
		}
	}
	return nil
}

// handleAccountDeletionTrigger runs the job once on demand (admin). A run
// already holding the lock answers 409 already_running.
func (s *Server) handleAccountDeletionTrigger(w http.ResponseWriter, r *http.Request) {
	if s.accountDeletion == nil {
		http.Error(w, "account deletion runner not available", http.StatusServiceUnavailable)
		return
	}
	ctx, cancel := adminTriggerContext(r)
	defer cancel()
	res, err := s.accountDeletion.RunOnce(ctx)
	if errors.Is(err, errJobAlreadyRunning) {
		writeJobAlreadyRunning(w, accountDeletionJob)
		return
	}
	actor, _ := r.Context().Value(userIDKey).(string)
	audit.Record(r.Context(), s.db, audit.Entry{UserID: actor, EventType: "admin", Action: "admin.account_deletion", Error: err,
		Metadata: map[string]any{"erased": res.Erased, "deferred": res.Deferred, "cancelled": res.Cancelled}})
	w.Header().Set("Content-Type", "application/json")
	if err != nil {
		// Deferred tenants are reported, not hidden: the body carries them.
		s.logger.Error("manual account deletion run deferred tenants", zap.Error(err))
		w.WriteHeader(http.StatusInternalServerError)
	}
	_ = json.NewEncoder(w).Encode(res)
}
