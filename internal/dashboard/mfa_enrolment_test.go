package dashboard

import (
	"context"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/FairForge/vaultaire/internal/auth"
	dashauth "github.com/FairForge/vaultaire/internal/dashboard/auth"
	"github.com/FairForge/vaultaire/internal/dashboard/middleware"
	"github.com/FairForge/vaultaire/internal/email"
	"github.com/go-chi/chi/v5"
	"github.com/pquerna/otp/totp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// WP-R12-8 through the production router and the real templates.

var (
	externalScript = regexp.MustCompile(`(?i)<script\b[^>]*\bsrc\s*=\s*["']?\s*(https?:)?//`)
	externalStyle  = regexp.MustCompile(`(?i)<link\b[^>]*\bhref\s*=\s*["']?\s*(https?:)?//`)
	cssImport      = regexp.MustCompile(`(?i)@import\s+(url\()?["']?\s*(https?:)?//`)
)

// No template this binary serves loads a script or a stylesheet from another
// host, and the CSP names no host at all. The 2FA setup page loaded a QR
// library from cdn.jsdelivr.net without SRI — on the page that shows a new
// TOTP secret — and `script-src` allowed the whole CDN.
func TestNoServedTemplateOrCSPNamesAnExternalScriptHost(t *testing.T) {
	var files int
	require.NoError(t, fs.WalkDir(Templates, "templates", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, rerr := Templates.ReadFile(p)
		require.NoError(t, rerr)
		files++
		src := string(b)
		assert.False(t, externalScript.MatchString(src), "%s loads a script from another host", p)
		assert.False(t, externalStyle.MatchString(src), "%s loads a stylesheet from another host", p)
		assert.False(t, cssImport.MatchString(src), "%s imports CSS from another host", p)
		return nil
	}))
	require.Greater(t, files, 30)
	require.NoError(t, fs.WalkDir(Static, "static", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".css") {
			return err
		}
		b, rerr := Static.ReadFile(p)
		require.NoError(t, rerr)
		assert.False(t, cssImport.MatchString(string(b)), "%s imports CSS from another host", p)
		return nil
	}))

	// The policy: every source is a keyword ('self', 'none', 'unsafe-inline')
	// or the data: scheme — never a host, a scheme://, or a wildcard.
	for _, directive := range strings.Split(middleware.ContentSecurityPolicy, ";") {
		fields := strings.Fields(directive)
		require.NotEmpty(t, fields)
		for _, src := range fields[1:] {
			ok := (strings.HasPrefix(src, "'") && strings.HasSuffix(src, "'")) || src == "data:"
			assert.True(t, ok, "CSP %s allows %q", fields[0], src)
		}
	}
	assert.NotContains(t, middleware.ContentSecurityPolicy, "jsdelivr")

	// And it is the policy actually sent, on a public page and behind a session.
	r, _, _ := setupTestRouter(t)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/login", nil))
	assert.Equal(t, middleware.ContentSecurityPolicy, w.Header().Get("Content-Security-Policy"))
}

func mfaRouter(t *testing.T) (chi.Router, *auth.AuthService, *http.Cookie, string) {
	t.Helper()
	logger := zap.NewNop()
	authSvc := auth.NewAuthService(nil, nil)
	sessions := dashauth.NewMemoryStore()
	r := chi.NewRouter()
	RegisterRoutes(r, Deps{Auth: authSvc, MFA: auth.NewMFAService("stored.ge"), Sessions: sessions, Logger: logger,
		Email: email.NewSender(logger), BaseURL: "http://localhost:8000"})
	u, err := authSvc.CreateUser(context.Background(), "enrol@stored.ge", "securepass123")
	require.NoError(t, err)
	tok, err := sessions.Create(context.Background(), dashauth.SessionData{UserID: u.ID, TenantID: "t-enrol", Email: u.Email, Role: "user"}, time.Hour)
	require.NoError(t, err)
	return r, authSvc, &http.Cookie{Name: dashauth.SessionCookieName, Value: tok}, u.ID
}

