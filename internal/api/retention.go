package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/FairForge/vaultaire/internal/audit"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"
)

// Retention job (Review R13-14 — checklist item 5, WP-R9-2, R9-04,
// R10-25/36/39, R11-11, R14-07, WP-R14-2, WP-R14-7).
//
// Nine tables grew without bound, four of them with per-request PII (source
// IP, user agent, referer/country, e-mail bodies in stripe_events.data);
// prod's three largest tables were logs (R9-04: 442k / 436k / 355k rows).
// The privacy policy and the DPA now state the periods below — change a
// period here and there together.
//
// Shape (the pattern WP-R13-3 extends to every daily runner): nightly at
// RunAtUTC with a catch-up at boot when the last success recorded in
// job_runs is older than a day (a plain 24 h ticker never fires on a box
// that is redeployed daily — R13-07); ONE pg_try_advisory_lock per run, so
// the ticker and the admin trigger cannot overlap; batched
// `DELETE … WHERE ctid = ANY(ARRAY(SELECT ctid … LIMIT n))` loops per table
// so a first run over months of backlog never takes one long lock; a metric
// per table.

// retentionLockKey is the pg_advisory_lock key of the retention job.
const retentionLockKey int64 = 0x7265746e // "retn"

// retentionAction is what the policy does to rows past MaxAge.
type retentionAction int

const (
	// retentionDelete removes the row.
	retentionDelete retentionAction = iota
	// retentionScrubColumns keeps the row and blanks ScrubColumns ('' — the
	// columns are NOT NULL and every reader scans them as strings) — for
	// tables where the row is the record (a sign-up) and only the PII
	// columns expire.
	retentionScrubColumns
)

// retentionPolicy is one table's rule.
type retentionPolicy struct {
	Table        string
	Column       string // timestamp column the age is measured on (quoted verbatim in SQL)
	MaxAge       time.Duration
	Action       retentionAction
	ScrubColumns []string
	// Extra is an additional WHERE predicate (no leading AND).
	Extra string
}

// retentionPeriods are the periods the privacy policy states. audit_logs is
// deliberately absent: the security record is kept for the life of the
// service; an account deletion removes the subject's rows (WP-R10-3).
const (
	retentionAccessLog    = 30 * 24 * time.Hour
	retentionEvents       = 90 * 24 * time.Hour
	retentionQuotaEvents  = 90 * 24 * time.Hour
	retentionStripeEvents = 90 * 24 * time.Hour
	retentionCDNAccessLog = 2 * 24 * time.Hour // rolled into cdn_stats_daily; yesterday is re-rolled every hour (R13-03)
	retentionWebhookDeliv = 30 * 24 * time.Hour
	retentionWaitlistPII  = 90 * 24 * time.Hour
)

// defaultRetentionPolicies is the shipped policy set.
func defaultRetentionPolicies() []retentionPolicy {
	return []retentionPolicy{
		{Table: "s3_access_log", Column: "logged_at", MaxAge: retentionAccessLog},
		{Table: "events", Column: "created_at", MaxAge: retentionEvents},
		{Table: "quota_usage_events", Column: `"timestamp"`, MaxAge: retentionQuotaEvents},
		{Table: "stripe_events", Column: "processed_at", MaxAge: retentionStripeEvents},
		{Table: "cdn_access_log", Column: "accessed_at", MaxAge: retentionCDNAccessLog},
		{Table: "webhook_deliveries", Column: "created_at", MaxAge: retentionWebhookDeliv},
		{Table: "waitlist_signups", Column: "created_at", MaxAge: retentionWaitlistPII,
			Action: retentionScrubColumns, ScrubColumns: []string{"ip_address", "user_agent"},
			Extra: "(ip_address <> '' OR user_agent <> '')"},
	}
}

var (
	retentionDeletedRows = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "vaultaire_retention_deleted_rows_total",
		Help: "Rows deleted (or PII-scrubbed) by the retention job, by table.",
	}, []string{"table"})
	retentionLastRun = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "vaultaire_retention_last_run_timestamp_seconds",
		Help: "Unix time of the last successful retention run.",
	})
	retentionRuns = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "vaultaire_retention_runs_total",
		Help: "Retention job runs by outcome.",
	}, []string{"outcome"})
)

// RetentionJob is the nightly log-table pruner.
type RetentionJob struct {
	db     *sql.DB
	logger *zap.Logger

	Policies  []retentionPolicy
	BatchSize int
	// RunAtUTC is the daily wall-clock time (hour, minute) a run is due.
	RunAtHour, RunAtMinute int
	// MaxRunTime bounds one run.
	MaxRunTime time.Duration
	// BootDelay is how long after Start the catch-up check waits.
	BootDelay time.Duration

	now func() time.Time
}

