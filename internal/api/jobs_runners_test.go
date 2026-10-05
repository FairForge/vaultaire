package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/FairForge/vaultaire/internal/config"
	"github.com/FairForge/vaultaire/internal/drivers"
	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// WP-R13-3: each runner on the shared scheduler. Every test uses its own job
// name (job_runs is global) and its own tenant.

// asJob registers a runner's spec under a test-only name on a scheduler
// whose clock the test controls.
func asJob(t *testing.T, f *jobsFixture, spec jobSpec) *scheduledJob {
	t.Helper()
	t.Cleanup(func() { _, _ = f.db.Exec(`DELETE FROM job_runs WHERE job = $1`, spec.Name) })
	j := f.sched.Register(spec)
	require.NotNil(t, j)
	return j
}

func testJobName(prefix string) string { return "test_" + prefix + "_" + uuid.New().String()[:8] }

// --- smart demotion ---------------------------------------------------------

func TestSmartDemotionJob_AnObjectThatCannotMoveIsANoteNotAFailedRun(t *testing.T) {
	// Arrange: two idle objects; the hot blob of one is gone, so its copy
	// fails. (Flaky backend, one object.)
	f := setupDemotionFixture(t, 1*tb, "standard")
	f.object("b", "good", 100, 30, 20)
	f.object("b", "broken", 100, 30, 21)
	require.NoError(t, os.Remove(filepath.Join(f.hotDir, f.tenantID+"_b", "broken")))
	jf := setupJobsFixture(t)
	f.runner.JobName = testJobName("demotion")
	j := asJob(t, jf, f.runner.spec())

	// Act: the scheduler's check, and the next hourly re-check.
	j.tick(context.Background())
	j.tick(context.Background())

	// Assert: the run did its pass — "ok", with the failure written down —
	// and is NOT repeated an hour later (an "error" would have re-run the
	// whole job every hour until the object could move).
	row := jf.row(f.runner.JobName)
	require.Equal(t, jobOutcomeOK, row.Outcome, row.Error)
	assert.Contains(t, row.Error, "1 item(s) failed")
	assert.Contains(t, row.Error, "broken")
	assert.Equal(t, int64(1), row.Rows)
	assert.Equal(t, "geyser", f.backendOf("b", "good"))
	assert.Equal(t, "idrive", f.backendOf("b", "broken"), "the failed object stays where it is, a candidate for tomorrow")
	assert.Equal(t, float64(1), counterValue(f.runner.JobName, jobOutcomeOK), "one run, not two")
}

func TestSmartDemotion_ByteBudgetIsPerDayNotPerRun(t *testing.T) {
	// Arrange: five idle 100 GiB objects, a 250 GiB budget.
	f := setupDemotionFixture(t, 10*tb, "standard")
	for i := 0; i < 5; i++ {
		f.object("b", fmt.Sprintf("idle%d", i), 100*gb, 30, 20)
	}
	f.runner.MaxBytesPerRun = 250 * gb

	// Act: a run, then — a deploy interrupted the job, or an admin posted
	// the trigger — a second run on the same day.
	first, err := f.runner.RunOnce(context.Background(), false)
	require.NoError(t, err)
	second, err := f.runner.RunOnce(context.Background(), false)
	require.NoError(t, err)

	// Assert: the second run only gets what the first left of the budget.
	assert.Equal(t, 3, first.Demoted)
	assert.Equal(t, 0, second.Demoted, "a second run within 24 h must not double the bytes that cross the box")
	assert.Equal(t, int64(300*gb), second.BytesDemotedBefore)

	// A day later the budget is whole again.
	later := f.now.Add(25 * time.Hour)
	f.runner.now = func() time.Time { return later }
	third, err := f.runner.RunOnce(context.Background(), false)
	require.NoError(t, err)
	assert.Equal(t, 2, third.Demoted)
}

func TestSmartDemotionJob_TenantListFailureIsAnError(t *testing.T) {
	// The run itself failing (here: the cold backend is gone) is an error:
	// last_success_at stays put and the hourly check retries.
	f := setupDemotionFixture(t, 1*tb, "standard")
	f.runner.ColdBackend = "no-such-backend"
	jf := setupJobsFixture(t)
	f.runner.JobName = testJobName("demotion_err")
	j := asJob(t, jf, f.runner.spec())

	_, err := j.RunNow(context.Background())

	require.Error(t, err)
	row := jf.row(f.runner.JobName)
	assert.Equal(t, jobOutcomeError, row.Outcome)
	assert.False(t, row.LastSuccess.Valid)
	due, derr := j.due(context.Background())
	require.NoError(t, derr)
	assert.True(t, due)
}

