package api

import (
	"context"
	"database/sql"
	"errors"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	dbtestutil "github.com/FairForge/vaultaire/internal/testutil"
	"github.com/google/uuid"
	promtestutil "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// WP-R13-3: the one scheduler. job_runs rows are global and the test
// database is shared by every package, so every test here uses its own job
// name (and therefore its own advisory lock).

type jobsFixture struct {
	t     *testing.T
	db    *sql.DB
	sched *jobScheduler
	now   time.Time
}

func setupJobsFixture(t *testing.T) *jobsFixture {
	t.Helper()
	db, err := sql.Open("postgres", dbtestutil.DSN())
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	require.NoError(t, db.Ping())
	f := &jobsFixture{t: t, db: db, sched: newJobScheduler(db, zap.NewNop()),
		now: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)}
	f.sched.now = func() time.Time { return f.now }
	return f
}

// name returns a job name no other test uses, and removes its row afterwards.
func (f *jobsFixture) name(prefix string) string {
	n := "test_" + prefix + "_" + uuid.New().String()[:8]
	f.t.Cleanup(func() { _, _ = f.db.Exec(`DELETE FROM job_runs WHERE job = $1`, n) })
	return n
}

func (f *jobsFixture) row(job string) jobRunRow {
	f.t.Helper()
	rows, err := jobRunRows(context.Background(), f.db)
	require.NoError(f.t, err)
	return rows[job]
}

func counterValue(job, outcome string) float64 {
	return promtestutil.ToFloat64(jobRuns.WithLabelValues(job, outcome))
}

func TestJob_DailyDueFollowsTheScheduleAndCatchesUp(t *testing.T) {
	// Arrange: a daily 03:30 job; the clock says noon on October 1st.
	f := setupJobsFixture(t)
	name := f.name("daily")
	j := f.sched.Register(jobSpec{Name: name, Hour: 3, Minute: 30,
		Run: func(context.Context) (jobReport, error) { return jobReport{}, nil }})

	// Assert the schedule arithmetic.
	assert.Equal(t, time.Date(2026, 10, 1, 3, 30, 0, 0, time.UTC), j.lastScheduled(f.now))
	assert.Equal(t, time.Date(2026, 9, 30, 3, 30, 0, 0, time.UTC), j.lastScheduled(time.Date(2026, 10, 1, 1, 0, 0, 0, time.UTC)))

	// No row yet → due (the first deploy, or a table that was emptied).
	due, err := j.due(context.Background())
	require.NoError(t, err)
	assert.True(t, due, "never run → due")

	for _, tc := range []struct {
		last time.Time
		due  bool
		why  string
	}{
		{time.Date(2026, 10, 1, 3, 31, 0, 0, time.UTC), false, "succeeded after today's 03:30"},
		{time.Date(2026, 9, 30, 3, 31, 0, 0, time.UTC), true, "last success was yesterday's run — the catch-up a 24 h ticker never did"},
		{time.Date(2026, 10, 1, 3, 29, 0, 0, time.UTC), true, "a manual run one minute before the schedule does not replace it"},
	} {
		_, err := f.db.Exec(`INSERT INTO job_runs (job, last_success_at, last_outcome) VALUES ($1, $2, 'ok')
			ON CONFLICT (job) DO UPDATE SET last_success_at = EXCLUDED.last_success_at`, name, tc.last)
		require.NoError(t, err)
		due, err := j.due(context.Background())
		require.NoError(t, err)
		assert.Equal(t, tc.due, due, tc.why)
	}
}

func TestJob_RunRecordsStartFinishSuccessAndCounts(t *testing.T) {
	// Arrange
	f := setupJobsFixture(t)
	name := f.name("ok")
	var sawRunning string
	j := f.sched.Register(jobSpec{Name: name, Hour: 3, Minute: 30, Run: func(context.Context) (jobReport, error) {
		sawRunning = f.row(name).Outcome
		return jobReport{Rows: 7}, nil
	}})
	before := counterValue(name, jobOutcomeOK)

	// Act
	rep, err := j.RunNow(context.Background())

	// Assert
	require.NoError(t, err)
	assert.Equal(t, int64(7), rep.Rows)
	assert.Equal(t, jobOutcomeRunning, sawRunning, "the row says 'running' while the run is in progress")
	row := f.row(name)
	assert.Equal(t, jobOutcomeOK, row.Outcome)
	assert.Equal(t, int64(7), row.Rows)
	assert.Empty(t, row.Error)
	require.True(t, row.LastSuccess.Valid)
	assert.True(t, row.LastSuccess.Time.Equal(f.now), "last_success_at = the finish time")
	assert.True(t, row.LastStarted.Valid && row.LastFinished.Valid)
	assert.Equal(t, before+1, counterValue(name, jobOutcomeOK))
	due, err := j.due(context.Background())
	require.NoError(t, err)
	assert.False(t, due, "a success after today's schedule time: not owed again")
}

