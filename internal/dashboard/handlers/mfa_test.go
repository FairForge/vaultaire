package handlers

import (
	"context"
	"fmt"
	"html/template"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/FairForge/vaultaire/internal/auth"
	dashauth "github.com/FairForge/vaultaire/internal/dashboard/auth"
	"github.com/go-chi/chi/v5"
	"github.com/pquerna/otp/totp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func testMFATemplate(t *testing.T) *template.Template {
	t.Helper()
	return template.Must(template.New("base").Parse(
		`{{define "base"}}` +
			`{{block "nav" .}}{{end}}` +
			`{{block "content" .}}` +
			`{{if .Secret}}secret:{{.Secret}}{{end}}` +
			`{{if .Error}}error:{{.Error}}{{end}}` +
			`{{if .BackupCodes}}codes:{{len .BackupCodes}}{{end}}` +
			`{{end}}{{end}}`))
}

func testSettingsTmpl(t *testing.T) *template.Template {
	t.Helper()
	return template.Must(template.New("base").Parse(
		`{{define "base"}}` +
			`{{block "nav" .}}{{end}}` +
			`{{block "content" .}}` +
			`{{if .MFAError}}error:{{.MFAError}}{{end}}` +
			`{{if .MFAEnabled}}mfa:enabled{{end}}` +
			`{{end}}{{end}}`))
}

func mfaSessionCtx(t *testing.T, authSvc *auth.AuthService) context.Context {
	t.Helper()
	user, err := authSvc.GetUserByEmail(context.Background(), "mfa@stored.ge")
	require.NoError(t, err)
	store := dashauth.NewMemoryStore()
	token, _ := store.Create(context.Background(), dashauth.SessionData{
		UserID:   user.ID,
		TenantID: user.TenantID,
		Email:    user.Email,
		Role:     "user",
	}, time.Hour)
	sd, _ := store.Get(context.Background(), token)
	return context.WithValue(context.Background(), dashauth.SessionKey, sd)
}

func newAuthWithMFAUser(t *testing.T) *auth.AuthService {
	t.Helper()
	svc := auth.NewAuthService(nil, nil)
	_, _, _, err := svc.CreateUserWithTenant(context.Background(), "mfa@stored.ge", "password123", "Test")
	require.NoError(t, err)
	return svc
}

// --- WP-R12-8: enrolment keeps the pending secret server-side ----------------
//
// The TOTP secret and the ten backup codes used to come back from the browser
// in hidden fields (a user could enrol a secret of their choosing — R12-23 /
// R5-15b). The pending secret now lives in MFAEnrolmentStore, keyed by the
// session; the enable POST carries the 6-digit code and nothing else; the
// backup codes are generated when the code has verified and shown once.

const mfaTestSession = "session-token-of-the-enrolling-tab"

type enrolFixture struct {
	t     *testing.T
	auth  *auth.AuthService
	mfa   *auth.MFAService
	store *MFAEnrolmentStore
	setup http.HandlerFunc
	enbl  http.HandlerFunc
	clock *time.Time
}

func newEnrolFixture(t *testing.T) *enrolFixture {
	t.Helper()
	now := time.Now()
	f := &enrolFixture{t: t, auth: newAuthWithMFAUser(t), mfa: auth.NewMFAService("stored.ge"), store: NewMFAEnrolmentStore(), clock: &now}
	f.store.now = func() time.Time { return *f.clock }
	tmpl := testMFATemplate(t)
	f.setup = HandleMFASetup(tmpl, f.auth, f.mfa, f.store, zap.NewNop())
	f.enbl = HandleMFAEnable(tmpl, f.auth, f.mfa, f.store, zap.NewNop())
	return f
}

func (f *enrolFixture) userID() string {
	u, err := f.auth.GetUserByEmail(context.Background(), "mfa@stored.ge")
	require.NoError(f.t, err)
	return u.ID
}

