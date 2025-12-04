// internal/auth/oauth_test.go
package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOAuthConfig_Validate(t *testing.T) {
	t.Run("valid config passes validation", func(t *testing.T) {
		config := &OAuthConfig{
			ClientID:     "client-id",
			ClientSecret: "client-secret",
			RedirectURL:  "https://app.example.com/callback",
			AuthURL:      "https://provider.example.com/auth",
			TokenURL:     "https://provider.example.com/token",
			Scopes:       []string{"openid", "email", "profile"},
		}
		err := config.Validate()
		assert.NoError(t, err)
	})

	t.Run("rejects empty client ID", func(t *testing.T) {
		config := &OAuthConfig{
			ClientSecret: "secret",
			RedirectURL:  "https://app.example.com/callback",
		}
		err := config.Validate()
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "client ID")
	})

	t.Run("rejects empty client secret", func(t *testing.T) {
		config := &OAuthConfig{
			ClientID:    "client-id",
			RedirectURL: "https://app.example.com/callback",
		}
		err := config.Validate()
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "client secret")
	})

	t.Run("rejects empty redirect URL", func(t *testing.T) {
		config := &OAuthConfig{
			ClientID:     "client-id",
			ClientSecret: "secret",
		}
		err := config.Validate()
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "redirect URL")
	})
}

func TestNewOAuthProvider(t *testing.T) {
	t.Run("creates provider with valid config", func(t *testing.T) {
		config := &OAuthConfig{
			ClientID:     "client-id",
			ClientSecret: "client-secret",
			RedirectURL:  "https://app.example.com/callback",
			AuthURL:      "https://provider.example.com/auth",
			TokenURL:     "https://provider.example.com/token",
		}
		provider, err := NewOAuthProvider(config)
		require.NoError(t, err)
		assert.NotNil(t, provider)
	})

	t.Run("rejects nil config", func(t *testing.T) {
		provider, err := NewOAuthProvider(nil)
		assert.Error(t, err)
		assert.Nil(t, provider)
	})
}

func TestOAuthProvider_BuildAuthURL(t *testing.T) {
	config := &OAuthConfig{
		ClientID:     "client-id",
		ClientSecret: "client-secret",
		RedirectURL:  "https://app.example.com/callback",
		AuthURL:      "https://provider.example.com/auth",
		TokenURL:     "https://provider.example.com/token",
		Scopes:       []string{"openid", "email"},
	}
	provider, _ := NewOAuthProvider(config)

	t.Run("builds auth URL with required params", func(t *testing.T) {
		url := provider.BuildAuthURL("state123")
		assert.Contains(t, url, "https://provider.example.com/auth")
		assert.Contains(t, url, "client_id=client-id")
		assert.Contains(t, url, "redirect_uri=")
		assert.Contains(t, url, "response_type=code")
		assert.Contains(t, url, "state=state123")
	})

	t.Run("includes scopes", func(t *testing.T) {
		url := provider.BuildAuthURL("state123")
		assert.Contains(t, url, "scope=")
	})

	t.Run("generates state if empty", func(t *testing.T) {
		url := provider.BuildAuthURL("")
		assert.Contains(t, url, "state=")
	})
}

func TestOAuthProvider_GenerateState(t *testing.T) {
	config := &OAuthConfig{
		ClientID:     "client-id",
		ClientSecret: "client-secret",
		RedirectURL:  "https://app.example.com/callback",
		AuthURL:      "https://provider.example.com/auth",
		TokenURL:     "https://provider.example.com/token",
	}
	provider, _ := NewOAuthProvider(config)

	t.Run("generates unique state", func(t *testing.T) {
		state1 := provider.GenerateState()
		state2 := provider.GenerateState()
		assert.NotEmpty(t, state1)
		assert.NotEmpty(t, state2)
		assert.NotEqual(t, state1, state2)
	})

	t.Run("state is URL-safe", func(t *testing.T) {
		state := provider.GenerateState()
		assert.NotContains(t, state, "+")
		assert.NotContains(t, state, "/")
		assert.NotContains(t, state, "=")
	})
}

