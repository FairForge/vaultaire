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
		`{{range .ManagementKeys}}<span class="mk">{{.ID}} {{.Hint}} {{.Scopes}} {{.Status}}</span>{{end}}` +
		`{{if .IsPlatform}}<span class="platform">platform</span>{{end}}` +
		`{{end}}`))
	return tmpl
}

func TestDashboard_ManagementKeysShownOnceAndRevoked(t *testing.T) {
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
		for _, q := range []string{`DELETE FROM management_keys WHERE tenant_id = $1`, `DELETE FROM audit_logs WHERE tenant_id = $1`,
			`DELETE FROM api_keys WHERE tenant_id = $1`, `DELETE FROM tenant_members WHERE tenant_id = $1`,
			`DELETE FROM tenant_quotas WHERE tenant_id = $1`, `DELETE FROM tenants WHERE id = $1`} {
			_, _ = db.Exec(q, user.TenantID)
		}
		_, _ = db.Exec(`DELETE FROM users WHERE id = $1`, user.ID)
	})
	sd := &dashauth.SessionData{UserID: user.ID, TenantID: user.TenantID, Email: user.Email, Role: "user"}
	tmpl := testManagementKeysTemplate(t)
	withSession := func(r *http.Request) *http.Request {
		return r.WithContext(context.WithValue(r.Context(), dashauth.SessionKey, sd))
	}

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