func (f *enrolFixture) req(method, session, body string) *http.Request {
	req := httptest.NewRequest(method, "/dashboard/settings/mfa", strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	req.AddCookie(&http.Cookie{Name: dashauth.SessionCookieName, Value: session})
	return req.WithContext(mfaSessionCtx(f.t, f.auth))
}

// open GETs the setup page for a session and returns the secret it shows.
func (f *enrolFixture) open(session string) string {
	f.t.Helper()
	w := httptest.NewRecorder()
	f.setup(w, f.req("GET", session, ""))
	require.Equal(f.t, http.StatusOK, w.Code)
	body := w.Body.String()
	require.Contains(f.t, body, "secret:")
	return strings.TrimPrefix(body, "secret:")
}

func (f *enrolFixture) enable(session, form string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	f.enbl(w, f.req("POST", session, form))
	return w
}

func (f *enrolFixture) code(secret string) string {
	c, err := totp.GenerateCode(secret, time.Now())
	require.NoError(f.t, err)
	return c
}

func (f *enrolFixture) enabled() bool {
	on, _ := f.auth.IsMFAEnabled(context.Background(), f.userID())
	return on
}

func TestMFAEnrolment_SetupShowsTheSecretAndNoBackupCodes(t *testing.T) {
	// Arrange + Act
	f := newEnrolFixture(t)
	w := httptest.NewRecorder()
	f.setup(w, f.req("GET", mfaTestSession, ""))

	// Assert: the secret to scan, kept server-side; no codes before the code verified.
	require.Equal(t, http.StatusOK, w.Code)
	body := w.Body.String()
	assert.Contains(t, body, "secret:")
	assert.NotContains(t, body, "codes:", "backup codes are shown once, after the code verified — not on the setup page")
	assert.Equal(t, "no-store", w.Header().Get("Cache-Control"), "a page that shows a secret is never cached")
	assert.Equal(t, 1, f.store.Len())
	assert.False(t, f.enabled())
}

func TestMFAEnrolment_AClientSuppliedSecretIsNeverAccepted(t *testing.T) {
	// Arrange: the page was opened (a pending secret exists server-side); the
	// user posts a secret of their own choosing with a code that is valid for
	// it — what the hidden field used to allow.
	f := newEnrolFixture(t)
	pending := f.open(mfaTestSession)
	const chosen = "JBSWY3DPEHPK3PXP"
	require.NotEqual(t, chosen, pending)

	// Act
	w := f.enable(mfaTestSession, "secret="+chosen+"&totp_code="+f.code(chosen)+"&backup_codes=CODE1,CODE2")

	// Assert: refused — the code is checked against the server's secret.
	assert.Equal(t, http.StatusSeeOther, w.Code)
	assert.Equal(t, "/dashboard/settings/mfa", w.Header().Get("Location"))
	assert.False(t, f.enabled(), "a secret from the form enrolled the account")

	// With the code for the server's secret (and the same junk fields), the
	// account is enrolled with the SERVER's secret and SERVER-made codes.
	w = f.enable(mfaTestSession, "secret="+chosen+"&totp_code="+f.code(pending)+"&backup_codes=CODE1,CODE2")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.True(t, f.enabled())
	stored, err := f.auth.GetMFASecret(context.Background(), f.userID())
	require.NoError(t, err)
	assert.Equal(t, pending, stored)
	ok, _ := f.auth.ValidateBackupCode(context.Background(), f.userID(), "CODE1")
	assert.False(t, ok, "a backup code from the form was stored")
}

func TestMFAEnrolment_BackupCodesAreShownOnceAfterTheCodeVerified(t *testing.T) {
	// Arrange
	f := newEnrolFixture(t)
	f.store = NewMFAEnrolmentStore()
	var shown []string
	tmpl := template.Must(template.New("base").Parse(`{{define "base"}}{{range .BackupCodes}}[{{.}}]{{end}}{{end}}`))
	f.setup = HandleMFASetup(testMFATemplate(t), f.auth, f.mfa, f.store, zap.NewNop())
	f.enbl = HandleMFAEnable(tmpl, f.auth, f.mfa, f.store, zap.NewNop())
	secret := f.open(mfaTestSession)

	// Act
	w := f.enable(mfaTestSession, "totp_code="+f.code(secret))

	// Assert: ten codes, in the response to the verified code, never cached.
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "no-store", w.Header().Get("Cache-Control"))
	for _, part := range strings.Split(w.Body.String(), "[")[1:] {
		shown = append(shown, strings.TrimSuffix(part, "]"))
	}
	require.Len(t, shown, 10)
	ok, err := f.auth.ValidateBackupCode(context.Background(), f.userID(), shown[0])
	require.NoError(t, err)
	assert.True(t, ok, "the codes shown are the codes stored")
	assert.Equal(t, 0, f.store.Len(), "the pending secret is single use")
	// The code that enrolled cannot be replayed as the second factor of a
	// sign-in: the login flow's single-use guard has it on record.
	assert.False(t, f.auth.ConsumeTOTPCode(f.userID(), f.code(secret)))
}

