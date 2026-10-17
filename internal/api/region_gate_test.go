package api

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	promtest "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/FairForge/vaultaire/internal/drivers"
	"github.com/FairForge/vaultaire/internal/engine"
)

// Prompt 10 Part A, the region gate. drivers.IDriveRegionAvailable means "a
// driver is registered for it" (WP-R7-1) and main.go registers a region
// driver whenever its key pair is set — on prod since 2026-10-11 every
// region pair answers AccessDenied to every object call, so the picker and
// both CreateBucket paths offered ten regions in which a bucket can store
// nothing (every PUT 503). A region is now offered only when its driver is
// registered AND healthy by the probe (the /health rule) AND its breaker is
// not open; a known, enabled region that is down answers 503 + Retry-After
// (S3) / 503 region_unavailable_now (management), never the 400 that stays
// for a region we do not know or have not enabled. The default region is
// always offered: it is served by the primary through the engine.

// regionGateServer is a management test server whose deployment serves the
// default region and eu-west-1 (registered, probed by the health checker).
func regionGateServer(t *testing.T) (*Server, sqlmock.Sqlmock) {
	t.Helper()
	s, mock, cleanup := newMgmtTestServer(t)
	t.Cleanup(cleanup)
	drivers.SetAvailableIDriveRegions("us-central-1", []string{"eu-west-1"})
	t.Cleanup(drivers.ResetAvailableIDriveRegions)
	s.healthChecker = NewBackendHealthChecker()
	s.healthChecker.RegisterBackend("idrive-eu-west-1")
	return s, mock
}

