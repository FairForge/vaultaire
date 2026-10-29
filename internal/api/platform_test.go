package api

// WP-PLAT-1 (Stage 1a Prompt 51; plan § WP-STRIPE): a platform tenant
// creates user-less child tenants by API under /api/v1/platform — the
// routes do not exist (404) for any other caller.

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/auth"
	"github.com/FairForge/vaultaire/internal/config"
	"github.com/FairForge/vaultaire/internal/testutil"
	"github.com/FairForge/vaultaire/internal/usage"
)

type platformFixture struct {
	t        *testing.T
	db       *sql.DB
	s        *Server
	user     *auth.User
	jwt      string
	admin    *auth.User
	adminJWT string
}

func setupPlatformFixture(t *testing.T) *platformFixture {
	t.Helper()
	t.Setenv("JWT_SECRET", "platform-test-secret")
	db, err := sql.Open("postgres", testutil.DSN())
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	require.NoError(t, db.Ping())
	authSvc := auth.NewAuthService(nil, db)
	f := &platformFixture{t: t, db: db}
	user, _, _, err := authSvc.CreateUserWithTenant(context.Background(), "plat-"+uuid.NewString()[:8]+"@stored.ge", "Str0ngPassw0rd!", "x")
	require.NoError(t, err)
	f.user = user
	t.Cleanup(func() { cleanupPlatform(db, user.TenantID) })
	f.admin = &auth.User{ID: uuid.NewString(), Email: "plat-admin-" + uuid.NewString()[:8] + "@stored.ge", TenantID: "tenant-admin-" + uuid.NewString()[:8]}
	_, err = db.Exec(`INSERT INTO users (id, email, password_hash, role) VALUES ($1, $2, 'x', 'admin')`, f.admin.ID, f.admin.Email)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM audit_logs WHERE user_id = $1`, f.admin.ID)
		_, _ = db.Exec(`DELETE FROM users WHERE id = $1`, f.admin.ID)
	})
	f.jwt, err = authSvc.GenerateJWT(user)
	require.NoError(t, err)
	f.adminJWT, err = authSvc.GenerateJWT(f.admin)
	require.NoError(t, err)
	qm := usage.NewQuotaManager(db)
	f.s = &Server{logger: zap.NewNop(), auth: authSvc, db: db, router: chi.NewRouter(), quotaManager: qm,
		config: &config.Config{Server: config.ServerConfig{Port: 8000}}}
	f.s.registerPlatformRoutes()
	f.s.router.Route("/api/v1/admin", func(r chi.Router) {
		r.Use(f.s.requireJWT)
		f.s.registerAdminPlatformRoutes(r)
	})
	return f
}

// cleanupPlatform removes a platform's children and the platform.
func cleanupPlatform(db *sql.DB, parent string) {
	for _, q := range []string{
		`DELETE FROM sts_tokens WHERE tenant_id IN (SELECT id FROM tenants WHERE parent_tenant_id = $1)`,
		`DELETE FROM api_keys WHERE tenant_id IN (SELECT id FROM tenants WHERE parent_tenant_id = $1)`,
		`DELETE FROM object_head_cache WHERE tenant_id IN (SELECT id FROM tenants WHERE parent_tenant_id = $1)`,
		`DELETE FROM object_versions WHERE tenant_id IN (SELECT id FROM tenants WHERE parent_tenant_id = $1)`,
		`DELETE FROM buckets WHERE tenant_id IN (SELECT id FROM tenants WHERE parent_tenant_id = $1)`,
		`DELETE FROM request_log WHERE tenant_id IN (SELECT id FROM tenants WHERE parent_tenant_id = $1)`,
		`DELETE FROM s3_access_log WHERE tenant_id IN (SELECT id FROM tenants WHERE parent_tenant_id = $1)`,
		`DELETE FROM events WHERE tenant_id IN (SELECT id FROM tenants WHERE parent_tenant_id = $1)`,
		`DELETE FROM audit_logs WHERE tenant_id IN (SELECT id FROM tenants WHERE parent_tenant_id = $1)`,
		`DELETE FROM tenant_quotas WHERE tenant_id IN (SELECT id FROM tenants WHERE parent_tenant_id = $1)`,
		`DELETE FROM tenants WHERE parent_tenant_id = $1`,
		`DELETE FROM audit_logs WHERE tenant_id = $1`,
		`DELETE FROM request_log WHERE tenant_id = $1`,
	} {
		_, _ = db.Exec(q, parent)
	}
	cleanupSandboxOf(db, parent)
}

func (f *platformFixture) call(method, path, jwt string, body any) (int, map[string]any) {
	f.t.Helper()
	var rd *bytes.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		require.NoError(f.t, err)
		rd = bytes.NewReader(raw)
	} else {
		rd = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, rd)
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	f.s.router.ServeHTTP(w, req)
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

// grant makes the fixture's tenant a platform through the admin route.
func (f *platformFixture) grant() {
	f.t.Helper()
	code, out := f.call(http.MethodPut, "/api/v1/admin/tenants/"+f.user.TenantID+"/platform", f.adminJWT, map[string]any{"platform": true})
	require.Equal(f.t, http.StatusOK, code, out)
}

// A tenant without platform = true gets 404 on every platform route (the
// routes do not exist for it); an admin's grant opens them, audited.
func TestPlatform_ANonPlatformTenantGets404(t *testing.T) {
	f := setupPlatformFixture(t)
	for _, c := range []struct{ method, path string }{
		{http.MethodPost, "/api/v1/platform/tenants"},
		{http.MethodGet, "/api/v1/platform/tenants"},
		{http.MethodGet, "/api/v1/platform/tenants/x"},
		{http.MethodDelete, "/api/v1/platform/tenants/x"},
	} {
		code, out := f.call(c.method, c.path, f.jwt, map[string]any{"label": "A"})
		assert.Equal(t, http.StatusNotFound, code, "%s %s: %v", c.method, c.path, out)
	}

	// Only an admin grants it.
	code, _ := f.call(http.MethodPut, "/api/v1/admin/tenants/"+f.user.TenantID+"/platform", f.jwt, map[string]any{"platform": true})
	assert.Equal(t, http.StatusForbidden, code)
	f.grant()
	var action, resource string
	require.NoError(t, f.db.QueryRow(`SELECT action, resource FROM audit_logs WHERE performed_by::text = $1 AND tenant_id = $2 ORDER BY timestamp DESC LIMIT 1`,
		f.admin.ID, f.user.TenantID).Scan(&action, &resource))
	assert.Equal(t, "admin.platform_granted", action)

	code, out := f.call(http.MethodPost, "/api/v1/platform/tenants", f.jwt, map[string]any{"label": "Acme user 1"})
	assert.Equal(t, http.StatusCreated, code, out)

	// An unknown tenant is 404; a revoke while children exist is 409.
	code, _ = f.call(http.MethodPut, "/api/v1/admin/tenants/no-such-tenant/platform", f.adminJWT, map[string]any{"platform": true})
	assert.Equal(t, http.StatusNotFound, code)
	code, out = f.call(http.MethodPut, "/api/v1/admin/tenants/"+f.user.TenantID+"/platform", f.adminJWT, map[string]any{"platform": false})
	assert.Equal(t, http.StatusConflict, code, out)
}

// POST /api/v1/platform/tenants makes a child: no users row, its
// parent_tenant_id the caller, a placeholder e-mail, a quota row of 0
// (the pool decides); an external id is unique per parent; a child is
// never a platform of its own.
func TestPlatform_CreatesATenantWithoutAUser(t *testing.T) {
	f := setupPlatformFixture(t)
	f.grant()
	code, out := f.call(http.MethodPost, "/api/v1/platform/tenants", f.jwt, map[string]any{"label": "Acme user 7", "external_id": "acme-7"})
	require.Equal(t, http.StatusCreated, code, out)
	assert.Equal(t, "tenant", out["object"])
	id, _ := out["id"].(string)
	require.NotEmpty(t, id)
	assert.Equal(t, "Acme user 7", out["label"])
	assert.Equal(t, "acme-7", out["external_id"])
	assert.NotEmpty(t, out["residency"])

	var parent, email string
	var platform bool
	require.NoError(t, f.db.QueryRow(`SELECT COALESCE(parent_tenant_id, ''), email, platform FROM tenants WHERE id = $1`, id).Scan(&parent, &email, &platform))
	assert.Equal(t, f.user.TenantID, parent)
	assert.Equal(t, "platform+"+id+"@platform.invalid", email)
	assert.False(t, platform)
	var users int
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM users WHERE email = $1`, email).Scan(&users))
	assert.Zero(t, users, "no users row")
	var limit int64
	require.NoError(t, f.db.QueryRow(`SELECT storage_limit_bytes FROM tenant_quotas WHERE tenant_id = $1`, id).Scan(&limit))
	assert.Zero(t, limit, "the pool decides")
	var keys int
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM api_keys WHERE tenant_id = $1`, id).Scan(&keys))
	assert.Zero(t, keys, "a child has no primary pair")

	code, out = f.call(http.MethodPost, "/api/v1/platform/tenants", f.jwt, map[string]any{"label": "dup", "external_id": "acme-7"})
	assert.Equal(t, http.StatusConflict, code)
	assert.Equal(t, "external_id_taken", errCode(out))

	code, out = f.call(http.MethodPost, "/api/v1/platform/tenants", f.jwt, map[string]any{"label": ""})
	assert.Equal(t, http.StatusBadRequest, code)
	assert.Equal(t, "label_required", errCode(out))

	// A child of a child: the core refuses it (a child can never be made a
	// platform through the admin route either).
	_, err := auth.CreateChildTenant(context.Background(), f.db, id, "grandchild", "")
	require.ErrorIs(t, err, auth.ErrChildOfChild)
	code, out = f.call(http.MethodPut, "/api/v1/admin/tenants/"+id+"/platform", f.adminJWT, map[string]any{"platform": true})
	assert.Equal(t, http.StatusBadRequest, code)
	assert.Equal(t, "child_of_child", errCode(out))
	// Nor a sandbox's.
	sb := "sandbox-plat-" + uuid.NewString()[:8]
	_, err = f.db.Exec(`INSERT INTO tenants (id, name, email, sandbox_of) VALUES ($1, 'test data', $2, $3)`, sb, "sandbox+"+sb+"@sandbox.invalid", f.user.TenantID)
	require.NoError(t, err)
	_, err = auth.CreateChildTenant(context.Background(), f.db, sb, "x", "")
	require.ErrorIs(t, err, auth.ErrChildOfChild)
}

// GET /api/v1/platform/tenants lists the caller's children with their
// usage (tenant_quotas); GET …/{id} reads one, 404 for another platform's;
// DELETE …/{id} schedules the child's erasure (30 days).
func TestPlatform_ListAndReadChildren(t *testing.T) {
	f := setupPlatformFixture(t)
	f.grant()
	var ids []string
	for i := 0; i < 3; i++ {
		code, out := f.call(http.MethodPost, "/api/v1/platform/tenants", f.jwt, map[string]any{"label": fmt.Sprintf("u%d", i), "external_id": fmt.Sprintf("e%d", i)})
		require.Equal(t, http.StatusCreated, code, out)
		ids = append(ids, out["id"].(string))
	}
	_, err := f.db.Exec(`UPDATE tenant_quotas SET storage_used_bytes = 1234 WHERE tenant_id = $1`, ids[1])
	require.NoError(t, err)

	code, out := f.call(http.MethodGet, "/api/v1/platform/tenants?limit=2", f.jwt, nil)
	require.Equal(t, http.StatusOK, code, out)
	data := out["data"].([]any)
	require.Len(t, data, 2)
	assert.Equal(t, true, out["has_more"])
	code, out = f.call(http.MethodGet, "/api/v1/platform/tenants?limit=2&starting_after="+data[1].(map[string]any)["id"].(string), f.jwt, nil)
	require.Equal(t, http.StatusOK, code, out)
	require.Len(t, out["data"].([]any), 1)

	code, out = f.call(http.MethodGet, "/api/v1/platform/tenants/"+ids[1], f.jwt, nil)
	require.Equal(t, http.StatusOK, code, out)
	assert.Equal(t, "u1", out["label"])
	assert.EqualValues(t, 1234, out["usage"].(map[string]any)["storage_used_bytes"])

	// Another platform's child is not this caller's.
	other := setupPlatformFixture(t)
	other.grant()
	code, _ = other.call(http.MethodGet, "/api/v1/platform/tenants/"+ids[1], other.jwt, nil)
	assert.Equal(t, http.StatusNotFound, code)
	code, _ = other.call(http.MethodDelete, "/api/v1/platform/tenants/"+ids[1], other.jwt, nil)
	assert.Equal(t, http.StatusNotFound, code)

	code, out = f.call(http.MethodDelete, "/api/v1/platform/tenants/"+ids[2], f.jwt, nil)
	require.Equal(t, http.StatusOK, code, out)
	assert.Equal(t, true, out["deletion_scheduled"])
	var at time.Time
	require.NoError(t, f.db.QueryRow(`SELECT deletion_scheduled_at FROM tenants WHERE id = $1`, ids[2]).Scan(&at))
	assert.WithinDuration(t, time.Now().Add(30*24*time.Hour), at, time.Minute)
	// Idempotent: the first date stays.
	code, _ = f.call(http.MethodDelete, "/api/v1/platform/tenants/"+ids[2], f.jwt, nil)
	require.Equal(t, http.StatusOK, code)
	var again time.Time
	require.NoError(t, f.db.QueryRow(`SELECT deletion_scheduled_at FROM tenants WHERE id = $1`, ids[2]).Scan(&again))
	assert.True(t, at.Equal(again))
}

// The export's tenant section names the four platform columns.
func TestExportTenantSection_NamesThePlatformColumns(t *testing.T) {
	for _, col := range []string{"parent_tenant_id", "platform", "external_id", "label"} {
		assert.Regexp(t, regexp.MustCompile(`\b`+col+`\b`), sqlExportTenant, col)
	}
}

// The platform routes are behind the JWT, the request log and the
// platform gate — every one of them.
func TestPlatformRoutes_EveryRouteLoggedAndGated(t *testing.T) {
	f := setupPlatformFixture(t)
	n := 0
	require.NoError(t, chi.Walk(f.s.router, func(method, route string, h http.Handler, mws ...func(http.Handler) http.Handler) error {
		if !strings.HasPrefix(route, "/api/v1/platform/") {
			return nil
		}
		n++
		assert.GreaterOrEqual(t, len(mws), 3, "%s %s: requireJWT, logManagementRequest, requirePlatform", method, route)
		return nil
	}))
	assert.GreaterOrEqual(t, n, 4)
}

func errCode(out map[string]any) string {
	e, _ := out["error"].(map[string]any)
	c, _ := e["code"].(string)
	return c
}

// A platform's suspension is its children's (RP80-2's join, through
// parent_tenant_id): a suspended platform's child answers AccountSuspended,
// and no child is made.
func TestPlatform_ParentSuspendedSuspendsTheChildren(t *testing.T) {
	f := setupPlatformFixture(t)
	f.grant()
	code, out := f.call(http.MethodPost, "/api/v1/platform/tenants", f.jwt, map[string]any{"label": "c"})
	require.Equal(t, http.StatusCreated, code, out)
	child := out["id"].(string)
	assert.False(t, isTenantSuspended(context.Background(), f.db, child))
	_, err := f.db.Exec(`UPDATE tenants SET suspended_at = NOW() WHERE id = $1`, f.user.TenantID)
	require.NoError(t, err)
	assert.True(t, isTenantSuspended(context.Background(), f.db, child))
	code, out = f.call(http.MethodPost, "/api/v1/platform/tenants", f.jwt, map[string]any{"label": "d"})
	assert.Equal(t, http.StatusForbidden, code)
	assert.Equal(t, "account_suspended", errCode(out))
}
