package api

// RP80 (Stage 1a Prompt 51, Part 0; docs/reviews/WP-STRIPE-2-REVIEW.md):
// a sandbox is metered without being billed, its presigned links live an
// hour at most, it is never Public, its parent's suspension covers it and a
// lookup that fails never places a write on a paid backend.

import (
	"context"
	"database/sql"
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/auth"
	"github.com/FairForge/vaultaire/internal/config"
	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/FairForge/vaultaire/internal/modes"
	"github.com/FairForge/vaultaire/internal/usage"
)

// RP80-1: a sandbox past its allowance is paced at the throttle's floor
// with egress_throttle on, and the same decision is counted with it off;
// its bytes are in bandwidth_usage_daily under the SANDBOX id; the
// parent's rows, payments and entitlements are unchanged.
func TestSandbox_DownloadsArePacedNeverBilled(t *testing.T) {
	f := setupSandboxFixture(t)
	enforced := false
	cfg := usage.DefaultEgressThrottle()
	m := newEgressMeter(f.db, cfg, func(id string) bool { return enforced && id == f.sandboxID }, zap.NewNop())
	// The sandbox has downloaded 6 GiB this month: past its fixed 5 GiB.
	m.loadUsed = func(_ context.Context, id string, _ time.Time) (int64, error) {
		if id == f.sandboxID {
			return 6 << 30, nil
		}
		return 0, nil
	}
	f.srv.egress = m
	f.srv.bandwidthTracker = NewBandwidthTracker(f.db)
	t.Cleanup(func() {
		_, _ = f.db.Exec(`DELETE FROM bandwidth_usage_daily WHERE tenant_id = $1`, f.tenantID)
		_, _ = f.db.Exec(`DELETE FROM backend_bandwidth_daily WHERE backend_name = 'local'`)
	})

	a, err := usage.NewQuotaManager(f.db).EgressAllowance(context.Background(), f.sandboxID)
	require.NoError(t, err)
	assert.Equal(t, sandboxEgressAllowance, a.Bytes, "a sandbox's allowance is the fixed 5 GiB, never prices.json's")
	assert.Equal(t, usage.EgressSourceSandbox, a.Source)

	countParent := func(q string) int {
		var n int
		require.NoError(t, f.db.QueryRow(q, f.tenantID).Scan(&n))
		return n
	}
	payments := countParent(`SELECT COUNT(*) FROM payments WHERE tenant_id = $1`)
	entitlements := countParent(`SELECT COUNT(*) FROM entitlements WHERE tenant_id = $1`)

	b := "/sbm-" + f.sandboxID[8:16]
	require.Equal(t, http.StatusOK, f.test(http.MethodPut, b, nil, nil).StatusCode)
	body := []byte(strings.Repeat("d", 4096))
	require.Equal(t, http.StatusOK, f.test(http.MethodPut, b+"/d.bin", body, nil).StatusCode)

	// Flag off: nothing is slowed, the decision is counted.
	would := wouldCount(egressSurfaceS3)
	r := f.test(http.MethodGet, b+"/d.bin", nil, nil)
	require.Equal(t, http.StatusOK, r.StatusCode)
	got, _ := io.ReadAll(r.Body)
	assert.Equal(t, body, got)
	assert.Equal(t, would+1, wouldCount(egressSurfaceS3), "a sandbox past its allowance is the throttle's decision")

	// Flag on: paced at the floor.
	enforced = true
	engaged := engagedCount(egressSurfaceS3)
	r = f.test(http.MethodGet, b+"/d.bin", nil, nil)
	require.Equal(t, http.StatusOK, r.StatusCode)
	_, _ = io.Copy(io.Discard, r.Body)
	assert.Equal(t, engaged+1, engagedCount(egressSurfaceS3), "paced")
	assert.Equal(t, cfg.MinBytesPerSec, m.tenant(context.Background(), f.sandboxID).rateBps.Load(),
		"at EGRESS_THROTTLE_MIN_BYTES_PER_SEC: 5 GiB a month is below the floor")

	f.srv.bandwidthTracker.Flush()
	var sandboxEgress int64
	require.NoError(t, f.db.QueryRow(`SELECT COALESCE(SUM(egress_bytes), 0) FROM bandwidth_usage_daily WHERE tenant_id = $1`, f.sandboxID).Scan(&sandboxEgress))
	assert.Equal(t, int64(2*len(body)), sandboxEgress, "metered under the sandbox id")
	assert.Zero(t, countParent(`SELECT COUNT(*) FROM bandwidth_usage_daily WHERE tenant_id = $1`), "never the parent's")
	assert.Equal(t, payments, countParent(`SELECT COUNT(*) FROM payments WHERE tenant_id = $1`))
	assert.Equal(t, entitlements, countParent(`SELECT COUNT(*) FROM entitlements WHERE tenant_id = $1`))
}