func TestMFAEnrolment_AReplayedPostChangesNothingAndShowsNoCodes(t *testing.T) {
	// Arrange: an enrolment that succeeded.
	f := newEnrolFixture(t)
	secret := f.open(mfaTestSession)
	form := "totp_code=" + f.code(secret)
	require.Equal(t, http.StatusOK, f.enable(mfaTestSession, form).Code)
	before, _ := f.auth.GetMFASecret(context.Background(), f.userID())

	// Act: the same POST again (a reload of the result page, a replay).
	w := f.enable(mfaTestSession, form)

	// Assert
	assert.Equal(t, http.StatusSeeOther, w.Code)
	assert.Equal(t, "/dashboard/settings", w.Header().Get("Location"))
	assert.NotContains(t, w.Body.String(), "codes:", "backup codes are never shown twice")
	after, _ := f.auth.GetMFASecret(context.Background(), f.userID())
	assert.Equal(t, before, after)
}

func TestMFAEnrolment_ASecondTabSeesTheSameSecretAndOnlyOneEnrols(t *testing.T) {
	// Arrange: the same session opens the page twice.
	f := newEnrolFixture(t)
	tab1 := f.open(mfaTestSession)
	tab2 := f.open(mfaTestSession)
	require.Equal(t, tab1, tab2, "a second tab (or a reload) must not replace the QR code the first one showed")
	require.Equal(t, 1, f.store.Len())

	// Another session of the same user has its own pending secret.
	other := f.open("another-session-of-the-same-user")
	assert.NotEqual(t, tab1, other)

	// Act: tab 1 enrols; tab 2 then submits its (same) code.
	require.Equal(t, http.StatusOK, f.enable(mfaTestSession, "totp_code="+f.code(tab1)).Code)
	w := f.enable(mfaTestSession, "totp_code="+f.code(tab2))

	// Assert: already enrolled — nothing is replaced, no codes are shown.
	assert.Equal(t, http.StatusSeeOther, w.Code)
	assert.Equal(t, "/dashboard/settings", w.Header().Get("Location"))
	stored, _ := f.auth.GetMFASecret(context.Background(), f.userID())
	assert.Equal(t, tab1, stored)

	// And the other session's pending secret cannot replace it either.
	w = f.enable("another-session-of-the-same-user", "totp_code="+f.code(other))
	assert.Equal(t, http.StatusSeeOther, w.Code)
	stored, _ = f.auth.GetMFASecret(context.Background(), f.userID())
	assert.Equal(t, tab1, stored, "a second pending enrolment overwrote the enrolled secret")
}

func TestMFAEnrolment_AnExpiredPendingSecretIsRefused(t *testing.T) {
	// Arrange: the page is opened and left for more than ten minutes.
	f := newEnrolFixture(t)
	secret := f.open(mfaTestSession)
	*f.clock = f.clock.Add(11 * time.Minute)

	// Act
	w := f.enable(mfaTestSession, "totp_code="+f.code(secret))

	// Assert: back to the setup page with a reason; a fresh secret there.
	assert.Equal(t, http.StatusSeeOther, w.Code)
	assert.Equal(t, "/dashboard/settings/mfa", w.Header().Get("Location"))
	assert.Contains(t, flashMsg(t, w), "expired")
	assert.False(t, f.enabled())
	assert.NotEqual(t, secret, f.open(mfaTestSession), "an expired secret must not come back")
}

func TestMFAEnrolment_ARestartMidEnrolmentAsksToScanAgain(t *testing.T) {
	// Arrange: the page was opened by the process before this one — the
	// pending store is in memory and a deploy empties it.
	f := newEnrolFixture(t)
	secret := f.open(mfaTestSession)
	f.store = NewMFAEnrolmentStore()
	f.enbl = HandleMFAEnable(testMFATemplate(t), f.auth, f.mfa, f.store, zap.NewNop())

	// Act
	w := f.enable(mfaTestSession, "totp_code="+f.code(secret))

	// Assert: never enrolled with a secret the server no longer holds.
	assert.Equal(t, http.StatusSeeOther, w.Code)
	assert.Equal(t, "/dashboard/settings/mfa", w.Header().Get("Location"))
	assert.Contains(t, flashMsg(t, w), "Scan the new QR code")
	assert.False(t, f.enabled())
}

