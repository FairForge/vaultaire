package api

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/FairForge/vaultaire/internal/account"
	"github.com/FairForge/vaultaire/internal/audit"
	"github.com/FairForge/vaultaire/internal/common"
	"github.com/FairForge/vaultaire/internal/crypto"
	dashauth "github.com/FairForge/vaultaire/internal/dashboard/auth"
	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/FairForge/vaultaire/internal/payments"
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
//	a. Billing — cancel every subscription the record names immediately
//	   (the entitlement's provider_ref, an invalid house's subscription) and
//	   stamp each in payment_events (deletion_cancel, keyed by the
//	   reference — WP-PAY-7; a new subscription is a new reference, so a
//	   stale stamp never lets an erase skip it). A provider error (or no
//	   provider while a house renews by itself) defers THIS tenant to the
//	   next run; nothing of theirs is touched before billing is settled.
//	b. Objects — every object_head_cache row is deleted on its RECORDED
//	   backend (engine.WithRecordedBackend + Delete, R6-05/WP-R6-1); chunked rows
//	   release their manifest under the key lock in the same transaction as
//	   the head row (deleteHeadRowReleasing, R8) and never touch _global
//	   blobs (dedup GC sweeps them); active multipart uploads are aborted
//	   and their staging dirs removed; quota is released per object so a
//	   cancel mid-walk leaves an honest ledger. Object Lock does not protect
//	   against the owner's own erasure — the count is logged. A backend
//	   failure leaves the row; the tenant is deferred and the next run
//	   resumes where it stopped.
//	   Then the SWEEP (erasure_sweep.go, WP-R10-3c): every registered backend
//	   is asked what it still holds for the tenant — blobs behind a delete
//	   marker, blobs no row names — and it is deleted. A backend that cannot
//	   be listed or refuses a delete defers the tenant.
//	c. Rows — account.EraseRows: one transaction over every tenant/user
//	   table (the contract is tested against information_schema), users and
//	   tenants last, ledgers kept and scrubbed.
//	d. Sessions — dashboard sessions and STS tokens go with the rows; the
//	   in-process credential cache is evicted (auth.Evict) so the erased
//	   user cannot sign in from memory until the next restart.
//	e. Report — one account.erased audit row with the counts per stage,
//	   the blobs swept per backend and the backends that could not be swept.
//
// A cancel wins any time before stage c: the user row is re-read under FOR
// UPDATE before every batch and inside the erase transaction. Objects a
// batch already deleted are gone — the grace period is the time to change
// one's mind, the walk is not.

const (
	accountDeletionJob      = "account_deletion"
	outcomeErased           = "erased"
	outcomeDeferred         = "deferred"
	outcomeCancelled        = "cancelled"
	accountDeletionDueLimit = 200
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
)

// paymentProviders is the slice of payments.Registry the runner needs: a
// provider by name, whatever its flag (Cancel stops money; it is never
// "new business"). An interface so the test can count calls.
type paymentProviders interface {
	Provider(name string) payments.Provider
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

	// Payments: the providers (WP-PAY-2). The tenant's provider cancels;
	// one that is unconfigured (Stripe without STRIPE_SECRET_KEY) defers a
	// tenant whose house it renews automatically — never erased unbilled.
	Payments paymentProviders
	// Auth evicts the erased account from the in-process cache (nil-safe).
	Auth accountEvictor
	// VaultParity erases the parity shards of the tenant's vault objects
	// before the sweep (WP-VAULT-1); a leg that cannot be reached defers
	// the tenant. Nil-safe.
	VaultParity *VaultParity
	// Keys forgets the erased tenant's unwrapped keys in this process (nil
	// without a master key). The rows themselves — the crypto-shred — go
	// with account.EraseRows (WP-R8-1).
	Keys *crypto.TenantKeys
	// Sessions revokes the user's dashboard sessions (nil-safe; the DB
	// store's rows are deleted by EraseRows anyway).
	Sessions dashauth.SessionStore

	BatchSize      int
	MaxRunTime     time.Duration
	TenantDeadline time.Duration
	// JobName is the job_runs name (tests use their own: the table is shared).
	JobName string
	// The sweep's bounds (erasure_sweep.go): one delete, one List of one
	// container, the failed deletes after which a backend is given up for
	// this run, the objects between two cancel checks.
	SweepDeleteTimeout time.Duration
	SweepListTimeout   time.Duration
	SweepMaxFailures   int
	SweepCancelEvery   int

	now         func() time.Time
	beforeBatch func() // test hook: runs before each object batch
	// onlyDue is a test hook: when set, RunOnce skips every due account it
	// rejects. Packages share one test database and seed their own past-due
	// accounts; a fixture's run must not erase them. Never set in production.
	onlyDue func(account.Due) bool
	// beforeSweep and beforeFinalSweep are test hooks: they run before the
	// sweep and before the pass after the row erase.
	beforeSweep      func()
	beforeFinalSweep func()

	// settles are the walks of lagging backends a run owes (erasure_settle.go),
	// loaded from job_runs.result at the start of RunOnce.
	settles erasureSettleState
}

