package dashboard

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
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

// WP-AGENT-1 PR A: the consent of the MCP authorization flow is a person's,
// with a fresh TOTP code; it shows who asks and what the agent never does,
// and issues one code back to the client's registered redirect. A sign-in
// it interrupted returns to it.
func TestAgentConnect_ConsentNeedsTOTPAndIssuesOneCode(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	logger := zap.NewNop()
	authSvc := auth.NewAuthService(nil, db)
	u, _, _, err := authSvc.CreateUserWithTenant(ctx, "agent-"+uuid.NewString()[:8]+"@agents.example", "Str0ngPassw0rd!", "x")
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM agent_oauth_codes WHERE tenant_id = $1`, u.TenantID)
		_, _ = db.Exec(`DELETE FROM feature_flags WHERE tenant_id = $1`, u.TenantID)
		_, _ = db.Exec(`DELETE FROM audit_logs WHERE tenant_id = $1`, u.TenantID)
	})
	const secret = "JBSWY3DPEHPK3PXP"
	require.NoError(t, authSvc.EnableMFA(ctx, u.ID, secret, nil))
	fl := flags.New(db, logger)
	fl.Register("agent_grants", false)
	require.NoError(t, fl.Set(ctx, "agent_grants", u.TenantID, true, "test"))
	require.NoError(t, fl.Refresh(ctx))
	const redirect = "https://claude.ai/api/mcp/auth_callback"
	client, err := auth.RegisterOAuthClient(ctx, db, "Claude", []string{redirect})
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM agent_oauth_clients WHERE client_id = $1`, client.ID) })

	sessions := dashauth.NewMemoryStore()
	r := chi.NewRouter()
	RegisterRoutes(r, Deps{DB: db, Auth: authSvc, MFA: auth.NewMFAService("stored.ge"), Sessions: sessions, Logger: logger,
		Email: email.NewSender(logger), BaseURL: "http://localhost:8000", Flags: fl})
	tok, err := sessions.Create(ctx, dashauth.SessionData{UserID: u.ID, TenantID: u.TenantID, Email: u.Email, Role: "user"}, time.Hour)
	require.NoError(t, err)
	session := &http.Cookie{Name: dashauth.SessionCookieName, Value: tok}

	sum := sha256.Sum256([]byte(strings.Repeat("v", 50)))
	q := url.Values{"response_type": {"code"}, "client_id": {client.ID}, "redirect_uri": {redirect}, "state": {"st-1"},
		"code_challenge": {base64.RawURLEncoding.EncodeToString(sum[:])}, "code_challenge_method": {"S256"}}
	page := "/dashboard/agents/connect?" + q.Encode()

	// Signed out: to the sign-in, which returns here.
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, page, nil))
	require.Equal(t, http.StatusSeeOther, w.Code)
	var returnTo *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == dashauth.ReturnToCookie {
			returnTo = c
		}
	}
	require.NotNil(t, returnTo, "the consent is remembered for after the sign-in")
	back := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(returnTo)
	assert.Equal(t, page, dashauth.TakeReturnTo(back, req))

	get := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, page, nil)
		req.AddCookie(session)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}
	w = get()
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	body := w.Body.String()
	assert.Contains(t, body, "Claude")
	assert.Contains(t, body, "https://claude.ai")
	assert.Contains(t, body, "delete a bucket")
	csrf := regexp.MustCompile(`name="csrf_token" value="([0-9a-f]{64})"`).FindStringSubmatch(body)
	require.NotNil(t, csrf)

	post := func(extra url.Values) *httptest.ResponseRecorder {
		form := url.Values{"csrf_token": {csrf[1]}}
		for k, v := range q {
			form[k] = v
		}
		for k, v := range extra {
			form[k] = v
		}
		req := httptest.NewRequest(http.MethodPost, "/dashboard/agents/connect", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.AddCookie(session)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}
	codes := func() int {
		var n int
		require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM agent_oauth_codes WHERE tenant_id = $1`, u.TenantID).Scan(&n))
		return n
	}
	w = post(url.Values{"decision": {"approve"}})
	assert.Equal(t, http.StatusUnprocessableEntity, w.Code, "no TOTP code, no consent")
	w = post(url.Values{"decision": {"approve"}, "totp_code": {"000000"}})
	assert.Equal(t, http.StatusUnprocessableEntity, w.Code)
	assert.Zero(t, codes())

	w = post(url.Values{"decision": {"deny"}})
	require.Equal(t, http.StatusSeeOther, w.Code)
	assert.Contains(t, w.Header().Get("Location"), "error=access_denied")
	assert.Zero(t, codes())

	code, err := totp.GenerateCode(secret, time.Now())
	require.NoError(t, err)
	w = post(url.Values{"decision": {"approve"}, "totp_code": {code}, "storage_gib": {"1"}, "days": {"7"}})
	require.Equal(t, http.StatusSeeOther, w.Code, w.Body.String())
	loc, err := url.Parse(w.Header().Get("Location"))
	require.NoError(t, err)
	assert.Equal(t, "claude.ai", loc.Host)
	assert.NotEmpty(t, loc.Query().Get("code"))
	assert.Equal(t, "st-1", loc.Query().Get("state"))
	assert.Equal(t, 1, codes())
	var storage int64
	var days int
	require.NoError(t, db.QueryRow(`SELECT storage_limit_bytes, lifetime_days FROM agent_oauth_codes WHERE tenant_id = $1`, u.TenantID).Scan(&storage, &days))
	assert.EqualValues(t, 1<<30, storage)
	assert.Equal(t, 7, days)
	w = post(url.Values{"decision": {"approve"}, "totp_code": {code}})
	assert.Equal(t, http.StatusUnprocessableEntity, w.Code, "a TOTP code is accepted once")

	// The flag off: no consent at all.
	require.NoError(t, fl.Set(ctx, "agent_grants", u.TenantID, false, "test"))
	require.NoError(t, fl.Refresh(ctx))
	assert.Equal(t, http.StatusNotFound, get().Code)
}
