package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/FairForge/vaultaire/internal/common"
	"github.com/FairForge/vaultaire/internal/config"
	"github.com/FairForge/vaultaire/internal/drivers"
	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/FairForge/vaultaire/internal/tenant"
	"github.com/FairForge/vaultaire/internal/testutil"
	"github.com/aws/smithy-go"
	"github.com/google/uuid"
	promtestutil "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// WP-R7-5 — routing truth. object_head_cache.backend_name is what GET,
// DELETE, the demotion ledger, the deletion runner and the erasure sweep
// trust; nothing checked it. These tests run on their own tenant's rows and
// their own backend names (the test database is shared); the checker is
// scoped to the fixture's tenant the way the deletion runner is (onlyDue).

// rtDriver is a backend whose Exists answers whatever the test says.
type rtDriver struct {
	exists func(ctx context.Context, container, artifact string) (bool, error)
	calls  atomic.Int32
}

func (d *rtDriver) Name() string { return "rt" }
func (d *rtDriver) Get(context.Context, string, string) (io.ReadCloser, error) {
	return nil, errors.New("rt: connection refused")
}
func (d *rtDriver) Put(context.Context, string, string, io.Reader, ...engine.PutOption) error {
	return nil
}
func (d *rtDriver) Delete(context.Context, string, string) error           { return nil }
func (d *rtDriver) List(context.Context, string, string) ([]string, error) { return nil, nil }
func (d *rtDriver) Exists(ctx context.Context, container, artifact string) (bool, error) {
	d.calls.Add(1)
	return d.exists(ctx, container, artifact)
}
func (d *rtDriver) HealthCheck(context.Context) error { return nil }

type rtFixture struct {
	t        *testing.T
	db       *sql.DB
	eng      *engine.CoreEngine
	tenantID string
	tn       *tenant.Tenant
	hot      string // a registered LocalDriver backend of this test
	hotDir   string
	checker  *RoutingTruthChecker
	sched    *jobScheduler
}

func setupRoutingTruthFixture(t *testing.T) *rtFixture {
	t.Helper()
	db, err := sql.Open("postgres", testutil.DSN())
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	require.NoError(t, db.Ping())

	f := &rtFixture{t: t, db: db, hotDir: t.TempDir()}
	f.eng = engine.NewEngine(nil, zap.NewNop(), nil)
	f.hot = "rt-hot-" + uuid.New().String()[:8]
	f.eng.AddDriver(f.hot, drivers.NewLocalDriver(f.hotDir, zap.NewNop()))
	f.eng.SetPrimary(f.hot)

	f.tenantID = "rt-" + uuid.New().String()[:12]
	f.tn = &tenant.Tenant{ID: f.tenantID}
	_, err = db.Exec(`INSERT INTO tenants (id, name, email, access_key, secret_key) VALUES ($1,$2,$3,$4,$5)`,
		f.tenantID, "routing truth", f.tenantID+"@test.local", "AK-"+f.tenantID, "SK-"+f.tenantID)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM smart_demotions WHERE tenant_id = $1`, f.tenantID)
		_, _ = db.Exec(`DELETE FROM object_versions WHERE tenant_id = $1`, f.tenantID)
		_, _ = db.Exec(`DELETE FROM object_head_cache WHERE tenant_id = $1`, f.tenantID)
		_, _ = db.Exec(`DELETE FROM tenants WHERE id = $1`, f.tenantID)
	})

	f.checker = NewRoutingTruthChecker(db, f.eng, nil, zap.NewNop())
	require.NotNil(t, f.checker)
	f.checker.scopeTenant = f.tenantID
	f.checker.JobName = "test_routing_" + uuid.New().String()[:8]
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM job_runs WHERE job = $1`, f.checker.JobName) })
	f.sched = newJobScheduler(db, zap.NewNop())
	return f
}