// TenantErasure is one tenant's outcome in a run.
type TenantErasure struct {
	TenantID         string `json:"tenant_id"`
	UserID           string `json:"user_id"`
	Outcome          string `json:"outcome"`
	StripeCancelled  bool   `json:"stripe_cancelled"`
	ObjectsDeleted   int    `json:"objects_deleted"`
	ChunkedReleased  int    `json:"chunked_released"`
	LockedErased     int    `json:"locked_erased"`
	ObjectFailures   int    `json:"object_failures"`
	MultipartAborted int    `json:"multipart_aborted"`
	// ParityErased counts vault objects whose parity shards were erased
	// from the leg (WP-VAULT-1).
	ParityErased int `json:"parity_erased,omitempty"`
	// Swept is the number of blobs with no head row the sweep deleted, per
	// backend (WP-R10-3c). SweptAfterErase of them were found by the pass
	// after the row erase (a write that was in flight).
	Swept           map[string]int `json:"swept,omitempty"`
	SweptAfterErase int            `json:"swept_after_erase,omitempty"`
	SweepFailures   int            `json:"sweep_failures,omitempty"`
	// ChunkBlobsLeft counts blobs of the shared chunk container found under
	// the tenant's prefix on a fixed-bucket backend: never swept (the content
	// index owns them). Dedup GC does not reach them there yet — WP-R8-7.
	ChunkBlobsLeft int `json:"chunk_blobs_left,omitempty"`
	// SweptBackends are the registered backends the sweep ran on;
	// UnsweptBackends are named by the tenant's rows and have no driver —
	// bytes there, if any, were not reached.
	SweptBackends   []string `json:"swept_backends,omitempty"`
	UnsweptBackends []string `json:"unswept_backends,omitempty"`
	// Settling are the backends whose listing lags (erasure_settle.go): the
	// rows are erased, and a walk started at SettleAfter or later that
	// finds nothing there finishes the erasure (outcome "settling").
	Settling    []string         `json:"settling,omitempty"`
	SettleAfter *time.Time       `json:"settle_after,omitempty"`
	Rows        map[string]int64 `json:"rows,omitempty"`
	// KeysShredded counts the tenant key versions deleted with the rows
	// (WP-R8-1): from then on every blob sealed under them — a copy the
	// sweep missed, a backup of a backend — is unreadable.
	KeysShredded int64  `json:"keys_shredded,omitempty"`
	Error        string `json:"error,omitempty"`
}