func expectNewBucket(mock sqlmock.Sqlmock, name string) {
	mock.ExpectQuery(`SELECT EXISTS\(SELECT 1 FROM buckets`).WithArgs("test-tenant", name).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM buckets WHERE tenant_id`).WithArgs("test-tenant").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
}

// downByTheProbe fails the region driver's probe twice in a row (after a
// success: the plain two-in-a-row rule).
func downByTheProbe(s *Server, backend string) {
	s.healthChecker.UpdateHealth(backend, true, time.Millisecond, nil)
	s.healthChecker.UpdateHealth(backend, false, 0, errors.New("AccessDenied"))
	s.healthChecker.UpdateHealth(backend, false, 0, errors.New("AccessDenied"))
}

func s3Create(t *testing.T, s *Server, bucket, region string) *httptest.ResponseRecorder {
	t.Helper()
	body := `<CreateBucketConfiguration><LocationConstraint>` + region + `</LocationConstraint></CreateBucketConfiguration>`
	req := httptest.NewRequest(http.MethodPut, "/"+bucket, bytes.NewBufferString(body))
	req = withTenantCtx(req, "test-tenant")
	w := httptest.NewRecorder()
	s.CreateBucket(w, req)
	return w
}

func regionRefusedCount(region, reason string) float64 {
	return promtest.ToFloat64(regionCreateRefused.WithLabelValues(region, reason))
}

func TestRegionGate_S3CreateBucketInARegionDownByTheProbeIs503(t *testing.T) {
	s, mock := regionGateServer(t)
	downByTheProbe(s, "idrive-eu-west-1")
	expectNewBucket(mock, "dead-region")
	before := regionRefusedCount("eu-west-1", "down")

	w := s3Create(t, s, "dead-region", "eu-west-1")

	assert.Equal(t, http.StatusServiceUnavailable, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "<Code>ServiceUnavailable</Code>")
	assert.Contains(t, w.Body.String(), "eu-west-1")
	assert.Contains(t, w.Body.String(), "temporarily unavailable")
	assert.Equal(t, "60", w.Header().Get("Retry-After"))
	assert.Equal(t, 1.0, regionRefusedCount("eu-west-1", "down")-before)
	assert.NoError(t, mock.ExpectationsWereMet(), "no bucket row is written")
}

func TestRegionGate_TheBucketRegionHeaderIsGatedToo(t *testing.T) {
	s, mock := regionGateServer(t)
	downByTheProbe(s, "idrive-eu-west-1")
	expectNewBucket(mock, "dead-region-hdr")
	req := httptest.NewRequest(http.MethodPut, "/dead-region-hdr", nil)
	req.Header.Set("x-amz-bucket-region", "eu-west-1")
	req = withTenantCtx(req, "test-tenant")
	w := httptest.NewRecorder()

	s.CreateBucket(w, req)

	assert.Equal(t, http.StatusServiceUnavailable, w.Code, w.Body.String())
}

func TestRegionGate_ManagementCreateInARegionDownByTheProbeIs503(t *testing.T) {
	s, mock := regionGateServer(t)
	downByTheProbe(s, "idrive-eu-west-1")
	expectNewBucket(mock, "dead-region")

	rec, resp := mgmtCreate(t, s, `{"name":"dead-region","region":"eu-west-1"}`)

	assert.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())
	e := resp["error"].(map[string]any)
	assert.Equal(t, "region_unavailable_now", e["code"])
	assert.Equal(t, "region", e["param"])
	assert.Contains(t, e["message"], "temporarily unavailable")
	assert.Equal(t, "60", rec.Header().Get("Retry-After"))
	assert.NoError(t, mock.ExpectationsWereMet())
}

func TestRegionGate_OneFailedProbeAfterASuccessIsStillOffered(t *testing.T) {
	s, _ := regionGateServer(t)
	s.healthChecker.UpdateHealth("idrive-eu-west-1", true, time.Millisecond, nil)
	s.healthChecker.UpdateHealth("idrive-eu-west-1", false, 0, errors.New("timeout"))

	assert.Equal(t, regionOffered, s.regionCreateState("eu-west-1"), "the /health rule: one failure after a success is not an outage")
}

func TestRegionGate_ARegionWhoseBreakerIsOpenIsDown(t *testing.T) {
	s, _ := regionGateServer(t)
	s.healthChecker.UpdateHealth("idrive-eu-west-1", true, time.Millisecond, nil)
	s.regionBreakers = func() map[string]string {
		return map[string]string{"idrive-eu-west-1": engine.StateOpen.String()}
	}

	assert.Equal(t, regionDown, s.regionCreateState("eu-west-1"), "probe green, breaker open: no write would land")

	s.regionBreakers = func() map[string]string {
		return map[string]string{"idrive-eu-west-1": engine.StateHalfOpen.String()}
	}
	assert.Equal(t, regionOffered, s.regionCreateState("eu-west-1"), "half-open lets the next call through (as CanAsk)")
}

func TestRegionGate_TheDefaultRegionIsAlwaysOffered(t *testing.T) {
	s, _ := regionGateServer(t)
	s.healthChecker.RegisterBackend("idrive")
	downByTheProbe(s, "idrive")
	s.regionBreakers = func() map[string]string { return map[string]string{"idrive": engine.StateOpen.String()} }

	assert.Equal(t, regionOffered, s.regionCreateState("us-central-1"), "the default region is served by the primary through the engine: /health's business")
}

func TestRegionGate_UnknownAndNotEnabledStay400(t *testing.T) {
	s, mock := regionGateServer(t)

	expectNewBucket(mock, "mars-bucket")
	beforeUnknown := regionRefusedCount("other", "unknown")
	w := s3Create(t, s, "mars-bucket", "mars-1")
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "InvalidLocationConstraint")
	assert.Equal(t, 1.0, regionRefusedCount("other", "unknown")-beforeUnknown, "an unknown id is counted under region=\"other\": the label is never caller input")

	expectNewBucket(mock, "tokyo-bucket")
	beforeNE := regionRefusedCount("ap-northeast-1", "not_enabled")
	w = s3Create(t, s, "tokyo-bucket", "ap-northeast-1")
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "InvalidLocationConstraint")
	assert.Equal(t, 1.0, regionRefusedCount("ap-northeast-1", "not_enabled")-beforeNE)
}

func TestRegionGate_AHealthyRegionIsCreated(t *testing.T) {
	s, mock := regionGateServer(t)
	s.healthChecker.UpdateHealth("idrive-eu-west-1", true, time.Millisecond, nil)
	expectNewBucket(mock, "live-region")
	mock.ExpectExec(`INSERT INTO buckets`).WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectQuery(`SELECT region FROM buckets`).WillReturnRows(sqlmock.NewRows([]string{"region"}).AddRow("eu-west-1"))

	out, err := s.createBucketRegistry(withTenantCtx(httptest.NewRequest(http.MethodPut, "/", nil), "test-tenant").Context(), "test-tenant", "live-region", "eu-west-1")

	require.NoError(t, err)
	assert.Equal(t, bucketCreateOK, out.state)
}

func TestRegionGate_AnOwnedBucketInADownRegionIsUnchanged(t *testing.T) {
	s, mock := regionGateServer(t)
	downByTheProbe(s, "idrive-eu-west-1")
	mock.ExpectQuery(`SELECT EXISTS\(SELECT 1 FROM buckets`).WithArgs("test-tenant", "mine").
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
	mock.ExpectQuery(`SELECT region FROM buckets`).WillReturnRows(sqlmock.NewRows([]string{"region"}).AddRow("eu-west-1"))

	w := s3Create(t, s, "mine", "eu-west-1")

	assert.Equal(t, http.StatusOK, w.Code, "an idempotent re-create of a bucket the tenant owns is not a new bucket in a dead region: %s", w.Body.String())
}

func TestRegionGate_ARefusalIsOneInfoLine(t *testing.T) {
	s, mock := regionGateServer(t)
	core, logs := observer.New(zap.InfoLevel)
	s.logger = zap.New(core)
	downByTheProbe(s, "idrive-eu-west-1")
	expectNewBucket(mock, "dead-region")

	_ = s3Create(t, s, "dead-region", "eu-west-1")

	lines := logs.FilterMessage("bucket create refused: region").All()
	require.Len(t, lines, 1)
	ctx := lines[0].ContextMap()
	assert.Equal(t, "test-tenant", ctx["tenant"])
	assert.Equal(t, "eu-west-1", ctx["region"])
	assert.Equal(t, "down", ctx["reason"])
}

func TestRegionGate_TheDashboardAsksTheSameRule(t *testing.T) {
	s, _ := regionGateServer(t)
	s.healthChecker.UpdateHealth("idrive-eu-west-1", true, time.Millisecond, nil)
	avail := s.regionAvailableNow
	assert.True(t, avail("eu-west-1"), "registered and its probe answered")
	downByTheProbe(s, "idrive-eu-west-1")
	assert.False(t, avail("eu-west-1"))
	assert.True(t, avail("us-central-1"))
	assert.False(t, avail("ap-northeast-1"), "not enabled is not available either (the picker tells the two apart itself)")
}

// Prod proof of #753 (2026-10-17 23:36 UTC): the picker rendered 60-odd
// seconds after the slot switch offered us-west-2 and us-west-4 — both dead
// — because their staggered first probes had not run yet and
// RegisterBackend seeds Healthy. "Healthy by the probe" means a probe said
// so: a region driver with probe state but no successful probe yet (never
// probed, or failed since boot) is not offered. A working region is then
// unavailable until its first probe answers (≤ one probe interval after
// boot); a create in that window is a 503 + Retry-After, not a dead bucket.
func TestRegionGate_ARegionNotYetProbedIsNotOffered(t *testing.T) {
	s, mock := regionGateServer(t) // idrive-eu-west-1 registered, no probe yet
	expectNewBucket(mock, "early-bucket")
	before := regionRefusedCount("eu-west-1", "down")

	w := s3Create(t, s, "early-bucket", "eu-west-1")

	assert.Equal(t, http.StatusServiceUnavailable, w.Code, w.Body.String())
	assert.Equal(t, 1.0, regionRefusedCount("eu-west-1", "down")-before)
	assert.False(t, s.regionAvailableNow("eu-west-1"), "the picker says the same")

	s.healthChecker.UpdateHealth("idrive-eu-west-1", true, time.Millisecond, nil)
	assert.True(t, s.regionAvailableNow("eu-west-1"), "its first successful probe offers it")
}
