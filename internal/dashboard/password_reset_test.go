package dashboard

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	dashauth "github.com/FairForge/vaultaire/internal/dashboard/auth"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGetForgotPassword(t *testing.T) {
	r, _, _ := setupTestRouter(t)
	req := httptest.NewRequest("GET", "/forgot-password", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	body := w.Body.String()
	assert.Contains(t, body, "Reset Password")
	assert.Contains(t, body, "Send Reset Link")
}

func TestPostForgotPassword_KnownEmail(t *testing.T) {
	r, authSvc, _ := setupTestRouter(t)
	authSvc.SetVerifySecret("test-secret-key")
	_, _ = authSvc.CreateUser(context.Background(), "knows@stored.ge", "OldPass123!")

	form := url.Values{"email": {"knows@stored.ge"}}
	req := httptest.NewRequest("POST", "/forgot-password", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	// Generic success message regardless of whether email exists.
	assert.Contains(t, w.Body.String(), "If that email is registered")
}

func TestPostForgotPassword_UnknownEmailStillSucceeds(t *testing.T) {
	// Anti-enumeration: unknown emails get the same success page.
	r, authSvc, _ := setupTestRouter(t)
	authSvc.SetVerifySecret("test-secret-key")

	form := url.Values{"email": {"ghost@stored.ge"}}
	req := httptest.NewRequest("POST", "/forgot-password", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "If that email is registered")
}

func TestGetResetPassword(t *testing.T) {
	r, _, _ := setupTestRouter(t)
	req := httptest.NewRequest("GET", "/reset-password?token=sometoken", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	body := w.Body.String()
	assert.Contains(t, body, "Choose a New Password")
	assert.Contains(t, body, `value="sometoken"`)
}

func TestGetResetPassword_MissingToken(t *testing.T) {
	r, _, _ := setupTestRouter(t)
	req := httptest.NewRequest("GET", "/reset-password", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "Missing reset token")
}

func TestPostResetPassword_Success(t *testing.T) {
	r, authSvc, sessions := setupTestRouter(t)
	authSvc.SetVerifySecret("test-secret-key")

	_, _ = authSvc.CreateUser(context.Background(), "reset@stored.ge", "OldPass123!")
	user, _ := authSvc.GetUserByEmail(context.Background(), "reset@stored.ge")

	// Pre-create some sessions for this user.
	tokens := make([]string, 0, 3)
	for i := 0; i < 3; i++ {
		tk, err := sessions.Create(context.Background(), dashauth.SessionData{
			UserID: user.ID, TenantID: user.TenantID, Email: user.Email, Role: "user",
		}, 24*time.Hour)
		require.NoError(t, err)
		tokens = append(tokens, tk)
	}

	resetToken, err := authSvc.RequestPasswordReset(context.Background(), "reset@stored.ge")
	require.NoError(t, err)

	form := url.Values{
		"token":            {resetToken},
		"new_password":     {"NewSecurePass1"},
		"confirm_password": {"NewSecurePass1"},
	}
	req := httptest.NewRequest("POST", "/reset-password", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusSeeOther, w.Code)
	assert.Equal(t, "/login", w.Header().Get("Location"))

	// New password should validate.
	valid, err := authSvc.ValidatePassword(context.Background(), "reset@stored.ge", "NewSecurePass1")
	require.NoError(t, err)
	assert.True(t, valid)

	// All existing sessions should be invalidated.
	for _, tk := range tokens {
		sd, _ := sessions.Get(context.Background(), tk)
		assert.Nil(t, sd, "session %s should be invalidated", tk)
	}
}

func TestPostResetPassword_InvalidToken(t *testing.T) {
	r, authSvc, _ := setupTestRouter(t)
	authSvc.SetVerifySecret("test-secret-key")

	form := url.Values{
		"token":            {"bogus"},
		"new_password":     {"NewSecurePass1"},
		"confirm_password": {"NewSecurePass1"},
	}
	req := httptest.NewRequest("POST", "/reset-password", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "Invalid or expired reset link")
}

func TestPostResetPassword_PasswordsDoNotMatch(t *testing.T) {
	r, authSvc, _ := setupTestRouter(t)
	authSvc.SetVerifySecret("test-secret-key")

	_, _ = authSvc.CreateUser(context.Background(), "mm@stored.ge", "OldPass123!")
	resetToken, err := authSvc.RequestPasswordReset(context.Background(), "mm@stored.ge")
	require.NoError(t, err)

	form := url.Values{
		"token":            {resetToken},
		"new_password":     {"NewSecurePass1"},
		"confirm_password": {"DifferentPass2"},
	}
	req := httptest.NewRequest("POST", "/reset-password", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "do not match")
}

func TestPostResetPassword_ShortPassword(t *testing.T) {
	r, authSvc, _ := setupTestRouter(t)
	authSvc.SetVerifySecret("test-secret-key")

	_, _ = authSvc.CreateUser(context.Background(), "short@stored.ge", "OldPass123!")
	resetToken, err := authSvc.RequestPasswordReset(context.Background(), "short@stored.ge")
	require.NoError(t, err)

	form := url.Values{
		"token":            {resetToken},
		"new_password":     {"short"},
		"confirm_password": {"short"},
	}
	req := httptest.NewRequest("POST", "/reset-password", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "at least 8 characters")
}
