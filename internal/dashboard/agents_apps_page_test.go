package dashboard

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/pquerna/otp/totp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/auth"
	dashauth "github.com/FairForge/vaultaire/internal/dashboard/auth"
	"github.com/FairForge/vaultaire/internal/email"
	"github.com/FairForge/vaultaire/internal/flags"
)

// WP-AGENT-1 / WP-APP-1 PR E: the Agents page shows the grants and what
// they ask, and an approval takes the authenticator code; the Apps page
// makes an app (its key shown once) and deletes it; a claim link's page
// keeps a partner's claimable as an app.
func TestDashboard_AgentsAndAppsPages(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	logger := zap.NewNop()
	authSvc := auth.NewAuthService(nil, db)
	u, _, _, err := authSvc.CreateUserWithTenant(ctx, "pages-"+uuid.NewString()[:8]+"@agents.example", "Str0ngPassw0rd!", "x")
	require.NoError(t, err)
	partner, _, _, err := authSvc.CreateUserWithTenant(ctx, "partner-"+uuid.NewString()[:8]+"@agents.example", "Str0ngPassw0rd!", "x")
	require.NoError(t, err)
	t.Cleanup(func() {
		for _, tid := range []string{u.TenantID, partner.TenantID} {
			for _, q := range []string{`DELETE FROM approvals WHERE tenant_id = $1`, `DELETE FROM agent_grants WHERE tenant_id = $1`,
				`DELETE FROM claims WHERE partner_tenant_id = $1`, `DELETE FROM management_keys WHERE tenant_id IN (SELECT id FROM tenants WHERE parent_tenant_id = $1 OR sandbox_of = $1)`,
				`DELETE FROM management_keys WHERE tenant_id = $1`, `DELETE FROM tenant_quotas WHERE tenant_id IN (SELECT id FROM tenants WHERE parent_tenant_id = $1)`,
				`DELETE FROM buckets WHERE tenant_id = $1`, `DELETE FROM tenants WHERE parent_tenant_id = $1`, `DELETE FROM feature_flags WHERE tenant_id = $1`,
				`DELETE FROM audit_logs WHERE tenant_id = $1`} {
				_, _ = db.Exec(q, tid)
			}
		}
	})
	const secret = "JBSWY3DPEHPK3PXP"
	require.NoError(t, authSvc.EnableMFA(ctx, u.ID, secret, nil))
	_, err = db.Exec(`UPDATE users SET email_verified = TRUE WHERE id = $1`, u.ID)
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE tenant_quotas SET tier = 'standard' WHERE tenant_id = $1`, u.TenantID)
	require.NoError(t, err)
	fl := flags.New(db, logger)
	fl.Register("agent_grants", false)
	fl.Register("apps", false)
	for _, k := range []string{"agent_grants", "apps"} {
		require.NoError(t, fl.Set(ctx, k, u.TenantID, true, "test"))
	}
	require.NoError(t, fl.Refresh(ctx))

	sessions := dashauth.NewMemoryStore()
	r := chi.NewRouter()
	RegisterRoutes(r, Deps{DB: db, Auth: authSvc, MFA: auth.NewMFAService("stored.ge"), Sessions: sessions, Logger: logger,
		Email: email.NewSender(logger), BaseURL: "http://localhost:8000", Flags: fl})
	tok, err := sessions.Create(ctx, dashauth.SessionData{UserID: u.ID, TenantID: u.TenantID, Email: u.Email, Role: "user"}, time.Hour)
	require.NoError(t, err)
	session := &http.Cookie{Name: dashauth.SessionCookieName, Value: tok}
	get := func(path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.AddCookie(session)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}
	csrfOf := func(body string) string {
		m := regexp.MustCompile(`name="csrf_token" value="([0-9a-f]{64})"`).FindStringSubmatch(body)
		require.NotNil(t, m)
		return m[1]
	}
	post := func(path string, form url.Values) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.AddCookie(session)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}

	// Agents: a grant and its pending approval.
	g, _, err := auth.MintAgentGrant(ctx, db, u.TenantID, auth.AgentGrantOptions{AgentName: "Claude", GrantedBy: u.ID})
	require.NoError(t, err)
	ap, err := auth.RequestApproval(ctx, db, u.TenantID, g.ID, "bucket.delete", "bucket:photos")
	require.NoError(t, err)
	w := get("/dashboard/agents?approval=" + ap.ID)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	body := w.Body.String()
	assert.Contains(t, body, "Claude")
	assert.Contains(t, body, "delete the bucket")
	assert.Contains(t, body, "bucket:photos")
	csrf := csrfOf(body)
	w = post("/dashboard/approvals/"+ap.ID+"/approve", url.Values{"csrf_token": {csrf}})
	require.Equal(t, http.StatusSeeOther, w.Code)
	got, err := auth.GetApproval(ctx, db, u.TenantID, ap.ID)
	require.NoError(t, err)
	assert.Nil(t, got.DecidedAt, "no authenticator code, no approval")
	code, err := totp.GenerateCode(secret, time.Now())
	require.NoError(t, err)
	w = post("/dashboard/approvals/"+ap.ID+"/approve", url.Values{"csrf_token": {csrf}, "totp_code": {code}})
	require.Equal(t, http.StatusSeeOther, w.Code)
	got, err = auth.GetApproval(ctx, db, u.TenantID, ap.ID)
	require.NoError(t, err)
	assert.Equal(t, "approved", got.Decision)
	w = post("/dashboard/agents/"+g.ID+"/revoke", url.Values{"csrf_token": {csrf}})
	require.Equal(t, http.StatusSeeOther, w.Code)
	var revoked bool
	require.NoError(t, db.QueryRow(`SELECT revoked_at IS NOT NULL FROM management_keys WHERE id = $1`, g.ManagementKeyID).Scan(&revoked))
	assert.True(t, revoked)

	// Apps: make one, its key shown once; delete it.
	w = post("/dashboard/apps", url.Values{"csrf_token": {csrf}, "label": {"photo app"}})
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	assert.Regexp(t, `sk_live_[0-9A-Za-z]{43}`, w.Body.String())
	var app string
	require.NoError(t, db.QueryRow(`SELECT id FROM tenants WHERE parent_tenant_id = $1 AND child_kind = 'app'`, u.TenantID).Scan(&app))
	w = get("/dashboard/apps")
	require.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "photo app")
	assert.NotRegexp(t, `sk_live_[0-9A-Za-z]{43}`, w.Body.String(), "never shown again")
	w = post("/dashboard/apps/"+app+"/delete", url.Values{"csrf_token": {csrf}})
	require.Equal(t, http.StatusSeeOther, w.Code)
	var due bool
	require.NoError(t, db.QueryRow(`SELECT deletion_scheduled_at IS NOT NULL FROM tenants WHERE id = $1`, app).Scan(&due))
	assert.True(t, due)

	// A claim link.
	require.NoError(t, auth.SetPartner(ctx, db, partner.TenantID, true))
	c, err := auth.CreateClaimable(ctx, db, partner.TenantID, "lovable app", partner.ID)
	require.NoError(t, err)
	w = get("/dashboard/claim/" + c.Token)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "lovable app")
	w = post("/dashboard/claim/"+c.Token, url.Values{"csrf_token": {csrf}})
	require.Equal(t, http.StatusSeeOther, w.Code, w.Body.String())
	var parent string
	require.NoError(t, db.QueryRow(`SELECT parent_tenant_id FROM tenants WHERE id = $1`, c.TenantID).Scan(&parent))
	assert.Equal(t, u.TenantID, parent, "kept as an app of this account")
	assert.Equal(t, http.StatusGone, get("/dashboard/claim/"+c.Token).Code, "used once")
}