// RetentionResult is one run's outcome.
type RetentionResult struct {
	Tables   map[string]int64 `json:"tables"`
	Total    int64            `json:"total"`
	Duration string           `json:"duration"`
	Errors   []string         `json:"errors,omitempty"`
}

// NewRetentionJob builds the job; nil without a DB.
func NewRetentionJob(db *sql.DB, logger *zap.Logger) *RetentionJob {
	if db == nil {
		return nil
	}
	if logger == nil {
		logger = zap.NewNop()
	}
	return &RetentionJob{
		db: db, logger: logger,
		Policies:    defaultRetentionPolicies(),
		BatchSize:   5000,
		RunAtHour:   3,
		RunAtMinute: 30,
		MaxRunTime:  10 * time.Minute,
		BootDelay:   time.Minute,
		now:         time.Now,
	}
}

// Start runs the scheduler until ctx is done: a catch-up check BootDelay
// after start, then one check per hour. A run happens when the last
// recorded success is older than the most recent RunAt time.
func (j *RetentionJob) Start(ctx context.Context) {
	if j == nil {
		return
	}
	go func() {
		select {
		case <-ctx.Done():
			return
		case <-time.After(j.BootDelay):
		}
		j.runIfDue(ctx)
		ticker := time.NewTicker(time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				j.runIfDue(ctx)
			}
		}
	}()
}

// lastScheduled is the most recent RunAt instant at or before now.
func (j *RetentionJob) lastScheduled(now time.Time) time.Time {
	now = now.UTC()
	at := time.Date(now.Year(), now.Month(), now.Day(), j.RunAtHour, j.RunAtMinute, 0, 0, time.UTC)
	if at.After(now) {
		at = at.Add(-24 * time.Hour)
	}
	return at
}

// due reports whether a run is owed: no recorded success, or the last one
// predates the most recent scheduled time.
func (j *RetentionJob) due(ctx context.Context) (bool, error) {
	last, err := lastJobSuccess(ctx, j.db, "retention")
	if err != nil {
		return false, err
	}
	return last.IsZero() || last.Before(j.lastScheduled(j.now())), nil
}

func (j *RetentionJob) runIfDue(ctx context.Context) {
	due, err := j.due(ctx)
	if err != nil {
		j.logger.Warn("retention: could not read job_runs", zap.Error(err))
		return
	}
	if !due {
		return
	}
	res, err := j.RunOnce(ctx)
	switch {
	case errors.Is(err, errJobAlreadyRunning):
		j.logger.Warn("retention: run skipped, another instance holds the lock")
	case err != nil:
		j.logger.Error("retention run failed", zap.Error(err))
	default:
		j.logger.Info("retention run completed", zap.Int64("rows", res.Total),
			zap.String("duration", res.Duration), zap.Any("tables", res.Tables), zap.Strings("errors", res.Errors))
	}
}

// RunOnce prunes every policy under the advisory lock. Per-table errors are
// collected and the next table continues; the run is recorded in job_runs
// as a success only when no table failed.
func (j *RetentionJob) RunOnce(ctx context.Context) (RetentionResult, error) {
	res := RetentionResult{Tables: map[string]int64{}}
	if j == nil {
		return res, errors.New("retention: job not configured")
	}
	ctx, cancel := context.WithTimeout(ctx, j.MaxRunTime)
	defer cancel()

	// Session-level lock on a dedicated connection: released on Close, so a
	// crash never leaves it held (the pool would hand the session back
	// otherwise).
	conn, err := j.db.Conn(ctx)
	if err != nil {
		return res, fmt.Errorf("retention: acquire conn: %w", err)
	}
	defer func() { _ = conn.Close() }()
	var locked bool
	if err := conn.QueryRowContext(ctx, `SELECT pg_try_advisory_lock($1)`, retentionLockKey).Scan(&locked); err != nil {
		return res, fmt.Errorf("retention: advisory lock: %w", err)
	}
	if !locked {
		return res, errJobAlreadyRunning
	}
	defer func() {
		_, _ = conn.ExecContext(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, retentionLockKey)
	}()

	start := j.now()
	recordJobStart(ctx, j.db, "retention", start)
	for _, p := range j.Policies {
		if ctx.Err() != nil {
			res.Errors = append(res.Errors, "run deadline reached before "+p.Table)
			break
		}
		n, err := j.prune(ctx, conn, p)
		res.Tables[p.Table] = n
		res.Total += n
		retentionDeletedRows.WithLabelValues(p.Table).Add(float64(n))
		if err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("%s: %v", p.Table, err))
			j.logger.Error("retention: table failed", zap.String("table", p.Table), zap.Int64("rows", n), zap.Error(err))
			continue
		}
		if n > 0 {
			j.logger.Info("retention: pruned", zap.String("table", p.Table), zap.Int64("rows", n), zap.Duration("max_age", p.MaxAge))
		}
	}
	res.Duration = j.now().Sub(start).String()

	outcome := "ok"
	var runErr error
	if len(res.Errors) > 0 {
		outcome = "error"
		runErr = errors.New(strings.Join(res.Errors, "; "))
	} else {
		retentionLastRun.Set(float64(j.now().Unix()))
	}
	retentionRuns.WithLabelValues(outcome).Inc()
	recordJobFinish(context.WithoutCancel(ctx), j.db, "retention", j.now(), outcome, runErr, res.Total)
	return res, runErr
}

