package dashboard

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/FairForge/vaultaire/internal/auth"
	dashauth "github.com/FairForge/vaultaire/internal/dashboard/auth"
	"github.com/FairForge/vaultaire/internal/dashboard/handlers"
	"github.com/FairForge/vaultaire/internal/email"
	"github.com/go-chi/chi/v5"
	"github.com/pquerna/otp/totp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// Review R12 — the sign-in flow: every outcome is an audit row, repeated
// failures lock the account, a TOTP code is single-use, public pages carry
// the security headers and /static/ does not list directories.

func postForm(r chi.Router, path string, form url.Values, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for _, c := range cookies {
		req.AddCookie(c)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestLogin_WritesAuditRowsForFailureAndSuccess(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	logger := zap.NewNop()
	authSvc := auth.NewAuthService(nil, nil)
	_, _ = authSvc.CreateUser(context.Background(), "audited@stored.ge", "securepass123")
	r := chi.NewRouter()
	RegisterRoutes(r, Deps{DB: db, Auth: authSvc, Sessions: dashauth.NewMemoryStore(), Logger: logger,
		Email: email.NewSender(logger), BaseURL: "http://localhost:8000"})

	// Failure: one auth.login_failed row with the reason and the e-mail.
	mock.ExpectExec(`INSERT INTO audit_logs`).
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), "auth", "auth.login_failed", sqlmock.AnyArg(), "success", "warning",
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))
	w := postForm(r, "/login", url.Values{"email": {"audited@stored.ge"}, "password": {"wrong"}})
	assert.Equal(t, http.StatusUnauthorized, w.Code)

	// Unknown user: same message, still a row (reason=unknown_user).
	mock.ExpectExec(`INSERT INTO audit_logs`).
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), "auth", "auth.login_failed", sqlmock.AnyArg(), "success", "warning",
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))
	w = postForm(r, "/login", url.Values{"email": {"ghost@stored.ge"}, "password": {"wrong"}})
	assert.Equal(t, http.StatusUnauthorized, w.Code)
	assert.Contains(t, w.Body.String(), "Invalid email or password")

	// Success: the role lookup, then auth.login_succeeded with mfa=none.
	mock.ExpectQuery(`SELECT role FROM users`).WillReturnRows(sqlmock.NewRows([]string{"role"}).AddRow("user"))
	mock.ExpectExec(`INSERT INTO audit_logs`).
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), "auth", "auth.login_succeeded", sqlmock.AnyArg(), "success", "info",
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))
	w = postForm(r, "/login", url.Values{"email": {"audited@stored.ge"}, "password": {"securepass123"}})
	assert.Equal(t, http.StatusSeeOther, w.Code)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestLogin_LocksAccountAfterRepeatedFailuresEvenWithTheRightPassword(t *testing.T) {
	r, authSvc, _ := setupTestRouter(t)
	_, _ = authSvc.CreateUser(context.Background(), "locked@stored.ge", "securepass123")

	// Ten failures from ten different IPs (the per-IP limiter would stop a
	// single source at five; the lockout is per account).
	for i := 0; i < 10; i++ {
		req := httptest.NewRequest("POST", "/login", strings.NewReader(url.Values{
			"email": {"locked@stored.ge"}, "password": {"wrong"}}.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.RemoteAddr = "198.51.100." + string(rune('1'+i)) + ":4000"
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		require.Equal(t, http.StatusUnauthorized, w.Code)
	}

	req := httptest.NewRequest("POST", "/login", strings.NewReader(url.Values{
		"email": {"locked@stored.ge"}, "password": {"securepass123"}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.RemoteAddr = "203.0.113.77:4000"
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusUnauthorized, w.Code, "locked: the right password is refused too")
	assert.Contains(t, w.Body.String(), "Invalid email or password", "the message does not reveal the lock")
	for _, c := range w.Result().Cookies() {
		assert.NotEqual(t, dashauth.SessionCookieName, c.Name)
	}
}

func TestVerify2FA_RejectsReplayedTOTPCode(t *testing.T) {
	logger := zap.NewNop()
	authSvc := auth.NewAuthService(nil, nil)
	user, _, _, err := authSvc.CreateUserWithTenant(context.Background(), "replay@stored.ge", "securepass123", "x")
	require.NoError(t, err)
	const secret = "JBSWY3DPEHPK3PXP"
	require.NoError(t, authSvc.EnableMFA(context.Background(), user.ID, secret, nil))
	pendingStore := NewMFAPendingStore()
	r := chi.NewRouter()
	RegisterRoutes(r, Deps{Auth: authSvc, MFA: auth.NewMFAService("stored.ge"), MFAPending: pendingStore,
		Sessions: dashauth.NewMemoryStore(), Logger: logger, Email: email.NewSender(logger), BaseURL: "http://localhost:8000"})

	code, err := totp.GenerateCode(secret, time.Now())
	require.NoError(t, err)

	login := func() *http.Cookie {
		w := postForm(r, "/login", url.Values{"email": {"replay@stored.ge"}, "password": {"securepass123"}})
		require.Equal(t, http.StatusSeeOther, w.Code)
		require.Equal(t, "/login/verify-2fa", w.Header().Get("Location"))
		for _, c := range w.Result().Cookies() {
			if c.Name == handlers.MFAPendingCookie {
				return c
			}
		}
		t.Fatal("no pending cookie")
		return nil
	}

	first := postForm(r, "/login/verify-2fa", url.Values{"totp_code": {code}}, login())
	assert.Equal(t, http.StatusSeeOther, first.Code)
	assert.Equal(t, "/dashboard", first.Header().Get("Location"), "first use of the code signs in")

	second := postForm(r, "/login/verify-2fa", url.Values{"totp_code": {code}}, login())
	assert.Equal(t, http.StatusUnauthorized, second.Code, "the same code again is a replay (RFC 6238 §5.2)")
	assert.Contains(t, second.Body.String(), "Invalid code")
}

func TestPublicPages_CarrySecurityHeaders(t *testing.T) {
	r, _, _ := setupTestRouter(t)
	for _, p := range []string{"/login", "/register", "/forgot-password", "/abuse", "/legal/privacy"} {
		req := httptest.NewRequest("GET", p, nil)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		require.Equal(t, http.StatusOK, w.Code, p)
		assert.Contains(t, w.Header().Get("Content-Security-Policy"), "default-src 'self'", p)
		assert.Equal(t, "DENY", w.Header().Get("X-Frame-Options"), p)
	}
}

func TestStatic_NoDirectoryListingAndVersionedCaching(t *testing.T) {
	r, _, _ := setupTestRouter(t)
	for _, p := range []string{"/static/", "/static/js/", "/static/css/"} {
		req := httptest.NewRequest("GET", p, nil)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		assert.Equal(t, http.StatusNotFound, w.Code, "no listing for %s", p)
	}
	req := httptest.NewRequest("GET", "/static/css/style.css?v=abc123", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "public, max-age=31536000, immutable", w.Header().Get("Cache-Control"))
	req = httptest.NewRequest("GET", "/static/css/style.css", nil)
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	assert.Equal(t, "public, max-age=300", w.Header().Get("Cache-Control"), "unversioned URLs stay short-lived")
}

func TestRegister_IsRateLimitedPerIP(t *testing.T) {
	r, _, _ := setupTestRouter(t)
	var last int
	// A too-short password is refused before bcrypt runs, so eleven posts
	// land inside the limiter's minute even under -race.
	for i := 0; i < 11; i++ {
		req := httptest.NewRequest("POST", "/register", strings.NewReader(url.Values{
			"email": {"mass" + string(rune('a'+i)) + "@stored.ge"}, "password": {"short"}}.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.RemoteAddr = "192.0.2.10:5000"
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		last = w.Code
	}
	assert.Equal(t, http.StatusTooManyRequests, last, "the 11th registration from one IP inside a minute is refused")
}

// Checklist item 7: the register page carries the landing URL's utm_* and the
// referring host as hidden fields and stores them on the user at signup.
func TestRegister_StoresSignupAttribution(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	logger := zap.NewNop()
	authSvc := auth.NewAuthService(nil, nil)
	r := chi.NewRouter()
	RegisterRoutes(r, Deps{DB: db, Auth: authSvc, Sessions: dashauth.NewMemoryStore(), Logger: logger,
		Email: email.NewSender(logger), BaseURL: "http://localhost:8000"})

	// GET /register?utm_source=let&ref=https://lowendtalk.com/x → hidden fields.
	req := httptest.NewRequest("GET", "/register?utm_source=let&utm_campaign=launch&ref=https://lowendtalk.com/discussion/1", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), `name="utm_source" value="let"`)
	assert.Contains(t, w.Body.String(), `name="referrer" value="lowendtalk.com"`, "host only, no path")

	// POST stores it on the user row (the account itself is in memory here).
	mock.ExpectExec(`UPDATE users SET signup_referrer`).
		WithArgs("lowendtalk.com", "let", "", "launch", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	w = postForm(r, "/register", url.Values{"email": {"attr@stored.ge"}, "password": {"securepass123"},
		"referrer": {"lowendtalk.com"}, "utm_source": {"let"}, "utm_campaign": {"launch"}})
	assert.Equal(t, http.StatusOK, w.Code)
	require.NoError(t, mock.ExpectationsWereMet())
}