// row plants a head row; withFile also writes the blob where the LocalDriver
// of that backend keeps it (only meaningful for f.hot).
func (f *rtFixture) row(bucket, key, backend string, withFile bool) {
	f.t.Helper()
	var b any = backend
	if backend == "" {
		b = nil
	}
	_, err := f.db.Exec(`INSERT INTO object_head_cache (tenant_id, bucket, object_key, size_bytes, etag, backend_name, is_chunked)
		VALUES ($1,$2,$3,10,$4,$5,false)`, f.tenantID, bucket, key, "etag-"+key, b)
	require.NoError(f.t, err)
	if withFile {
		dir := filepath.Join(f.hotDir, f.tenantID+"_"+bucket)
		require.NoError(f.t, os.MkdirAll(dir, 0o750))
		require.NoError(f.t, os.WriteFile(filepath.Join(dir, key), []byte("0123456789"), 0o600))
	}
}

func (f *rtFixture) run() RoutingTruthResult {
	f.t.Helper()
	res, err := f.checker.RunOnce(context.Background())
	require.NoError(f.t, err)
	return res
}

func (f *rtFixture) metricsBody(s *Server) string {
	w := httptest.NewRecorder()
	s.handleMetrics(w, httptest.NewRequest("GET", "/metrics", nil))
	return w.Body.String()
}

// --- boot check -------------------------------------------------------------

func TestRoutingTruth_BootCheck_NamesEveryBackendNoDriverAnswersTo(t *testing.T) {
	// Arrange: rows on the registered backend, on a name no driver has
	// (prod's `onedrive`), and with no backend at all (prod's 62 NULL
	// multipart rows); a ledger row and a version row on the ghost too.
	f := setupRoutingTruthFixture(t)
	ghost := "rt-ghost-" + uuid.New().String()[:8]
	f.row("b", "ok1", f.hot, true)
	f.row("b", "ok2", f.hot, true)
	f.row("b", "g1", ghost, false)
	f.row("b", "g2", ghost, false)
	f.row("b", "n1", "", false)
	_, err := f.db.Exec(`INSERT INTO smart_demotions (tenant_id, bucket, object_key, etag, size_bytes, hot_backend, cold_backend)
		VALUES ($1,'b','g1','etag-g1',10,$2,$3)`, f.tenantID, ghost, f.hot)
	require.NoError(t, err)
	_, err = f.db.Exec(`INSERT INTO object_versions (tenant_id, bucket, object_key, version_id, size_bytes, etag, is_latest, backend_name)
		VALUES ($1,'b','g1','v1',10,'etag-g1',true,$2)`, f.tenantID, ghost)
	require.NoError(t, err)

	// Act
	rep, err := f.checker.BootCheck(context.Background())
	require.NoError(t, err)

	// Assert: every table, every unknown name, the NULL rows as "".
	got := map[string]int64{}
	for _, u := range rep.Unknown {
		got[u.Table+"/"+u.Backend] = u.Rows
	}
	assert.Equal(t, map[string]int64{
		"object_head_cache/" + ghost: 2,
		"object_head_cache/":         1,
		"smart_demotions/" + ghost:   1,
		"object_versions/" + ghost:   1,
	}, got)
	assert.Empty(t, rep.SharedStores)
	assert.False(t, rep.At.IsZero())
}

func TestRoutingTruth_BootCheck_TwoNamesOnOneStoreIsAnError(t *testing.T) {
	// Arrange: the same LocalDriver dir registered under a second name.
	f := setupRoutingTruthFixture(t)
	twin := f.hot + "-twin"
	f.eng.AddDriver(twin, drivers.NewLocalDriver(f.hotDir, zap.NewNop()))

	// Act
	rep, err := f.checker.BootCheck(context.Background())
	require.NoError(t, err)

	// Assert
	require.Len(t, rep.SharedStores, 1)
	assert.ElementsMatch(t, []string{f.hot, twin}, rep.SharedStores[0].Backends)
}

// --- the sampled job --------------------------------------------------------