func TestMFAEnrolment_ThroughTheRouterWithTheRealPage(t *testing.T) {
	// Arrange
	r, authSvc, session, userID := mfaRouter(t)
	get := func(path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", path, nil)
		req.AddCookie(session)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}

	// Act: open the setup page.
	w := get("/dashboard/settings/mfa")

	// Assert: the page as served.
	require.Equal(t, http.StatusOK, w.Code)
	page := w.Body.String()
	assert.Equal(t, "no-store", w.Header().Get("Cache-Control"))
	assert.False(t, externalScript.MatchString(page), "the setup page loads a script from another host")
	assert.True(t, strings.Contains(page, `<img src="/dashboard/settings/mfa/qr.png"`), "the QR code is an image the server renders")
	assert.False(t, strings.Contains(page, `name="secret"`), "the secret must not be a form field")
	assert.False(t, strings.Contains(page, `name="backup_codes"`), "backup codes must not be a form field")
	assert.False(t, strings.Contains(page, "Backup Codes"), "backup codes are not on the setup page")
	assert.True(t, strings.Contains(page, `name="password"`), "enrolment asks for the password (WP-R12-15)")
	key := regexp.MustCompile(`<code>([A-Z2-7]{32})</code>`).FindStringSubmatch(page)
	require.NotNil(t, key, "the manual key is shown")
	token := regexp.MustCompile(`name="csrf_token" value="([0-9a-f]{64})"`).FindStringSubmatch(page)
	require.NotNil(t, token)

	// The QR image belongs to the session's pending secret.
	qr := get("/dashboard/settings/mfa/qr.png")
	require.Equal(t, http.StatusOK, qr.Code)
	assert.Equal(t, "image/png", qr.Header().Get("Content-Type"))
	assert.Equal(t, "no-store", qr.Header().Get("Cache-Control"))
	noSession := httptest.NewRecorder()
	r.ServeHTTP(noSession, httptest.NewRequest("GET", "/dashboard/settings/mfa/qr.png", nil))
	assert.Equal(t, http.StatusSeeOther, noSession.Code, "no session, no QR code")

	// Act: confirm with the 6-digit code and the password — and nothing else.
	code, err := totp.GenerateCode(key[1], time.Now())
	require.NoError(t, err)
	post := func(path string, form url.Values) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", path, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.AddCookie(session)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}
	w = post("/dashboard/settings/mfa/enable", url.Values{"csrf_token": {token[1]}, "totp_code": {code}})
	require.Equal(t, http.StatusSeeOther, w.Code, "without the password the code alone enrols nothing")
	on, _ := authSvc.IsMFAEnabled(context.Background(), userID)
	require.False(t, on)
	w = post("/dashboard/settings/mfa/enable", url.Values{"csrf_token": {token[1]}, "totp_code": {code}, "password": {"securepass123"}})

	// Assert: enrolled with the server's secret; ten backup codes, shown here.
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "no-store", w.Header().Get("Cache-Control"))
	done := w.Body.String()
	codes := regexp.MustCompile(`<code>([A-Z2-7]{8})</code>`).FindAllStringSubmatch(done, -1)
	require.Len(t, codes, 10)
	assert.False(t, strings.Contains(done, key[1]), "the result page no longer shows the secret")
	stored, err := authSvc.GetMFASecret(context.Background(), userID)
	require.NoError(t, err)
	assert.Equal(t, key[1], stored)
	ok, err := authSvc.ValidateBackupCode(context.Background(), userID, codes[0][1])
	require.NoError(t, err)
	assert.True(t, ok)

	// The setup page and the QR code are gone once enrolled.
	assert.Equal(t, http.StatusSeeOther, get("/dashboard/settings/mfa").Code)
	assert.Equal(t, http.StatusNotFound, get("/dashboard/settings/mfa/qr.png").Code)

	// Regenerate the backup codes from the settings page (WP-R12-15): the
	// form is there, it needs the password, the old codes die.
	settings := get("/dashboard/settings")
	require.Equal(t, http.StatusOK, settings.Code)
	assert.True(t, strings.Contains(settings.Body.String(), `action="/dashboard/settings/mfa/backup-codes"`))
	w = post("/dashboard/settings/mfa/backup-codes", url.Values{"csrf_token": {token[1]}})
	assert.Equal(t, http.StatusSeeOther, w.Code, "no password, no new codes")
	ok, err = authSvc.ValidateBackupCode(context.Background(), userID, codes[1][1])
	require.NoError(t, err)
	assert.True(t, ok, "the old codes still work after a refused attempt")
	w = post("/dashboard/settings/mfa/backup-codes", url.Values{"csrf_token": {token[1]}, "password": {"securepass123"}})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, "no-store", w.Header().Get("Cache-Control"))
	fresh := regexp.MustCompile(`<code>([A-Z2-7]{8})</code>`).FindAllStringSubmatch(w.Body.String(), -1)
	require.Len(t, fresh, 10)
	assert.True(t, strings.Contains(w.Body.String(), "New Backup Codes"))
	ok, _ = authSvc.ValidateBackupCode(context.Background(), userID, codes[2][1])
	assert.False(t, ok, "an old code is dead")
	ok, _ = authSvc.ValidateBackupCode(context.Background(), userID, fresh[0][1])
	assert.True(t, ok, "a new code works")
}