// --- dedup GC ---------------------------------------------------------------

func TestDedupGCJob_AChunkTheBackendWillNotDeleteIsANoteNotAFailedRun(t *testing.T) {
	// Arrange: one chunked object, deleted, its chunks past grace — and a
	// backend that refuses deletes.
	runner, f := setupGCFixture(t)
	putChunkedObject(t, f, "gc-flaky.bin", generateTestData(8*1024), "application/octet-stream")
	pairs := tenantChunkPairs(t, f.db, f.tenant.ID)
	require.NotEmpty(t, pairs)
	delReq := httptest.NewRequest("DELETE", "/test-bucket/gc-flaky.bin", nil)
	delReq = delReq.WithContext(s3Ctx(delReq.Context(), f.tenant))
	dw := httptest.NewRecorder()
	f.adapter.HandleDelete(dw, delReq, "test-bucket", "gc-flaky.bin")
	require.Equal(t, http.StatusNoContent, dw.Code)
	backdateGCIRows(t, f.db, pairs)
	f.fixed.failDelete.Store(true)
	jf := setupJobsFixture(t)
	runner.JobName = testJobName("gc")
	j := asJob(t, jf, runner.spec())

	// Act
	j.tick(context.Background())
	j.tick(context.Background())

	// Assert: "ok" with a note; one run.
	row := jf.row(runner.JobName)
	require.Equal(t, jobOutcomeOK, row.Outcome, row.Error)
	assert.Contains(t, row.Error, "chunk(s) not swept")
	assert.Equal(t, float64(1), counterValue(runner.JobName, jobOutcomeOK))
}

// --- inventory --------------------------------------------------------------

type inventoryJobFixture struct {
	*inventoryFixture
	jf     *jobsFixture
	runner *InventoryRunner
	job    *scheduledJob
}

func setupInventoryJobFixture(t *testing.T) *inventoryJobFixture {
	t.Helper()
	f := setupInventoryFixture(t)
	f.server.accessLogTracker = nil
	for i := 0; i < 2; i++ {
		req := httptest.NewRequest("PUT", fmt.Sprintf("/%s/file-%d.txt", f.bucket, i), strings.NewReader("hello"))
		req.ContentLength = 5
		req = req.WithContext(s3Ctx(req.Context(), f.tenant))
		rr := httptest.NewRecorder()
		f.server.handleS3Request(rr, req)
		require.Equal(t, 200, rr.Code, rr.Body.String())
	}
	jf := setupJobsFixture(t)
	runner := NewInventoryRunner(f.db, f.eng, zap.NewNop())
	runner.SetWriter(newGeneratedObjectWriter(f.db, f.eng, nil, nil, zap.NewNop()))
	runner.JobName = testJobName("inventory")
	runner.onlyTenant = f.tenantID // the test database holds other packages' configurations
	return &inventoryJobFixture{inventoryFixture: f, jf: jf, runner: runner, job: asJob(t, jf, runner.spec(jf.sched))}
}

func (f *inventoryJobFixture) enable(t *testing.T, schedule string) {
	t.Helper()
	_, err := f.db.Exec(`UPDATE buckets SET inventory_enabled = TRUE, inventory_schedule = $3,
		inventory_target_bucket = $4, inventory_prefix = 'inv/', inventory_format = 'CSV'
		WHERE tenant_id = $1 AND name = $2`, f.tenantID, f.bucket, schedule, f.invBucket)
	require.NoError(t, err)
}

func (f *inventoryJobFixture) manifests(t *testing.T) []string {
	t.Helper()
	rows, err := f.db.Query(`SELECT object_key FROM object_head_cache WHERE tenant_id = $1 AND bucket = $2 ORDER BY 1`, f.tenantID, f.invBucket)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	var keys []string
	for rows.Next() {
		var k string
		require.NoError(t, rows.Scan(&k))
		keys = append(keys, k)
	}
	require.NoError(t, rows.Err())
	return keys
}

