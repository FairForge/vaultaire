package middleware_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/FairForge/vaultaire/internal/auth"
	dashauth "github.com/FairForge/vaultaire/internal/dashboard/auth"
	"github.com/FairForge/vaultaire/internal/dashboard/middleware"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func injectSession(r *http.Request, sd *dashauth.SessionData) *http.Request {
	ctx := context.WithValue(r.Context(), dashauth.SessionKey, sd)
	return r.WithContext(ctx)
}

func TestAdminMFA_Required(t *testing.T) {
	authSvc := auth.NewAuthService(nil, nil)

	handler := middleware.RequireAdminMFA(authSvc)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/admin", nil)
	req = injectSession(req, &dashauth.SessionData{
		UserID: "admin-no-mfa",
		Role:   "admin",
	})
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusSeeOther, w.Code)
	assert.Equal(t, "/dashboard/settings/mfa", w.Header().Get("Location"))
}

func TestAdminMFA_Enabled(t *testing.T) {
	authSvc := auth.NewAuthService(nil, nil)
	user, _, _, err := authSvc.CreateUserWithTenant(context.Background(), "mfa-admin@test.com", "password123", "TestCo")
	require.NoError(t, err)
	require.NoError(t, authSvc.EnableMFA(context.Background(), user.ID, "JBSWY3DPEHPK3PXP", []string{"backup1"}))

	handler := middleware.RequireAdminMFA(authSvc)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/admin", nil)
	req = injectSession(req, &dashauth.SessionData{
		UserID: user.ID,
		Role:   "admin",
	})
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
}

func TestAdminMFA_NilAuthService(t *testing.T) {
	handler := middleware.RequireAdminMFA(nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/admin", nil)
	req = injectSession(req, &dashauth.SessionData{
		UserID: "admin-user",
		Role:   "admin",
	})
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
}
