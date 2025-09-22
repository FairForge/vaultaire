package auth

import (
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestSSO_OAuthConfig(t *testing.T) {
	t.Run("creates google oauth config", func(t *testing.T) {
		sso := NewSSOService()

		config := sso.GetGoogleConfig()
		require.NotNil(t, config)

		// Google's oauth2 package uses this URL
		assert.Equal(t, "https://accounts.google.com/o/oauth2/auth", config.Endpoint.AuthURL)
		assert.Contains(t, config.Scopes, "email")
		assert.Contains(t, config.Scopes, "profile")
	})
}

func TestSSO_HandleCallback(t *testing.T) {
	t.Run("processes oauth callback", func(t *testing.T) {
		sso := NewSSOService()

		// Mock callback with code
		user, err := sso.HandleCallback("mock-code")

		// For testing, accept mock code
		require.NoError(t, err)
		assert.NotNil(t, user)
		assert.Equal(t, "test@google.com", user.Email)
	})
}