func TestInventoryJob_ScheduledByJobRunsNotByTheHourOfTheClock(t *testing.T) {
	// Arrange: a daily inventory; it is 14:00 UTC on Thursday October 1st —
	// the process was restarted and the 00:30 run never happened. The old
	// job acted only when the wall clock's hour was 0.
	f := setupInventoryJobFixture(t)
	f.enable(t, "daily")
	f.jf.now = time.Date(2026, 10, 1, 14, 0, 0, 0, time.UTC)

	// Act: the boot catch-up, then two more checks; then a second restart
	// the same day (a new scheduler), and its catch-up.
	f.job.tick(context.Background())
	f.job.tick(context.Background())
	restarted := newJobScheduler(f.db, zap.NewNop())
	restarted.now = func() time.Time { return f.jf.now }
	restarted.Register(f.runner.spec(restarted)).tick(context.Background())

	// Assert: exactly one report, dated the day it was owed.
	assert.Equal(t, []string{"inv/" + f.bucket + "/2026-10-01T00-00Z/manifest.csv"}, f.manifests(t))
	assert.Equal(t, float64(1), counterValue(f.runner.JobName, jobOutcomeOK), "one run today: a restart neither skips nor doubles the day")
	assert.Equal(t, int64(1), f.jf.row(f.runner.JobName).Rows)

	// The next day's run writes the next report.
	f.jf.now = time.Date(2026, 10, 2, 0, 45, 0, 0, time.UTC)
	f.job.tick(context.Background())
	assert.Len(t, f.manifests(t), 2)
}

func TestInventoryJob_WeeklyBelongsToTheSundayRun(t *testing.T) {
	// Arrange
	f := setupInventoryJobFixture(t)
	f.enable(t, "weekly")

	// Act: Thursday's run.
	f.jf.now = time.Date(2026, 10, 1, 1, 0, 0, 0, time.UTC)
	f.job.tick(context.Background())
	assert.Empty(t, f.manifests(t), "not Sunday: nothing")
	assert.Equal(t, jobOutcomeOK, f.jf.row(f.runner.JobName).Outcome, "a run with nothing owed still succeeds")

	// Sunday's run — caught up at 23:00, still Sunday's.
	f.jf.now = time.Date(2026, 10, 4, 23, 0, 0, 0, time.UTC)
	f.job.tick(context.Background())

	// Assert
	assert.Equal(t, []string{"inv/" + f.bucket + "/2026-10-04T00-00Z/manifest.csv"}, f.manifests(t))
}

func TestInventoryJob_OneReportFailingDoesNotFailTheRunAndHasItsOwnDeadline(t *testing.T) {
	// Arrange: two buckets with inventory; the first's target bucket is gone.
	f := setupInventoryJobFixture(t)
	f.enable(t, "daily")
	_, err := f.db.Exec(`INSERT INTO buckets (tenant_id, name, visibility, inventory_enabled, inventory_schedule, inventory_target_bucket, inventory_prefix, inventory_format)
		VALUES ($1, 'aaa-first', 'private', TRUE, 'daily', 'no-such-target', 'inv/', 'CSV')`, f.tenantID)
	require.NoError(t, err)
	_, err = f.db.Exec(`INSERT INTO object_head_cache (tenant_id, bucket, object_key, size_bytes, etag) VALUES ($1, 'aaa-first', 'k', 1, 'e')`, f.tenantID)
	require.NoError(t, err)
	f.jf.now = time.Date(2026, 10, 1, 1, 0, 0, 0, time.UTC)

	// Act
	_, err = f.job.RunNow(context.Background())

	// Assert: the run is ok, the other bucket's report is written, the
	// failure is a note.
	require.NoError(t, err)
	row := f.jf.row(f.runner.JobName)
	assert.Equal(t, jobOutcomeOK, row.Outcome)
	assert.Contains(t, row.Error, "1 report(s) not written")
	assert.Contains(t, row.Error, "aaa-first")
	assert.Len(t, f.manifests(t), 1)

	// Each report has its own deadline: one that cannot finish in time does
	// not take the run's budget with it — the run still ends "ok".
	f.runner.ReportDeadline = time.Nanosecond
	res, err := f.runner.RunOnce(context.Background(), f.jf.now)
	require.NoError(t, err)
	assert.Equal(t, 2, res.Failed)
	assert.Contains(t, strings.Join(res.Errors, " "), "deadline")
}

// --- the table --------------------------------------------------------------