func TestMFAEnrolment_AWrongCodeKeepsThePendingSecret(t *testing.T) {
	// Arrange
	f := newEnrolFixture(t)
	secret := f.open(mfaTestSession)

	// Act: a typo.
	w := f.enable(mfaTestSession, "totp_code=000000")

	// Assert: the QR code already scanned stays valid.
	assert.Equal(t, http.StatusSeeOther, w.Code)
	assert.Equal(t, "/dashboard/settings/mfa", w.Header().Get("Location"))
	assert.Contains(t, flashMsg(t, w), "did not match")
	assert.False(t, f.enabled())
	assert.Equal(t, secret, f.open(mfaTestSession))
}

func TestMFAEnrolment_QRIsRenderedByTheServer(t *testing.T) {
	// Arrange
	f := newEnrolFixture(t)
	f.open(mfaTestSession)
	qr := HandleMFAQR(f.store, zap.NewNop())

	// Act
	w := httptest.NewRecorder()
	qr(w, f.req("GET", mfaTestSession, ""))

	// Assert: a PNG of the pending secret's otpauth URL, never cached.
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "image/png", w.Header().Get("Content-Type"))
	assert.Equal(t, "no-store", w.Header().Get("Cache-Control"))
	assert.Equal(t, "same-origin", w.Header().Get("Cross-Origin-Resource-Policy"))
	assert.Equal(t, "\x89PNG", w.Body.String()[:4])

	// No pending enrolment for the session: nothing to draw.
	w = httptest.NewRecorder()
	qr(w, f.req("GET", "a-session-with-no-enrolment", ""))
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestMFAEnrolment_AFailedEnableKeepsThePendingSecret(t *testing.T) {
	// Arrange: the code is right but enabling fails (here: the user is gone;
	// in production: the database refused the write).
	f := newEnrolFixture(t)
	secret := f.open(mfaTestSession)
	ghost := &dashauth.SessionData{UserID: "no-such-user", Email: "ghost@stored.ge", Role: "user"}
	_, err := f.store.Begin("ghost-session", ghost.UserID, func() (string, string, error) { return secret, "otpauth://totp/x", nil })
	require.NoError(t, err)
	req := httptest.NewRequest("POST", "/dashboard/settings/mfa/enable", strings.NewReader("totp_code="+f.code(secret)))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: dashauth.SessionCookieName, Value: "ghost-session"})
	req = req.WithContext(context.WithValue(req.Context(), dashauth.SessionKey, ghost))

	// Act
	w := httptest.NewRecorder()
	f.enbl(w, req)

	// Assert: an error, and the scanned QR code is still the one to use.
	assert.Equal(t, http.StatusSeeOther, w.Code)
	assert.Contains(t, flashMsg(t, w), "could not be enabled")
	_, still := f.store.Peek("ghost-session", ghost.UserID)
	assert.True(t, still, "the pending secret was thrown away with a failed enable")
}

func TestMFAEnrolmentStore_IsBoundedAndKeyedBySessionAndUser(t *testing.T) {
	// Arrange
	s := NewMFAEnrolmentStore()
	now := time.Now()
	s.now = func() time.Time { return now }
	s.max = 3
	n := 0
	gen := func() (string, string, error) { n++; return fmt.Sprintf("SECRET%d", n), "otpauth://totp/x", nil }

	// One pending secret per session: asking again returns the same one.
	a1, err := s.Begin("s1", "alice", gen)
	require.NoError(t, err)
	a2, err := s.Begin("s1", "alice", gen)
	require.NoError(t, err)
	assert.Equal(t, a1.Secret, a2.Secret)
	assert.Equal(t, now.Add(10*time.Minute), a1.Expires, "10-minute TTL from the first open")

	// A session id presented by another user gets nothing of the first one's.
	b, err := s.Begin("s1", "bob", gen)
	require.NoError(t, err)
	assert.NotEqual(t, a1.Secret, b.Secret)
	_, res := s.Confirm("s1", "alice", func(string) bool { return true })
	assert.Equal(t, MFAEnrolNone, res)

	// The cap: full of live entries → refused, not grown.
	_, err = s.Begin("s2", "u2", gen)
	require.NoError(t, err)
	_, err = s.Begin("s3", "u3", gen)
	require.NoError(t, err)
	_, err = s.Begin("s4", "u4", gen)
	require.ErrorIs(t, err, ErrMFAEnrolmentBusy)
	assert.Equal(t, 3, s.Len())

	// Expired entries are swept to make room.
	now = now.Add(11 * time.Minute)
	_, err = s.Begin("s4", "u4", gen)
	require.NoError(t, err)
	assert.Equal(t, 1, s.Len())

	// Confirm: a wrong code keeps the entry, a right one takes it (single use).
	_, res = s.Confirm("s4", "u4", func(string) bool { return false })
	assert.Equal(t, MFAEnrolMismatch, res)
	e, res := s.Confirm("s4", "u4", func(string) bool { return true })
	assert.Equal(t, MFAEnrolOK, res)
	assert.NotEmpty(t, e.Secret)
	_, res = s.Confirm("s4", "u4", func(string) bool { return true })
	assert.Equal(t, MFAEnrolNone, res)
}

