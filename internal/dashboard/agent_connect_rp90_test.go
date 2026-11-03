package dashboard

// RP90-7 (Stage 1a Prompt 56 Part 0): the consent's TOTP mints a 30-day
// credential, and nothing throttled its guesses (NIST SP 800-63B § 5.2.2).
// One per-user counter for every verifier site: 10 wrong codes in 15 min
// lock the code paths of that user — here, on the approvals, at the
// account's deletion — audited auth.mfa_locked once.

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
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

func TestConsent_TOTPAttemptsAreLimited(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	logger := zap.NewNop()
	authSvc := auth.NewAuthService(nil, db)
	u, _, _, err := authSvc.CreateUserWithTenant(ctx, "agent-"+uuid.NewString()[:8]+"@agents.example", "Str0ngPassw0rd!", "x")
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM agent_oauth_codes WHERE tenant_id = $1`, u.TenantID)
		_, _ = db.Exec(`DELETE FROM feature_flags WHERE tenant_id = $1`, u.TenantID)
		_, _ = db.Exec(`DELETE FROM audit_logs WHERE tenant_id = $1 OR user_id = $2`, u.TenantID, u.ID)
	})
	const secret = "JBSWY3DPEHPK3PXP"
	require.NoError(t, authSvc.EnableMFA(ctx, u.ID, secret, nil))
	fl := flags.New(db, logger)
	fl.Register("agent_grants", false)
	require.NoError(t, fl.Set(ctx, "agent_grants", u.TenantID, true, "test"))
	require.NoError(t, fl.Refresh(ctx))
	const redirect = "https://claude.ai/api/mcp/auth_callback"
	client, err := auth.RegisterOAuthClient(ctx, db, "Claude", []string{redirect}, "")
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
	req := httptest.NewRequest(http.MethodGet, "/dashboard/agents/connect?"+q.Encode(), nil)
	req.AddCookie(session)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	csrf := regexp.MustCompile(`name="csrf_token" value="([0-9a-f]{64})"`).FindStringSubmatch(w.Body.String())
	require.NotNil(t, csrf)
	post := func(code string) *httptest.ResponseRecorder {
		form := url.Values{"csrf_token": {csrf[1]}, "decision": {"approve"}, "totp_code": {code}}
		for k, v := range q {
			form[k] = v
		}
		req := httptest.NewRequest(http.MethodPost, "/dashboard/agents/connect", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.AddCookie(session)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}
	right, err := totp.GenerateCode(secret, time.Now())
	require.NoError(t, err)
	n, _ := strconv.Atoi(right)
	wrong := fmt.Sprintf("%06d", (n+500000)%1000000) // never inside the ±1 step window of a 6-digit code but by chance

	for i := 0; i < auth.TOTPMaxFailures; i++ {
		assert.Equal(t, http.StatusUnprocessableEntity, post(wrong).Code, "wrong code %d", i+1)
	}
	assert.True(t, authSvc.TOTPLocked(u.ID), "10 wrong codes lock the user's code paths")
	w = post(right)
	assert.Equal(t, http.StatusUnprocessableEntity, w.Code, "the right code is refused while locked: a guess that lands is no consent")
	assert.Contains(t, w.Body.String(), "Too many wrong codes")
	var codes int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM agent_oauth_codes WHERE tenant_id = $1`, u.TenantID).Scan(&codes))
	assert.Zero(t, codes)

	// One counter: every other verifier site of this user is locked too.
	err = authSvc.CheckFreshTOTP(ctx, auth.NewMFAService("stored.ge"), u.ID, right, "approval")
	assert.ErrorIs(t, err, auth.ErrTOTPLocked)

	var locked int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM audit_logs WHERE user_id = $1 AND action = 'auth.mfa_locked'`, u.ID).Scan(&locked))
	assert.Equal(t, 1, locked, "the lock is audited once")
}
