package api

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

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
// Schedule: a daily job of the shared scheduler (jobs.go, WP-R13-3) — 03:30
// UTC with a catch-up at boot when job_runs has no success since the last
// 03:30 (a plain 24 h ticker never fires on a box that is redeployed daily —
// R13-07), one advisory lock per run so the scheduler and the admin trigger
// cannot overlap. The work: batched
// `DELETE … WHERE ctid = ANY(ARRAY(SELECT ctid … LIMIT n))` loops per table
// so a first run over months of backlog never takes one long lock; a metric
// per table.

// retentionJobName is the job's name in job_runs and on the metrics.
const retentionJobName = "retention"

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

// retentionDeletedRows is the job's own series; runs and the last success
// are the scheduler's (vaultaire_job_runs_total{job_name="retention"},
// vaultaire_job_last_success_timestamp_seconds{job_name="retention"}).
var retentionDeletedRows = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "vaultaire_retention_deleted_rows_total",
	Help: "Rows deleted (or PII-scrubbed) by the retention job, by table.",
}, []string{"table"})

// RetentionJob is the nightly log-table pruner.
type RetentionJob struct {
	db     *sql.DB
	logger *zap.Logger

	Policies  []retentionPolicy
	BatchSize int
	// MaxRunTime bounds one run.
	MaxRunTime time.Duration
	// JobName is the job_runs name (tests use their own: the table is shared).
	JobName string

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
		Policies:   defaultRetentionPolicies(),
		BatchSize:  5000,
		MaxRunTime: 10 * time.Minute,
		JobName:    retentionJobName,
		now:        time.Now,
	}
}

// spec is the job's schedule: daily at 03:30 UTC (after the 03:00 database
// backup), catch-up one minute after boot.
//
// What makes a run fail: any table that could not be pruned (a statement
// error, the run deadline). The run is then retried at every hourly check —
// the statements are idempotent and cheap, and a table that is never pruned
// is a privacy-policy breach worth repeating for.
func (j *RetentionJob) spec() jobSpec {
	return jobSpec{
		Name: j.JobName, Hour: 3, Minute: 30,
		BootDelay: time.Minute, MaxRunTime: j.MaxRunTime,
		Run: func(ctx context.Context) (jobReport, error) {
			res, err := j.RunOnce(ctx)
			if len(res.Tables) > 0 {
				j.logger.Info("retention run", zap.Int64("rows", res.Total),
					zap.String("duration", res.Duration), zap.Any("tables", res.Tables), zap.Strings("errors", res.Errors))
			}
			return jobReport{Rows: res.Total}, err
		},
	}
}

// RunOnce prunes every policy. Per-table errors are collected and the next
// table continues; the returned error is non-nil when any table failed.
// Locking and job_runs are the scheduler's (run it through the registered
// job; a bare RunOnce is for tests of the pruning itself).
func (j *RetentionJob) RunOnce(ctx context.Context) (RetentionResult, error) {
	res := RetentionResult{Tables: map[string]int64{}}
	if j == nil {
		return res, errors.New("retention: job not configured")
	}
	start := j.now()
	for _, p := range j.Policies {
		if ctx.Err() != nil {
			res.Errors = append(res.Errors, "run deadline reached before "+p.Table)
			break
		}
		n, err := j.prune(ctx, p)
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
	if len(res.Errors) > 0 {
		return res, errors.New(strings.Join(res.Errors, "; "))
	}
	return res, nil
}

// prune applies one policy in batches until a batch comes back short.
func (j *RetentionJob) prune(ctx context.Context, p retentionPolicy) (int64, error) {
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
		res, err := j.db.ExecContext(ctx, stmt, cutoff, j.BatchSize)
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
