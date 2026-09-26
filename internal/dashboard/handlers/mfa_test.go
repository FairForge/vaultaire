package handlers

import (
	"context"
	"html/template"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/FairForge/vaultaire/internal/auth"
	dashauth "github.com/FairForge/vaultaire/internal/dashboard/auth"
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

func TestHandleMFASetup_ShowsSecret(t *testing.T) {
	tmpl := testMFATemplate(t)
	authSvc := newAuthWithMFAUser(t)
	mfaSvc := auth.NewMFAService("stored.ge")

	handler := HandleMFASetup(tmpl, authSvc, mfaSvc, zap.NewNop())

	req := httptest.NewRequest("GET", "/dashboard/settings/mfa", nil)
	req = req.WithContext(mfaSessionCtx(t, authSvc))

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	body := w.Body.String()
	assert.Contains(t, body, "secret:")
	assert.Contains(t, body, "codes:10")
}

func TestHandleMFASetup_RedirectsWhenAlreadyEnabled(t *testing.T) {
	tmpl := testMFATemplate(t)
	authSvc := newAuthWithMFAUser(t)
	mfaSvc := auth.NewMFAService("stored.ge")

	// Enable MFA for this user.
	user, _ := authSvc.GetUserByEmail(context.Background(), "mfa@stored.ge")
	require.NoError(t, authSvc.EnableMFA(context.Background(), user.ID, "SECRET", nil))

	handler := HandleMFASetup(tmpl, authSvc, mfaSvc, zap.NewNop())

	req := httptest.NewRequest("GET", "/dashboard/settings/mfa", nil)
	req = req.WithContext(mfaSessionCtx(t, authSvc))

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusSeeOther, w.Code)
	assert.Equal(t, "/dashboard/settings", w.Header().Get("Location"))
}

func TestHandleMFASetup_NoSession(t *testing.T) {
	tmpl := testMFATemplate(t)
	handler := HandleMFASetup(tmpl, nil, nil, zap.NewNop())

	req := httptest.NewRequest("GET", "/dashboard/settings/mfa", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusSeeOther, w.Code)
	assert.Equal(t, "/login", w.Header().Get("Location"))
}

func TestHandleMFAEnable_ValidCode(t *testing.T) {
	tmpl := testSettingsTmpl(t)
	authSvc := newAuthWithMFAUser(t)
	mfaSvc := auth.NewMFAService("stored.ge")

	handler := HandleMFAEnable(tmpl, authSvc, mfaSvc, zap.NewNop())

	// A real TOTP for the secret — ValidateCode has no test shortcut (R5-15).
	code, err := totp.GenerateCode("JBSWY3DPEHPK3PXP", time.Now())
	require.NoError(t, err)
	form := strings.NewReader("secret=JBSWY3DPEHPK3PXP&totp_code=" + code + "&backup_codes=CODE1,CODE2")
	req := httptest.NewRequest("POST", "/dashboard/settings/mfa/enable", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req = req.WithContext(mfaSessionCtx(t, authSvc))

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusSeeOther, w.Code)
	assert.Equal(t, "/dashboard/settings", w.Header().Get("Location"))

	// Verify MFA is now enabled.
	user, _ := authSvc.GetUserByEmail(context.Background(), "mfa@stored.ge")
	enabled, _ := authSvc.IsMFAEnabled(context.Background(), user.ID)
	assert.True(t, enabled)
}

func TestHandleMFAEnable_InvalidCode(t *testing.T) {
	tmpl := testSettingsTmpl(t)
	authSvc := newAuthWithMFAUser(t)
	mfaSvc := auth.NewMFAService("stored.ge")

	handler := HandleMFAEnable(tmpl, authSvc, mfaSvc, zap.NewNop())

	form := strings.NewReader("secret=JBSWY3DPEHPK3PXP&totp_code=000000")
	req := httptest.NewRequest("POST", "/dashboard/settings/mfa/enable", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req = req.WithContext(mfaSessionCtx(t, authSvc))

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	// Should redirect back to setup page.
	assert.Equal(t, http.StatusSeeOther, w.Code)
	assert.Equal(t, "/dashboard/settings/mfa", w.Header().Get("Location"))
}

func TestHandleMFADisable_ValidPassword(t *testing.T) {
	tmpl := testSettingsTmpl(t)
	authSvc := newAuthWithMFAUser(t)

	// Enable MFA first.
	user, _ := authSvc.GetUserByEmail(context.Background(), "mfa@stored.ge")
	require.NoError(t, authSvc.EnableMFA(context.Background(), user.ID, "SECRET", nil))

	handler := HandleMFADisable(tmpl, authSvc, zap.NewNop())

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

	handler := HandleMFADisable(tmpl, authSvc, zap.NewNop())

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

	handler := HandleMFADisable(tmpl, authSvc, zap.NewNop())

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

	handler := HandleMFADisable(tmpl, authSvc, zap.NewNop())
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
