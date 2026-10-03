package api

// Review R11 (docs/reviews/R11-management-apis.md) — negative and parity
// tests for the JSON APIs: compliance admin gate (R11-01), user-API mocks
// removed (R11-02), STS parent scope (R11-03), customer webhook target
// policy (R11-04), management bucket-create parity (R11-05), the RBAC /
// patterns / quota stubs gone (R11-06), dead-by-auth routes mounted under
// the JWT (R11-07), the audit trail (R11-09), auth-failure metrics
// (R11-10) and honest rate-limit headers (R11-14).

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/FairForge/vaultaire/internal/audit"
	"github.com/FairForge/vaultaire/internal/auth"
	"github.com/FairForge/vaultaire/internal/config"
	"github.com/FairForge/vaultaire/internal/drivers"
	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	promtest "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// productionRoutes walks the full router (nil DB is the supported dev path)
// and returns "METHOD /pattern" keys.
func productionRoutes(t *testing.T) map[string]bool {
	t.Helper()
	s := NewServer(
		&config.Config{Server: config.ServerConfig{Port: 8000}},
		zap.NewNop(),
		engine.NewEngine(nil, zap.NewNop(), nil),
		nil, nil,
	)
	routes := map[string]bool{}
	require.NoError(t, chi.Walk(s.GetRouter(),
		func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
			routes[method+" "+strings.TrimSuffix(route, "/")] = true
			return nil
		}))
	return routes
}

// --- R11-06: the unauthenticated authority stubs are gone ---------------

func TestR11_RemovedRoutes_NotMounted(t *testing.T) {
	routes := productionRoutes(t)
	for key := range routes {
		route := key[strings.Index(key, " ")+1:]
		for _, gone := range []string{"/api/rbac", "/api/patterns", "/api/v1/quota", "/api/v1/usage", "/api/v1/presigned"} {
			assert.False(t, route == gone || strings.HasPrefix(route, gone+"/"),
				"route %s must not exist any more (Review R11-06/07)", key)
		}
	}
	// The mocks (R11-02) are gone too; the real routes stayed.
	for _, gone := range []string{
		"GET /api/v1/user/profile", "PUT /api/v1/user/profile", "GET /api/v1/user/preferences",
		"PUT /api/v1/user/preferences", "GET /api/v1/user/activity", "POST /api/v1/user/mfa/enable",
		"POST /api/v1/user/mfa/disable", "GET /api/v1/user/mfa/backup-codes", "PUT /api/v1/user",
	} {
		assert.False(t, routes[gone], "%s was a mock and must be gone", gone)
	}
	for _, kept := range []string{
		"GET /api/v1/user", "DELETE /api/v1/user", "GET /api/v1/user/quota", "GET /api/v1/user/quota/history",
		"GET /api/v1/user/usage", "GET /api/v1/user/usage/alerts", "GET /api/v1/user/presigned",
		"GET /api/v1/user/apikeys", "GET /api/v1/admin/audit", "POST /api/v1/sts/token",
	} {
		assert.True(t, routes[kept], "%s must be mounted", kept)
	}
}

// --- R11-01: /api/compliance is admin-only ------------------------------

type adminGateFixture struct {
	s       *Server
	adminID string
	userID  string
}

