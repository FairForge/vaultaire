package api

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"hash/fnv"
	"sort"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"
)

// The background-job scheduler (WP-R13-3; Review R13-07 / R13-11 / R13-23).
//
// Before this file every job had its own loop. Dedup GC and Smart demotion
// were plain 24 h tickers with no run at boot: on a box that is redeployed
// several times a day the first tick never came, so neither had ever run on
// prod on its own. Inventory was an hourly tick with an hour-0 gate (a
// restart during hour 0 skipped or doubled a day). Retention and account
// deletion had the right shape — written twice. None of the hourly loops
// said whether it had run.
//
// One runner now: a job is DAILY (due once per day at HH:MM UTC; a catch-up
// check a short delay after boot, then one check per hour — a run happens
// whenever job_runs has no success since the most recent HH:MM) or an
// INTERVAL job (one run a short delay after boot, then one per interval).
// Every run, of either kind:
//
//   - holds ONE pg advisory lock per job (session-level, on a dedicated
//     connection, released when the connection closes — so a crash never
//     leaves it held). The scheduler, an admin trigger and a second process
//     on the same database cannot overlap; the loser gets
//     errJobAlreadyRunning and is not queued.
//   - writes job_runs: last_started_at + 'running' at the start;
//     last_finished_at, the outcome, rows and — on "ok" — last_success_at at
//     the end.
//   - counts vaultaire_job_runs_total{job_name,outcome}.
//
// Outcomes: "ok" (the run did its pass; failures of single objects, tenants,
// chunks or reports are a note in last_error and do NOT make the run an
// error — a daily job is not repeated for them), "error" (the run itself
// failed; last_success_at stays put and a daily job is retried at the next
// hourly check), "interrupted" (the process was stopping).
//
// vaultaire_job_last_success_timestamp_seconds{job_name} is read from
// job_runs, not kept in memory: a freshly started process reports the last
// success of the process before it (the old per-job gauges read 0 after
// every deploy and made a staleness rule true until the next nightly run).
//
// The label is job_name, NOT job: `job` is the label Prometheus itself puts
// on every scraped series (the scrape job — "vaultaire" on prod). With the
// default honor_labels=false a series that brings its own `job` is stored as
// `exported_job`, and a rule selecting {job="retention"} matches nothing —
// a staleness rule that can never fire.

const (
	jobOutcomeOK          = "ok"
	jobOutcomeError       = "error"
	jobOutcomeInterrupted = "interrupted"
	jobOutcomeRunning     = "running"
)

// jobRecheck is how often a daily job re-checks whether it is due.
const jobRecheck = time.Hour

// jobLastSuccessTTL is how long the last-success collector trusts its read
// of job_runs.
const jobLastSuccessTTL = 15 * time.Second

var jobRuns = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "vaultaire_job_runs_total",
	Help: "Background job runs in this process, by job and outcome (ok, error, interrupted).",
}, []string{"job_name", "outcome"})

// jobReport is what one run hands back to the scheduler.
type jobReport struct {
	// Rows is recorded as job_runs.rows_affected (rows pruned, objects
	// moved, chunks swept, reports written — the job's own unit).
	Rows int64
	// Note is recorded in job_runs.last_error with outcome "ok": per-item
	// failures and anything else worth reading that did not fail the run.
	Note string
}

// jobSpec describes one job.
type jobSpec struct {
	Name string
	// Hour/Minute: a DAILY job, due at this UTC wall-clock time. Ignored
	// when Every is set.
	Hour, Minute int
	// Every: an INTERVAL job — one run BootDelay after start, then one per
	// Every.
	Every time.Duration
	// BootDelay is how long after Start the first check waits.
	BootDelay time.Duration
	// MaxRunTime bounds one run (0 = unbounded).
	MaxRunTime time.Duration
	// Run does one run. A returned error makes the outcome "error".
	Run func(ctx context.Context) (jobReport, error)
}

func (sp jobSpec) daily() bool { return sp.Every <= 0 }

func (sp jobSpec) schedule() string {
	if sp.daily() {
		return fmt.Sprintf("daily %02d:%02d UTC", sp.Hour, sp.Minute)
	}
	return "every " + sp.Every.String()
}

// scheduledJob is a registered job.
type scheduledJob struct {
	spec  jobSpec
	sched *jobScheduler
}