func TestRegisterJobs_EveryLoopIsAJobAndDailyTimesDoNotCollide(t *testing.T) {
	// Arrange: a server with a database, a hot and a cold backend.
	jf := setupJobsFixture(t)
	eng := engine.NewEngine(nil, zap.NewNop(), nil)
	eng.AddDriver("idrive", drivers.NewLocalDriver(t.TempDir(), zap.NewNop()))
	eng.AddDriver("geyser", drivers.NewLocalDriver(t.TempDir(), zap.NewNop()))
	eng.AddDriver("permafrost", drivers.NewLocalDriver(t.TempDir(), zap.NewNop())) // the parity leg (WP-VAULT-1)
	eng.SetPrimary("idrive")

	// Act
	s := NewServer(&config.Config{Server: config.ServerConfig{Port: 8000}}, zap.NewNop(), eng, nil, jf.db)

	// Assert: the jobs NewServer registers (account_deletion joins in Start).
	want := map[string]string{
		"vault_parity":        "every 2m0s",
		"inventory":           "daily 00:30 UTC",
		"dedup_gc":            "daily 02:30 UTC",
		"retention":           "daily 03:30 UTC",
		"routing_truth":       "daily 05:30 UTC",
		"smart_demotion":      "daily 06:30 UTC",
		"multipart_reaper":    "every 1h0m0s",
		"cdn_rollup":          "every 1h0m0s",
		"idempotency_cleanup": "every 1h0m0s",
		"sts_cleanup":         "every 1h0m0s",
		"session_cleanup":     "every 1h0m0s",
		"bandwidth_alerts":    "every 1h0m0s",
		"access_log_delivery": "every 5m0s",
		"account_export":      "every 1m0s",
	}
	got := map[string]string{}
	daily := map[string]string{"04:30": "account_deletion"}
	for _, name := range s.jobs.names() {
		sp := s.jobs.job(name).spec
		got[name] = sp.schedule()
		assert.Greater(t, sp.MaxRunTime, time.Duration(0), "%s has a ceiling", name)
		assert.Greater(t, sp.BootDelay, time.Duration(0), "%s does not run in the first instant of a boot", name)
		if sp.daily() {
			at := fmt.Sprintf("%02d:%02d", sp.Hour, sp.Minute)
			assert.Empty(t, daily[at], "%s and %s are both due at %s", name, daily[at], at)
			daily[at] = name
		}
	}
	assert.Equal(t, want, got)

	// Without the cold backend the demotion job is not a job of this process.
	hotOnly := engine.NewEngine(nil, zap.NewNop(), nil)
	hotOnly.AddDriver("idrive", drivers.NewLocalDriver(t.TempDir(), zap.NewNop()))
	hotOnly.SetPrimary("idrive")
	s2 := NewServer(&config.Config{Server: config.ServerConfig{Port: 8000}}, zap.NewNop(), hotOnly, nil, jf.db)
	assert.Nil(t, s2.jobs.job("smart_demotion"))
	assert.Nil(t, s2.jobs.job("vault_parity"), "no parity leg, no parity job")
	assert.NotNil(t, s2.jobs.job("retention"))
}

// --- admin ------------------------------------------------------------------

