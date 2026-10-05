package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/FairForge/vaultaire/internal/auth"
	dashauth "github.com/FairForge/vaultaire/internal/dashboard/auth"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// WP-R12-15: enrolling a second factor and regenerating backup codes ask
// for the password — a stolen dashboard session must not be enough to
// enrol the thief's authenticator and lock the owner out, nor to mint a
// fresh set of recovery codes. An OAuth-only account (no password) needs a
// session younger than ten minutes instead: a fresh sign-in.

func TestMFAEnrolment_RequiresThePassword(t *testing.T) {
	f := newEnrolFixture(t)
	secret := f.open(mfaTestSession)

	// No password: refused, nothing enrolled, the pending secret kept.
	w := f.enable(mfaTestSession, "password=&totp_code="+f.code(secret))
	assert.Equal(t, http.StatusSeeOther, w.Code)
	assert.Equal(t, "/dashboard/settings/mfa", w.Header().Get("Location"))
	assert.False(t, f.enabled())
	assert.Equal(t, 1, f.store.Len(), "a refused attempt keeps the QR code the user scanned")

	// Wrong password: the same, and the code was NOT consumed.
	code := f.code(secret)
	w = f.enable(mfaTestSession, "password=not-the-password&totp_code="+code)
	assert.Equal(t, http.StatusSeeOther, w.Code)
	assert.False(t, f.enabled())
	assert.Equal(t, 1, f.store.Len())

	// The right password with the same code: enrolled.
	w = f.enable(mfaTestSession, "password=password123&totp_code="+code)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.True(t, f.enabled())
	assert.Contains(t, w.Body.String(), "codes:10")
}

// oauthFixture: an account with no password, a session store, and the
// handlers wired to it.
type oauthFixture struct {
	t        *testing.T
	auth     *auth.AuthService
	mfa      *auth.MFAService
	store    *MFAEnrolmentStore
	sessions *dashauth.MemoryStore
	user     *auth.User
	enable   http.HandlerFunc
	regen    http.HandlerFunc
	setup    http.HandlerFunc
}

func newOAuthFixture(t *testing.T) *oauthFixture {
	t.Helper()
	svc := auth.NewAuthService(nil, nil)
	user, _, _, err := svc.CreateUserFromOAuth(context.Background(), "oauth-mfa@stored.ge", "Co", "google", "g-1")
	require.NoError(t, err)
	require.Empty(t, user.PasswordHash, "an OAuth-only account has no password")
	f := &oauthFixture{t: t, auth: svc, mfa: auth.NewMFAService("stored.ge"), store: NewMFAEnrolmentStore(),
		sessions: dashauth.NewMemoryStore(), user: user}
	tmpl := testMFATemplate(t)
	f.setup = HandleMFASetup(tmpl, svc, f.mfa, f.store, zap.NewNop())
	f.enable = HandleMFAEnable(tmpl, svc, f.mfa, f.store, f.sessions, zap.NewNop())
	f.regen = HandleMFARegenerateBackupCodes(tmpl, svc, f.mfa, f.sessions, zap.NewNop())
	return f
}

// session creates a dashboard session for the user and returns its token.
func (f *oauthFixture) session() string {
	tok, err := f.sessions.Create(context.Background(), dashauth.SessionData{UserID: f.user.ID, TenantID: f.user.TenantID, Email: f.user.Email, Role: "user"}, time.Hour)
	require.NoError(f.t, err)
	return tok
}

func (f *oauthFixture) req(method, path, session, body string) *http.Request {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	req.AddCookie(&http.Cookie{Name: dashauth.SessionCookieName, Value: session})
	sd, err := f.sessions.Get(context.Background(), session)
	require.NoError(f.t, err)
	require.NotNil(f.t, sd)
	return req.WithContext(context.WithValue(context.Background(), dashauth.SessionKey, sd))
}

func (f *oauthFixture) code(secret string) string {
	return (&enrolFixture{t: f.t}).code(secret)
}