func TestJob_PerItemFailuresAreANoteNotAnError(t *testing.T) {
	// Arrange: a run that could not move two objects but did its pass.
	f := setupJobsFixture(t)
	name := f.name("note")
	runs := 0
	j := f.sched.Register(jobSpec{Name: name, Hour: 3, Minute: 30, Run: func(context.Context) (jobReport, error) {
		runs++
		return jobReport{Rows: 40, Note: "2 objects not moved: backend timeout"}, nil
	}})

	// Act: the scheduler's own path — the first check, then the hourly re-check.
	j.tick(context.Background())
	j.tick(context.Background())

	// Assert: one run; it is "ok" with the failures written down, and it is
	// not repeated an hour later.
	assert.Equal(t, 1, runs, "a run with per-item failures must not repeat every hour")
	row := f.row(name)
	assert.Equal(t, jobOutcomeOK, row.Outcome)
	assert.Equal(t, "2 objects not moved: backend timeout", row.Error)
	assert.True(t, row.LastSuccess.Valid)
}

func TestJob_ErrorKeepsLastSuccessAndIsRetriedAtTheNextCheck(t *testing.T) {
	// Arrange: yesterday's success on record; today the run fails twice,
	// then works.
	f := setupJobsFixture(t)
	name := f.name("err")
	yesterday := time.Date(2026, 9, 30, 3, 31, 0, 0, time.UTC)
	_, err := f.db.Exec(`INSERT INTO job_runs (job, last_success_at, last_outcome) VALUES ($1, $2, 'ok')`, name, yesterday)
	require.NoError(t, err)
	runs := 0
	j := f.sched.Register(jobSpec{Name: name, Hour: 3, Minute: 30, Run: func(context.Context) (jobReport, error) {
		runs++
		if runs < 3 {
			return jobReport{Rows: 1}, errors.New("could not list tenants")
		}
		return jobReport{}, nil
	}})
	errsBefore := counterValue(name, jobOutcomeError)

	// Act + Assert: first check fails.
	j.tick(context.Background())
	row := f.row(name)
	assert.Equal(t, jobOutcomeError, row.Outcome)
	assert.Equal(t, "could not list tenants", row.Error)
	require.True(t, row.LastSuccess.Valid)
	assert.True(t, row.LastSuccess.Time.Equal(yesterday), "a failed run must not move last_success_at")

	// The hourly re-checks retry until it works, then stop.
	j.tick(context.Background())
	j.tick(context.Background())
	j.tick(context.Background())
	assert.Equal(t, 3, runs, "retried while failing, not after the success")
	assert.Equal(t, errsBefore+2, counterValue(name, jobOutcomeError))
	assert.Equal(t, jobOutcomeOK, f.row(name).Outcome)
	assert.Empty(t, f.row(name).Error, "a success clears the error text")
}

func TestJob_APanicIsAnErrorNotACrash(t *testing.T) {
	f := setupJobsFixture(t)
	name := f.name("panic")
	j := f.sched.Register(jobSpec{Name: name, Every: time.Hour, Run: func(context.Context) (jobReport, error) {
		panic("a bug in the job")
	}})

	_, err := j.RunNow(context.Background())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "panic")
	assert.Equal(t, jobOutcomeError, f.row(name).Outcome)
}

