// internal/auth/apikey_test.go
package auth

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAPIKeyGeneration(t *testing.T) {
	// Create auth service
	auth := NewAuthService(nil)
	ctx := context.Background()

	// Create user first - CreateUser returns (user, error)
	user, err := auth.CreateUser(ctx, "test@example.com", "password")
	require.NoError(t, err)

	t.Run("generates API key with new format", func(t *testing.T) {
		key, err := auth.GenerateAPIKey(ctx, user.ID, "Test Key")
		require.NoError(t, err)

		assert.NotEmpty(t, key.Key)
		assert.Contains(t, key.Key, "VLT_")
		assert.Len(t, key.Secret, 40)
		assert.Equal(t, "Test Key", key.Name)
		assert.Equal(t, user.TenantID, key.TenantID)
		assert.Equal(t, []string{"s3:*"}, key.Permissions)
	})

	t.Run("validates API key", func(t *testing.T) {
		key, err := auth.GenerateAPIKey(ctx, user.ID, "Validate Test")
		require.NoError(t, err)

		// Valid key
		validUser, err := auth.ValidateAPIKey(ctx, key.Key, key.Secret)
		require.NoError(t, err)
		assert.Equal(t, user.ID, validUser.ID)

		// Invalid secret
		_, err = auth.ValidateAPIKey(ctx, key.Key, "wrong-secret")
		assert.Error(t, err)

		// Invalid key
		_, err = auth.ValidateAPIKey(ctx, "VLT_INVALIDKEY", key.Secret)
		assert.Error(t, err)
	})

	t.Run("rotates API key", func(t *testing.T) {
		// Create original key
		originalKey, err := auth.GenerateAPIKey(ctx, user.ID, "Original")
		require.NoError(t, err)

		// Rotate it
		newKey, err := auth.RotateAPIKey(ctx, user.ID, originalKey.ID)
		require.NoError(t, err)

		assert.NotEqual(t, originalKey.Key, newKey.Key)
		assert.NotEqual(t, originalKey.Secret, newKey.Secret)
		assert.Contains(t, newKey.Name, "rotated")
		assert.NotNil(t, originalKey.RevokedAt)
	})

	t.Run("revokes API key", func(t *testing.T) {
		key, err := auth.GenerateAPIKey(ctx, user.ID, "Revoke Test")
		require.NoError(t, err)

		// Revoke key
		err = auth.RevokeAPIKey(ctx, user.ID, key.ID)
		require.NoError(t, err)

		// Cannot use revoked key
		_, err = auth.ValidateAPIKey(ctx, key.Key, key.Secret)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "revoked")
	})

	t.Run("handles key expiration", func(t *testing.T) {
		key, err := auth.GenerateAPIKey(ctx, user.ID, "Expire Test")
		require.NoError(t, err)

		// Set expiration to past
		pastTime := time.Now().Add(-1 * time.Hour)
		err = auth.SetAPIKeyExpiration(ctx, user.ID, key.ID, pastTime)
		require.NoError(t, err)

		// Cannot use expired key
		_, err = auth.ValidateAPIKey(ctx, key.Key, key.Secret)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "expired")
	})

	t.Run("lists user API keys", func(t *testing.T) {
		// Create a few keys
		_, err := auth.GenerateAPIKey(ctx, user.ID, "Key 1")
		require.NoError(t, err)
		_, err = auth.GenerateAPIKey(ctx, user.ID, "Key 2")
		require.NoError(t, err)

		keys, err := auth.ListAPIKeys(ctx, user.ID)
		require.NoError(t, err)

		// Should have multiple keys
		assert.True(t, len(keys) >= 2)

		// Secrets should not be included in list
		for _, k := range keys {
			assert.Empty(t, k.Secret)
		}
	})
}

func TestCheckRotationNeeded(t *testing.T) {
	auth := NewAuthService(nil)

	t.Run("checks age-based rotation", func(t *testing.T) {
		key := &APIKey{
			CreatedAt: time.Now().Add(-31 * 24 * time.Hour), // 31 days old
		}

		policy := &KeyRotationPolicy{
			MaxAge: 30 * 24 * time.Hour, // 30 days
		}

		assert.True(t, auth.CheckRotationNeeded(key, policy))
	})

	t.Run("checks usage-based rotation", func(t *testing.T) {
		key := &APIKey{
			CreatedAt:  time.Now(),
			UsageCount: 10001,
		}

		policy := &KeyRotationPolicy{
			MaxUsageCount: 10000,
		}

		assert.True(t, auth.CheckRotationNeeded(key, policy))
	})

	t.Run("doesn't rotate revoked keys", func(t *testing.T) {
		now := time.Now()
		key := &APIKey{
			CreatedAt: time.Now().Add(-31 * 24 * time.Hour),
			RevokedAt: &now,
		}

		policy := &KeyRotationPolicy{
			MaxAge: 30 * 24 * time.Hour,
		}

		assert.False(t, auth.CheckRotationNeeded(key, policy))
	})
}