func TestRoutingTruth_Run_PresentMissingAndChangedAreToldApart(t *testing.T) {
	// Arrange: three objects on the hot backend; one lost its bytes; one
	// is deleted by its owner while the run is looking at it.
	f := setupRoutingTruthFixture(t)
	f.row("b", "present1", f.hot, true)
	f.row("b", "present2", f.hot, true)
	f.row("b", "lost", f.hot, true)
	require.NoError(t, os.Remove(filepath.Join(f.hotDir, f.tenantID+"_b", "lost")))
	f.row("b", "racing", f.hot, false) // bytes never there — and the row goes away mid-run
	f.checker.beforeExists = func(h headRef) {
		if h.key == "racing" {
			_, _ = f.db.Exec(`DELETE FROM object_head_cache WHERE tenant_id = $1 AND object_key = 'racing'`, f.tenantID)
		}
	}
	before := promtestutil.ToFloat64(routingTruthChecks.WithLabelValues(f.hot, "missing"))

	// Act
	res := f.run()

	// Assert
	b := res.Backends[f.hot]
	require.NotNil(t, b)
	assert.Equal(t, int64(4), b.Rows)
	assert.Equal(t, 4, b.Sampled)
	assert.Equal(t, 2, b.Present)
	assert.Equal(t, 1, b.Missing, "bytes gone, row still there")
	assert.Equal(t, 1, b.Changed, "a row that is gone when the miss is re-read is not a miss")
	assert.Equal(t, 0, b.Errors)
	assert.InDelta(t, 1.0/3.0, b.MissingRatio, 1e-9, "missing over the rows with a verdict (present + missing)")
	assert.Equal(t, before+1, promtestutil.ToFloat64(routingTruthChecks.WithLabelValues(f.hot, "missing")))
	assert.Contains(t, res.Summary, "1 missing")
}

func TestRoutingTruth_Run_ABackendErrorIsNeverAMiss(t *testing.T) {
	// Arrange: a backend that answers NoSuchBucket (misconfigured, not
	// empty) for every HEAD — isBackendFailure's class, the one a bare 404
	// check would read as "gone". Six rows: more than the breaker's
	// threshold of five failures.
	f := setupRoutingTruthFixture(t)
	bad := "rt-bad-" + uuid.New().String()[:8]
	f.eng.AddDriver(bad, &rtDriver{exists: func(context.Context, string, string) (bool, error) {
		return false, fmt.Errorf("head: %w", &smithy.GenericAPIError{Code: "NoSuchBucket", Message: "The specified bucket does not exist"})
	}})
	for i := 0; i < 6; i++ {
		f.row("b", fmt.Sprintf("k%d", i), bad, false)
	}

	// Act
	res := f.run()

	// Assert: six errors, zero missing, the backend is named in the note —
	// and its breaker is still closed: a diagnostic observes the breaker,
	// it never charges it (an open primary breaker moves customer PUTs to
	// the next backend for 30 s).
	b := res.Backends[bad]
	require.NotNil(t, b)
	assert.Equal(t, 0, b.Missing)
	assert.Equal(t, 6, b.Errors)
	assert.Zero(t, b.MissingRatio)
	assert.Contains(t, res.Summary, bad+": ")
	assert.Contains(t, res.Summary, "6 error")
	assert.Equal(t, engine.StateClosed.String(), f.eng.GetFailoverStatus()[bad], "the job never charges a breaker")
}

func TestRoutingTruth_Run_ABackendThatKeepsFailingIsGivenUpForTheRun(t *testing.T) {
	// Arrange: forty rows on a backend whose every HEAD fails (prod's primary
	// key answers 403 to every GET and HEAD, 2026-10-03).
	f := setupRoutingTruthFixture(t)
	dead := &rtDriver{exists: func(context.Context, string, string) (bool, error) {
		return false, errors.New("api error AccessDenied: Access Denied")
	}}
	name := "rt-403-" + uuid.New().String()[:8]
	f.eng.AddDriver(name, dead)
	for i := 0; i < 40; i++ {
		f.row("b", fmt.Sprintf("k%02d", i), name, false)
	}

	// Act
	res := f.run()

	// Assert: ten calls, then the backend is given up — not forty HEADs at
	// a backend that is refusing them.
	b := res.Backends[name]
	require.NotNil(t, b)
	assert.Equal(t, int32(routingErrorCutoff), dead.calls.Load())
	assert.Equal(t, routingErrorCutoff, b.Errors)
	assert.Equal(t, 0, b.Missing)
	assert.Contains(t, b.Skipped, "consecutive errors")
}

