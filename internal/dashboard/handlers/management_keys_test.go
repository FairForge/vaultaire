package handlers

// WP-KEY-1 PR C: the dashboard's management keys (the API keys page): mint
// with scopes, the secret shown once, listed with its hint only, revoked.

import (
	"context"
	"database/sql"
	"html/template"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/auth"
	dashauth "github.com/FairForge/vaultaire/internal/dashboard/auth"
	"github.com/FairForge/vaultaire/internal/testutil"
)

func testManagementKeysTemplate(t *testing.T) *template.Template {
	t.Helper()
	tmpl := template.Must(template.New("base").Parse(`{{define "base"}}{{block "content" .}}{{end}}{{end}}`))
	template.Must(tmpl.Parse(`{{define "content"}}` +
		`{{if .NewManagementKey}}<span class="new-mk">{{.NewManagementKey.Secret}}</span>{{end}}` +
		`{{if .GenerateError}}<span class="error">{{.GenerateError}}</span>{{end}}` +
		`{{range .ManagementKeys}}<span class="mk">{{.ID}} {{.Hint}} {{.Scopes}} {{.Status}} {{.Mode}} {{.ExpiresFmt}}</span>{{end}}` +
		`{{range .Keys}}<span class="key">{{.ID}} caps={{.CapsSummary}}</span>{{end}}` +
		`{{if .IsPlatform}}<span class="platform">platform</span>{{end}}` +
		`{{end}}`))
	return tmpl
}

// dashKeysFixture is a registered account and its dashboard session.
type dashKeysFixture struct {
	db      *sql.DB
	authSvc *auth.AuthService
	user    *auth.User
	sd      *dashauth.SessionData
	tmpl    *template.Template
}

func setupDashKeys(t *testing.T) *dashKeysFixture {
	t.Helper()
	db, err := sql.Open("postgres", testutil.DSN())
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Ping(); err != nil {
		t.Skipf("test database unreachable: %v", err)
	}
	authSvc := auth.NewAuthService(nil, db)
	user, _, _, err := authSvc.CreateUserWithTenant(context.Background(), "dash-mk-"+uuid.NewString()[:8]+"@stored.ge", "Str0ngPassw0rd!", "x")
	require.NoError(t, err)
	t.Cleanup(func() {
		for _, q := range []string{`DELETE FROM management_keys WHERE tenant_id IN (SELECT id FROM tenants WHERE sandbox_of = $1)`,
			`DELETE FROM tenant_quotas WHERE tenant_id IN (SELECT id FROM tenants WHERE sandbox_of = $1)`,
			`DELETE FROM tenants WHERE sandbox_of = $1`,
			`DELETE FROM management_keys WHERE tenant_id = $1`, `DELETE FROM audit_logs WHERE tenant_id = $1`,
			`DELETE FROM api_keys WHERE tenant_id = $1`, `DELETE FROM tenant_members WHERE tenant_id = $1`,
			`DELETE FROM tenant_quotas WHERE tenant_id = $1`, `DELETE FROM tenants WHERE id = $1`} {
			_, _ = db.Exec(q, user.TenantID)
		}
		_, _ = db.Exec(`DELETE FROM users WHERE id = $1`, user.ID)
	})
	return &dashKeysFixture{db: db, authSvc: authSvc, user: user, tmpl: testManagementKeysTemplate(t),
		sd: &dashauth.SessionData{UserID: user.ID, TenantID: user.TenantID, Email: user.Email, Role: "user"}}
}

func (f *dashKeysFixture) withSession(r *http.Request) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), dashauth.SessionKey, f.sd))
}

func (f *dashKeysFixture) post(h http.HandlerFunc, path string, form url.Values, params map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rctx := chi.NewRouteContext()
	for k, v := range params {
		rctx.URLParams.Add(k, v)
	}
	req = f.withSession(req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx)))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func (f *dashKeysFixture) page() string {
	w := httptest.NewRecorder()
	HandleAPIKeys(f.tmpl, f.authSvc, f.db, zap.NewNop()).ServeHTTP(w, f.withSession(httptest.NewRequest(http.MethodGet, "/dashboard/apikeys", nil)))
	return w.Body.String()
}

func newMKSecret(t *testing.T, body string) string {
	t.Helper()
	start := strings.Index(body, `<span class="new-mk">`)
	require.GreaterOrEqual(t, start, 0, body)
	secret := body[start+len(`<span class="new-mk">`):]
	return secret[:strings.Index(secret, "<")]
}

