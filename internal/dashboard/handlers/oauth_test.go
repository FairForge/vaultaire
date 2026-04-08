package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/FairForge/vaultaire/internal/auth"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"golang.org/x/oauth2"
)

func testOAuthConfig() *oauth2.Config {
	return &oauth2.Config{
		ClientID:     "test-client-id",
		ClientSecret: "test-secret",
		RedirectURL:  "http://localhost:8000/auth/test/callback",
		Scopes:       []string{"email"},
		Endpoint: oauth2.Endpoint{
			AuthURL:  "https://example.com/auth",
			TokenURL: "https://example.com/token",
		},
	}
}

func TestHandleOAuthLogin_Redirect(t *testing.T) {
	cfg := testOAuthConfig()
	handler := HandleOAuthLogin(cfg, zap.NewNop())

	req := httptest.NewRequest("GET", "/auth/test", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	// Should redirect to OAuth provider.
	assert.Equal(t, http.StatusTemporaryRedirect, w.Code)
	location := w.Header().Get("Location")
	assert.Contains(t, location, "https://example.com/auth")
	assert.Contains(t, location, "client_id=test-client-id")
	assert.Contains(t, location, "scope=email")
	assert.Contains(t, location, "state=")

	// Should set state cookie.
	cookies := w.Result().Cookies()
	var stateCookie *http.Cookie
	for _, c := range cookies {
		if c.Name == "oauth_state" {
			stateCookie = c
		}
	}
	require.NotNil(t, stateCookie)
	assert.True(t, stateCookie.HttpOnly)
	assert.NotEmpty(t, stateCookie.Value)
}

func TestHandleOAuthCallback_MissingStateCookie(t *testing.T) {
	cfg := testOAuthConfig()
	handler := HandleOAuthCallback(cfg, "test",
		func(ctx context.Context, token *oauth2.Token) (oauthUser, error) {
			return oauthUser{}, nil
		},
		nil, nil, nil, zap.NewNop())

	req := httptest.NewRequest("GET", "/auth/test/callback?state=abc&code=xyz", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusSeeOther, w.Code)
	assert.Equal(t, "/login", w.Header().Get("Location"))
}

func TestHandleOAuthCallback_StateMismatch(t *testing.T) {
	cfg := testOAuthConfig()
	handler := HandleOAuthCallback(cfg, "test",
		func(ctx context.Context, token *oauth2.Token) (oauthUser, error) {
			return oauthUser{}, nil
		},
		nil, nil, nil, zap.NewNop())

	req := httptest.NewRequest("GET", "/auth/test/callback?state=wrong&code=xyz", nil)
	req.AddCookie(&http.Cookie{Name: "oauth_state", Value: "correct"})
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusSeeOther, w.Code)
	assert.Equal(t, "/login", w.Header().Get("Location"))
}

func TestHandleOAuthCallback_ProviderError(t *testing.T) {
	cfg := testOAuthConfig()
	handler := HandleOAuthCallback(cfg, "test",
		func(ctx context.Context, token *oauth2.Token) (oauthUser, error) {
			return oauthUser{}, nil
		},
		nil, nil, nil, zap.NewNop())

	req := httptest.NewRequest("GET", "/auth/test/callback?state=abc&error=access_denied", nil)
	req.AddCookie(&http.Cookie{Name: "oauth_state", Value: "abc"})
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusSeeOther, w.Code)
	assert.Equal(t, "/login", w.Header().Get("Location"))
}

func TestHandleOAuthCallback_MissingCode(t *testing.T) {
	cfg := testOAuthConfig()
	handler := HandleOAuthCallback(cfg, "test",
		func(ctx context.Context, token *oauth2.Token) (oauthUser, error) {
			return oauthUser{}, nil
		},
		nil, nil, nil, zap.NewNop())

	req := httptest.NewRequest("GET", "/auth/test/callback?state=abc", nil)
	req.AddCookie(&http.Cookie{Name: "oauth_state", Value: "abc"})
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusSeeOther, w.Code)
	assert.Equal(t, "/login", w.Header().Get("Location"))
}

func TestFindOrCreateOAuthUser_NewUser(t *testing.T) {
	authSvc := createTestAuthSvc(t)

	ou := oauthUser{ID: "google-123", Email: "new@example.com", Name: "New User"}
	user, err := findOrCreateOAuthUser(context.Background(), authSvc, nil, "google", ou)

	require.NoError(t, err)
	require.NotNil(t, user)
	assert.Equal(t, "new@example.com", user.Email)
	assert.Equal(t, "", user.PasswordHash) // OAuth user, no password
	assert.NotEmpty(t, user.TenantID)
}

func TestFindOrCreateOAuthUser_ExistingEmail(t *testing.T) {
	authSvc := createTestAuthSvc(t)

	// Create existing user with password.
	existing, _, _, err := authSvc.CreateUserWithTenant(context.Background(), "existing@example.com", "password123", "TestCo")
	require.NoError(t, err)

	// OAuth login with same email.
	ou := oauthUser{ID: "google-456", Email: "existing@example.com", Name: "Existing User"}
	user, err := findOrCreateOAuthUser(context.Background(), authSvc, nil, "google", ou)

	require.NoError(t, err)
	require.NotNil(t, user)
	assert.Equal(t, existing.ID, user.ID) // Same user, not a new one
}

func TestFindOrCreateOAuthUser_ExistingOAuth(t *testing.T) {
	authSvc := createTestAuthSvc(t)

	// Create user via OAuth.
	ou := oauthUser{ID: "github-789", Email: "oauth@example.com", Name: "OAuth User"}
	first, err := findOrCreateOAuthUser(context.Background(), authSvc, nil, "github", ou)
	require.NoError(t, err)

	// Login again with same OAuth.
	second, err := findOrCreateOAuthUser(context.Background(), authSvc, nil, "github", ou)
	require.NoError(t, err)

	// Should be the same user (looked up by email since no DB for GetUserByOAuth).
	assert.Equal(t, first.ID, second.ID)
}

func createTestAuthSvc(t *testing.T) *auth.AuthService {
	t.Helper()
	return auth.NewAuthService(nil, nil)
}
