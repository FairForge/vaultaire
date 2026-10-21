package api

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// RB-1: on a backend whose listing lags (Blomp's Swift container hid ~30
// existing objects from its listing for over 90 minutes, 2026-10-21) an
// empty walk is no proof. The tenant is declared erased only after a walk
// that started at least the lag after writes stopped and found nothing.

// laggingDriver is a fixed-bucket store whose listing lags: the artifacts
// in hidden exist and are not listed.
type laggingDriver struct {
	*fixedBucketDriver
	lag      time.Duration
	failWalk atomic.Bool

	mu     sync.Mutex
	hidden map[string]bool
}

func (d *laggingDriver) ListingLag() time.Duration { return d.lag }

func (d *laggingDriver) hide(artifact string, hidden bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.hidden[artifact] = hidden
}

func (d *laggingDriver) WalkTenant(ctx context.Context, tenantID string, fn func(engine.TenantObject) error) error {
	if d.failWalk.Load() {
		return errors.New("lagging: list: 503 Service Unavailable")
	}
	return d.fixedBucketDriver.WalkTenant(ctx, tenantID, func(o engine.TenantObject) error {
		d.mu.Lock()
		h := d.hidden[o.Artifact]
		d.mu.Unlock()
		if h {
			return nil
		}
		return fn(o)
	})
}

type settleFixture struct {
	*deletionFixture
	lagging *laggingDriver
	clock   time.Time
}

func setupSettleFixture(t *testing.T) *settleFixture {
	t.Helper()
	f := &settleFixture{deletionFixture: setupDeletionFixture(t)}
	f.lagging = &laggingDriver{fixedBucketDriver: &fixedBucketDriver{dir: t.TempDir()}, lag: 3 * time.Hour, hidden: map[string]bool{}}
	f.eng.AddDriver("lagging", f.lagging)
	f.eng.SetPrimary("local")
	f.clock = time.Now()
	f.runner.now = func() time.Time { return f.clock }
	f.seed(f.clock.Add(-time.Hour))
	return f
}

// onLagging writes a blob of the tenant that no row names.
func (f *settleFixture) onLagging(key string) string {
	f.t.Helper()
	p := f.lagging.path(f.tenantID, f.container(), key)
	require.NoError(f.t, os.MkdirAll(filepath.Dir(p), 0o750))
	require.NoError(f.t, os.WriteFile(p, []byte("bytes"), 0o600))
	return p
}

func (f *settleFixture) auditRows(action string) int {
	return f.count(`SELECT COUNT(*) FROM audit_logs WHERE tenant_id = $1 AND action = $2`, f.tenantID, action)
}

func (f *settleFixture) owed() []erasureSettle {
	f.t.Helper()
	st, err := f.runner.loadSettles(context.Background())
	require.NoError(f.t, err)
	return st.Settling
}