// RP88-1 / RP88-2 on the dashboard: a test-mode key is the sandbox's
// (sk_test_), and a key can be given an expiry.
func TestDashboard_ManagementKeyTestModeAndExpiry(t *testing.T) {
	f := setupDashKeys(t)
	h := HandleGenerateManagementKey(f.tmpl, f.authSvc, f.db, zap.NewNop())
	w := f.post(h, "/dashboard/management-keys", url.Values{"name": {"ci"}, "scope": {"manage:write"}, "test": {"on"}, "expires_days": {"30"}}, nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	secret := newMKSecret(t, w.Body.String())
	assert.True(t, strings.HasPrefix(secret, "sk_test_"), secret)
	k, err := auth.LookupManagementKey(context.Background(), f.db, secret)
	require.NoError(t, err)
	sandbox, err := auth.SandboxOf(context.Background(), f.db, f.user.TenantID)
	require.NoError(t, err)
	assert.Equal(t, sandbox, k.TenantID, "the test key acts on the sandbox")
	require.NotNil(t, k.ExpiresAt)
	assert.WithinDuration(t, time.Now().Add(30*24*time.Hour), *k.ExpiresAt, time.Minute)
	page := f.page()
	assert.Contains(t, page, "test", "the list marks a test key")
	assert.Contains(t, page, k.ExpiresAt.Format("Jan 2, 2006"), "and shows its expiry")

	w = f.post(h, "/dashboard/management-keys", url.Values{"scope": {"manage:read"}, "expires_days": {"-3"}}, nil)
	assert.Contains(t, w.Body.String(), `class="error"`, "a junk expiry is refused")
}

// WP-KEY-1 PR C: the keys page sets a scoped key's caps (GiB), and never the
// primary's (F-39 (c)).
func TestDashboard_KeyCapsSetAndShown(t *testing.T) {
	f := setupDashKeys(t)
	k, err := f.authSvc.GenerateAPIKeyForTenant(context.Background(), f.user.TenantID, f.user.ID, "app", nil)
	require.NoError(t, err)
	h := HandleSetKeyCaps(f.db, zap.NewNop())
	w := f.post(h, "/dashboard/apikeys/"+k.ID+"/caps", url.Values{"storage_gib": {"0.5"}, "egress_gib": {"10"}}, map[string]string{"id": k.ID})
	assert.Equal(t, http.StatusSeeOther, w.Code, w.Body.String())
	var storage, egress sql.NullInt64
	require.NoError(t, f.db.QueryRow(`SELECT storage_limit_bytes, egress_limit_bytes FROM api_keys WHERE id = $1`, k.ID).Scan(&storage, &egress))
	assert.EqualValues(t, 512<<20, storage.Int64)
	assert.EqualValues(t, int64(10)<<30, egress.Int64)
	assert.Contains(t, f.page(), k.ID+" caps=512 MiB stored · 10 GiB/month out")

	// Empty fields clear the caps.
	w = f.post(h, "/dashboard/apikeys/"+k.ID+"/caps", url.Values{"storage_gib": {""}, "egress_gib": {""}}, map[string]string{"id": k.ID})
	assert.Equal(t, http.StatusSeeOther, w.Code)
	require.NoError(t, f.db.QueryRow(`SELECT storage_limit_bytes, egress_limit_bytes FROM api_keys WHERE id = $1`, k.ID).Scan(&storage, &egress))
	assert.False(t, storage.Valid || egress.Valid)

	var primary string
	require.NoError(t, f.db.QueryRow(`SELECT id::text FROM api_keys WHERE tenant_id = $1 AND is_primary`, f.user.TenantID).Scan(&primary))
	w = f.post(h, "/dashboard/apikeys/"+primary+"/caps", url.Values{"storage_gib": {"1"}}, map[string]string{"id": primary})
	assert.Equal(t, http.StatusSeeOther, w.Code)
	require.NoError(t, f.db.QueryRow(`SELECT storage_limit_bytes FROM api_keys WHERE id = $1`, primary).Scan(&storage))
	assert.False(t, storage.Valid, "the primary is never capped")
	w = f.post(h, "/dashboard/apikeys/"+k.ID+"/caps", url.Values{"storage_gib": {"lots"}}, map[string]string{"id": k.ID})
	assert.Equal(t, http.StatusSeeOther, w.Code, "a junk number is a flash, nothing set")
}

func TestDashboard_ManagementKeysShownOnceAndRevoked(t *testing.T) {
	f := setupDashKeys(t)
	db, authSvc, user, tmpl := f.db, f.authSvc, f.user, f.tmpl
	withSession := f.withSession

	form := url.Values{"name": {"ci server"}, "scope": {"manage:read", "manage:write"}}
	req := httptest.NewRequest(http.MethodPost, "/dashboard/management-keys", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	HandleGenerateManagementKey(tmpl, authSvc, db, zap.NewNop()).ServeHTTP(w, withSession(req))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	body := w.Body.String()
	start := strings.Index(body, `<span class="new-mk">`)
	require.GreaterOrEqual(t, start, 0, body)
	secret := body[start+len(`<span class="new-mk">`):]
	secret = secret[:strings.Index(secret, "<")]
	assert.True(t, strings.HasPrefix(secret, "sk_live_"), secret)
	assert.NotContains(t, body, `class="platform"`, "a platform scope is offered to a platform only")

	w = httptest.NewRecorder()
	HandleAPIKeys(tmpl, authSvc, db, zap.NewNop()).ServeHTTP(w, withSession(httptest.NewRequest(http.MethodGet, "/dashboard/apikeys", nil)))
	require.Equal(t, http.StatusOK, w.Code)
	assert.NotContains(t, w.Body.String(), secret, "the secret is shown once")
	assert.Contains(t, w.Body.String(), "manage:write")
	keys, err := auth.ListManagementKeys(context.Background(), db, user.TenantID)
	require.NoError(t, err)
	require.Len(t, keys, 1)
	assert.Equal(t, user.ID, keys[0].CreatedBy)

	// No scope: refused, nothing minted.
	req = httptest.NewRequest(http.MethodPost, "/dashboard/management-keys", strings.NewReader(url.Values{"name": {"x"}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w = httptest.NewRecorder()
	HandleGenerateManagementKey(tmpl, authSvc, db, zap.NewNop()).ServeHTTP(w, withSession(req))
	assert.Contains(t, w.Body.String(), `class="error"`)

	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", keys[0].ID)
	req = httptest.NewRequest(http.MethodPost, "/dashboard/management-keys/"+keys[0].ID+"/revoke", nil)
	req = withSession(req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx)))
	w = httptest.NewRecorder()
	HandleRevokeManagementKey(db, zap.NewNop()).ServeHTTP(w, req)
	assert.Equal(t, http.StatusSeeOther, w.Code)
	_, err = auth.LookupManagementKey(context.Background(), db, secret)
	assert.ErrorIs(t, err, auth.ErrManagementKeyInvalid, "revoked from the dashboard")
}
