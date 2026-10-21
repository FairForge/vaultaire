package api

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/FairForge/vaultaire/internal/audit"
	"github.com/FairForge/vaultaire/internal/flags"
	"github.com/FairForge/vaultaire/internal/payments"
	"github.com/FairForge/vaultaire/internal/usage"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"
)

// The entitlement_lapse job (WP-PAY-1, docs/PAYMENTS.md § 1.3): the calendar
// is what ends a house. Daily 07:30 UTC (+5 m boot catch-up, 10 min
// ceiling), two passes (the pre-097 backfill was removed ahead of migration 100 —
// WP-PAY-7: it read the tenants columns 100 drops, and prod never had a house
// to backfill):
//
//	(a) lapse      rows with lapsed_at NULL and paid_through + grace in the
//	               past. A tenant whose provider was unready at any time in
//	               the last 24 h is skipped (payments.ReadyHistory; every
//	               provider counts as ready until WP-PAY-2 feeds one). With
//	               the entitlement_lapse flag OFF for the tenant (the
//	               default) the run only counts it (would_clear, one Info
//	               line per run); ON: payments.Revoke — the floors cleared,
//	               used bytes untouched, lapsed_at = now — and an audit row
//	               billing.house_lapsed.
//	(b) reconcile  every granted row's floors must equal its house; a
//	               tenant whose floors drifted is healed (usage.SetHouseTx)
//	               with one Error line — the two tables cannot drift
//	               silently. It heals whatever the flag: every writer writes
//	               both since 097, so a drift is a bug or a pre-097 state,
//	               and the entitlement is the billing truth.
//
// A run fails only when a list cannot be read; a tenant that could not be
// handled is a note on an ok run (tomorrow's work). The job is the only
// writer of lapsed_at besides the Stripe webhook's clearing branch, which
// WP-PAY-2 moves behind the flag.

const (
	entitlementLapseJobName = "entitlement_lapse"
	// entitlementUnreadyWindow: a provider unready at any time in it keeps
	// its tenants' houses for this run.
	entitlementUnreadyWindow = 24 * time.Hour
)

var entitlementsLapsed = prometheus.NewCounter(prometheus.CounterOpts{
	Name: "vaultaire_entitlements_lapsed_total",
	Help: "Houses the entitlement_lapse job cleared (paid_through + grace passed, flag entitlement_lapse on for the tenant).",
})

// EntitlementLapseRunner is the job.
type EntitlementLapseRunner struct {
	db     *sql.DB
	logger *zap.Logger
	flags  *flags.Service
	// Grace: ENTITLEMENT_GRACE (payments.GraceFromEnv).
	Grace time.Duration
	// Ready: which providers could take a payment (payments.AlwaysReady).
	Ready payments.ReadyHistory
	// Mailer sends house_lapsed.html after a Revoke committed (WP-PAY-3;
	// nil = no mail).
	Mailer *payments.Mailer
	// JobName is the job_runs name (tests use their own: the table is shared).
	JobName    string
	MaxRunTime time.Duration

	// onlyTenants is a test hook: when set, every pass skips the tenants it
	// rejects (the test database is shared by every package).
	onlyTenants func(tenantID string) bool
	now         func() time.Time
}

// EntitlementLapseResult is one run's outcome (job_runs.result).
type EntitlementLapseResult struct {
	Candidates     int      `json:"candidates"`
	WouldClear     int      `json:"would_clear"`
	Cleared        int      `json:"cleared"`
	SkippedUnready int      `json:"skipped_unready"`
	Healed         int      `json:"healed"`
	Grace          string   `json:"grace"`
	Errors         []string `json:"errors,omitempty"`
}

// NewEntitlementLapseRunner builds the job; nil without a database.
func NewEntitlementLapseRunner(db *sql.DB, fl *flags.Service, logger *zap.Logger, grace time.Duration) *EntitlementLapseRunner {
	if db == nil {
		return nil
	}
	if logger == nil {
		logger = zap.NewNop()
	}
	if grace <= 0 {
		grace = payments.DefaultGrace
	}
	return &EntitlementLapseRunner{db: db, logger: logger, flags: fl, Grace: grace, Ready: payments.AlwaysReady{},
		JobName: entitlementLapseJobName, MaxRunTime: 10 * time.Minute, now: time.Now}
}