// AccountDeletionResult is one run's outcome.
type AccountDeletionResult struct {
	Tenants   []TenantErasure `json:"tenants"`
	Erased    int             `json:"erased"`
	Deferred  int             `json:"deferred"`
	Cancelled int             `json:"cancelled"`
	// Settling counts tenants whose rows went in this run and whose lagging
	// backends still owe a walk (erasure_settle.go).
	Settling int      `json:"settling,omitempty"`
	Duration string   `json:"duration"`
	Errors   []string `json:"errors,omitempty"`
	// Notes are what did not fail the run: erasures waiting for a listing
	// to settle, a settle walk that found something.
	Notes []string `json:"notes,omitempty"`

	// owed is the settle state the run ends with (nil when it could not be
	// read): the job's job_runs.result.
	owed *erasureSettleState
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
	// Start every result at 0: AccountDeletionDeferred reads an increase, and
	// a series that first appears at 1 after a restart is not one.
	for _, r := range []string{outcomeErased, outcomeDeferred, outcomeCancelled, outcomeSettling} {
		accountDeletionTenants.WithLabelValues(r)
	}
	// The sweep's series too, for every backend registered at this point
	// (drivers are registered at boot, before the server builds its jobs).
	if ce, ok := eng.(*engine.CoreEngine); ok {
		for _, backend := range ce.GetDriverNames() {
			for _, result := range []string{"deleted", "failed", "shared_left"} {
				accountDeletionSwept.WithLabelValues(backend, result)
			}
		}
	}
	return &AccountDeletionRunner{
		db: db, logger: logger, eng: eng, gci: gci, quota: quota, account: acct,
		BatchSize:      1000,
		MaxRunTime:     6 * time.Hour,
		TenantDeadline: 2 * time.Hour,
		JobName:        accountDeletionJob,

		SweepDeleteTimeout: defaultSweepDeleteTimeout,
		SweepListTimeout:   defaultSweepListTimeout,
		SweepMaxFailures:   defaultSweepMaxFailures,
		SweepCancelEvery:   defaultSweepCancelEvery,

		now: time.Now,
	}
}

// spec is the job's schedule: daily at 04:30 UTC (an hour after retention),
// catch-up two minutes after boot.
//
// What makes a run fail: a tenant that was deferred (Stripe refused the
// cancel, a backend refused a delete, the per-tenant deadline) or the due
// list could not be read. last_success_at then stays put and every hourly
// check retries — that retry is the point: the erasure is owed on its date,
// and each stage is idempotent. A cancelled tenant does not fail the run,
// nor does one waiting for a lagging listing to settle: the job runs again
// at the first hourly check after its walk is due (Again, erasure_settle.go).
// The owed walks are the job's job_runs.result.
func (r *AccountDeletionRunner) spec() jobSpec {
	return jobSpec{
		Name: r.JobName, Hour: 4, Minute: 30,
		BootDelay: 2 * time.Minute, MaxRunTime: r.MaxRunTime,
		Again: r.settleDue,
		Run: func(ctx context.Context) (jobReport, error) {
			res, err := r.RunOnce(ctx)
			if len(res.Tenants) > 0 || err != nil {
				r.logger.Info("account deletion run", zap.Int("erased", res.Erased), zap.Int("deferred", res.Deferred),
					zap.Int("cancelled", res.Cancelled), zap.Int("settling", res.Settling),
					zap.String("duration", res.Duration), zap.Strings("errors", res.Errors))
			}
			rep := jobReport{Rows: int64(res.Erased), Note: strings.Join(res.Notes, "; ")}
			if res.owed != nil {
				rep.Result = *res.owed
			}
			return rep, err
		},
	}
}