func TestJob_OneRunAtATime_InProcessAndAcrossProcesses(t *testing.T) {
	// Arrange: a run that blocks until released.
	f := setupJobsFixture(t)
	name := f.name("lock")
	entered, release := make(chan struct{}), make(chan struct{})
	var runs atomic.Int32
	j := f.sched.Register(jobSpec{Name: name, Hour: 3, Minute: 30, Run: func(context.Context) (jobReport, error) {
		runs.Add(1)
		close(entered)
		<-release
		return jobReport{}, nil
	}})
	done := make(chan error, 1)
	go func() { _, err := j.RunNow(context.Background()); done <- err }()
	<-entered

	// Act + Assert: the scheduler's check, an admin trigger and a dry run
	// all find the lock held — refused, never queued.
	_, err := j.RunNow(context.Background())
	assert.ErrorIs(t, err, errJobAlreadyRunning)
	assert.ErrorIs(t, j.StartDetached(context.Background(), nil), errJobAlreadyRunning)
	assert.ErrorIs(t, j.WithLock(context.Background(), func(context.Context) error { return nil }), errJobAlreadyRunning)
	j.tick(context.Background()) // logs and returns
	close(release)
	require.NoError(t, <-done)
	assert.Equal(t, int32(1), runs.Load())

	// A second PROCESS on the same database (another session holding the
	// job's advisory lock) is the same refusal.
	conn, err := f.db.Conn(context.Background())
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	var got bool
	require.NoError(t, conn.QueryRowContext(context.Background(), `SELECT pg_try_advisory_lock($1)`, jobLockKey(name)).Scan(&got))
	require.True(t, got)
	_, err = j.RunNow(context.Background())
	assert.ErrorIs(t, err, errJobAlreadyRunning)
	_, _ = conn.ExecContext(context.Background(), `SELECT pg_advisory_unlock($1)`, jobLockKey(name))
	assert.Equal(t, int32(1), runs.Load(), "refused runs never start the work")
}

func TestJob_WithLockRecordsNothing(t *testing.T) {
	// A dry run holds the lock and must not count as the day's success.
	f := setupJobsFixture(t)
	name := f.name("dry")
	j := f.sched.Register(jobSpec{Name: name, Hour: 3, Minute: 30,
		Run: func(context.Context) (jobReport, error) { return jobReport{}, nil }})

	ran := false
	require.NoError(t, j.WithLock(context.Background(), func(context.Context) error { ran = true; return nil }))

	assert.True(t, ran)
	assert.Empty(t, f.row(name).Job, "no job_runs row")
	due, err := j.due(context.Background())
	require.NoError(t, err)
	assert.True(t, due)
}