func (r *EntitlementLapseRunner) spec() jobSpec {
	return jobSpec{
		Name: r.JobName, Hour: 7, Minute: 30, BootDelay: 5 * time.Minute, MaxRunTime: r.MaxRunTime,
		Run: func(ctx context.Context) (jobReport, error) {
			res, err := r.RunOnce(ctx)
			rep := jobReport{Rows: int64(res.Cleared + res.Healed), Result: res}
			if len(res.Errors) > 0 {
				rep.Note = strings.Join(res.Errors, "; ")
			}
			return rep, err
		},
	}
}

func (r *EntitlementLapseRunner) scoped(tenantID string) bool {
	return r.onlyTenants == nil || r.onlyTenants(tenantID)
}

// enforcing reports the entitlement_lapse flag for the tenant (default OFF;
// no flag service = off).
func (r *EntitlementLapseRunner) enforcing(tenantID string) bool {
	return r.flags != nil && r.flags.Enabled(flagEntitlementLapse, tenantID)
}

// RunOnce is the work (no lock, no job_runs: the scheduler's).
func (r *EntitlementLapseRunner) RunOnce(ctx context.Context) (EntitlementLapseResult, error) {
	res := EntitlementLapseResult{Grace: r.Grace.String()}
	if r == nil || r.db == nil {
		return res, errors.New("entitlement lapse: not configured")
	}
	if err := r.lapse(ctx, &res); err != nil {
		return res, err
	}
	if err := r.reconcile(ctx, &res); err != nil {
		return res, err
	}
	r.logger.Info("entitlement lapse run",
		zap.Int("candidates", res.Candidates),
		zap.Int("would_clear", res.WouldClear), zap.Int("cleared", res.Cleared),
		zap.Int("skipped_unready", res.SkippedUnready), zap.Int("healed", res.Healed),
		zap.Duration("grace", r.Grace), zap.Int("errors", len(res.Errors)))
	return res, ctx.Err()
}

// --- (a) lapse ------------------------------------------------------------

type lapseCandidate struct {
	tenantID, provider string
	paidThrough        time.Time
}

func (r *EntitlementLapseRunner) lapse(ctx context.Context, res *EntitlementLapseResult) error {
	now := r.now()
	cutoff := now.Add(-r.Grace)
	rows, err := r.db.QueryContext(ctx, `
		SELECT tenant_id, provider, paid_through FROM entitlements
		 WHERE lapsed_at IS NULL AND paid_through < $1
		 ORDER BY paid_through, tenant_id`, cutoff.UTC())
	if err != nil {
		return fmt.Errorf("entitlement lapse: candidates: %w", err)
	}
	var cands []lapseCandidate
	for rows.Next() {
		var c lapseCandidate
		if err := rows.Scan(&c.tenantID, &c.provider, &c.paidThrough); err != nil {
			_ = rows.Close()
			return fmt.Errorf("entitlement lapse: scan: %w", err)
		}
		if r.scoped(c.tenantID) {
			cands = append(cands, c)
		}
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("entitlement lapse: candidates: %w", err)
	}
	res.Candidates = len(cands)
	for _, c := range cands {
		if ctx.Err() != nil {
			return nil
		}
		if r.Ready != nil && r.Ready.UnreadyWithin(ctx, c.provider, entitlementUnreadyWindow, now) {
			res.SkippedUnready++
			res.Errors = append(res.Errors, fmt.Sprintf("%s: provider_unready (%s)", c.tenantID, c.provider))
			continue
		}
		if !r.enforcing(c.tenantID) {
			res.WouldClear++
			continue
		}
		cleared, e, err := r.clear(ctx, c, cutoff)
		if err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("lapse %s: %v", c.tenantID, err))
			r.logger.Warn("entitlement lapse: house not cleared", zap.String("tenant", c.tenantID), zap.Error(err))
			continue
		}
		if !cleared {
			continue // paid or cleared meanwhile
		}
		res.Cleared++
		entitlementsLapsed.Inc()
		audit.Record(ctx, r.db, audit.Entry{TenantID: c.tenantID, EventType: "billing", Action: "billing.house_lapsed",
			Metadata: map[string]any{"tenant": c.tenantID, "paid_through": c.paidThrough.UTC().Format(time.RFC3339),
				"provider": c.provider, "grace": r.Grace.String()}})
		r.logger.Warn("house lapsed — back to the free tier, data untouched",
			zap.String("tenant", c.tenantID), zap.String("provider", c.provider), zap.Time("paid_through", c.paidThrough))
		// The customer is told after the commit, best effort (RP20-A2).
		if r.Mailer != nil {
			mctx, cancel := payments.Detached(ctx)
			if err := r.Mailer.SendHouseLapsed(mctx, c.tenantID, e); err != nil {
				r.logger.Warn("entitlement lapse: house_lapsed mail not sent", zap.String("tenant", c.tenantID), zap.Error(err))
			}
			cancel()
		}
	}
	return nil
}