// newAdminGateFixture seeds one admin and one plain user (real users rows,
// so requireAdmin's role query runs for real) and mounts a probe route
// under the same middleware chain /api/compliance uses.
func newAdminGateFixture(t *testing.T) *adminGateFixture {
	t.Helper()
	db := setupTestDBFixed(t)
	if db == nil {
		return nil
	}
	adminID, userID := uuid.NewString(), uuid.NewString()
	for id, role := range map[string]string{adminID: "admin", userID: "user"} {
		_, err := db.Exec(`INSERT INTO users (id, email, password_hash, role) VALUES ($1, $2, 'x', $3)`,
			id, fmt.Sprintf("r11-%s-%d@test.local", role, time.Now().UnixNano()), role)
		require.NoError(t, err)
	}
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM audit_logs WHERE user_id::text IN ($1, $2) OR performed_by::text IN ($1, $2)`, adminID, userID)
		_, _ = db.Exec(`DELETE FROM users WHERE id IN ($1, $2)`, adminID, userID)
	})
	s := &Server{logger: zap.NewNop(), router: chi.NewRouter(), db: db,
		config: &config.Config{Server: config.ServerConfig{Port: 8000}}}
	s.router.Route("/api/compliance", func(r chi.Router) {
		r.Use(func(next http.Handler) http.Handler { // stand-in for requireJWT
			return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				ctx := context.WithValue(req.Context(), userIDKey, req.Header.Get("X-Test-User"))
				next.ServeHTTP(w, req.WithContext(ctx))
			})
		})
		r.Use(s.requireAdminMiddleware)
		r.Get("/breach", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	})
	return &adminGateFixture{s: s, adminID: adminID, userID: userID}
}

func TestR11_ComplianceRoutes_RequireAdmin(t *testing.T) {
	f := newAdminGateFixture(t)
	if f == nil {
		return
	}
	for _, tc := range []struct {
		user string
		want int
	}{
		{f.userID, http.StatusForbidden},
		{"", http.StatusForbidden},
		{uuid.NewString(), http.StatusForbidden},
		{f.adminID, http.StatusOK},
	} {
		req := httptest.NewRequest(http.MethodGet, "/api/compliance/breach", nil)
		req.Header.Set("X-Test-User", tc.user)
		rec := httptest.NewRecorder()
		f.s.router.ServeHTTP(rec, req)
		assert.Equal(t, tc.want, rec.Code, "user %q", tc.user)
	}
}

// --- R11-02: GET /api/v1/user is the account as stored ------------------

func TestR11_UserInfo_ReadsTheDatabase(t *testing.T) {
	db := setupTestDBFixed(t)
	if db == nil {
		return
	}
	userID := uuid.NewString()
	email := fmt.Sprintf("r11-info-%d@test.local", time.Now().UnixNano())
	_, err := db.Exec(`INSERT INTO users (id, email, password_hash, company, status, deletion_scheduled_at)
		VALUES ($1, $2, 'x', 'R11 Co', 'pending_deletion', NOW() + INTERVAL '30 days')`, userID, email)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM users WHERE id = $1`, userID) })

	s := &Server{logger: zap.NewNop(), db: db, auth: auth.NewAuthService(nil, nil),
		quotaManager: &stubQuotaManager{used: 1, limit: 10, tier: "free"},
		config:       &config.Config{Server: config.ServerConfig{Port: 8000}}}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/user", nil)
	ctx := context.WithValue(req.Context(), userIDKey, userID)
	ctx = context.WithValue(ctx, tenantIDKey, "tenant-r11")
	ctx = context.WithValue(ctx, emailKey, email)
	rec := httptest.NewRecorder()
	s.handleGetUserInfo(rec, req.WithContext(ctx))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var info UserInfo
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &info))
	assert.Equal(t, "R11 Co", info.Company)
	assert.Equal(t, "pending_deletion", info.Status)
	require.NotNil(t, info.DeletionScheduledAt, "the grace-period date is reported (D-16)")
	assert.False(t, info.MFAEnabled)
	assert.False(t, info.CreatedAt.IsZero())
	assert.Equal(t, "free", info.Quota.Tier)

	// An unknown user is 404, not a fabricated profile.
	req2 := httptest.NewRequest(http.MethodGet, "/api/v1/user", nil)
	ctx2 := context.WithValue(req2.Context(), userIDKey, uuid.NewString())
	ctx2 = context.WithValue(ctx2, tenantIDKey, "tenant-r11")
	rec2 := httptest.NewRecorder()
	s.handleGetUserInfo(rec2, req2.WithContext(ctx2))
	assert.Equal(t, http.StatusNotFound, rec2.Code)
}

// --- R11-03: STS mints for a fresh account and narrows to a named key ----

func stsFixture(t *testing.T) (*Server, *auth.User) {
	t.Helper()
	authSvc := auth.NewAuthService(nil, nil)
	user, _, _, err := authSvc.CreateUserWithTenant(context.Background(),
		fmt.Sprintf("sts-%d@test.local", time.Now().UnixNano()), "Str0ngPassw0rd!", "x")
	require.NoError(t, err)
	s := &Server{logger: zap.NewNop(), auth: authSvc, config: &config.Config{Server: config.ServerConfig{Port: 8000}}}
	return s, user
}