func TestErasureSettle_ALaggingListingIsNotProofUntilAWalkAfterTheLagFindsNothing(t *testing.T) {
	// Arrange: a blob of the tenant's on the lagging backend that no row
	// names and that its listing does not show yet.
	f := setupSettleFixture(t)
	ctx := context.Background()
	blob := f.onLagging("lagged.bin")
	f.lagging.hide("lagged.bin", true)

	// Act 1: the erasure on its date.
	res, err := f.runner.RunOnce(ctx)

	// Assert 1: the rows went, the bytes did not, and nothing says erased.
	require.NoError(t, err, "waiting for a listing to settle is not a failure: %+v", res)
	require.Len(t, res.Tenants, 1)
	te := res.Tenants[0]
	assert.Equal(t, outcomeSettling, te.Outcome, "%+v", te)
	assert.Equal(t, []string{"lagging"}, te.Settling)
	require.NotNil(t, te.SettleAfter)
	assert.WithinDuration(t, f.clock.Add(3*time.Hour), *te.SettleAfter, time.Second, "the lag counts from the end of the row erase")
	assert.Zero(t, res.Erased)
	assert.Equal(t, 1, res.Settling)
	assert.Zero(t, f.count(`SELECT COUNT(*) FROM users WHERE id::text = $1`, f.userID), "the rows are erased")
	assert.True(t, fileExists(blob), "the listing did not show it: nothing deleted it")
	assert.Zero(t, f.auditRows("account.erased"), "never declared erased while a lagging listing may hide bytes")
	assert.Equal(t, 1, f.auditRows("account.erasure_settling"))
	require.Len(t, f.owed(), 1, "the walk owed is in job_runs.result")
	due, err := f.runner.settleDue(ctx)
	require.NoError(t, err)
	assert.False(t, due, "nothing is due before the lag")

	// Act 2: an hour later the listing shows the blob — but a run before the
	// lag has passed walks nothing.
	f.lagging.hide("lagged.bin", false)
	f.clock = f.clock.Add(time.Hour)
	res, err = f.runner.RunOnce(ctx)
	require.NoError(t, err, "%+v", res)
	assert.Empty(t, res.Tenants, "no walk before its due time")
	assert.NotEmpty(t, res.Notes, "the run says what it waits for")
	assert.True(t, fileExists(blob))

	// Act 3: past the lag, the walk cannot run (the listing fails).
	f.clock = f.clock.Add(2*time.Hour + time.Minute)
	due, err = f.runner.settleDue(ctx)
	require.NoError(t, err)
	assert.True(t, due, "the job's Again runs it at the next hourly check")
	f.lagging.failWalk.Store(true)
	res, err = f.runner.RunOnce(ctx)
	require.Error(t, err, "a walk that could not run defers the tenant: the run fails and is retried hourly")
	require.Len(t, res.Tenants, 1)
	assert.Equal(t, outcomeDeferred, res.Tenants[0].Outcome)
	assert.Zero(t, f.auditRows("account.erased"))
	require.Len(t, f.owed(), 1, "still owed")
	assert.NotEmpty(t, f.owed()[0].LastError)

	// Act 4: the walk runs and finds the blob the earlier passes missed.
	f.lagging.failWalk.Store(false)
	res, err = f.runner.RunOnce(ctx)
	require.NoError(t, err, "%+v", res)
	assert.False(t, fileExists(blob), "every walk deletes what it finds")
	assert.Empty(t, res.Tenants, "a walk that found something proves nothing yet")
	assert.Zero(t, f.auditRows("account.erased"))
	require.Len(t, f.owed(), 1)
	assert.Equal(t, 1, f.owed()[0].LastFound)

	// Act 5: the next walk finds nothing — the erasure is declared.
	f.clock = f.clock.Add(time.Hour)
	res, err = f.runner.RunOnce(ctx)
	require.NoError(t, err, "%+v", res)
	require.Len(t, res.Tenants, 1)
	assert.Equal(t, outcomeErased, res.Tenants[0].Outcome)
	assert.Equal(t, map[string]int{"lagging": 1}, res.Tenants[0].Swept, "what the settle walks deleted")
	assert.Equal(t, 1, res.Erased)
	assert.Equal(t, 1, f.auditRows("account.erased"))
	assert.Empty(t, f.owed(), "nothing owed any more")
	due, err = f.runner.settleDue(ctx)
	require.NoError(t, err)
	assert.False(t, due)
}

// The scheduler runs a daily job again when its Again says so — the day's
// success notwithstanding.
func TestErasureSettle_TheSchedulerRunsTheJobAgainWhenAWalkIsDue(t *testing.T) {
	f := setupSettleFixture(t)
	ctx := context.Background()
	_, err := f.sj.RunNow(ctx)
	require.NoError(t, err)
	require.Len(t, f.owed(), 1)

	due, err := f.sj.due(ctx)
	require.NoError(t, err)
	assert.False(t, due, "succeeded today, nothing owed yet")

	f.clock = f.clock.Add(3*time.Hour + time.Minute)
	due, err = f.sj.due(ctx)
	require.NoError(t, err)
	assert.True(t, due, "the settle walk is due: the hourly check runs the job")

	rep, err := f.sj.RunNow(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(1), rep.Rows, "the erasure declared")
	rows, err := jobRunRows(ctx, f.db)
	require.NoError(t, err)
	assert.JSONEq(t, `{"settling": null}`, string(rows[f.runner.JobName].Result), "the result is the owed walks")
}

// A walk recorded before EraseRows: kept with its time from the next run
// when the rows went (the process stopped in between), dropped when they
// did not.
func TestErasureSettle_AnEntryWithNoTimeIsTimedOrDropped(t *testing.T) {
	f := setupSettleFixture(t)
	ctx := context.Background()
	gone := uuid.New().String() // no tenants row: its rows were erased
	require.NoError(t, f.runner.saveSettles(ctx, erasureSettleState{Settling: []erasureSettle{
		{TenantID: gone, Backend: "lagging", Settle: "3h0m0s"},
		{TenantID: f.tenantID, Backend: "lagging", Settle: "3h0m0s"}, // still there: its erase did not happen
	}}))
	f.exec(`UPDATE users SET deletion_scheduled_at = $2 WHERE id::text = $1`, f.userID, f.clock.Add(time.Hour)) // not due this run

	res, err := f.runner.RunOnce(ctx)

	require.NoError(t, err, "%+v", res)
	owed := f.owed()
	require.Len(t, owed, 1)
	assert.Equal(t, gone, owed[0].TenantID)
	assert.WithinDuration(t, f.clock, owed[0].WritesStoppedAt, time.Second, "counted from the run that found it")
	assert.WithinDuration(t, f.clock.Add(3*time.Hour), owed[0].DueAt, time.Second)
}