func TestRoutingTruth_Run_AnOpenBreakerSkipsTheBackend(t *testing.T) {
	// Arrange: a backend whose breaker is open (five failures), with rows.
	f := setupRoutingTruthFixture(t)
	down := &rtDriver{exists: func(context.Context, string, string) (bool, error) { return false, nil }}
	name := "rt-down-" + uuid.New().String()[:8]
	f.eng.AddDriver(name, down)
	for i := 0; i < 5; i++ {
		_, _ = f.eng.GetOn(common.WithTenantID(context.Background(), f.tenantID), name, "c", "a")
	}
	require.Equal(t, engine.StateOpen.String(), f.eng.GetFailoverStatus()[name])
	for i := 0; i < 20; i++ {
		f.row("b", fmt.Sprintf("d%d", i), name, false)
	}

	// Act
	res := f.run()

	// Assert: one error for the backend, no HEADs, no misses.
	b := res.Backends[name]
	require.NotNil(t, b)
	assert.Equal(t, int32(0), down.calls.Load(), "nothing is asked of a backend whose breaker is open")
	assert.Equal(t, 0, b.Sampled)
	assert.Equal(t, 0, b.Missing)
	assert.Equal(t, 1, b.Errors)
	assert.Contains(t, b.Skipped, "breaker")
}

func TestRoutingTruth_Run_RowsOnAnUnregisteredBackendAreUnknownNotMissing(t *testing.T) {
	// Arrange: rows on a name no driver answers to, and NULL rows.
	f := setupRoutingTruthFixture(t)
	ghost := "rt-ghost-" + uuid.New().String()[:8]
	f.row("b", "g1", ghost, false)
	f.row("b", "g2", ghost, false)
	f.row("b", "n1", "", false)
	before := promtestutil.ToFloat64(routingTruthChecks.WithLabelValues(ghost, "unknown_backend"))

	// Act
	res := f.run()

	// Assert
	assert.Equal(t, map[string]int64{ghost: 2, "": 1}, res.Unknown)
	assert.Nil(t, res.Backends[ghost], "an unregistered backend is not sampled")
	assert.Equal(t, before+2, promtestutil.ToFloat64(routingTruthChecks.WithLabelValues(ghost, "unknown_backend")))
	for _, b := range res.Backends {
		assert.Zero(t, b.Missing)
	}
	assert.Contains(t, res.Summary, ghost+" 2")
}

func TestRoutingTruth_Run_SampleIsBoundedAndSmallerOnPermafrost(t *testing.T) {
	// Arrange: more rows than the sample on two backends — one of them the
	// fleet (whose Exists probes every account).
	f := setupRoutingTruthFixture(t)
	fleet := &rtDriver{exists: func(context.Context, string, string) (bool, error) { return true, nil }}
	f.eng.AddDriver("permafrost", fleet)
	for i := 0; i < 12; i++ {
		f.row("b", fmt.Sprintf("h%d", i), f.hot, true)
		f.row("b", fmt.Sprintf("p%d", i), "permafrost", false)
	}
	f.checker.Sample = 5
	f.checker.PermafrostSample = 2

	// Act
	res := f.run()

	// Assert
	assert.Equal(t, 5, res.Backends[f.hot].Sampled)
	assert.Equal(t, int64(12), res.Backends[f.hot].Rows)
	assert.Equal(t, 2, res.Backends["permafrost"].Sampled)
	assert.Equal(t, int32(2), fleet.calls.Load())
	assert.Equal(t, 5, res.Backends[f.hot].Present)
}