func TestHandleMFASetup_RedirectsWhenAlreadyEnabled(t *testing.T) {
	tmpl := testMFATemplate(t)
	authSvc := newAuthWithMFAUser(t)
	mfaSvc := auth.NewMFAService("stored.ge")

	// Enable MFA for this user.
	user, _ := authSvc.GetUserByEmail(context.Background(), "mfa@stored.ge")
	require.NoError(t, authSvc.EnableMFA(context.Background(), user.ID, "SECRET", nil))

	handler := HandleMFASetup(tmpl, authSvc, mfaSvc, NewMFAEnrolmentStore(), zap.NewNop())

	req := httptest.NewRequest("GET", "/dashboard/settings/mfa", nil)
	req = req.WithContext(mfaSessionCtx(t, authSvc))

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusSeeOther, w.Code)
	assert.Equal(t, "/dashboard/settings", w.Header().Get("Location"))
}

func TestHandleMFASetup_NoSession(t *testing.T) {
	tmpl := testMFATemplate(t)
	handler := HandleMFASetup(tmpl, nil, nil, NewMFAEnrolmentStore(), zap.NewNop())

	req := httptest.NewRequest("GET", "/dashboard/settings/mfa", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusSeeOther, w.Code)
	assert.Equal(t, "/login", w.Header().Get("Location"))
}

// flashMsg is the message of the flash a response set.
func flashMsg(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	_, msg := flashOf(t, w)
	return msg
}

func TestHandleMFADisable_ValidPassword(t *testing.T) {
	tmpl := testSettingsTmpl(t)
	authSvc := newAuthWithMFAUser(t)

	// Enable MFA first.
	user, _ := authSvc.GetUserByEmail(context.Background(), "mfa@stored.ge")
	require.NoError(t, authSvc.EnableMFA(context.Background(), user.ID, "SECRET", nil))

	handler := HandleMFADisable(tmpl, authSvc, nil, zap.NewNop())

	form := strings.NewReader("password=password123")
	req := httptest.NewRequest("POST", "/dashboard/settings/mfa/disable", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req = req.WithContext(mfaSessionCtx(t, authSvc))

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusSeeOther, w.Code)

	enabled, _ := authSvc.IsMFAEnabled(context.Background(), user.ID)
	assert.False(t, enabled)
}

func TestHandleMFADisable_WrongPassword(t *testing.T) {
	tmpl := testSettingsTmpl(t)
	authSvc := newAuthWithMFAUser(t)

	user, _ := authSvc.GetUserByEmail(context.Background(), "mfa@stored.ge")
	require.NoError(t, authSvc.EnableMFA(context.Background(), user.ID, "SECRET", nil))

	handler := HandleMFADisable(tmpl, authSvc, nil, zap.NewNop())

	form := strings.NewReader("password=wrongpassword")
	req := httptest.NewRequest("POST", "/dashboard/settings/mfa/disable", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req = req.WithContext(mfaSessionCtx(t, authSvc))

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "error:Incorrect password")

	// MFA should still be enabled.
	enabled, _ := authSvc.IsMFAEnabled(context.Background(), user.ID)
	assert.True(t, enabled)
}

// R5-04: an empty or missing password field used to skip the password check
// entirely, so a hijacked session could strip the second factor.
func TestHandleMFADisable_MissingPasswordIsRejected(t *testing.T) {
	tmpl := testSettingsTmpl(t)
	authSvc := newAuthWithMFAUser(t)
	user, _ := authSvc.GetUserByEmail(context.Background(), "mfa@stored.ge")
	require.NoError(t, authSvc.EnableMFA(context.Background(), user.ID, "SECRET", nil))

	handler := HandleMFADisable(tmpl, authSvc, nil, zap.NewNop())

	for _, body := range []string{"", "password="} {
		req := httptest.NewRequest("POST", "/dashboard/settings/mfa/disable", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req = req.WithContext(mfaSessionCtx(t, authSvc))
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)

		assert.NotEqual(t, http.StatusSeeOther, w.Code, "body %q must not redirect as success", body)
		enabled, _ := authSvc.IsMFAEnabled(context.Background(), user.ID)
		assert.True(t, enabled, "MFA must stay enabled without a password (body %q)", body)
	}
}