// jobScheduler runs the registered jobs.
type jobScheduler struct {
	db     *sql.DB
	logger *zap.Logger
	now    func() time.Time
	// recheck is the daily jobs' re-check interval (jobRecheck; tests shorten it).
	recheck time.Duration

	mu      sync.Mutex
	jobs    map[string]*scheduledJob
	order   []string
	base    context.Context // the Start context; detached runs stop with it
	started bool

	cacheMu   sync.Mutex
	cacheAt   time.Time
	cacheRows map[string]time.Time
}

// newJobScheduler builds the scheduler; nil without a database (jobs need
// job_runs and the advisory lock).
func newJobScheduler(db *sql.DB, logger *zap.Logger) *jobScheduler {
	if db == nil {
		return nil
	}
	if logger == nil {
		logger = zap.NewNop()
	}
	return &jobScheduler{db: db, logger: logger, now: time.Now, recheck: jobRecheck,
		jobs: map[string]*scheduledJob{}, base: context.Background()}
}

// Register adds a job. Nil-safe: a nil scheduler returns a nil job, and
// every method of a nil job is a no-op that reports "not configured".
// Registering after Start launches the job's loop at once; registering a
// name twice keeps the first.
func (s *jobScheduler) Register(spec jobSpec) *scheduledJob {
	if s == nil || spec.Run == nil || spec.Name == "" {
		return nil
	}
	j := &scheduledJob{spec: spec, sched: s}
	// Every outcome starts at 0, so the first failure of a new process is an
	// increase a rule can see (a series that first appears at 1 is not).
	for _, o := range []string{jobOutcomeOK, jobOutcomeError, jobOutcomeInterrupted} {
		jobRuns.WithLabelValues(spec.Name, o)
	}
	s.mu.Lock()
	if existing, dup := s.jobs[spec.Name]; dup {
		// One job per name: a second registration would start a second loop
		// for the same lock and the same job_runs row.
		s.mu.Unlock()
		s.logger.Warn("job registered twice; keeping the first", zap.String("job", spec.Name))
		return existing
	}
	s.order = append(s.order, spec.Name)
	s.jobs[spec.Name] = j
	started, base := s.started, s.base
	s.mu.Unlock()
	if started {
		go j.loop(base)
	}
	return j
}

// job returns a registered job (nil when unknown).
func (s *jobScheduler) job(name string) *scheduledJob {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.jobs[name]
}

// names lists the registered jobs in registration order.
func (s *jobScheduler) names() []string {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.order...)
}

// Start launches one loop per registered job; they stop when ctx is done.
func (s *jobScheduler) Start(ctx context.Context) {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return
	}
	s.started = true
	s.base = ctx
	jobs := make([]*scheduledJob, 0, len(s.order))
	for _, n := range s.order {
		jobs = append(jobs, s.jobs[n])
	}
	s.mu.Unlock()
	for _, j := range jobs {
		go j.loop(ctx)
	}
}

// loop is one job's schedule.
func (j *scheduledJob) loop(ctx context.Context) {
	// A row still marked 'running' belongs to a run the previous process
	// did not finish (a deploy stopped it): say so at once, instead of
	// showing a run that is not running until the job's next start.
	j.markInterrupted(ctx)
	select {
	case <-ctx.Done():
		return
	case <-time.After(j.spec.BootDelay):
	}
	every := j.sched.recheck
	if !j.spec.daily() {
		every = j.spec.Every
	}
	j.tick(ctx)
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			j.tick(ctx)
		}
	}
}

// tick runs the job if it is owed: always for an interval job, when no
// success is recorded since the last scheduled time for a daily one.
func (j *scheduledJob) tick(ctx context.Context) {
	if j.spec.daily() {
		due, err := j.due(ctx)
		if err != nil {
			j.sched.logger.Warn("job: could not read job_runs", zap.String("job", j.spec.Name), zap.Error(err))
			return
		}
		if !due {
			return
		}
	}
	if _, err := j.RunNow(ctx); errors.Is(err, errJobAlreadyRunning) {
		j.sched.logger.Warn("job: run skipped, another run holds the lock", zap.String("job", j.spec.Name))
	}
}

