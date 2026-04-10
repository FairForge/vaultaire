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
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func testSettingsTemplate(t *testing.T) *template.Template {
	t.Helper()
	tmpl := template.Must(template.New("base").Parse(
		`{{define "base"}}` +
			`{{block "nav" .}}{{end}}` +
			`{{block "content" .}}{{end}}` +
			`{{end}}`))
	template.Must(tmpl.Parse(
		`{{define "title"}}Settings{{end}}` +
			`{{define "content"}}` +
			`<h1>Settings</h1>` +
			`<span class="email">{{.ProfileEmail}}</span>` +
			`<span class="company">{{.ProfileCompany}}</span>` +
			`<span class="notif">{{.EmailNotifications}}</span>` +
			`{{if .ProfileSuccess}}<span class="profile-ok">{{.ProfileSuccess}}</span>{{end}}` +
			`{{if .PasswordError}}<span class="pw-err">{{.PasswordError}}</span>{{end}}` +
			`{{if .PasswordSuccess}}<span class="pw-ok">{{.PasswordSuccess}}</span>{{end}}` +
			`{{if .NotifSuccess}}<span class="notif-ok">{{.NotifSuccess}}</span>{{end}}` +
			`{{end}}`))
	return tmpl
}

func settingsSessionCtx(t *testing.T) context.Context {
	t.Helper()
	store := dashauth.NewMemoryStore()
	token, err := store.Create(context.Background(), dashauth.SessionData{
		UserID:   "user-s1",
		TenantID: "tenant-s1",
		Email:    "settings@stored.ge",
		Role:     "user",
	}, time.Hour)
	require.NoError(t, err)

	sd, err := store.Get(context.Background(), token)
	require.NoError(t, err)

	return context.WithValue(context.Background(), dashauth.SessionKey, sd)
}

func TestHandleSettings_NoDB(t *testing.T) {
	tmpl := testSettingsTemplate(t)
	handler := HandleSettings(tmpl, nil, nil, nil, zap.NewNop())

	req := httptest.NewRequest("GET", "/dashboard/settings", nil)
	req = req.WithContext(settingsSessionCtx(t))

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	body := w.Body.String()
	assert.Contains(t, body, "Settings")
	assert.Contains(t, body, "settings@stored.ge")
}

func TestHandleSettings_NoSession(t *testing.T) {
	tmpl := testSettingsTemplate(t)
	handler := HandleSettings(tmpl, nil, nil, nil, zap.NewNop())

	req := httptest.NewRequest("GET", "/dashboard/settings", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusSeeOther, w.Code)
	assert.Equal(t, "/login", w.Header().Get("Location"))
}

func TestHandleChangePassword_Validation(t *testing.T) {
	tmpl := testSettingsTemplate(t)
	handler := HandleChangePassword(tmpl, nil, nil, nil, zap.NewNop())

	t.Run("short password", func(t *testing.T) {
		form := strings.NewReader("current_password=old&new_password=short&confirm_password=short")
		req := httptest.NewRequest("POST", "/dashboard/settings/password", form)
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req = req.WithContext(settingsSessionCtx(t))

		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)

		assert.Equal(t, http.StatusBadRequest, w.Code)
		assert.Contains(t, w.Body.String(), "at least 8 characters")
	})

	t.Run("mismatch", func(t *testing.T) {
		form := strings.NewReader("current_password=old&new_password=longpassword1&confirm_password=longpassword2")
		req := httptest.NewRequest("POST", "/dashboard/settings/password", form)
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req = req.WithContext(settingsSessionCtx(t))

		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)

		assert.Equal(t, http.StatusBadRequest, w.Code)
		assert.Contains(t, w.Body.String(), "do not match")
	})

	t.Run("same password", func(t *testing.T) {
		form := strings.NewReader("current_password=samepassword&new_password=samepassword&confirm_password=samepassword")
		req := httptest.NewRequest("POST", "/dashboard/settings/password", form)
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req = req.WithContext(settingsSessionCtx(t))

		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)

		assert.Equal(t, http.StatusBadRequest, w.Code)
		assert.Contains(t, w.Body.String(), "different from current")
	})
}

func TestChangePassword_WithAuth(t *testing.T) {
	// Create a real AuthService with a user to test password change end-to-end.
	authSvc := auth.NewAuthService(nil, nil)
	ctx := context.Background()

	user, _, _, err := authSvc.CreateUserWithTenant(ctx, "pw-test@stored.ge", "oldpassword123", "Test Corp")
	require.NoError(t, err)

	// Should fail with wrong current password.
	err = authSvc.ChangePassword(ctx, user.ID, "wrongpassword", "newpassword123")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "incorrect")

	// Should succeed with correct current password.
	err = authSvc.ChangePassword(ctx, user.ID, "oldpassword123", "newpassword123")
	require.NoError(t, err)

	// Old password should no longer work.
	valid, _ := authSvc.ValidatePassword(ctx, "pw-test@stored.ge", "oldpassword123")
	assert.False(t, valid)

	// New password should work.
	valid, _ = authSvc.ValidatePassword(ctx, "pw-test@stored.ge", "newpassword123")
	assert.True(t, valid)
}

func TestHandleChangePassword_InvalidatesOtherSessions(t *testing.T) {
	// Successful password change should wipe every session for the user
	// except the one that made the request.
	tmpl := testSettingsTemplate(t)
	authSvc := auth.NewAuthService(nil, nil)
	ctx := context.Background()

	user, _, _, err := authSvc.CreateUserWithTenant(ctx, "multi@stored.ge", "oldpassword123", "Test")
	require.NoError(t, err)

	store := dashauth.NewMemoryStore()
	// Current device token (will be on the request cookie).
	currentToken, err := store.Create(ctx, dashauth.SessionData{
		UserID: user.ID, TenantID: user.TenantID, Email: user.Email, Role: "user",
	}, time.Hour)
	require.NoError(t, err)
	// Two extra devices we expect to be logged out.
	otherA, err := store.Create(ctx, dashauth.SessionData{
		UserID: user.ID, TenantID: user.TenantID, Email: user.Email, Role: "user",
	}, time.Hour)
	require.NoError(t, err)
	otherB, err := store.Create(ctx, dashauth.SessionData{
		UserID: user.ID, TenantID: user.TenantID, Email: user.Email, Role: "user",
	}, time.Hour)
	require.NoError(t, err)

	handler := HandleChangePassword(tmpl, authSvc, nil, store, zap.NewNop())

	form := strings.NewReader("current_password=oldpassword123&new_password=newpassword123&confirm_password=newpassword123")
	req := httptest.NewRequest("POST", "/dashboard/settings/password", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: dashauth.SessionCookieName, Value: currentToken})

	sd, _ := store.Get(ctx, currentToken)
	req = req.WithContext(context.WithValue(context.Background(), dashauth.SessionKey, sd))

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "signed out of other devices")

	// Current session must still be valid.
	if sd, _ := store.Get(ctx, currentToken); sd == nil {
		t.Error("expected current session to survive")
	}
	// Other sessions must be revoked.
	if sd, _ := store.Get(ctx, otherA); sd != nil {
		t.Error("expected other device A to be signed out")
	}
	if sd, _ := store.Get(ctx, otherB); sd != nil {
		t.Error("expected other device B to be signed out")
	}
}