func stsCall(t *testing.T, s *Server, user *auth.User, body string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/sts/token", strings.NewReader(body))
	ctx := context.WithValue(req.Context(), userIDKey, user.ID)
	ctx = context.WithValue(ctx, tenantIDKey, user.TenantID)
	rec := httptest.NewRecorder()
	s.handleSTSCreateToken(rec, req.WithContext(ctx))
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	return rec, resp
}

func TestR11_STS_FreshAccountMints(t *testing.T) {
	s, user := stsFixture(t)
	rec, resp := stsCall(t, s, user, `{"duration_seconds":900}`)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	assert.True(t, strings.HasPrefix(resp["access_key"].(string), "ASIA"))

	rec2, _ := stsCall(t, s, user, `{"permissions":["GetObject"],"bucket_scope":["b1"]}`)
	assert.Equal(t, http.StatusCreated, rec2.Code, rec2.Body.String())
}

func TestR11_STS_ParentKeyNarrowsAndIsOwned(t *testing.T) {
	s, user := stsFixture(t)
	key, err := s.auth.GenerateAPIKey(context.Background(), user.ID, "ro", &auth.KeyCreateOptions{
		Permissions: []string{"GetObject"}, BucketScope: []string{"only"}})
	require.NoError(t, err)

	// Requesting more than the parent has → no overlap → 400.
	rec, resp := stsCall(t, s, user, `{"parent_key_id":"`+key.ID+`","permissions":["PutObject"]}`)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Equal(t, "scope_error", resp["error"].(map[string]any)["code"])

	// Within the parent → token, still bounded by the parent's bucket.
	rec, _ = stsCall(t, s, user, `{"parent_key_id":"`+key.ID+`"}`)
	assert.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	// Another user's key is not a valid parent.
	other, _, _, err := s.auth.CreateUserWithTenant(context.Background(),
		fmt.Sprintf("sts-other-%d@test.local", time.Now().UnixNano()), "Str0ngPassw0rd!", "y")
	require.NoError(t, err)
	rec, resp = stsCall(t, s, other, `{"parent_key_id":"`+key.ID+`"}`)
	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Equal(t, "parent_key_not_found", resp["error"].(map[string]any)["code"])

	// A revoked key is not a valid parent.
	require.NoError(t, s.auth.RevokeAPIKey(context.Background(), user.ID, key.ID))
	rec, resp = stsCall(t, s, user, `{"parent_key_id":"`+key.ID+`"}`)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Equal(t, "parent_key_revoked", resp["error"].(map[string]any)["code"])
}

func TestR11_STS_PersistsUnscopedToken(t *testing.T) {
	// The live build failed every unscoped mint with
	// `null value in column "bucket_scope"` — nil slices became NULL.
	db := setupTestDBFixed(t)
	if db == nil {
		return
	}
	tenant := "tenant-sts-" + uuid.NewString()[:8]
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM sts_tokens WHERE tenant_id = $1`, tenant) })
	tok, err := auth.GenerateSTSToken(context.Background(), db, tenant, "tenant:"+tenant,
		&auth.KeyScope{Permissions: []string{"*"}}, auth.STSRequest{})
	require.NoError(t, err)
	var n int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM sts_tokens WHERE access_key = $1 AND bucket_scope = '{}' AND ip_restrict = '{}'`, tok.AccessKey).Scan(&n))
	assert.Equal(t, 1, n)

	// A scope error is the caller's (400, typed); it never carries SQL.
	_, err = auth.GenerateSTSToken(context.Background(), db, tenant, "k", &auth.KeyScope{Permissions: []string{"GetObject"}},
		auth.STSRequest{Permissions: []string{"PutObject"}})
	assert.ErrorIs(t, err, auth.ErrSTSScope)
}

// --- R11-04: customer webhook targets follow the R4 policy --------------