// RP80-1: a VLT_TEST_ presigned GET lives at most an hour.
func TestTestKey_PresignCappedAtOneHour(t *testing.T) {
	f := setupSandboxFixture(t)
	b := "sbp-" + f.sandboxID[8:16]
	require.Equal(t, http.StatusOK, f.test(http.MethodPut, "/"+b, nil, nil).StatusCode)
	require.Equal(t, http.StatusOK, f.test(http.MethodPut, "/"+b+"/p.txt", []byte("presigned"), nil).StatusCode)

	get := func(expires int) *httptest.ResponseRecorder {
		u, _ := generatePresignedS3URL("https://"+f.host, f.tak, f.tsk, b, "p.txt", http.MethodGet, expires)
		req := httptest.NewRequest(http.MethodGet, u, nil)
		req.Host = f.host
		w := httptest.NewRecorder()
		f.srv.handleS3Request(w, req)
		return w
	}
	w := get(7200)
	require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	var e S3Error
	require.NoError(t, xml.Unmarshal(w.Body.Bytes(), &e))
	assert.Equal(t, ErrAuthorizationQueryParametersError, e.Code)
	assert.Contains(t, e.Message, "3600")

	w = get(3600)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, "presigned", w.Body.String())
}

// RP80-1: a sandbox bucket is never Public — not through the door, not at
// its creation.
func TestSandbox_NeverPublic(t *testing.T) {
	f := setupSandboxFixture(t)
	b := "sbpub-" + f.sandboxID[8:16]
	require.Equal(t, http.StatusOK, f.test(http.MethodPut, "/"+b, nil, nil).StatusCode)

	public := modes.VisibilityPublic
	_, err := modes.SetBucket(context.Background(), f.db, f.sandboxID, b, modes.Change{Visibility: &public})
	require.ErrorIs(t, err, modes.ErrSandboxNeverPublic)
	var vis string
	require.NoError(t, f.db.QueryRow(`SELECT visibility FROM buckets WHERE tenant_id = $1 AND name = $2`, f.sandboxID, b).Scan(&vis))
	assert.Equal(t, modes.VisibilityPrivate, vis)

	assert.Equal(t, "sandbox_never_public", modes.ErrSandboxNeverPublic.Error(), "the 409's code")

	r := f.test(http.MethodPut, "/"+b+"-acl", nil, map[string]string{"x-amz-acl": "public-read"})
	require.Equal(t, http.StatusBadRequest, r.StatusCode)
	raw, _ := io.ReadAll(r.Body)
	var e S3Error
	require.NoError(t, xml.Unmarshal(raw, &e))
	assert.Equal(t, ErrInvalidRequest, e.Code)
	var n int
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM buckets WHERE tenant_id = $1 AND name = $2`, f.sandboxID, b+"-acl").Scan(&n))
	assert.Zero(t, n, "nothing created")

	// A live tenant's public-read create is unchanged (the header is not
	// honoured; the bucket is born Standard).
	pub := live(f, http.MethodPut, "/live-"+f.sandboxID[8:16], map[string]string{"x-amz-acl": "public-read"})
	assert.Equal(t, http.StatusOK, pub.Code, pub.Body.String())
}

func live(f *sandboxFixture, method, path string, hdr map[string]string) *httptest.ResponseRecorder {
	return f.do(method, path, nil, f.ak, f.sk, hdr)
}

// RP80-2: the parent's suspension covers its sandbox: a test-key request
// is 403 AccountSuspended and no test key is minted.
func TestSandbox_ParentSuspendedRefusesTestKeys(t *testing.T) {
	f := setupSandboxFixture(t)
	b := "/sbs-" + f.sandboxID[8:16]
	require.Equal(t, http.StatusOK, f.test(http.MethodPut, b, nil, nil).StatusCode)
	f.exec(`UPDATE tenants SET suspended_at = NOW() WHERE id = $1`, f.tenantID)
	t.Cleanup(func() { _, _ = f.db.Exec(`UPDATE tenants SET suspended_at = NULL WHERE id = $1`, f.tenantID) })

	r := f.test(http.MethodPut, b+"/x.txt", []byte("x"), nil)
	require.Equal(t, http.StatusForbidden, r.StatusCode)
	raw, _ := io.ReadAll(r.Body)
	assert.Contains(t, string(raw), "AccountSuspended")

	assert.True(t, isTenantSuspended(context.Background(), f.db, f.sandboxID))
	assert.True(t, isTenantSuspended(context.Background(), f.db, f.tenantID))

	// The mint: POST /api/v1/manage/keys {test: true} of a suspended
	// tenant's user is 403.
	db := setupSandboxDB(t)
	authSvc := auth.NewAuthService(nil, db)
	user, _, _, err := authSvc.CreateUserWithTenant(context.Background(), "susp-testkey-"+uuid.NewString()[:8]+"@stored.ge", "Str0ngPassw0rd!", "x")
	require.NoError(t, err)
	t.Cleanup(func() { cleanupSandboxOf(db, user.TenantID) })
	_, err = db.Exec(`UPDATE tenants SET suspended_at = NOW() WHERE id = $1`, user.TenantID)
	require.NoError(t, err)
	s := &Server{logger: zap.NewNop(), auth: authSvc, db: db, config: &config.Config{Server: config.ServerConfig{Port: 8000}}}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/manage/keys", strings.NewReader(`{"name":"ci","test":true}`))
	ctx := context.WithValue(req.Context(), userIDKey, user.ID)
	req = req.WithContext(context.WithValue(ctx, tenantIDKey, user.TenantID))
	w := httptest.NewRecorder()
	s.handleMgmtCreateKey(w, req)
	assert.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "account_suspended")
	sb, err := auth.SandboxOf(context.Background(), db, user.TenantID)
	require.NoError(t, err)
	assert.Empty(t, sb, "no sandbox made")
}

// RP80-5: a sandbox lookup that fails is a 503, never a paid backend.
func TestSandbox_LookupErrorFailsClosed(t *testing.T) {
	f := setupSandboxFixture(t)
	failing := "lookup-fail-" + f.sandboxID[8:16]
	orig := parentOfSandbox
	parentOfSandbox = func(ctx context.Context, db *sql.DB, id string) (string, error) {
		if id == failing {
			return "", errors.New("connection refused")
		}
		return orig(ctx, db, id)
	}
	t.Cleanup(func() { parentOfSandbox = orig })

	_, err := placeOn(context.Background(), f.db, f.srv.engine, failing, "b", failing+"_b", "k", strings.NewReader("x"))
	require.Error(t, err)
	assert.ErrorIs(t, err, engine.ErrAllBackendsUnavailable, "a 503, never the primary")
	for _, d := range f.paid {
		assert.Zero(t, d.calls.Load(), d.name)
	}

	a := &S3ToEngine{db: f.db}
	p := a.decidePlacement(context.Background(), placementRequest{TenantID: failing, Bucket: "b", Size: 1 << 30})
	require.NotNil(t, p.Refusal, "the placement refuses rather than chunk to the primary")
	assert.Equal(t, http.StatusServiceUnavailable, p.Refusal.Status)
	_, cached := sandboxCache.Load(failing)
	assert.False(t, cached, "a failed lookup is never cached")
}

// RB8-2: the parent's JWT reads its sandbox's request log with ?mode=test;
// without it, its own.
func TestRequestLog_ModeTestReadsTheSandbox(t *testing.T) {
	f := setupSandboxFixture(t)
	b := "/sbl-" + f.sandboxID[8:16]
	require.Equal(t, http.StatusOK, f.test(http.MethodPut, b, nil, nil).StatusCode)
	require.Equal(t, http.StatusOK, f.test(http.MethodPut, b+"/t.txt", []byte("t"), nil).StatusCode)
	lw := live(f, http.MethodGet, "/"+f.bucket+"?list-type=2", nil)
	require.Equal(t, http.StatusOK, lw.Code, lw.Body.String())

	code, _, rows := f.manage("mode=test")
	require.Equal(t, http.StatusOK, code)
	require.Len(t, rows, 2)
	for _, r := range rows {
		assert.Contains(t, []string{"CreateBucket", "PutObject"}, r.Op)
	}
	code, _, rows = f.manage("")
	require.Equal(t, http.StatusOK, code)
	require.Len(t, rows, 1, "live: its own")
	assert.Equal(t, "ListObjects", rows[0].Op)
	code, _, rows = f.manage("mode=live")
	require.Equal(t, http.StatusOK, code)
	assert.Len(t, rows, 1)
	code, _, _ = f.manage("mode=nope")
	assert.Equal(t, http.StatusBadRequest, code)

	// A tenant with no sandbox reads an empty test log.
	r := httptest.NewRequest(http.MethodGet, "/api/v1/manage/requests?mode=test", nil)
	r = r.WithContext(context.WithValue(r.Context(), tenantIDKey, "no-sandbox-"+f.sandboxID[8:16]))
	w := httptest.NewRecorder()
	f.srv.handleMgmtRequests(w, r)
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), `"data":[]`)
}