func TestAdminJobs_ListShowsScheduleStateAndStrangers(t *testing.T) {
	// Arrange: one job that ran, one that never did, and a job_runs row no
	// job of this process claims.
	jf := setupJobsFixture(t)
	ran, never, stranger := jf.name("ran"), jf.name("never"), jf.name("stranger")
	noop := func(context.Context) (jobReport, error) { return jobReport{Rows: 4, Note: "1 item skipped"}, nil }
	j := jf.sched.Register(jobSpec{Name: ran, Hour: 3, Minute: 30, Run: noop})
	jf.sched.Register(jobSpec{Name: never, Every: time.Hour, Run: noop})
	_, err := j.RunNow(context.Background())
	require.NoError(t, err)
	_, err = jf.db.Exec(`INSERT INTO job_runs (job, last_outcome) VALUES ($1, 'error')`, stranger)
	require.NoError(t, err)
	s := &Server{logger: zap.NewNop(), db: jf.db, jobs: jf.sched}

	// Act
	rr := doJSON(t, s.handleAdminJobsList, "GET", "/api/v1/admin/jobs")

	// Assert
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	var body struct {
		Jobs []adminJobView `json:"jobs"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body))
	by := map[string]adminJobView{}
	for _, v := range body.Jobs {
		by[v.Job] = v
	}
	got := by[ran]
	assert.Equal(t, "daily 03:30 UTC", got.Schedule)
	assert.True(t, got.Registered)
	assert.False(t, got.Running)
	assert.Equal(t, jobOutcomeOK, got.LastOutcome)
	assert.Equal(t, "1 item skipped", got.LastError)
	assert.Equal(t, int64(4), got.RowsAffected)
	require.NotNil(t, got.LastSuccessAt)
	require.NotNil(t, got.NextRunAt)
	assert.Equal(t, time.Date(2026, 10, 2, 3, 30, 0, 0, time.UTC), *got.NextRunAt)
	assert.Equal(t, "every 1h0m0s", by[never].Schedule)
	assert.Nil(t, by[never].LastSuccessAt)
	assert.Empty(t, by[never].LastOutcome)
	assert.False(t, by[stranger].Registered)
	assert.Equal(t, jobOutcomeError, by[stranger].LastOutcome)
}

func TestAdminTriggers_202Then409WhileRunning_DryRunStaysSynchronous(t *testing.T) {
	// Arrange
	f := setupDemotionFixture(t, 1*tb, "standard")
	f.object("b", "idle", 100, 30, 20)
	jf := setupJobsFixture(t)
	jf.sched.now = time.Now
	f.runner.JobName = testJobName("demotion_admin")
	asJob(t, jf, f.runner.spec())
	s := &Server{logger: zap.NewNop(), db: f.db, jobs: jf.sched, smartDemotion: f.runner}
	trigger := s.adminJobTrigger(f.runner.JobName)

	// A run in progress (another session holds the job's lock): both the
	// dry run and the real one are refused.
	conn, err := f.db.Conn(context.Background())
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	var got bool
	key := jobLockKey(f.runner.JobName)
	require.NoError(t, conn.QueryRowContext(context.Background(), `SELECT pg_try_advisory_lock($1)`, key).Scan(&got))
	require.True(t, got)
	for _, target := range []string{"/api/v1/admin/smart-demotion?dry_run=true", "/api/v1/admin/smart-demotion"} {
		rr := doJSON(t, trigger, "POST", target)
		assert.Equal(t, http.StatusConflict, rr.Code, rr.Body.String())
		assert.Contains(t, rr.Body.String(), `"already_running"`)
		assert.Contains(t, rr.Body.String(), f.runner.JobName)
	}
	_, _ = conn.ExecContext(context.Background(), `SELECT pg_advisory_unlock($1)`, key)

	// The dry run: synchronous, the report in the body, nothing moved,
	// nothing recorded as a run.
	rr := doJSON(t, trigger, "POST", "/api/v1/admin/smart-demotion?dry_run=true")
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	assert.Contains(t, rr.Body.String(), `"dry_run":true`)
	assert.Contains(t, rr.Body.String(), `"candidates":1`)
	assert.Equal(t, "idrive", f.backendOf("b", "idle"))
	assert.Empty(t, jf.row(f.runner.JobName).Job, "a dry run is not the day's run")

	// The real run: 202 + the job name at once; the run is detached.
	rr = doJSON(t, trigger, "POST", "/api/v1/admin/smart-demotion")
	require.Equal(t, http.StatusAccepted, rr.Code, rr.Body.String())
	var body map[string]string
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body))
	assert.Equal(t, f.runner.JobName, body["job"])
	assert.Equal(t, "started", body["status"])
	require.Eventually(t, func() bool { return jf.row(f.runner.JobName).Outcome == jobOutcomeOK }, 10*time.Second, 20*time.Millisecond)
	assert.Equal(t, "geyser", f.backendOf("b", "idle"))
	assert.Equal(t, int64(1), jf.row(f.runner.JobName).Rows)

	// The generic route is the same call; an unknown job is 404 there and
	// 503 on a named route; dry_run on any other job is refused.
	assert.Equal(t, http.StatusNotFound, doJSON(t, s.handleAdminJobRun, "POST", "/api/v1/admin/jobs/nope/run").Code)
	// The requested name is a lookup key only: an unknown one is never
	// written back (it used to be quoted into the answer).
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("job", `<script>alert(1)</script>`)
	xr := httptest.NewRequest("POST", "/api/v1/admin/jobs/x/run", nil)
	xr = xr.WithContext(context.WithValue(xr.Context(), chi.RouteCtxKey, rctx))
	xw := httptest.NewRecorder()
	s.handleAdminJobRun(xw, xr)
	assert.Equal(t, http.StatusNotFound, xw.Code)
	assert.NotContains(t, xw.Body.String(), "script")
	assert.Contains(t, xw.Header().Get("Content-Type"), "text/plain")
	assert.Equal(t, http.StatusServiceUnavailable, doJSON(t, s.adminJobTrigger("nope"), "POST", "/api/v1/admin/dedup-gc").Code)
	other := jf.name("other")
	jf.sched.Register(jobSpec{Name: other, Every: time.Hour, Run: func(context.Context) (jobReport, error) { return jobReport{}, nil }})
	assert.Equal(t, http.StatusBadRequest, doJSON(t, s.adminJobTrigger(other), "POST", "/x?dry_run=true").Code)

	// quota-reconcile is not a scheduled job: its in-process gate stays.
	s.quotaManager = &fakeReconciler{}
	require.True(t, s.quotaReconcileGate.tryAcquire())
	rr = doJSON(t, s.handleQuotaReconcile, "POST", "/api/v1/admin/quota-reconcile")
	assert.Equal(t, http.StatusConflict, rr.Code, rr.Body.String())
	assert.Contains(t, rr.Body.String(), `"quota_reconcile"`)
	s.quotaReconcileGate.release()
	rr = doJSON(t, s.handleQuotaReconcile, "POST", "/api/v1/admin/quota-reconcile")
	assert.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
}

// fakeReconciler is the storageReconciler slice of a QuotaManager, enough
// for the trigger.
type fakeReconciler struct{ stubQuotaManager }

func (*fakeReconciler) ReconcileStorageUsage(context.Context) (int64, error) { return 3, nil }

// The quota-reconcile trigger runs on a context that survives the request: a
// closed admin tab no longer cancels a half-done rewrite (Review R13-08).
func TestAdminTriggerContext_SurvivesRequestCancel(t *testing.T) {
	rctx, rcancel := context.WithCancel(context.Background())
	req := httptest.NewRequest("POST", "/api/v1/admin/quota-reconcile", nil).WithContext(rctx)
	ctx, cancel := adminTriggerContext(req)
	defer cancel()
	rcancel()
	select {
	case <-ctx.Done():
		t.Fatal("job context was cancelled with the request")
	case <-time.After(20 * time.Millisecond):
	}
	dl, ok := ctx.Deadline()
	require.True(t, ok)
	assert.WithinDuration(t, time.Now().Add(adminTriggerTimeout), dl, time.Minute)
}

// Post-merge review: a deploy cancels the scheduler's context in the middle
// of a demotion run. With one flagged tenant (the canary: tenant zero) the
// cancelled copies were appended as per-object failures, the loop ended on
// its last tenant and RunOnce returned nil — the scheduler recorded the cut
// run as the day's success and the next boot's catch-up did not run it again.
func TestSmartDemotionJob_ARunCutByAShutdownIsNotTheDaysSuccess(t *testing.T) {
	// Arrange: three idle objects of one tenant; the process starts stopping
	// while the first is being moved.
	// A tier of its own, so this tenant is the only — and therefore the
	// last — one the run lists, as the one managed tenant is on prod.
	tier := "cut-" + uuid.New().String()[:8]
	f := setupDemotionFixture(t, 1*tb, tier)
	f.runner.Tiers = []string{tier}
	f.object("b", "one", 100, 30, 22)
	f.object("b", "two", 100, 30, 21)
	f.object("b", "three", 100, 30, 20)
	jf := setupJobsFixture(t)
	f.runner.JobName = testJobName("demotion-cut")
	j := asJob(t, jf, f.runner.spec())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.runner.beforeFlip = func(string, string) { cancel() }

	// Act
	j.tick(ctx)

	// Assert: interrupted, no success recorded — the next boot runs it again.
	row := jf.row(f.runner.JobName)
	assert.Equal(t, jobOutcomeInterrupted, row.Outcome, row.Error)
	assert.False(t, row.LastSuccess.Valid, "a run the shutdown cut short must not count as today's run")
	due, err := j.due(context.Background())
	require.NoError(t, err)
	assert.True(t, due)
}