func TestR11_Webhooks_RefusePrivateTargets(t *testing.T) {
	webhookAllowPrivateTargets.Store(false) // the production policy
	s, mock, cleanup := newWebhookTestServer(t)
	defer cleanup()

	bad := []string{
		"http://127.0.0.1:8098/hook",
		"http://169.254.169.254/latest/meta-data/",
		"http://10.0.0.5/hook",
		"http://localhost/hook",
		"ftp://example.com/hook",
		"http://user:pw@example.com/hook",
		"https://[::1]/hook",
	}
	for _, u := range bad {
		body := fmt.Sprintf(`{"url":%q,"events":["*"]}`, u)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks", strings.NewReader(body))
		rec := httptest.NewRecorder()
		s.router.ServeHTTP(rec, req)
		assert.Equal(t, http.StatusBadRequest, rec.Code, "create %s", u)
		var resp map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &resp)
		assert.Equal(t, "invalid_url", resp["error"].(map[string]any)["code"], u)

		// PATCH is the second entry point for the same field.
		req = httptest.NewRequest(http.MethodPatch, "/api/v1/webhooks/wh-1", strings.NewReader(fmt.Sprintf(`{"url":%q}`, u)))
		rec = httptest.NewRecorder()
		s.router.ServeHTTP(rec, req)
		assert.Equal(t, http.StatusBadRequest, rec.Code, "update %s", u)
	}
	// Nothing reached the database.
	assert.NoError(t, mock.ExpectationsWereMet())
}

func TestR11_WebhookDispatch_UsesGuardedClient(t *testing.T) {
	// A loopback endpoint stored before the fix (or through any future
	// gap) must not be dialled by the dispatcher either.
	webhookAllowPrivateTargets.Store(false)
	hit := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hit <- struct{}{}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	mock.ExpectQuery(`SELECT id, url, event_filter, secret`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "url", "event_filter", "secret"}).
			AddRow("wh-1", srv.URL+"/hook", "{*}", "whsec_x"))
	mock.ExpectExec(`INSERT INTO webhook_deliveries`).
		WithArgs(sqlmock.AnyArg(), "wh-1", "ev-1", "failed", 0, sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))

	dispatchWebhooks(db, zap.NewNop(), "ev-1", "webhook.test", "t1", []byte(`{}`))

	select {
	case <-hit:
		t.Fatal("the dispatcher connected to a loopback target")
	case <-time.After(200 * time.Millisecond):
	}
	assert.NoError(t, mock.ExpectationsWereMet())
}

// --- R11-05: management bucket create = S3 CreateBucket ------------------

func mgmtCreate(t *testing.T, s *Server, body string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/manage/buckets", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.router.ServeHTTP(rec, req)
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	return rec, resp
}

func TestR11_ManagementCreateBucket_FreeTierCap(t *testing.T) {
	s, mock, cleanup := newMgmtTestServer(t)
	defer cleanup()
	s.quotaManager = &stubQuotaManager{used: 0, limit: 1, tier: "free"}

	mock.ExpectQuery(`SELECT EXISTS\(SELECT 1 FROM buckets`).WithArgs("test-tenant", "second").
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM buckets WHERE tenant_id`).WithArgs("test-tenant").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))

	rec, resp := mgmtCreate(t, s, `{"name":"second"}`)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Equal(t, "free_tier_bucket_limit", resp["error"].(map[string]any)["code"])
	assert.NoError(t, mock.ExpectationsWereMet(), "no row may be inserted past the cap")
}

func TestR11_ManagementCreateBucket_HardCap(t *testing.T) {
	s, mock, cleanup := newMgmtTestServer(t)
	defer cleanup()
	mock.ExpectQuery(`SELECT EXISTS\(SELECT 1 FROM buckets`).WithArgs("test-tenant", "bkt-hard").
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM buckets WHERE tenant_id`).WithArgs("test-tenant").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(maxBucketsPerTenant))
	rec, resp := mgmtCreate(t, s, `{"name":"bkt-hard"}`)
	assert.Equal(t, http.StatusConflict, rec.Code)
	assert.Equal(t, "bucket_limit_exceeded", resp["error"].(map[string]any)["code"])
}