// clear revokes one candidate under its entitlement lock, after reading the
// row again: a payment that extended it meanwhile wins.
func (r *EntitlementLapseRunner) clear(ctx context.Context, c lapseCandidate, cutoff time.Time) (bool, payments.Entitlement, error) {
	var cleared bool
	var e payments.Entitlement
	err := payments.InTx(ctx, r.db, func(tx *sql.Tx) error {
		if err := payments.Lock(ctx, tx, c.tenantID); err != nil {
			return err
		}
		row, err := payments.Get(ctx, tx, c.tenantID)
		if errors.Is(err, payments.ErrNoEntitlement) {
			return nil
		}
		if err != nil {
			return err
		}
		if row.LapsedAt != nil || !row.PaidThrough.Before(cutoff) {
			return nil // paid or cleared meanwhile
		}
		cleared, e = true, row
		return payments.Revoke(ctx, tx, c.tenantID)
	})
	return cleared, e, err
}

// --- the skipped-unready gauge ---------------------------------------------

// lapseSkippedCollector reads vaultaire_entitlement_lapse_skipped_unready —
// the last entitlement_lapse run's skipped_unready — from job_runs.result
// on scrape (15 s cache; no series until a run has written one). The rule
// EntitlementLapseSkippedUnready reads two days of it.
type lapseSkippedCollector struct {
	db      *sql.DB
	jobName string
	logger  *zap.Logger
	desc    *prometheus.Desc

	mu  sync.Mutex
	at  time.Time
	val float64
	ok  bool
}

func newLapseSkippedCollector(db *sql.DB, jobName string, logger *zap.Logger) *lapseSkippedCollector {
	return &lapseSkippedCollector{db: db, jobName: jobName, logger: logger,
		desc: prometheus.NewDesc("vaultaire_entitlement_lapse_skipped_unready",
			"Houses the last entitlement_lapse run kept because their provider was unready in the last 24 h (skipped_unready). Read from job_runs.result; no series before the first run.", nil, nil)}
}

func (c *lapseSkippedCollector) Describe(ch chan<- *prometheus.Desc) { ch <- c.desc }

func (c *lapseSkippedCollector) Collect(ch chan<- prometheus.Metric) {
	if v, ok := c.read(); ok {
		ch <- prometheus.MustNewConstMetric(c.desc, prometheus.GaugeValue, v)
	}
}

func (c *lapseSkippedCollector) read() (float64, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.db == nil || (!c.at.IsZero() && time.Since(c.at) < 15*time.Second) {
		return c.val, c.ok
	}
	c.at = time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var v sql.NullFloat64
	err := c.db.QueryRowContext(ctx, `SELECT (result->>'skipped_unready')::float FROM job_runs WHERE job = $1`, c.jobName).Scan(&v)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		c.ok = false
	case err != nil:
		if c.logger != nil {
			c.logger.Warn("entitlement lapse metrics: could not read job_runs", zap.Error(err))
		}
	default:
		c.val, c.ok = v.Float64, v.Valid
	}
	return c.val, c.ok
}

// --- (b) reconcile --------------------------------------------------------