func TestRoutingTruth_Run_ARegionNamedBackendIsJustARegisteredName(t *testing.T) {
	// Arrange: rows on "idrive-eu-west-1" — registered under that exact
	// name, as WP-R7-1 does — are checked there, not folded into "idrive".
	f := setupRoutingTruthFixture(t)
	region := "idrive-eu-west-1"
	regionDir := t.TempDir()
	f.eng.AddDriver(region, drivers.NewLocalDriver(regionDir, zap.NewNop()))
	_, err := f.db.Exec(`INSERT INTO object_head_cache (tenant_id, bucket, object_key, size_bytes, etag, backend_name, is_chunked)
		VALUES ($1,'eu','k',10,'etag-k',$2,false)`, f.tenantID, region)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Join(regionDir, f.tenantID+"_eu"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(regionDir, f.tenantID+"_eu", "k"), []byte("x"), 0o600))

	// Act
	res := f.run()

	// Assert
	require.NotNil(t, res.Backends[region])
	assert.Equal(t, 1, res.Backends[region].Present)
	assert.Empty(t, res.Unknown)
}

func TestRoutingTruth_Run_NeverWrites(t *testing.T) {
	// Arrange: a miss and an unknown-backend row.
	f := setupRoutingTruthFixture(t)
	f.row("b", "lost", f.hot, false)
	f.row("b", "g", "rt-ghost-"+uuid.New().String()[:8], false)
	var before, after int
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM object_head_cache WHERE tenant_id = $1`, f.tenantID).Scan(&before))

	// Act
	f.run()

	// Assert: the rows are exactly as planted.
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM object_head_cache WHERE tenant_id = $1`, f.tenantID).Scan(&after))
	assert.Equal(t, before, after)
	var backend sql.NullString
	require.NoError(t, f.db.QueryRow(`SELECT backend_name FROM object_head_cache WHERE tenant_id = $1 AND object_key = 'lost'`, f.tenantID).Scan(&backend))
	assert.Equal(t, f.hot, backend.String)
}

// --- the job row, the collector, the restart -------------------------------

func TestRoutingTruth_Job_WritesTheResultAndTheCollectorReadsItAfterARestart(t *testing.T) {
	// Arrange: a run with one miss on the scheduler.
	f := setupRoutingTruthFixture(t)
	f.row("b", "ok", f.hot, true)
	f.row("b", "lost", f.hot, false)
	j := f.sched.Register(f.checker.spec())

	// Act: one run through the scheduler.
	_, err := j.RunNow(context.Background())
	require.NoError(t, err)

	// Assert: job_runs has the outcome, the note and the structured result.
	rows, err := jobRunRows(context.Background(), f.db)
	require.NoError(t, err)
	r := rows[f.checker.JobName]
	assert.Equal(t, jobOutcomeOK, r.Outcome)
	assert.Equal(t, int64(2), r.Rows, "rows = checks made")
	assert.Contains(t, r.Error, "1 missing")
	var res RoutingTruthResult
	require.NoError(t, json.Unmarshal(r.Result, &res), string(r.Result))
	assert.Equal(t, 1, res.Backends[f.hot].Missing)

	// A NEW checker on a NEW process (nothing in memory) exports the last
	// run's ratio from the table — the R13 lesson: never an in-process value.
	fresh := NewRoutingTruthChecker(f.db, f.eng, nil, zap.NewNop())
	fresh.JobName = f.checker.JobName
	fresh.scopeTenant = f.tenantID
	gauges := fresh.lastRunGauges(context.Background())
	require.Contains(t, gauges.missingRatio, f.hot)
	assert.InDelta(t, 0.5, gauges.missingRatio[f.hot], 1e-9)
	assert.False(t, gauges.finishedAt.IsZero())
}