// prune applies one policy in batches until a batch comes back short.
func (j *RetentionJob) prune(ctx context.Context, conn *sql.Conn, p retentionPolicy) (int64, error) {
	cutoff := j.now().Add(-p.MaxAge)
	where := p.Column + " < $1"
	if p.Extra != "" {
		where += " AND " + p.Extra
	}
	var stmt string
	switch p.Action {
	case retentionScrubColumns:
		sets := make([]string, len(p.ScrubColumns))
		for i, c := range p.ScrubColumns {
			sets[i] = c + " = ''"
		}
		stmt = fmt.Sprintf(`UPDATE %s SET %s WHERE ctid = ANY(ARRAY(SELECT ctid FROM %s WHERE %s LIMIT $2))`,
			p.Table, strings.Join(sets, ", "), p.Table, where)
	default:
		stmt = fmt.Sprintf(`DELETE FROM %s WHERE ctid = ANY(ARRAY(SELECT ctid FROM %s WHERE %s LIMIT $2))`,
			p.Table, p.Table, where)
	}
	var total int64
	for {
		if ctx.Err() != nil {
			return total, ctx.Err()
		}
		res, err := conn.ExecContext(ctx, stmt, cutoff, j.BatchSize)
		if err != nil {
			return total, err
		}
		n, _ := res.RowsAffected()
		total += n
		if n < int64(j.BatchSize) {
			return total, nil
		}
	}
}

// --- job_runs helpers (shared by every runner that adopts this shape) ---

func recordJobStart(ctx context.Context, db *sql.DB, job string, at time.Time) {
	_, _ = db.ExecContext(ctx, `
		INSERT INTO job_runs (job, last_started_at, last_outcome) VALUES ($1, $2, 'running')
		ON CONFLICT (job) DO UPDATE SET last_started_at = EXCLUDED.last_started_at, last_outcome = 'running', last_error = ''`,
		job, at)
}

func recordJobFinish(ctx context.Context, db *sql.DB, job string, at time.Time, outcome string, runErr error, rows int64) {
	errText := ""
	if runErr != nil {
		errText = runErr.Error()
		if len(errText) > 2000 {
			errText = errText[:2000]
		}
	}
	_, _ = db.ExecContext(ctx, `
		UPDATE job_runs SET last_finished_at = $2, last_outcome = $3, last_error = $4, rows_affected = $5,
		       last_success_at = CASE WHEN $3 = 'ok' THEN $2 ELSE last_success_at END
		WHERE job = $1`, job, at, outcome, errText, rows)
}

// lastJobSuccess returns the zero time when the job never succeeded.
func lastJobSuccess(ctx context.Context, db *sql.DB, job string) (time.Time, error) {
	var t sql.NullTime
	err := db.QueryRowContext(ctx, `SELECT last_success_at FROM job_runs WHERE job = $1`, job).Scan(&t)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, err
	}
	if !t.Valid {
		return time.Time{}, nil
	}
	return t.Time, nil
}

// handleRetentionTrigger runs the retention job once on demand (admin).
// A run already holding the lock (ticker or another admin) answers 409.
func (s *Server) handleRetentionTrigger(w http.ResponseWriter, r *http.Request) {
	if s.retention == nil {
		http.Error(w, "retention not available", http.StatusServiceUnavailable)
		return
	}
	ctx, cancel := adminTriggerContext(r)
	defer cancel()
	res, err := s.retention.RunOnce(ctx)
	if errors.Is(err, errJobAlreadyRunning) {
		writeJobAlreadyRunning(w, "retention")
		return
	}
	actor, _ := r.Context().Value(userIDKey).(string)
	audit.Record(r.Context(), s.db, audit.Entry{UserID: actor, EventType: "admin", Action: "admin.retention", Error: err,
		Metadata: map[string]any{"rows": res.Total, "tables": res.Tables}})
	if err != nil {
		s.logger.Error("manual retention run failed", zap.Error(err))
		http.Error(w, "retention failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(res)
}