func TestR11_ManagementCreateBucket_RegionRules(t *testing.T) {
	s, mock, cleanup := newMgmtTestServer(t)
	defer cleanup()

	// Unknown region id → 400 (it used to be silently replaced by the default).
	mock.ExpectQuery(`SELECT EXISTS\(SELECT 1 FROM buckets`).WithArgs("test-tenant", "bkt-one").
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM buckets WHERE tenant_id`).WithArgs("test-tenant").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	rec, resp := mgmtCreate(t, s, `{"name":"bkt-one","region":"mars-1"}`)
	assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assert.Equal(t, "invalid_region", resp["error"].(map[string]any)["code"])

	// A real region this deployment has no driver for → 400 (WP-R7-1).
	drivers.SetAvailableIDriveRegions("us-central-1", nil)
	t.Cleanup(drivers.ResetAvailableIDriveRegions)
	mock.ExpectQuery(`SELECT EXISTS\(SELECT 1 FROM buckets`).WithArgs("test-tenant", "bkt-two").
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM buckets WHERE tenant_id`).WithArgs("test-tenant").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	rec, resp = mgmtCreate(t, s, `{"name":"bkt-two","region":"eu-west-1"}`)
	assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assert.Equal(t, "region_unavailable", resp["error"].(map[string]any)["code"])
	assert.NoError(t, mock.ExpectationsWereMet())
}

// --- R11-07: the quota/usage handlers read the JWT tenant ----------------

func TestR11_UserQuota_ReadsJWTTenant(t *testing.T) {
	s := &Server{logger: zap.NewNop(), quotaManager: &stubQuotaManager{used: 500, limit: 1000, tier: "starter"},
		config: &config.Config{Server: config.ServerConfig{Port: 8000}}}
	for name, h := range map[string]http.HandlerFunc{
		"quota": s.handleGetQuota, "history": s.handleGetQuotaHistory,
		"usage": s.handleGetUsageStats, "alerts": s.handleGetUsageAlerts,
	} {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/user/"+name, nil)
		rec := httptest.NewRecorder()
		h(rec, req.WithContext(context.WithValue(req.Context(), tenantIDKey, "t1")))
		assert.Equal(t, http.StatusOK, rec.Code, name)

		rec = httptest.NewRecorder()
		h(rec, req) // no tenant → 401, never a default tenant
		assert.Equal(t, http.StatusUnauthorized, rec.Code, name)
	}
}

// --- R11-09: the audit trail -------------------------------------------