// lastScheduled is the most recent daily run time at or before now.
func (j *scheduledJob) lastScheduled(now time.Time) time.Time {
	now = now.UTC()
	at := time.Date(now.Year(), now.Month(), now.Day(), j.spec.Hour, j.spec.Minute, 0, 0, time.UTC)
	if at.After(now) {
		at = at.Add(-24 * time.Hour)
	}
	return at
}

// nextRun is when the job is next expected to run (zero when unknown).
func (j *scheduledJob) nextRun(now, lastStart time.Time) time.Time {
	if j.spec.daily() {
		return j.lastScheduled(now).Add(24 * time.Hour)
	}
	if lastStart.IsZero() {
		return time.Time{}
	}
	return lastStart.Add(j.spec.Every)
}

// due reports whether a daily run is owed: no recorded success, or the last
// one predates the most recent scheduled time.
func (j *scheduledJob) due(ctx context.Context) (bool, error) {
	last, err := lastJobSuccess(ctx, j.sched.db, j.spec.Name)
	if err != nil {
		return false, err
	}
	return last.IsZero() || last.Before(j.lastScheduled(j.sched.now())), nil
}

// jobLockKey is the advisory-lock key of a job: stable across processes.
func jobLockKey(name string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte("vaultaire.job." + name))
	return int64(h.Sum64() & 0x7fffffffffffffff)
}

// acquire takes the job's advisory lock without blocking. The lock is
// session-level on a dedicated connection: closing the connection releases
// it, so a crash never leaves it held (the pool would otherwise hand the
// session back with the lock still on it).
func (j *scheduledJob) acquire(ctx context.Context) (release func(), err error) {
	conn, err := j.sched.db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("job %s: acquire conn: %w", j.spec.Name, err)
	}
	key := jobLockKey(j.spec.Name)
	var locked bool
	if err := conn.QueryRowContext(ctx, `SELECT pg_try_advisory_lock($1)`, key).Scan(&locked); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("job %s: advisory lock: %w", j.spec.Name, err)
	}
	if !locked {
		_ = conn.Close()
		return nil, errJobAlreadyRunning
	}
	return func() {
		uctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, _ = conn.ExecContext(uctx, `SELECT pg_advisory_unlock($1)`, key)
		cancel()
		_ = conn.Close()
	}, nil
}

// RunNow does one recorded run under the job's lock and waits for it.
// errJobAlreadyRunning when another run holds the lock.
func (j *scheduledJob) RunNow(ctx context.Context) (jobReport, error) {
	if j == nil {
		return jobReport{}, errors.New("job not configured")
	}
	return j.runWith(ctx, j.spec.Run)
}

// runWith is RunNow with the work supplied by the caller: the same lock,
// job_runs row and counters, for a caller that needs the work's own typed
// result (it must be the job's work).
func (j *scheduledJob) runWith(ctx context.Context, run func(context.Context) (jobReport, error)) (jobReport, error) {
	release, err := j.acquire(ctx)
	if err != nil {
		return jobReport{}, err
	}
	return j.execute(ctx, release, run)
}

// StartDetached takes the lock now — so the caller can answer 409 at once —
// and runs in the background on the scheduler's context: a closed request
// cannot cancel it, a shutdown does. done (optional) is called with the
// result when the run ends.
func (j *scheduledJob) StartDetached(ctx context.Context, done func(jobReport, error)) error {
	if j == nil {
		return errors.New("job not configured")
	}
	release, err := j.acquire(ctx)
	if err != nil {
		return err
	}
	j.sched.mu.Lock()
	base := j.sched.base
	j.sched.mu.Unlock()
	go func() {
		rep, runErr := j.execute(base, release, j.spec.Run)
		if done != nil {
			done(rep, runErr)
		}
	}()
	return nil
}

// WithLock runs fn under the job's lock WITHOUT recording a run: for a dry
// run, which must not overlap a real one (its report would describe a
// moving target) and must not count as the day's success.
func (j *scheduledJob) WithLock(ctx context.Context, fn func(ctx context.Context) error) error {
	if j == nil {
		return errors.New("job not configured")
	}
	release, err := j.acquire(ctx)
	if err != nil {
		return err
	}
	defer release()
	return fn(ctx)
}