func TestRoutingTruth_Metrics_SeriesExistAtZeroFromBootAndTheGaugesReadTheTable(t *testing.T) {
	// Arrange: a server whose engine has two backends and whose database
	// holds rows on a ghost backend (planted before the server exists).
	f := setupRoutingTruthFixture(t)
	ghost := "rt-ghost-" + uuid.New().String()[:8]
	f.row("b", "g", ghost, false)
	s := newRoutingTruthServer(t, f)

	// Act
	body := f.metricsBody(s)

	// Assert: every result series at 0 for the registered backend before any
	// run; the unknown-rows gauge is read from the table.
	for _, result := range []string{"present", "missing", "changed", "error"} {
		series := fmt.Sprintf(`vaultaire_routing_truth_checks_total{backend="%s",result="%s"} 0`, f.hot, result)
		assert.Contains(t, body, series)
	}
	assert.Contains(t, body, fmt.Sprintf(`vaultaire_routing_unknown_backend_rows{backend="%s",table="object_head_cache"} 1`, ghost))
	assert.Contains(t, body, "vaultaire_routing_shared_store_backends 0")
	assert.NotContains(t, body, "vaultaire_routing_truth_last_run_missing_ratio", "no run yet: no ratio, not a fake 0")
}

// newRoutingTruthServer builds a Server on the fixture's engine and
// database, with the fixture's scoped checker in place of the server's own.
func newRoutingTruthServer(t *testing.T, f *rtFixture) *Server {
	t.Helper()
	s := NewServer(&config.Config{Server: config.ServerConfig{Port: 8000}}, zap.NewNop(), f.eng, nil, f.db)
	require.NotNil(t, s.routingTruth)
	s.routingTruth.scopeTenant = f.tenantID
	s.routingTruth.JobName = f.checker.JobName
	// registerJobs registered the production name; the test's runs go under
	// the fixture's own name (job_runs is shared).
	s.jobs.Register(s.routingTruth.spec())
	return s
}

// --- admin ------------------------------------------------------------------

func TestRoutingTruth_Admin_ShowsTheLastRunAndTheUnknownRows(t *testing.T) {
	// Arrange: a run has happened; one ghost row.
	f := setupRoutingTruthFixture(t)
	f.row("b", "ok", f.hot, true)
	f.row("b", "g", "rt-ghost-"+uuid.New().String()[:8], false)
	s := newRoutingTruthServer(t, f)
	j := s.jobs.job(s.routingTruth.JobName)
	require.NotNil(t, j, "the job is registered under the checker's name")
	_, err := j.RunNow(context.Background())
	require.NoError(t, err)

	// Act
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/routing-truth", nil)
	w := httptest.NewRecorder()
	s.handleAdminRoutingTruth(w, req)

	// Assert
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var out struct {
		Job     string          `json:"job"`
		LastRun json.RawMessage `json:"last_run"`
		Outcome string          `json:"last_outcome"`
		Unknown []unknownRows   `json:"unknown_backend_rows"`
		Shared  []engine.SharedStore
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))
	assert.Equal(t, s.routingTruth.JobName, out.Job)
	assert.Equal(t, jobOutcomeOK, out.Outcome)
	assert.Contains(t, string(out.LastRun), `"present":1`)
	require.Len(t, out.Unknown, 1)
	assert.Equal(t, int64(1), out.Unknown[0].Rows)
}