func TestR11_KeyLifecycle_WritesAuditRows(t *testing.T) {
	db := setupTestDBFixed(t)
	if db == nil {
		return
	}
	authSvc := auth.NewAuthService(nil, db)
	email := fmt.Sprintf("r11-audit-%d@test.local", time.Now().UnixNano())
	user, tenant, _, err := authSvc.CreateUserWithTenant(context.Background(), email, "Str0ngPassw0rd!", "R11")
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM audit_logs WHERE tenant_id = $1`, tenant.ID)
		_, _ = db.Exec(`DELETE FROM api_keys WHERE user_id = $1`, user.ID)
		_, _ = db.Exec(`DELETE FROM tenant_quotas WHERE tenant_id = $1`, tenant.ID)
		_, _ = db.Exec(`DELETE FROM tenants WHERE id = $1`, tenant.ID)
		_, _ = db.Exec(`DELETE FROM users WHERE id = $1`, user.ID)
	})

	ctx := audit.WithRequest(context.Background(), "198.51.100.7", "r11-test")
	key, err := authSvc.GenerateAPIKey(ctx, user.ID, "k", &auth.KeyCreateOptions{Permissions: []string{"GetObject"}})
	require.NoError(t, err)
	require.NoError(t, authSvc.RevokeAPIKey(ctx, user.ID, key.ID))
	require.NoError(t, authSvc.ChangePassword(ctx, user.ID, "Str0ngPassw0rd!", "An0therStr0ng!"))

	rows, err := db.Query(`SELECT action, resource, COALESCE(host(ip),''), COALESCE(performed_by::text,'') FROM audit_logs WHERE tenant_id = $1 ORDER BY timestamp`, tenant.ID)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	var actions []string
	for rows.Next() {
		var action, resource, ip, by string
		require.NoError(t, rows.Scan(&action, &resource, &ip, &by))
		actions = append(actions, action)
		if action == "key.revoked" {
			assert.Equal(t, "key:"+key.ID, resource)
			assert.Equal(t, "198.51.100.7", ip)
			assert.Equal(t, user.ID, by)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate rows: %v", err)
	}
	assert.Equal(t, []string{"account.created", "key.created", "key.revoked", "auth.password_changed"}, actions)
}

func TestR11_AdminFlagSet_WritesAuditRow(t *testing.T) {
	f := setupAdminFlagsFixture(t)
	if f == nil {
		return
	}
	t.Cleanup(func() { _, _ = f.db.Exec(`DELETE FROM audit_logs WHERE resource = $1`, "flag:"+f.key) })

	rec := f.do(t, f.adminID, http.MethodPut, "/api/v1/admin/flags/"+f.key, map[string]any{"enabled": true})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	rec = f.do(t, f.adminID, http.MethodDelete, "/api/v1/admin/flags/"+f.key, nil)
	require.Equal(t, http.StatusOK, rec.Code)

	var n int
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM audit_logs WHERE resource = $1 AND action IN ('flag.set','flag.unset')`, "flag:"+f.key).Scan(&n))
	assert.Equal(t, 2, n, "one row per flip")

	// The read side: an admin lists them, a plain user cannot.
	page, err := audit.List(context.Background(), f.db, audit.Filter{Action: "flag.set", Limit: 5})
	require.NoError(t, err)
	assert.NotEmpty(t, page.Rows)
	// A non-admin trying to read the trail through the API is refused.
	f.server.router.Route("/api/v1/admin-audit-probe", func(r chi.Router) {
		r.Use(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				next.ServeHTTP(w, req.WithContext(context.WithValue(req.Context(), userIDKey, req.Header.Get("X-Test-User"))))
			})
		})
		r.Get("/", f.server.requireAdmin(f.server.handleAdminAuditList))
	})
	rec = f.do(t, f.userID, http.MethodGet, "/api/v1/admin-audit-probe/", nil)
	assert.Equal(t, http.StatusForbidden, rec.Code)
	rec = f.do(t, f.adminID, http.MethodGet, "/api/v1/admin-audit-probe/?action=flag.set&limit=1", nil)
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	rec = f.do(t, f.adminID, http.MethodGet, "/api/v1/admin-audit-probe/?cursor=garbage", nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// --- R11-10: auth-failure metrics --------------------------------------

func TestR11_AuthFailureReasons(t *testing.T) {
	cases := []struct {
		err    error
		reason string
		known  bool
	}{
		{auth.ErrUnknownAccessKey, "unknown_access_key", false},
		{auth.ErrAccessKeyRevoked, "revoked", true},
		{fmt.Errorf("%w: the parent key of this token is revoked", auth.ErrAccessKeyRevoked), "revoked", true},
		{fmt.Errorf("wrap: %w", auth.ErrSignatureMismatch), "signature_mismatch", true},
		{auth.ErrRequestTimeSkewed, "time_skewed", true},
		{auth.ErrInvalidContentSHA256, "invalid_content_sha256", true},
		{errors.New("missing authorization"), "missing_authorization", false},
		{errors.New("expired STS token"), "expired", true},
		{errors.New("auth lookup failed: pq: boom"), "lookup_error", false},
		{errors.New("invalid authorization format"), "malformed", false},
	}
	for _, c := range cases {
		reason, known := authFailureReason(c.err)
		assert.Equal(t, c.reason, reason, c.err.Error())
		assert.Equal(t, c.known, known, c.err.Error())
	}
	r, k := presignFailureReason(errors.New(ErrExpiredPresignedRequest))
	assert.Equal(t, "presign_expired", r)
	assert.True(t, k)
	r, k = presignFailureReason(errors.New(ErrAccessDenied))
	assert.Equal(t, "presign_unknown_key", r)
	assert.False(t, k)
	r, k = presignFailureReason(presignAuthError(auth.ErrAccessKeyRevoked))
	assert.Equal(t, "presign_revoked", r, "a presigned URL of a rotated key is known-and-dead")
	assert.True(t, k)
	assert.Equal(t, ErrInvalidAccessKeyId, presignAuthError(auth.ErrAccessKeyRevoked).Error())
}

func TestR11_AuthFailureMetrics_KnownKeyHashedUnknownNot(t *testing.T) {
	ak := "VKr11metric" + uuid.NewString()[:8]
	req := httptest.NewRequest(http.MethodGet, "/b/k", nil)
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+ak+"/20260929/us-east-1/s3/aws4_request, SignedHeaders=host, Signature=abc")

	before := promtest.ToFloat64(authFailures.WithLabelValues("signature_mismatch", "true"))
	recordAuthFailure(req, "signature_mismatch", true)
	assert.Equal(t, before+1, promtest.ToFloat64(authFailures.WithLabelValues("signature_mismatch", "true")))
	assert.Equal(t, float64(1), promtest.ToFloat64(authFailuresByKey.WithLabelValues(accessKeyHash(ak))),
		"a real key gets its own bounded hash series")

	unknown := "VKunknown" + uuid.NewString()[:8]
	req2 := httptest.NewRequest(http.MethodGet, "/b/k", nil)
	req2.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+unknown+"/20260929/us-east-1/s3/aws4_request, SignedHeaders=host, Signature=abc")
	recordAuthFailure(req2, "unknown_access_key", false)
	assert.Equal(t, float64(0), promtest.ToFloat64(authFailuresByKey.WithLabelValues(accessKeyHash(unknown))),
		"unknown ids are never hashed into a series (unbounded cardinality)")
	assert.Len(t, accessKeyHash(ak), 8)
	assert.NotContains(t, accessKeyHash(ak), ak[:6])
}