// RunOnce erases every due account. A deferred tenant (Stripe or a backend
// refused) makes the run an error, so job_runs.last_success_at stays put
// and the hourly check retries; a cancelled tenant does not. Locking and
// job_runs are the scheduler's (run it through the registered job).
func (r *AccountDeletionRunner) RunOnce(ctx context.Context) (AccountDeletionResult, error) {
	res := AccountDeletionResult{}
	if r == nil {
		return res, errors.New("account deletion: runner not configured")
	}
	start := r.now()

	// The walks earlier erasures owe a lagging backend (erasure_settle.go),
	// first: they are the oldest debts.
	st, err := r.loadSettles(ctx)
	if err != nil {
		return res, fmt.Errorf("account deletion: %w", err)
	}
	r.settles = st
	r.settleErasures(ctx, &res)

	dues, err := r.account.ListDue(ctx, start, accountDeletionDueLimit)
	if err != nil {
		owed := r.settles
		res.owed = &owed
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
		case outcomeSettling:
			res.Settling++
			res.Notes = append(res.Notes, fmt.Sprintf("tenant %s: rows erased, %s's listing settling until %s",
				d.TenantID, strings.Join(te.Settling, ", "), te.SettleAfter.UTC().Format(time.RFC3339)))
		default:
			res.Deferred++
			res.Errors = append(res.Errors, fmt.Sprintf("tenant %s deferred: %s", d.TenantID, te.Error))
		}
	}
	res.Duration = r.now().Sub(start).String()
	owed := r.settles
	res.owed = &owed

	if len(res.Errors) > 0 {
		return res, errors.New(strings.Join(res.Errors, "; "))
	}
	return res, nil
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
		case outcomeSettling:
			// The rows are gone; the account.erased row is written once the
			// lagging backends' walks come back empty (erasure_settle.go).
			meta := r.auditMeta(ctx, te, d)
			meta["settling"], meta["settle_after"] = te.Settling, te.SettleAfter
			audit.Record(ctx, r.db, audit.Entry{TenantID: d.TenantID, EventType: "account", Action: "account.erasure_settling",
				Severity: "warning", Metadata: meta})
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
		// b1. The parity copies of the vault objects (WP-VAULT-1): every
		// row's shards on the leg, before the sweep; a leg that is not
		// registered or whose breaker is open defers the tenant — never
		// "erased" while parity bytes may remain.
		n, err := r.VaultParity.EraseTenant(ctx, d.TenantID)
		te.ParityErased += n
		if err != nil {
			return r.defer_(te, log, "parity", err)
		}
	}

	// b2. The sweep: what the backends still hold for the tenant with no head
	// row to name it. Read the plan now — the pass after the erase has no
	// rows left to ask.
	var plan sweepPlan
	if d.TenantID != "" {
		var err error
		if plan, err = r.planSweep(ctx, d.TenantID); err != nil {
			return r.defer_(te, log, "sweep plan", err)
		}
		te.SweptBackends, te.UnsweptBackends = plan.backends, plan.unregistered
		if r.beforeSweep != nil {
			r.beforeSweep()
		}
		stop, err := r.sweepTenant(ctx, d, plan, &te, log, false)
		if err != nil {
			return r.defer_(te, log, "sweep", err)
		}
		if stop {
			te.Outcome = outcomeCancelled
			log.Warn("account deletion: cancelled during the sweep; objects and blobs already deleted are gone",
				zap.Int("objects_deleted", te.ObjectsDeleted), zap.Any("swept", te.Swept))
			return te
		}
	}

	// A backend whose listing lags cannot prove itself empty today: the walk
	// it owes is recorded BEFORE the rows go, so a run stopped right after
	// the erase still owes it (erasure_settle.go).
	lagging := r.laggingBackends(plan.backends)
	if len(lagging) > 0 {
		if err := r.recordSettles(ctx, d, lagging); err != nil {
			return r.defer_(te, log, "record the settle walk", err)
		}
	}

	// c. Rows (one transaction; refuses when a cancel landed meanwhile).
	rows, err := r.account.EraseRows(ctx, d.UserID, d.TenantID, d.Email, r.now())
	if errors.Is(err, account.ErrNotPending) {
		r.forgetSettles(ctx, d.TenantID, log)
		te.Outcome = outcomeCancelled
		log.Warn("account deletion: cancelled before the row erase; objects already deleted are gone")
		return te
	}
	if err != nil {
		r.forgetSettles(ctx, d.TenantID, log)
		return r.defer_(te, log, "erase rows", err)
	}
	te.Rows = rows.Rows
	te.KeysShredded = rows.Rows["tenant_key_versions"]
	if r.Keys != nil && d.TenantID != "" {
		r.Keys.Forget(d.TenantID)
	}
	// The webhook endpoints went with the rows: move the tenant's webhook
	// generation so a job queued before the erase re-reads them (and finds
	// none) instead of POSTing with what it captured (Prompt 2b 0.6).
	if d.TenantID != "" {
		forgetWebhookEndpoints(d.TenantID)
	}

	// c2. Once more over the backends, now that the credentials are gone: a
	// write that was in flight during the sweep may have put its bytes down
	// after its container was listed. Nothing can defer any more — the rows
	// are erased — so a failure here is an Error line and a count, and the
	// bytes it names are for an operator.
	if d.TenantID != "" {
		if r.beforeFinalSweep != nil {
			r.beforeFinalSweep()
		}
		if _, err := r.sweepTenant(ctx, d, plan, &te, log, true); err != nil {
			te.Error = "sweep after the erase: " + err.Error()
			log.Error("account deletion: the sweep after the row erase did not complete — bytes written during the erasure may remain on a backend",
				zap.Error(err), zap.Any("swept", te.Swept))
		}
	}

	// d. Sessions and the in-process cache.
	if r.Sessions != nil {
		if err := r.Sessions.DeleteByUserID(context.WithoutCancel(ctx), d.UserID); err != nil {
			log.Error("account deletion: revoke sessions", zap.Error(err))
		}
	}
	if r.Auth != nil {
		r.Auth.Evict(d.UserID, d.TenantID)
	}

	// From here no write for the tenant can land: the lagging backends'
	// walks count from now.
	te.Outcome = outcomeErased
	if len(lagging) > 0 {
		due, err := r.startSettles(ctx, d.TenantID, r.now())
		if err != nil {
			// The state is still the run's result at its end.
			log.Error("account deletion: could not record when writes stopped — the run's result carries it", zap.Error(err))
		}
		te.Outcome, te.SettleAfter = outcomeSettling, &due
		for _, lb := range lagging {
			te.Settling = append(te.Settling, lb.name)
		}
		log.Info("account rows erased — waiting for a lagging listing to settle before the erasure is declared",
			zap.Strings("settling", te.Settling), zap.Time("settle_after", due))
	}

	// e. Report.
	msg := "account erased"
	if te.Outcome == outcomeSettling {
		msg = "account rows erased, lagging backends settling"
	}
	log.Info(msg,
		zap.Bool("stripe_cancelled", te.StripeCancelled),
		zap.Int("objects_deleted", te.ObjectsDeleted), zap.Int("chunked_released", te.ChunkedReleased),
		zap.Int("locked_erased", te.LockedErased), zap.Int("multipart_aborted", te.MultipartAborted),
		zap.Any("swept", te.Swept), zap.Int("swept_after_erase", te.SweptAfterErase),
		zap.Int("chunk_blobs_left", te.ChunkBlobsLeft), zap.Strings("swept_backends", te.SweptBackends),
		zap.Strings("unswept_backends", te.UnsweptBackends),
		zap.Int64("rows", rows.Total))
	if len(te.UnsweptBackends) > 0 {
		log.Warn("account deletion: the tenant's rows named a backend with no registered driver — the sweep cannot reach it; bytes there, if any, were not deleted",
			zap.Strings("unswept_backends", te.UnsweptBackends))
	}
	if te.ChunkBlobsLeft > 0 {
		log.Warn("account deletion: chunk blobs under the tenant's prefix were not swept — the shared chunk container is the content index's, and on a fixed-bucket backend dedup GC does not reach them yet (WP-R8-7)",
			zap.Int("chunk_blobs_left", te.ChunkBlobsLeft))
	}
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
	meta := map[string]any{
		"actor": actor, "user_id": d.UserID, "scheduled_at": d.ScheduledAt,
		"stripe_cancelled": te.StripeCancelled, "objects_deleted": te.ObjectsDeleted,
		"chunked_released": te.ChunkedReleased, "locked_erased": te.LockedErased,
		"multipart_aborted": te.MultipartAborted, "object_failures": te.ObjectFailures,
		"swept": te.Swept, "swept_after_erase": te.SweptAfterErase, "sweep_failures": te.SweepFailures,
		"chunk_blobs_left": te.ChunkBlobsLeft, "swept_backends": te.SweptBackends,
		"unswept_backends": te.UnsweptBackends,
		"rows":             te.Rows,
		"keys_shredded":    te.KeysShredded,
	}
	if te.Error != "" {
		// An erased account with an error: the pass after the row erase did
		// not complete. The record says what may be left.
		meta["note"] = te.Error
	}
	return meta
}