func TestOAuthProvider_ExchangeCode(t *testing.T) {
	t.Run("exchanges code for token", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, "POST", r.Method)
			assert.Equal(t, "application/x-www-form-urlencoded", r.Header.Get("Content-Type"))

			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{
				"access_token": "access-token-123",
				"token_type": "Bearer",
				"expires_in": 3600,
				"refresh_token": "refresh-token-456"
			}`))
		}))
		defer server.Close()

		config := &OAuthConfig{
			ClientID:     "client-id",
			ClientSecret: "client-secret",
			RedirectURL:  "https://app.example.com/callback",
			AuthURL:      "https://provider.example.com/auth",
			TokenURL:     server.URL,
		}
		provider, _ := NewOAuthProvider(config)

		token, err := provider.ExchangeCode(context.Background(), "auth-code")
		require.NoError(t, err)
		assert.Equal(t, "access-token-123", token.AccessToken)
		assert.Equal(t, "Bearer", token.TokenType)
		assert.Equal(t, "refresh-token-456", token.RefreshToken)
	})

	t.Run("returns error for empty code", func(t *testing.T) {
		config := &OAuthConfig{
			ClientID:     "client-id",
			ClientSecret: "client-secret",
			RedirectURL:  "https://app.example.com/callback",
			AuthURL:      "https://provider.example.com/auth",
			TokenURL:     "https://provider.example.com/token",
		}
		provider, _ := NewOAuthProvider(config)

		_, err := provider.ExchangeCode(context.Background(), "")
		assert.Error(t, err)
	})
}

func TestOAuthProvider_RefreshToken(t *testing.T) {
	t.Run("refreshes access token", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_ = r.ParseForm()
			assert.Equal(t, "refresh_token", r.Form.Get("grant_type"))

			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{
				"access_token": "new-access-token",
				"token_type": "Bearer",
				"expires_in": 3600
			}`))
		}))
		defer server.Close()

		config := &OAuthConfig{
			ClientID:     "client-id",
			ClientSecret: "client-secret",
			RedirectURL:  "https://app.example.com/callback",
			AuthURL:      "https://provider.example.com/auth",
			TokenURL:     server.URL,
		}
		provider, _ := NewOAuthProvider(config)

		token, err := provider.RefreshToken(context.Background(), "old-refresh-token")
		require.NoError(t, err)
		assert.Equal(t, "new-access-token", token.AccessToken)
	})
}