func TestR11_S3AuthFailure_IncrementsMetric(t *testing.T) {
	s, mock, cleanup := newMgmtTestServer(t)
	defer cleanup()
	s.router.HandleFunc("/*", s.handleS3Request)
	mock.ExpectQuery(`FROM api_keys ak`).WillReturnRows(sqlmock.NewRows([]string{"tenant_id"}))

	before := promtest.ToFloat64(authFailures.WithLabelValues("unknown_access_key", "false"))
	req := httptest.NewRequest(http.MethodGet, "/some-bucket/key", nil)
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=VKnobody/20260929/us-east-1/s3/aws4_request, SignedHeaders=host, Signature=abc")
	req.Header.Set("x-amz-date", "20260929T000000Z")
	rec := httptest.NewRecorder()
	s.router.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Equal(t, before+1, promtest.ToFloat64(authFailures.WithLabelValues("unknown_access_key", "false")))
}

// --- R11-14: honest rate-limit headers -----------------------------------

func TestR11_RateLimit_ResetIsTheRefillTime(t *testing.T) {
	rl := NewManagementRateLimiter()
	base := time.Unix(1_800_000_000, 0)
	now := base
	rl.now = func() time.Time { return now }
	h := rl.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	call := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/manage/usage", nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req.WithContext(context.WithValue(req.Context(), tenantIDKey, "t-rl")))
		return rec
	}
	var last *httptest.ResponseRecorder
	for i := 0; i < 10; i++ { // the burst
		last = call()
		require.Equal(t, http.StatusOK, last.Code, "request %d", i)
	}
	// After the burst the bucket refills at 100/min: a full refill is 6 s away.
	var resetUnix int64
	_, _ = fmt.Sscan(last.Header().Get("X-RateLimit-Reset"), &resetUnix)
	assert.InDelta(t, base.Unix()+6, resetUnix, 1, "Reset = when the bucket is full again, not now+0.6s")

	rec := call() // 11th → 429
	require.Equal(t, http.StatusTooManyRequests, rec.Code)
	_, _ = fmt.Sscan(rec.Header().Get("X-RateLimit-Reset"), &resetUnix)
	assert.Greater(t, resetUnix, base.Unix(), "Reset is in the future")
	assert.LessOrEqual(t, resetUnix, base.Unix()+1, "the next token arrives within 0.6 s")
	assert.Equal(t, "1", rec.Header().Get("Retry-After"))

	now = base.Add(700 * time.Millisecond) // one token later
	assert.Equal(t, http.StatusOK, call().Code)
}
