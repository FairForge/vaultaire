package dashboard

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/FairForge/vaultaire/internal/auth"
	dashauth "github.com/FairForge/vaultaire/internal/dashboard/auth"
	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func setupTestRouter(t *testing.T) (chi.Router, *auth.AuthService, dashauth.SessionStore) {
	t.Helper()
	authSvc := auth.NewAuthService(nil, nil)
	sessions := dashauth.NewMemoryStore()
	r := chi.NewRouter()
	RegisterRoutes(r, Deps{
		Auth:     authSvc,
		Sessions: sessions,
		Logger:   zap.NewNop(),
	})
	return r, authSvc, sessions
}

func TestGetLogin(t *testing.T) {
	r, _, _ := setupTestRouter(t)
	req := httptest.NewRequest("GET", "/login", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "Sign In")
}

func TestGetRegister(t *testing.T) {
	r, _, _ := setupTestRouter(t)
	req := httptest.NewRequest("GET", "/register", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "Create Account")
}

func TestPostRegister(t *testing.T) {
	r, _, _ := setupTestRouter(t)

	form := url.Values{
		"email":    {"test@stored.ge"},
		"password": {"securepass123"},
		"company":  {"Test Corp"},
	}
	req := httptest.NewRequest("POST", "/register", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	// Should redirect to /dashboard
	assert.Equal(t, http.StatusSeeOther, w.Code)
	assert.Equal(t, "/dashboard", w.Header().Get("Location"))

	// Should have a session cookie
	cookies := w.Result().Cookies()
	var sessionCookie *http.Cookie
	for _, c := range cookies {
		if c.Name == "vaultaire_session" {
			sessionCookie = c
		}
	}
	require.NotNil(t, sessionCookie, "expected session cookie")
	assert.NotEmpty(t, sessionCookie.Value)
}

func TestPostRegister_DuplicateEmail(t *testing.T) {
	r, authSvc, _ := setupTestRouter(t)

	// Pre-register a user
	_, _ = authSvc.CreateUser(context.Background(), "taken@stored.ge", "password123")

	form := url.Values{
		"email":    {"taken@stored.ge"},
		"password": {"securepass123"},
	}
	req := httptest.NewRequest("POST", "/register", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "already exists")
}

func TestPostRegister_ShortPassword(t *testing.T) {
	r, _, _ := setupTestRouter(t)

	form := url.Values{
		"email":    {"test@stored.ge"},
		"password": {"short"},
	}
	req := httptest.NewRequest("POST", "/register", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "at least 8 characters")
}

func TestPostLogin(t *testing.T) {
	r, authSvc, _ := setupTestRouter(t)

	// Register a user first
	_, _ = authSvc.CreateUser(context.Background(), "login@stored.ge", "securepass123")

	form := url.Values{
		"email":    {"login@stored.ge"},
		"password": {"securepass123"},
	}
	req := httptest.NewRequest("POST", "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusSeeOther, w.Code)
	assert.Equal(t, "/dashboard", w.Header().Get("Location"))

	// Should have session cookie
	cookies := w.Result().Cookies()
	var found bool
	for _, c := range cookies {
		if c.Name == "vaultaire_session" {
			found = true
		}
	}
	assert.True(t, found, "expected session cookie")
}

func TestPostLogin_BadPassword(t *testing.T) {
	r, authSvc, _ := setupTestRouter(t)

	_, _ = authSvc.CreateUser(context.Background(), "login@stored.ge", "securepass123")

	form := url.Values{
		"email":    {"login@stored.ge"},
		"password": {"wrongpassword"},
	}
	req := httptest.NewRequest("POST", "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusUnauthorized, w.Code)
	assert.Contains(t, w.Body.String(), "Invalid email or password")
}

func TestPostLogin_NoSuchUser(t *testing.T) {
	r, _, _ := setupTestRouter(t)

	form := url.Values{
		"email":    {"nobody@stored.ge"},
		"password": {"whatever123"},
	}
	req := httptest.NewRequest("POST", "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusUnauthorized, w.Code)
	assert.Contains(t, w.Body.String(), "Invalid email or password")
}

func TestDashboard_RequiresSession(t *testing.T) {
	r, _, _ := setupTestRouter(t)

	req := httptest.NewRequest("GET", "/dashboard/", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusSeeOther, w.Code)
	assert.Equal(t, "/login", w.Header().Get("Location"))
}

func TestLogout(t *testing.T) {
	r, _, _ := setupTestRouter(t)

	req := httptest.NewRequest("GET", "/logout", nil)
	req.AddCookie(&http.Cookie{Name: "vaultaire_session", Value: "anything"})
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusSeeOther, w.Code)
	assert.Equal(t, "/login", w.Header().Get("Location"))
}

func TestStaticAssets(t *testing.T) {
	r, _, _ := setupTestRouter(t)

	req := httptest.NewRequest("GET", "/static/css/style.css", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "--primary")
}