func TestOAuthProvider_GetUserInfo(t *testing.T) {
	t.Run("fetches user info", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, "Bearer access-token", r.Header.Get("Authorization"))

			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{
				"sub": "user-123",
				"email": "jsmith@example.com",
				"name": "John Smith",
				"picture": "https://example.com/avatar.jpg"
			}`))
		}))
		defer server.Close()

		config := &OAuthConfig{
			ClientID:     "client-id",
			ClientSecret: "client-secret",
			RedirectURL:  "https://app.example.com/callback",
			AuthURL:      "https://provider.example.com/auth",
			TokenURL:     "https://provider.example.com/token",
			UserInfoURL:  server.URL,
		}
		provider, _ := NewOAuthProvider(config)

		user, err := provider.GetUserInfo(context.Background(), "access-token")
		require.NoError(t, err)
		assert.Equal(t, "user-123", user.ID)
		assert.Equal(t, "jsmith@example.com", user.Email)
		assert.Equal(t, "John Smith", user.Name)
	})
}

func TestOAuthToken(t *testing.T) {
	t.Run("checks if expired", func(t *testing.T) {
		token := &OAuthToken{
			AccessToken: "token",
			ExpiresAt:   time.Now().Add(-1 * time.Hour),
		}
		assert.True(t, token.IsExpired())
	})

	t.Run("not expired", func(t *testing.T) {
		token := &OAuthToken{
			AccessToken: "token",
			ExpiresAt:   time.Now().Add(1 * time.Hour),
		}
		assert.False(t, token.IsExpired())
	})

	t.Run("has refresh token", func(t *testing.T) {
		token := &OAuthToken{
			AccessToken:  "token",
			RefreshToken: "refresh",
		}
		assert.True(t, token.HasRefreshToken())
	})
}

func TestPredefinedProviders(t *testing.T) {
	t.Run("Google provider config", func(t *testing.T) {
		config := GoogleOAuthConfig("client-id", "client-secret", "https://app.example.com/callback")
		assert.Equal(t, "client-id", config.ClientID)
		assert.Contains(t, config.AuthURL, "accounts.google.com")
		assert.Contains(t, config.Scopes, "openid")
		assert.Contains(t, config.Scopes, "email")
	})

	t.Run("GitHub provider config", func(t *testing.T) {
		config := GitHubOAuthConfig("client-id", "client-secret", "https://app.example.com/callback")
		assert.Equal(t, "client-id", config.ClientID)
		assert.Contains(t, config.AuthURL, "github.com")
		assert.Contains(t, config.Scopes, "user:email")
	})

	t.Run("Microsoft provider config", func(t *testing.T) {
		config := MicrosoftOAuthConfig("client-id", "client-secret", "https://app.example.com/callback", "common")
		assert.Equal(t, "client-id", config.ClientID)
		assert.Contains(t, config.AuthURL, "login.microsoftonline.com")
		assert.Contains(t, config.Scopes, "openid")
	})
}

func TestOAuthProvider_PKCE(t *testing.T) {
	config := &OAuthConfig{
		ClientID:     "client-id",
		ClientSecret: "client-secret",
		RedirectURL:  "https://app.example.com/callback",
		AuthURL:      "https://provider.example.com/auth",
		TokenURL:     "https://provider.example.com/token",
		UsePKCE:      true,
	}
	provider, _ := NewOAuthProvider(config)

	t.Run("generates code verifier", func(t *testing.T) {
		verifier := provider.GenerateCodeVerifier()
		assert.Len(t, verifier, 43) // Base64url encoded 32 bytes
	})

	t.Run("generates code challenge", func(t *testing.T) {
		verifier := "test-verifier-string-for-pkce-flow"
		challenge := provider.GenerateCodeChallenge(verifier)
		assert.NotEmpty(t, challenge)
		assert.NotEqual(t, verifier, challenge)
	})

	t.Run("includes PKCE in auth URL", func(t *testing.T) {
		verifier := provider.GenerateCodeVerifier()
		url := provider.BuildAuthURLWithPKCE("state123", verifier)
		assert.Contains(t, url, "code_challenge=")
		assert.Contains(t, url, "code_challenge_method=S256")
	})
}

func TestOAuthProvider_ProviderInfo(t *testing.T) {
	t.Run("returns provider info", func(t *testing.T) {
		config := &OAuthConfig{
			ProviderName: "google",
			ClientID:     "client-id",
			ClientSecret: "client-secret",
			RedirectURL:  "https://app.example.com/callback",
			AuthURL:      "https://accounts.google.com/o/oauth2/auth",
			TokenURL:     "https://oauth2.googleapis.com/token",
		}
		provider, _ := NewOAuthProvider(config)

		info := provider.Info()
		assert.Equal(t, "oauth", info.Type)
		assert.Equal(t, "google", info.Name)
	})
}

func TestDefaultOAuthConfig(t *testing.T) {
	t.Run("provides sensible defaults", func(t *testing.T) {
		config := DefaultOAuthConfig()
		assert.Equal(t, 30*time.Second, config.Timeout)
		assert.False(t, config.UsePKCE)
	})
}