func (r *EntitlementLapseRunner) reconcile(ctx context.Context, res *EntitlementLapseResult) error {
	rows, err := r.db.QueryContext(ctx, `
		SELECT e.tenant_id
		  FROM entitlements e
		  LEFT JOIN tenant_quotas q ON q.tenant_id = e.tenant_id
		  LEFT JOIN tenant_floor_quotas fs ON fs.tenant_id = e.tenant_id AND fs.floor = 'standard'
		  LEFT JOIN tenant_floor_quotas fv ON fv.tenant_id = e.tenant_id AND fv.floor = 'vault'
		 WHERE e.lapsed_at IS NULL
		   AND (fs.storage_limit_bytes IS DISTINCT FROM e.std_tb::bigint * $1
		     OR fv.storage_limit_bytes IS DISTINCT FROM e.vault_tb::bigint * $1
		     OR q.pin_hot_bytes IS DISTINCT FROM e.pin_hot_tb::bigint * $1
		     OR q.storage_limit_bytes IS DISTINCT FROM (e.std_tb + e.vault_tb)::bigint * $1)
		 ORDER BY e.tenant_id`, usage.TB)
	if err != nil {
		return fmt.Errorf("entitlement reconcile: list: %w", err)
	}
	var drifted []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return fmt.Errorf("entitlement reconcile: scan: %w", err)
		}
		if r.scoped(id) {
			drifted = append(drifted, id)
		}
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("entitlement reconcile: list: %w", err)
	}
	for _, id := range drifted {
		if ctx.Err() != nil {
			return nil
		}
		var house usage.House
		err := payments.InTx(ctx, r.db, func(tx *sql.Tx) error {
			if err := payments.Lock(ctx, tx, id); err != nil {
				return err
			}
			e, err := payments.Get(ctx, tx, id)
			if err != nil {
				return err
			}
			if e.LapsedAt != nil {
				return nil // cleared meanwhile
			}
			house = e.Order.House()
			return usage.SetHouseTx(ctx, tx, id, house)
		})
		if err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("heal %s: %v", id, err))
			r.logger.Warn("entitlement reconcile: floors not healed", zap.String("tenant", id), zap.Error(err))
			continue
		}
		res.Healed++
		r.logger.Error("entitlement reconcile: the floors did not match the entitlement — healed to it",
			zap.String("tenant", id), zap.Int("std_tb", house.StdTB()), zap.Int("vault_tb", house.VaultTB()),
			zap.Int("pin_hot_tb", house.PinHotTB()))
	}
	return nil
}

// --- the expiring gauge ---------------------------------------------------

// entitlementsExpiringCollector reads vaultaire_entitlements_expiring{window}
// from the table on scrape (cached): granted houses whose paid_through falls
// within 7 or 30 days, and those already in grace (paid_through passed).
type entitlementsExpiringCollector struct {
	db     *sql.DB
	logger *zap.Logger
	desc   *prometheus.Desc

	mu   sync.Mutex
	at   time.Time
	vals map[string]float64
}

const entitlementsExpiringTTL = time.Minute

var entitlementWindows = []string{"7d", "30d", "in_grace"}

func newEntitlementsExpiringCollector(db *sql.DB, logger *zap.Logger) *entitlementsExpiringCollector {
	return &entitlementsExpiringCollector{db: db, logger: logger,
		desc: prometheus.NewDesc("vaultaire_entitlements_expiring",
			"Granted houses by when their paid_through date falls: within 7 days, within 30 days, or already passed (in_grace — the lapse job clears it after ENTITLEMENT_GRACE). Read from entitlements.",
			[]string{"window"}, nil)}
}

func (c *entitlementsExpiringCollector) Describe(ch chan<- *prometheus.Desc) { ch <- c.desc }

func (c *entitlementsExpiringCollector) Collect(ch chan<- prometheus.Metric) {
	vals := c.read()
	for _, w := range entitlementWindows {
		v, ok := vals[w]
		if !ok {
			continue
		}
		ch <- prometheus.MustNewConstMetric(c.desc, prometheus.GaugeValue, v, w)
	}
}

func (c *entitlementsExpiringCollector) read() map[string]float64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.db == nil || (c.vals != nil && time.Since(c.at) < entitlementsExpiringTTL) {
		return c.vals
	}
	c.at = time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var d7, d30, grace int64
	if err := c.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FILTER (WHERE paid_through >= NOW() AND paid_through < NOW() + INTERVAL '7 days'),
		       COUNT(*) FILTER (WHERE paid_through >= NOW() AND paid_through < NOW() + INTERVAL '30 days'),
		       COUNT(*) FILTER (WHERE paid_through < NOW())
		  FROM entitlements WHERE lapsed_at IS NULL`).Scan(&d7, &d30, &grace); err != nil {
		if c.logger != nil {
			c.logger.Warn("entitlement metrics: could not read the table", zap.Error(err))
		}
		return c.vals
	}
	c.vals = map[string]float64{"7d": float64(d7), "30d": float64(d30), "in_grace": float64(grace)}
	return c.vals
}