func TestJob_KilledMidRun_NextBootSaysInterruptedAndRunsItAgain(t *testing.T) {
	// Arrange: the previous process was stopped by a deploy in the middle of
	// today's run — the row says 'running', there is no success for today,
	// and (the process being dead) nobody holds the lock.
	f := setupJobsFixture(t)
	name := f.name("killed")
	started := f.now.Add(-10 * time.Minute)
	_, err := f.db.Exec(`INSERT INTO job_runs (job, last_started_at, last_success_at, last_outcome)
		VALUES ($1, $2, $3, 'running')`, name, started, time.Date(2026, 9, 30, 3, 31, 0, 0, time.UTC))
	require.NoError(t, err)
	ran := make(chan string, 1)
	f.sched.Register(jobSpec{Name: name, Hour: 3, Minute: 30, Run: func(context.Context) (jobReport, error) {
		ran <- f.row(name).Outcome
		return jobReport{Rows: 3}, nil
	}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Act: the next boot.
	f.sched.Start(ctx)

	// Assert: the catch-up runs the job again; the stale 'running' was
	// closed as 'interrupted' before it did.
	select {
	case <-ran:
	case <-time.After(10 * time.Second):
		t.Fatal("the boot catch-up did not run the job")
	}
	require.Eventually(t, func() bool { return f.row(name).Outcome == jobOutcomeOK }, 5*time.Second, 10*time.Millisecond)
	assert.Equal(t, int64(3), f.row(name).Rows)
}

func TestJob_StaleRunningRowIsClosedEvenWhenTheJobIsNotDue(t *testing.T) {
	// Arrange: today's scheduled run succeeded; a later manual run was cut
	// short by a deploy. The job is not due, so nothing would overwrite the
	// 'running' row until tomorrow.
	f := setupJobsFixture(t)
	name := f.name("stale")
	_, err := f.db.Exec(`INSERT INTO job_runs (job, last_started_at, last_success_at, last_outcome)
		VALUES ($1, $2, $3, 'running')`, name, f.now.Add(-time.Minute), time.Date(2026, 10, 1, 3, 31, 0, 0, time.UTC))
	require.NoError(t, err)
	runs := 0
	j := f.sched.Register(jobSpec{Name: name, Hour: 3, Minute: 30,
		Run: func(context.Context) (jobReport, error) { runs++; return jobReport{}, nil }})

	// Act
	j.markInterrupted(context.Background())
	j.tick(context.Background())

	// Assert
	row := f.row(name)
	assert.Equal(t, jobOutcomeInterrupted, row.Outcome)
	assert.Contains(t, row.Error, "the process stopped")
	assert.Equal(t, 0, runs, "not due: no run")

	// A row that is 'running' while its lock IS held belongs to a live run
	// and is left alone.
	_, err = f.db.Exec(`UPDATE job_runs SET last_outcome = 'running' WHERE job = $1`, name)
	require.NoError(t, err)
	conn, err := f.db.Conn(context.Background())
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	var got bool
	require.NoError(t, conn.QueryRowContext(context.Background(), `SELECT pg_try_advisory_lock($1)`, jobLockKey(name)).Scan(&got))
	require.True(t, got)
	j.markInterrupted(context.Background())
	assert.Equal(t, jobOutcomeRunning, f.row(name).Outcome)
}

func TestJob_ShutdownDuringARunIsInterruptedNotAnError(t *testing.T) {
	// Arrange: a run that honours its context, and a scheduler context that
	// is cancelled (Shutdown) while the run is in progress.
	f := setupJobsFixture(t)
	name := f.name("shutdown")
	entered := make(chan struct{})
	j := f.sched.Register(jobSpec{Name: name, Hour: 3, Minute: 30, Run: func(ctx context.Context) (jobReport, error) {
		close(entered)
		<-ctx.Done()
		return jobReport{Rows: 2}, ctx.Err()
	}})
	ctx, cancel := context.WithCancel(context.Background())
	before := counterValue(name, jobOutcomeInterrupted)
	done := make(chan struct{})
	go func() { _, _ = j.RunNow(ctx); close(done) }()
	<-entered

	// Act
	cancel()
	<-done

	// Assert
	row := f.row(name)
	assert.Equal(t, jobOutcomeInterrupted, row.Outcome, "a deploy is not a job failure")
	assert.False(t, row.LastSuccess.Valid)
	assert.Equal(t, before+1, counterValue(name, jobOutcomeInterrupted))
	assert.Equal(t, float64(0), counterValue(name, jobOutcomeError))
	due, err := j.due(context.Background())
	require.NoError(t, err)
	assert.True(t, due, "the next boot's catch-up runs it again")
}

func TestJob_DeadlineIsAnError(t *testing.T) {
	f := setupJobsFixture(t)
	name := f.name("deadline")
	j := f.sched.Register(jobSpec{Name: name, Hour: 3, Minute: 30, MaxRunTime: 20 * time.Millisecond,
		Run: func(ctx context.Context) (jobReport, error) { <-ctx.Done(); return jobReport{}, ctx.Err() }})

	_, err := j.RunNow(context.Background())

	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Equal(t, jobOutcomeError, f.row(name).Outcome)
}

func TestJob_IntervalJobRunsAfterBootAndOnEveryTick(t *testing.T) {
	// Arrange
	f := setupJobsFixture(t)
	name := f.name("interval")
	var runs atomic.Int32
	f.sched.Register(jobSpec{Name: name, Every: 30 * time.Millisecond,
		Run: func(context.Context) (jobReport, error) { runs.Add(1); return jobReport{}, nil }})
	ctx, cancel := context.WithCancel(context.Background())

	// Act
	f.sched.Start(ctx)

	// Assert: a first run without waiting for the first tick (the hourly
	// cleanups used to wait an hour after every deploy), then more.
	require.Eventually(t, func() bool { return runs.Load() >= 3 }, 10*time.Second, 5*time.Millisecond)
	cancel()
	require.Eventually(t, func() bool { return f.row(name).Outcome != jobOutcomeRunning }, 5*time.Second, 10*time.Millisecond)
	n := runs.Load()
	time.Sleep(120 * time.Millisecond)
	assert.LessOrEqual(t, runs.Load(), n+1, "the loop stops with its context")
	assert.True(t, f.row(name).LastSuccess.Valid)
}

func TestJob_DailyJobIsNotRunAtBootWhenNotDue(t *testing.T) {
	// Arrange: today's run already succeeded (the common restart).
	f := setupJobsFixture(t)
	name := f.name("notdue")
	_, err := f.db.Exec(`INSERT INTO job_runs (job, last_success_at, last_outcome) VALUES ($1, $2, 'ok')`,
		name, time.Date(2026, 10, 1, 3, 31, 0, 0, time.UTC))
	require.NoError(t, err)
	var runs atomic.Int32
	f.sched.recheck = 20 * time.Millisecond
	f.sched.Register(jobSpec{Name: name, Hour: 3, Minute: 30,
		Run: func(context.Context) (jobReport, error) { runs.Add(1); return jobReport{}, nil }})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Act: boot, and several re-checks.
	f.sched.Start(ctx)
	time.Sleep(150 * time.Millisecond)

	// Assert
	assert.Equal(t, int32(0), runs.Load(), "a redeploy must not repeat a daily job that already ran today")
}

func TestJob_DetachedRunSurvivesTheRequestAndReportsBack(t *testing.T) {
	// Arrange
	f := setupJobsFixture(t)
	name := f.name("detached")
	j := f.sched.Register(jobSpec{Name: name, Hour: 3, Minute: 30, Run: func(ctx context.Context) (jobReport, error) {
		select {
		case <-ctx.Done():
			return jobReport{}, ctx.Err()
		case <-time.After(50 * time.Millisecond):
			return jobReport{Rows: 9}, nil
		}
	}})
	reqCtx, cancelReq := context.WithCancel(context.Background())
	got := make(chan jobReport, 1)

	// Act: the trigger starts it and the request ends at once.
	require.NoError(t, j.StartDetached(reqCtx, func(r jobReport, _ error) { got <- r }))
	cancelReq()

	// Assert
	select {
	case r := <-got:
		assert.Equal(t, int64(9), r.Rows, "a closed request does not cancel the run")
	case <-time.After(10 * time.Second):
		t.Fatal("detached run did not finish")
	}
	assert.Equal(t, jobOutcomeOK, f.row(name).Outcome)
}

// The trap, on the new series: a process that has just started must report
// the last success of the process before it.
func TestJobMetrics_LastSuccessIsReadFromJobRunsAfterARestart(t *testing.T) {
	// Arrange: the previous process succeeded an hour ago; this "process"
	// (a fresh scheduler, a fresh registry) has not run the job.
	f := setupJobsFixture(t)
	name, never := f.name("metric"), f.name("never")
	anHourAgo := time.Now().Add(-time.Hour).Truncate(time.Second)
	_, err := f.db.Exec(`INSERT INTO job_runs (job, last_started_at, last_finished_at, last_success_at, last_outcome)
		VALUES ($1, $2, $2, $2, 'ok')`, name, anHourAgo)
	require.NoError(t, err)
	noop := func(context.Context) (jobReport, error) { return jobReport{}, nil }
	f.sched.Register(jobSpec{Name: name, Hour: 3, Minute: 30, Run: noop})
	f.sched.Register(jobSpec{Name: never, Hour: 3, Minute: 30, Run: noop})
	s := &Server{db: f.db, jobs: f.sched}

	// Act
	w := httptest.NewRecorder()
	s.handleMetrics(w, httptest.NewRequest("GET", "/metrics", nil))
	body := w.Body.String()
	read := func(job string) float64 {
		m := regexp.MustCompile(`(?m)^vaultaire_job_last_success_timestamp_seconds\{job_name="` + job + `"\} (\S+)$`).FindStringSubmatch(body)
		require.NotNil(t, m, "no vaultaire_job_last_success_timestamp_seconds series for %s", job)
		v, err := strconv.ParseFloat(m[1], 64)
		require.NoError(t, err)
		return v
	}

	// Assert: what a staleness rule computes right after a restart.
	assert.Equal(t, float64(anHourAgo.Unix()), read(name))
	staleFor := time.Duration(float64(time.Now().Unix())-read(name)) * time.Second
	assert.Less(t, staleFor, 36*time.Hour, "JobStale must not be true after a restart")
	assert.Equal(t, float64(0), read(never), "a registered job that has never succeeded reads 0 (stale after the rule's `for`)")

	// The gauges that were only Set in-process are gone.
	assert.False(t, strings.Contains(body, "vaultaire_retention_last_run_timestamp_seconds"), "the in-process retention gauge is gone")
	assert.False(t, strings.Contains(body, "vaultaire_account_deletion_last_run_timestamp_seconds"), "the in-process deletion gauge is gone")

	// A run in this process shows up on the next scrape (no stale cache).
	_, err = f.sched.job(never).RunNow(context.Background())
	require.NoError(t, err)
	w = httptest.NewRecorder()
	s.handleMetrics(w, httptest.NewRequest("GET", "/metrics", nil))
	body = w.Body.String()
	assert.Equal(t, float64(f.now.Unix()), read(never))
	assert.True(t, strings.Contains(body, `vaultaire_job_runs_total{job_name="`+never+`",outcome="ok"} 1`), "the run is counted")
}

func TestJobMetrics_AFailedReadKeepsThePreviousValues(t *testing.T) {
	// Arrange: one good read, then the database goes away.
	f := setupJobsFixture(t)
	name := f.name("dbdown")
	j := f.sched.Register(jobSpec{Name: name, Hour: 3, Minute: 30,
		Run: func(context.Context) (jobReport, error) { return jobReport{}, nil }})
	_, err := j.RunNow(context.Background())
	require.NoError(t, err)
	last, ok := f.sched.lastSuccesses()
	require.True(t, ok)
	require.False(t, last[name].IsZero())

	broken, err := sql.Open("postgres", "postgres://nobody@127.0.0.1:1/none?sslmode=disable&connect_timeout=1")
	require.NoError(t, err)
	defer func() { _ = broken.Close() }()
	f.sched.db = broken
	f.sched.invalidateLastSuccess()

	// Act
	again, ok := f.sched.lastSuccesses()

	// Assert: not a zero (a zero reads as "never succeeded" and pages).
	require.True(t, ok)
	assert.True(t, again[name].Equal(last[name]))

	// A scheduler that has never read job_runs exports nothing.
	cold := newJobScheduler(broken, zap.NewNop())
	cold.Register(jobSpec{Name: name, Hour: 3, Minute: 30, Run: j.spec.Run})
	_, ok = cold.lastSuccesses()
	assert.False(t, ok)
	f.sched.db = f.db
}

func TestJobLockKey_IsStableAndDistinct(t *testing.T) {
	assert.Equal(t, jobLockKey("retention"), jobLockKey("retention"))
	seen := map[int64]string{}
	for _, n := range []string{"retention", "account_deletion", "dedup_gc", "smart_demotion", "inventory",
		"multipart_reaper", "idempotency_cleanup", "sts_cleanup", "session_cleanup", "cdn_rollup",
		"access_log_delivery", "bandwidth_alerts"} {
		k := jobLockKey(n)
		assert.GreaterOrEqual(t, k, int64(0))
		assert.Empty(t, seen[k], "lock key collision between %s and %s", n, seen[k])
		seen[k] = n
	}
}

func TestJob_FinishRecordsTheSuccessEvenWhenTheStartRowIsMissing(t *testing.T) {
	// Arrange: the start row was never written (a database blip at the
	// start of the run) — modelled by the run removing its own row.
	f := setupJobsFixture(t)
	name := f.name("nostart")
	j := f.sched.Register(jobSpec{Name: name, Hour: 3, Minute: 30, Run: func(ctx context.Context) (jobReport, error) {
		_, err := f.db.ExecContext(ctx, `DELETE FROM job_runs WHERE job = $1`, name)
		return jobReport{Rows: 5}, err
	}})

	// Act
	_, err := j.RunNow(context.Background())
	require.NoError(t, err)

	// Assert: the success is on record, so the daily job is not owed again —
	// an UPDATE of the missing row would have left it running every hour.
	row := f.row(name)
	assert.Equal(t, jobOutcomeOK, row.Outcome)
	assert.True(t, row.LastSuccess.Valid)
	assert.Equal(t, int64(5), row.Rows)
	due, err := j.due(context.Background())
	require.NoError(t, err)
	assert.False(t, due)
}

func TestJobScheduler_ANameIsRegisteredOnce(t *testing.T) {
	f := setupJobsFixture(t)
	name := f.name("twice")
	var first, second atomic.Int32
	a := f.sched.Register(jobSpec{Name: name, Every: time.Hour, Run: func(context.Context) (jobReport, error) { first.Add(1); return jobReport{}, nil }})
	b := f.sched.Register(jobSpec{Name: name, Every: time.Hour, Run: func(context.Context) (jobReport, error) { second.Add(1); return jobReport{}, nil }})

	_, err := b.RunNow(context.Background())

	require.NoError(t, err)
	assert.Same(t, a, b, "the second registration returns the first job")
	assert.Equal(t, int32(1), first.Load())
	assert.Equal(t, int32(0), second.Load())
	n := 0
	for _, registered := range f.sched.names() {
		if registered == name {
			n++
		}
	}
	assert.Equal(t, 1, n)
}