func TestHandleMFADisable_PasswordlessOAuthUserCannotBypass(t *testing.T) {
	tmpl := testSettingsTmpl(t)
	authSvc := newAuthWithMFAUser(t)
	// An OAuth-only account has no password hash at all.
	oauthUser, _, _, err := authSvc.CreateUserFromOAuth(context.Background(), "oauth-mfa@stored.ge", "Test", "google", "g-1")
	require.NoError(t, err)
	require.NoError(t, authSvc.EnableMFA(context.Background(), oauthUser.ID, "SECRET", nil))

	store := dashauth.NewMemoryStore()
	token, _ := store.Create(context.Background(), dashauth.SessionData{UserID: oauthUser.ID, TenantID: oauthUser.TenantID, Email: oauthUser.Email, Role: "user"}, time.Hour)
	sd, _ := store.Get(context.Background(), token)

	handler := HandleMFADisable(tmpl, authSvc, nil, zap.NewNop())
	for _, body := range []string{"", "password=", "password=anything"} {
		req := httptest.NewRequest("POST", "/dashboard/settings/mfa/disable", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req = req.WithContext(context.WithValue(context.Background(), dashauth.SessionKey, sd))
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)

		assert.NotEqual(t, http.StatusSeeOther, w.Code, "body %q", body)
		enabled, _ := authSvc.IsMFAEnabled(context.Background(), oauthUser.ID)
		assert.True(t, enabled, "a passwordless account cannot disable MFA from the form (body %q)", body)
	}
}

// R5-22 / WP-R5-8: dropping the second factor signs out every OTHER device;
// the device that made the change keeps its session.
func TestHandleMFADisable_RevokesOtherSessions(t *testing.T) {
	tmpl := testSettingsTmpl(t)
	authSvc := newAuthWithMFAUser(t)
	user, _ := authSvc.GetUserByEmail(context.Background(), "mfa@stored.ge")
	require.NoError(t, authSvc.EnableMFA(context.Background(), user.ID, "JBSWY3DPEHPK3PXP", nil))

	store := dashauth.NewMemoryStore()
	sd := dashauth.SessionData{UserID: user.ID, TenantID: user.TenantID, Email: user.Email, Role: "user"}
	current, _ := store.Create(context.Background(), sd, time.Hour)
	other, _ := store.Create(context.Background(), sd, time.Hour)

	handler := HandleMFADisable(tmpl, authSvc, store, zap.NewNop())
	req := httptest.NewRequest("POST", "/dashboard/settings/mfa/disable", strings.NewReader("password=password123"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: dashauth.SessionCookieName, Value: current})
	cur, _ := store.Get(context.Background(), current)
	req = req.WithContext(context.WithValue(req.Context(), dashauth.SessionKey, cur))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	require.Equal(t, http.StatusSeeOther, w.Code)
	kept, _ := store.Get(context.Background(), current)
	assert.NotNil(t, kept, "the issuing device stays signed in")
	gone, _ := store.Get(context.Background(), other)
	assert.Nil(t, gone, "every other device is signed out")
}

// The admin reset answers the htmx form with a fragment and signs the user
// out everywhere.
func TestHandleAdminResetMFA_FragmentAndRevokesAllSessions(t *testing.T) {
	authSvc := newAuthWithMFAUser(t)
	user, _ := authSvc.GetUserByEmail(context.Background(), "mfa@stored.ge")
	require.NoError(t, authSvc.EnableMFA(context.Background(), user.ID, "JBSWY3DPEHPK3PXP", nil))
	store := dashauth.NewMemoryStore()
	tok, _ := store.Create(context.Background(), dashauth.SessionData{UserID: user.ID, Email: user.Email}, time.Hour)

	handler := HandleAdminResetMFA(authSvc, store, zap.NewNop())
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", user.TenantID)
	req := httptest.NewRequest("POST", "/admin/tenants/"+user.TenantID+"/reset-mfa", nil)
	req = req.WithContext(context.WithValue(adminCtx(t), chi.RouteCtxKey, rctx))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code, "a fragment, not a redirect")
	assert.Contains(t, w.Body.String(), "reset")
	enabled, _ := authSvc.IsMFAEnabled(context.Background(), user.ID)
	assert.False(t, enabled)
	s, _ := store.Get(context.Background(), tok)
	assert.Nil(t, s, "all sessions revoked")
}