// execute is one recorded run. The lock is held by the caller and released
// here.
func (j *scheduledJob) execute(ctx context.Context, release func(), run func(context.Context) (jobReport, error)) (rep jobReport, err error) {
	defer release()
	s := j.sched
	name := j.spec.Name
	start := s.now()
	recordJobStart(ctx, s.db, name, start)

	runCtx, cancel := ctx, context.CancelFunc(func() {})
	if j.spec.MaxRunTime > 0 {
		runCtx, cancel = context.WithTimeout(ctx, j.spec.MaxRunTime)
	}
	func() {
		defer cancel()
		defer func() {
			// A job bug must not take the server down with it.
			if p := recover(); p != nil {
				err = fmt.Errorf("panic: %v", p)
			}
		}()
		rep, err = run(runCtx)
	}()

	outcome, text := jobOutcomeOK, rep.Note
	switch {
	case errors.Is(ctx.Err(), context.Canceled):
		// The scheduler's own context ended: the process is stopping. Not
		// a failure of the job, and never the day's success either, whatever
		// the run returned — a job that treats its items' failures as notes
		// reports a pass the shutdown cut short as done. The next boot's
		// catch-up runs it again.
		outcome, text = jobOutcomeInterrupted, "the process stopped during this run"
		if err != nil {
			text += ": " + err.Error()
		} else if rep.Note != "" {
			text += ": " + rep.Note
		}
	case err == nil:
	default:
		outcome, text = jobOutcomeError, err.Error()
	}
	finished := s.now()
	recordJobFinish(s.db, name, finished, outcome, text, rep.Rows)
	jobRuns.WithLabelValues(name, outcome).Inc()
	s.invalidateLastSuccess()

	fields := []zap.Field{zap.String("job", name), zap.String("outcome", outcome),
		zap.Int64("rows", rep.Rows), zap.Duration("duration", finished.Sub(start))}
	if text != "" {
		fields = append(fields, zap.String("detail", text))
	}
	switch outcome {
	case jobOutcomeOK:
		if rep.Rows > 0 || rep.Note != "" || j.spec.daily() {
			s.logger.Info("job run completed", fields...)
		} else {
			s.logger.Debug("job run completed", fields...)
		}
	case jobOutcomeInterrupted:
		s.logger.Warn("job run interrupted", fields...)
	default:
		s.logger.Error("job run failed", fields...)
	}
	return rep, err
}

// markInterrupted closes a row the previous process left 'running'. It
// takes the job's lock first: a row that is 'running' while the lock is
// held belongs to a live run (another process) and is left alone.
func (j *scheduledJob) markInterrupted(ctx context.Context) {
	release, err := j.acquire(ctx)
	if err != nil {
		return
	}
	defer release()
	res, err := j.sched.db.ExecContext(ctx, `
		UPDATE job_runs
		   SET last_outcome = $2, last_finished_at = $3,
		       last_error = 'the process stopped during this run (found at the next start)'
		 WHERE job = $1 AND last_outcome = $4`,
		j.spec.Name, jobOutcomeInterrupted, j.sched.now(), jobOutcomeRunning)
	if err != nil {
		return
	}
	if n, _ := res.RowsAffected(); n > 0 {
		j.sched.logger.Warn("job: the previous process stopped during a run", zap.String("job", j.spec.Name))
	}
}

// --- job_runs ---

func recordJobStart(ctx context.Context, db *sql.DB, job string, at time.Time) {
	_, _ = db.ExecContext(ctx, `
		INSERT INTO job_runs (job, last_started_at, last_outcome) VALUES ($1, $2, 'running')
		ON CONFLICT (job) DO UPDATE SET last_started_at = EXCLUDED.last_started_at, last_outcome = 'running', last_error = ''`,
		job, at)
}