func TestRoutingTruth_Admin_ResolveNullAsksEveryDriverOnceAndWritesOnlyOnAYes(t *testing.T) {
	// Arrange: three NULL rows — one whose bytes one backend holds, one
	// nobody holds, one two backends hold (ambiguous).
	f := setupRoutingTruthFixture(t)
	other := "rt-other-" + uuid.New().String()[:8]
	otherDir := t.TempDir()
	f.eng.AddDriver(other, drivers.NewLocalDriver(otherDir, zap.NewNop()))
	f.row("b", "found", "", true)
	f.row("b", "nowhere", "", false)
	f.row("b", "both", "", true)
	require.NoError(t, os.MkdirAll(filepath.Join(otherDir, f.tenantID+"_b"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(otherDir, f.tenantID+"_b", "both"), []byte("x"), 0o600))
	s := newRoutingTruthServer(t, f)

	post := func(dry bool) resolveNullResult {
		req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/v1/admin/routing-truth/resolve-null?dry_run=%t", dry), nil)
		w := httptest.NewRecorder()
		s.handleAdminRoutingTruthResolveNull(w, req)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		var res resolveNullResult
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &res))
		return res
	}
	backendOf := func(key string) string {
		var b sql.NullString
		require.NoError(t, f.db.QueryRow(`SELECT backend_name FROM object_head_cache WHERE tenant_id = $1 AND object_key = $2`, f.tenantID, key).Scan(&b))
		return b.String
	}

	// Act 1: the dry run.
	dry := post(true)

	// Assert 1: classified, nothing written.
	assert.True(t, dry.DryRun)
	assert.Equal(t, 3, dry.Rows)
	assert.Equal(t, 1, dry.Resolved)
	assert.Equal(t, 1, dry.Nowhere)
	assert.Equal(t, 1, dry.Ambiguous)
	assert.Equal(t, "", backendOf("found"))

	// Act 2: for real.
	real := post(false)

	// Assert 2: the one row with one owner is written; the others untouched.
	assert.Equal(t, 1, real.Resolved)
	assert.Equal(t, f.hot, backendOf("found"))
	assert.Equal(t, "", backendOf("nowhere"))
	assert.Equal(t, "", backendOf("both"))
}

// --- the readers of backend_name --------------------------------------------

func TestRoutingTruth_AReadOfARowOnAnUnregisteredBackendIsCounted(t *testing.T) {
	// Arrange
	ghost := "rt-ghost-" + uuid.New().String()[:8]
	eng := engine.NewEngine(nil, zap.NewNop(), nil)
	eng.AddDriver("idrive", drivers.NewLocalDriver(t.TempDir(), zap.NewNop()))
	before := promtestutil.ToFloat64(routingUnknownReads.WithLabelValues("get", ghost))

	// Act
	noteRecordedBackend(eng, zap.NewNop(), "get", ghost)
	noteRecordedBackend(eng, zap.NewNop(), "get", "idrive")

	// Assert: the unregistered name counts; a registered one does not.
	assert.Equal(t, before+1, promtestutil.ToFloat64(routingUnknownReads.WithLabelValues("get", ghost)))
	assert.Zero(t, promtestutil.ToFloat64(routingUnknownReads.WithLabelValues("get", "idrive")))
}

// --- local is not a durable class -------------------------------------------

func TestLocalIsNeverADurableClass(t *testing.T) {
	// REDUCED_REDUNDANCY used to place on the hub's disk (DATA_PATH, not
	// backed up — R9-12); 449 of prod's 451 `local` head rows point at bytes
	// that are gone. The class resolves to the primary like any other
	// unsold class, and an object on `local` reports STANDARD (on a box where
	// local IS the primary that is simply true).
	drvs := map[string]engine.Driver{"local": nil, "idrive": nil}
	backend, class := engine.ResolveStorageClass("REDUCED_REDUNDANCY", "idrive", drvs)
	assert.Equal(t, "idrive", backend)
	assert.Equal(t, "STANDARD", class)
	assert.Equal(t, "STANDARD", engine.BackendToStorageClass("local"))
	assert.Equal(t, "STANDARD", engine.CustomerStorageClass("standard", "local"))
}

// --- chunked rows -------------------------------------------------------------

