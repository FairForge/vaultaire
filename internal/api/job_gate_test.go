package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// Review R13-05 / WP-R11-8: the admin triggers and the tickers are
// single-flight per process. A second run while one is in progress is
// refused (409 already_running from the trigger, a skipped tick from the
// loop) — never queued, never concurrent.

func TestAdminTriggers_SecondRunIs409(t *testing.T) {
	f := setupDemotionFixture(t, 1*tb, "standard")
	gc, _ := setupGCFixture(t)
	s := &Server{logger: zap.NewNop(), smartDemotion: f.runner, dedupGCRunner: gc}

	// Hold the smart-demotion gate as a running cycle would.
	require.True(t, f.runner.gate.tryAcquire())
	rr := doJSON(t, s.handleSmartDemotionTrigger, "POST", "/api/v1/admin/smart-demotion?dry_run=true")
	assert.Equal(t, http.StatusConflict, rr.Code, rr.Body.String())
	assert.Contains(t, rr.Body.String(), `"already_running"`)
	assert.Contains(t, rr.Body.String(), `"smart_demotion"`)
	f.runner.gate.release()
	rr = doJSON(t, s.handleSmartDemotionTrigger, "POST", "/api/v1/admin/smart-demotion?dry_run=true")
	assert.Equal(t, http.StatusOK, rr.Code, "released gate admits the next run")

	// Same for dedup GC.
	require.True(t, gc.gate.tryAcquire())
	rr = doJSON(t, s.handleDedupGCTrigger, "POST", "/api/v1/admin/dedup-gc")
	assert.Equal(t, http.StatusConflict, rr.Code, rr.Body.String())
	assert.Contains(t, rr.Body.String(), `"dedup_gc"`)
	gc.gate.release()
	rr = doJSON(t, s.handleDedupGCTrigger, "POST", "/api/v1/admin/dedup-gc")
	assert.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	// And quota-reconcile: the gate lives on the Server (the manager is not a runner).
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

func TestSmartDemotion_TickerSkipsWhileManualRunHolds(t *testing.T) {
	f := setupDemotionFixture(t, 1*tb, "standard")
	f.object("b", "idle", 100, 30, 20)

	require.True(t, f.runner.gate.tryAcquire()) // a manual run is in progress
	_, err := f.runner.RunOnceGuarded(context.Background(), false)
	assert.ErrorIs(t, err, errJobAlreadyRunning)
	assert.Equal(t, "idrive", f.backendOf("b", "idle"), "the guarded run moved nothing")
	f.runner.gate.release()

	res, err := f.runner.RunOnceGuarded(context.Background(), false)
	require.NoError(t, err)
	assert.Equal(t, 1, res.Demoted)

	gc, _ := setupGCFixture(t)
	require.True(t, gc.gate.tryAcquire())
	_, err = gc.RunOnceGuarded(context.Background())
	assert.ErrorIs(t, err, errJobAlreadyRunning)
	gc.gate.release()
}

// The manual trigger runs on a context that survives the request: a closed
// admin tab no longer cancels a half-done move (Review R13-08).
func TestAdminTriggerContext_SurvivesRequestCancel(t *testing.T) {
	rctx, rcancel := context.WithCancel(context.Background())
	req := httptest.NewRequest("POST", "/api/v1/admin/smart-demotion", nil).WithContext(rctx)
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