func TestMFAEnrolment_OAuthOnlyAccountNeedsAFreshSession(t *testing.T) {
	f := newOAuthFixture(t)
	fresh := f.session()

	// The setup page tells an OAuth-only account what it needs.
	w := httptest.NewRecorder()
	f.setup(w, f.req("GET", "/dashboard/settings/mfa", fresh, ""))
	require.Equal(t, http.StatusOK, w.Code)
	secret := strings.TrimPrefix(w.Body.String(), "secret:")

	// Eleven minutes later the same session is not fresh: refused, and the
	// pending secret is kept.
	mfaNow = func() time.Time { return time.Now().Add(mfaFreshSessionWindow + time.Minute) }
	t.Cleanup(func() { mfaNow = time.Now })
	w = httptest.NewRecorder()
	f.enable(w, f.req("POST", "/dashboard/settings/mfa/enable", fresh, "totp_code="+f.code(secret)))
	assert.Equal(t, http.StatusSeeOther, w.Code)
	on, _ := f.auth.IsMFAEnabled(context.Background(), f.user.ID)
	assert.False(t, on, "a stale session cannot enrol an authenticator")
	assert.Equal(t, 1, f.store.Len())

	// Signed in again (a session created now): enrolled without a password.
	mfaNow = time.Now
	w = httptest.NewRecorder()
	f.enable(w, f.req("POST", "/dashboard/settings/mfa/enable", fresh, "totp_code="+f.code(secret)))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	on, _ = f.auth.IsMFAEnabled(context.Background(), f.user.ID)
	assert.True(t, on)

	// Regenerating backup codes follows the same rule.
	mfaNow = func() time.Time { return time.Now().Add(mfaFreshSessionWindow + time.Minute) }
	w = httptest.NewRecorder()
	f.regen(w, f.req("POST", "/dashboard/settings/mfa/backup-codes", fresh, ""))
	assert.Equal(t, http.StatusSeeOther, w.Code, "stale session: no new codes")
	mfaNow = time.Now
	w = httptest.NewRecorder()
	f.regen(w, f.req("POST", "/dashboard/settings/mfa/backup-codes", fresh, ""))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "codes:10")
}

func TestMFARegenerateBackupCodes(t *testing.T) {
	f := newEnrolFixture(t)
	regen := HandleMFARegenerateBackupCodes(testMFATemplate(t), f.auth, f.mfa, nil, zap.NewNop())
	post := func(form string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/dashboard/settings/mfa/backup-codes", strings.NewReader(form))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.AddCookie(&http.Cookie{Name: dashauth.SessionCookieName, Value: mfaTestSession})
		regen(w, req.WithContext(mfaSessionCtx(t, f.auth)))
		return w
	}

	// 2FA off: nothing to regenerate.
	w := post("password=password123")
	assert.Equal(t, http.StatusSeeOther, w.Code)
	assert.Equal(t, "/dashboard/settings", w.Header().Get("Location"))

	// Enrol, keeping the original codes.
	secret := f.open(mfaTestSession)
	originals := []string{"AAAAAAAA", "BBBBBBBB", "CCCCCCCC"}
	require.Equal(t, http.StatusOK, f.enable(mfaTestSession, "totp_code="+f.code(secret)).Code)
	require.NoError(t, f.auth.RegenerateBackupCodes(context.Background(), f.userID(), originals))
	ok, _ := f.auth.ValidateBackupCode(context.Background(), f.userID(), "AAAAAAAA")
	require.True(t, ok)

	// No password / wrong password: refused, the remaining originals live.
	for _, form := range []string{"", "password=", "password=wrong"} {
		w = post(form)
		assert.Equal(t, http.StatusSeeOther, w.Code, form)
		ok, _ = f.auth.ValidateBackupCode(context.Background(), f.userID(), "BBBBBBBB")
		require.True(t, ok, "a refused attempt must not touch the codes")
		require.NoError(t, f.auth.RegenerateBackupCodes(context.Background(), f.userID(), []string{"BBBBBBBB", "CCCCCCCC"}))
	}

	// The password: ten new codes shown once; the originals are dead.
	w = post("password=password123")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, "no-store", w.Header().Get("Cache-Control"))
	assert.Contains(t, w.Body.String(), "codes:10")
	ok, _ = f.auth.ValidateBackupCode(context.Background(), f.userID(), "CCCCCCCC")
	assert.False(t, ok, "the old codes stop working")
	on, _ := f.auth.IsMFAEnabled(context.Background(), f.userID())
	assert.True(t, on, "2FA itself is untouched")
	stored, err := f.auth.GetMFASecret(context.Background(), f.userID())
	require.NoError(t, err)
	assert.Equal(t, secret, stored, "the authenticator secret is untouched")
}