// Prod's first run (2026-10-03 01:42 UTC): the whole-object pass asked the
// driver directly and left the primary's breaker alone — the chunk pass went
// through the chunk store's ExistsOn, five 403s opened the breaker, and the
// run then counted 127 "errors" for chunks nobody asked about. The chunk
// pass now observes the breaker like the whole-object pass, and is given up
// after the same ten consecutive errors.
func TestRoutingTruth_Run_ChunkChecksNeverChargeTheBreakerAndAreGivenUp(t *testing.T) {
	// Arrange: two chunked objects (dozens of chunks) on a primary whose
	// every Exists fails.
	f := setupChunkAddrFixture(t)
	chunks := 0
	for i := 0; i < routingErrorCutoff+2; i++ {
		chunks += len(f.put(f.a, fmt.Sprintf("o%02d.bin", i), generateTestData(8*1024)))
	}
	require.Greater(t, chunks, routingErrorCutoff, "enough chunks to pass the cutoff")
	f.fixed.failExists.Store(true)
	c := NewRoutingTruthChecker(f.db, f.eng, f.adapter.gci, zap.NewNop())
	c.scopeTenant = f.a.ID

	// Act
	res, err := c.RunOnce(context.Background())
	require.NoError(t, err)

	// Assert: ten errors, then given up; the breaker is still closed.
	assert.Equal(t, routingErrorCutoff, res.Chunks.Errors)
	assert.Equal(t, routingErrorCutoff, res.Chunks.Chunks, "no chunk is counted that nobody asked about")
	assert.Zero(t, res.Chunks.Missing)
	assert.Equal(t, engine.StateClosed.String(), f.eng.GetFailoverStatus()[f.backend], "the job never charges a breaker")
}

// A chunked row is its chunks: each is checked at the one address through
// the chunk store (WP-R8-7) — a blob still under its uploader's prefix is
// present (and the chunk move's work), a blob at neither address is missing,
// a chunk whose index row names an unregistered backend is counted as such.
func TestRoutingTruth_Run_ChunkedRowsAreCheckedChunkByChunkAtTheOneAddress(t *testing.T) {
	// Arrange: three chunked objects on the fixed-bucket primary — one at the
	// one address, one made legacy, one whose blobs are deleted.
	f := setupChunkAddrFixture(t)
	chunksOK := f.put(f.a, "ok.bin", generateTestData(8*1024))
	chunksLegacy := f.put(f.a, "legacy.bin", generateTestData(8*1024))
	chunksGone := f.put(f.a, "gone.bin", generateTestData(8*1024))
	f.makeLegacy(f.a, chunksLegacy)
	for _, c := range chunksGone {
		require.NoError(t, os.Remove(f.fixed.path(engine.ChunkAddressTenant, chunkContainer, c.storageKey)))
	}
	c := NewRoutingTruthChecker(f.db, f.eng, f.adapter.gci, zap.NewNop())
	c.scopeTenant = f.a.ID

	// Act
	res, err := c.RunOnce(context.Background())
	require.NoError(t, err)

	// Assert
	ct := res.Chunks
	assert.Equal(t, 3, ct.Objects)
	assert.Equal(t, len(chunksOK)+len(chunksLegacy)+len(chunksGone), ct.Chunks)
	assert.Equal(t, len(chunksOK)+len(chunksLegacy), ct.Present, "a legacy blob is present")
	assert.Equal(t, len(chunksLegacy), ct.Legacy)
	assert.Equal(t, len(chunksGone), ct.Missing)
	assert.Zero(t, ct.Errors)
	assert.Zero(t, ct.UnknownBackend)
	assert.Contains(t, res.Summary, "at a legacy address: run the chunk move")
	// The whole-object pass saw no whole rows for this tenant (all chunked).
	assert.Equal(t, int64(0), res.Backends[f.backend].Rows)

	// A chunk whose index row names a backend no driver has: unknown, never
	// missing (nobody was asked).
	_, err = f.db.Exec(`UPDATE global_content_index SET backend_id = 'rt-ghost' WHERE dedup_scope = $1 AND plaintext_hash = $2`,
		chunksOK[0].scope, chunksOK[0].hash)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = f.db.Exec(`UPDATE global_content_index SET backend_id = $3 WHERE dedup_scope = $1 AND plaintext_hash = $2`,
			chunksOK[0].scope, chunksOK[0].hash, f.backend)
	})
	f.adapter.gci.InvalidateCache(chunksOK[0].scope, chunksOK[0].hash)
	res, err = c.RunOnce(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, res.Chunks.UnknownBackend)
	assert.Equal(t, len(chunksGone), res.Chunks.Missing)
}