func (r *AccountDeletionRunner) defer_(te TenantErasure, log *zap.Logger, stage string, err error) TenantErasure {
	te.Outcome = outcomeDeferred
	te.Error = stage + ": " + err.Error()
	log.Error("account deletion: tenant deferred to the next run", zap.String("stage", stage), zap.Error(err),
		zap.Int("objects_deleted", te.ObjectsDeleted), zap.Int("object_failures", te.ObjectFailures),
		zap.Any("swept", te.Swept), zap.Int("sweep_failures", te.SweepFailures))
	return te
}

// cancelSubscription is stage a: stop the money with the provider that
// bills the tenant (WP-PAY-2, docs/PAYMENTS.md § 2.3), read from the record
// only (WP-PAY-7): the entitlement row names the provider, its reference
// and whether it renews by itself, and payment_events names any Stripe
// subscription the webhook acknowledged as an invalid house (RP22-3). Each
// reference is cancelled once: a deletion_cancel row (payments.
// MarkDeletionCancelled, written only after the provider answered) is
// checked before calling, so a crash between the answer and the stamp only
// repeats an idempotent cancel, and a NEW subscription — a new reference —
// is never skipped by an old stamp. An unconfigured provider defers the
// tenant only when the house renews by itself (renewal 'auto'); a prepaid
// row has nothing to cancel. A tenant with neither has nothing billing in
// the record (prod has no Stripe key, so no subscription exists outside
// it; a legacy pack bought before WP-PAY-1 would be the operator's to
// cancel — WP-PAY-7 PR note).
func (r *AccountDeletionRunner) cancelSubscription(ctx context.Context, tenantID string, te *TenantErasure) error {
	type target struct {
		provider, ref string
		auto          bool
	}
	var targets []target
	e, err := payments.Get(ctx, r.db, tenantID)
	switch {
	case errors.Is(err, payments.ErrNoEntitlement):
	case err != nil:
		return fmt.Errorf("read entitlement: %w", err)
	default:
		targets = append(targets, target{provider: e.Provider, ref: e.ProviderRef, auto: e.Renewal == payments.RenewalAuto})
	}
	invalid, err := payments.InvalidHouseRefs(ctx, r.db, tenantID)
	if err != nil {
		return err
	}
	for _, ref := range invalid {
		if len(targets) > 0 && targets[0].provider == payments.ProviderStripe && targets[0].ref == ref {
			continue
		}
		targets = append(targets, target{provider: payments.ProviderStripe, ref: ref, auto: true})
	}

	for _, t := range targets {
		if t.ref != "" {
			done, err := payments.DeletionCancelled(ctx, r.db, tenantID, t.provider, t.ref)
			if err != nil {
				return err
			}
			if done {
				te.StripeCancelled = te.StripeCancelled || t.provider == payments.ProviderStripe
				continue
			}
		}
		var p payments.Provider
		if r.Payments != nil {
			p = r.Payments.Provider(t.provider)
		}
		cerr := payments.ErrProviderUnconfigured
		if p != nil {
			if rc, ok := p.(payments.RefCanceller); ok && t.ref != "" {
				cerr = rc.CancelRef(ctx, tenantID, t.ref)
			} else {
				cerr = p.Cancel(ctx, tenantID)
			}
		}
		if errors.Is(cerr, payments.ErrProviderUnconfigured) {
			if t.auto {
				hint := ""
				if t.provider == payments.ProviderStripe {
					hint = " (STRIPE_SECRET_KEY)"
				}
				return fmt.Errorf("the house is renewed by %s, which is not configured%s — the account is not erased until billing can be cancelled", t.provider, hint)
			}
			continue
		}
		if cerr != nil {
			return fmt.Errorf("cancel with %s: %w", t.provider, cerr)
		}
		if t.ref != "" {
			if err := payments.MarkDeletionCancelled(ctx, r.db, tenantID, t.provider, t.ref); err != nil {
				return fmt.Errorf("stamp subscription cancel: %w", err)
			}
			te.StripeCancelled = te.StripeCancelled || t.provider == payments.ProviderStripe
		}
	}
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
		storageKey           string // the row's blob address ("" = the key, WP-R2-1)
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
			SELECT bucket, object_key, COALESCE(backend_name, ''), is_chunked, COALESCE(storage_key, '')
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
			if err := rows.Scan(&h.bucket, &h.key, &h.backend, &h.chunked, &h.storageKey); err != nil {
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
			if err := r.deleteObject(tctx, t, h.bucket, h.key, h.storageKey, h.backend, h.chunked, te); err != nil {
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
// _global chunks to dedup GC. A whole object's blob is at the row's address
// (storageKey, WP-R2-1; the key when it records none). Retained version
// blobs and anything else no row names are the sweep's (erasure_sweep.go).
func (r *AccountDeletionRunner) deleteObject(ctx context.Context, t *tenant.Tenant, bucket, key, storageKey, backend string, chunked bool, te *TenantErasure) error {
	container, blob := objectBlobAddress(t.ID, bucket, key, storageKey)
	if !chunked {
		// A demoted object whose hot copy is not yet reclaimed has bytes on
		// TWO backends; the recorded one is the cold copy. Delete the hot
		// copy first (a failure keeps the row so the next run retries) — at
		// the address the demotion copied under.
		var hot, hotKey string
		err := r.db.QueryRowContext(ctx, `
			SELECT hot_backend, COALESCE(storage_key, '') FROM smart_demotions
			 WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3 AND hot_deleted_at IS NULL`,
			t.ID, bucket, key).Scan(&hot, &hotKey)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("read demotion ledger: %w", err)
		}
		if hotBlob := blobArtifact(key, hotKey); hot != "" && (hot != backend || hotBlob != blob) {
			if err := r.deleteOnBackend(ctx, container, hotBlob, hot); err != nil {
				return fmt.Errorf("hot copy on %s: %w", hot, err)
			}
		}
		if err := r.deleteOnBackend(ctx, container, blob, backend); err != nil {
			return err
		}
	}
	// The row and its bytes' release in one transaction (WP-R10-8).
	if _, _, err := deleteHeadRowReleasing(ctx, r.db, r.quota != nil, t.ID, bucket, key); err != nil {
		return fmt.Errorf("head row: %w", err)
	}
	if chunked {
		te.ChunkedReleased++
		accountDeletionObjects.WithLabelValues("chunked_released").Inc()
	} else {
		te.ObjectsDeleted++
		accountDeletionObjects.WithLabelValues("deleted").Inc()
	}
	return nil
}

// deleteOnBackend deletes one blob (blob = the row's address) where the head
// row says it is (the R6-05 rule: hint first, or a post-restart delete goes
// to the primary alone). A miss is fine — the row drifted; anything else is
// the caller's failure.
func (r *AccountDeletionRunner) deleteOnBackend(ctx context.Context, container, blob, backend string) error {
	if err := r.eng.Delete(engine.WithRecordedBackend(ctx, backend), container, blob); err != nil && !isObjectMissingErr(err) {
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
			// Aborted with what it holds released (WP-R10-8 Part C).
			if _, err := endUploadReleasing(ctx, r.db, u.id, tenantID, "", ""); err != nil {
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