// recordJobFinish writes the end of a run on its own short context: the run's
// context may already be cancelled (deadline, shutdown) and the row must
// still say how the run ended.
func recordJobFinish(db *sql.DB, job string, at time.Time, outcome, text string, rows int64) {
	if len(text) > 2000 {
		text = text[:2000]
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// An upsert: when the start row could not be written (a database blip)
	// the finish must still record the success — an UPDATE of a missing row
	// would leave a daily job "never succeeded" and it would run every hour.
	_, _ = db.ExecContext(ctx, `
		INSERT INTO job_runs (job, last_started_at, last_finished_at, last_outcome, last_error, rows_affected, last_success_at)
		VALUES ($1, $2, $2, $3, $4, $5, CASE WHEN $3 = 'ok' THEN $2::timestamptz END)
		ON CONFLICT (job) DO UPDATE SET
		       last_finished_at = EXCLUDED.last_finished_at, last_outcome = EXCLUDED.last_outcome,
		       last_error = EXCLUDED.last_error, rows_affected = EXCLUDED.rows_affected,
		       last_success_at = COALESCE(EXCLUDED.last_success_at, job_runs.last_success_at)`,
		job, at, outcome, text, rows)
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

// jobRunRow is one job_runs row.
type jobRunRow struct {
	Job          string
	LastStarted  sql.NullTime
	LastFinished sql.NullTime
	LastSuccess  sql.NullTime
	Outcome      string
	Error        string
	Rows         int64
}

// jobRunRows reads job_runs.
func jobRunRows(ctx context.Context, db *sql.DB) (map[string]jobRunRow, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT job, last_started_at, last_finished_at, last_success_at, last_outcome, last_error, rows_affected
		  FROM job_runs`)
	if err != nil {
		return nil, fmt.Errorf("read job_runs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]jobRunRow{}
	for rows.Next() {
		var r jobRunRow
		if err := rows.Scan(&r.Job, &r.LastStarted, &r.LastFinished, &r.LastSuccess, &r.Outcome, &r.Error, &r.Rows); err != nil {
			return nil, fmt.Errorf("scan job_runs: %w", err)
		}
		out[r.Job] = r
	}
	return out, rows.Err()
}

// --- vaultaire_job_last_success_timestamp_seconds ---

// lastSuccesses returns the last success of every REGISTERED job, read from
// job_runs at most once per jobLastSuccessTTL. A job that has never
// succeeded maps to the zero time. ok is false when job_runs has never been
// read successfully (database down since boot): the collector then exports
// nothing rather than a zero that a staleness rule would read as "never".
// A failed refresh keeps the previous read.
func (s *jobScheduler) lastSuccesses() (map[string]time.Time, bool) {
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	if s.cacheRows == nil || time.Since(s.cacheAt) > jobLastSuccessTTL {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		rows, err := jobRunRows(ctx, s.db)
		cancel()
		if err == nil {
			fresh := make(map[string]time.Time, len(rows))
			for name, r := range rows {
				if r.LastSuccess.Valid {
					fresh[name] = r.LastSuccess.Time
				}
			}
			s.cacheRows = fresh
		} else {
			s.logger.Warn("job metrics: could not read job_runs", zap.Error(err))
		}
		// Stamped on failure too: with the database down every scrape would
		// otherwise wait out the query timeout behind this mutex.
		s.cacheAt = time.Now()
	}
	if s.cacheRows == nil {
		return nil, false
	}
	out := map[string]time.Time{}
	for _, name := range s.names() {
		out[name] = s.cacheRows[name]
	}
	return out, true
}

// invalidateLastSuccess makes the next scrape re-read job_runs (a run just
// finished).
func (s *jobScheduler) invalidateLastSuccess() {
	s.cacheMu.Lock()
	s.cacheAt = time.Time{}
	s.cacheMu.Unlock()
}

type jobsCollector struct {
	s    *jobScheduler
	desc *prometheus.Desc
}

func newJobsCollector(s *jobScheduler) *jobsCollector {
	return &jobsCollector{s: s, desc: prometheus.NewDesc("vaultaire_job_last_success_timestamp_seconds",
		"Unix time of the job's last successful run, read from job_runs (0 = the job has never succeeded).",
		[]string{"job_name"}, nil)}
}

func (c *jobsCollector) Describe(ch chan<- *prometheus.Desc) { ch <- c.desc }

func (c *jobsCollector) Collect(ch chan<- prometheus.Metric) {
	last, ok := c.s.lastSuccesses()
	if !ok {
		return
	}
	names := make([]string, 0, len(last))
	for n := range last {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		v := 0.0
		if t := last[n]; !t.IsZero() {
			v = float64(t.Unix())
		}
		ch <- prometheus.MustNewConstMetric(c.desc, prometheus.GaugeValue, v, n)
	}
}
