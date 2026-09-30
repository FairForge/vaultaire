package auth

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// WP-R10-3: the account-deletion runner erases the database rows, but the
// login and API-key paths read the maps LoadFromDB filled at boot. An erased
// user must not authenticate from the cache until the next restart.
func TestEvict_RemovesTheAccountFromTheCredentialCache(t *testing.T) {
	svc := NewAuthService(nil, nil)
	ctx := context.Background()
	user, tenant, _, err := svc.CreateUserWithTenant(ctx, "evict@test.local", "password-123", "Evict Co")
	require.NoError(t, err)
	key, err := svc.GenerateAPIKey(ctx, user.ID, "scoped", nil)
	require.NoError(t, err)

	ok, err := svc.ValidatePassword(ctx, "evict@test.local", "password-123")
	require.NoError(t, err)
	require.True(t, ok, "fixture: the user can log in before the eviction")
	_, err = svc.ValidateS3Request(ctx, tenant.AccessKey)
	require.NoError(t, err)
	_, err = svc.ValidateAPIKey(ctx, key.Key, key.Secret)
	require.NoError(t, err)

	svc.Evict(user.ID, tenant.ID)

	ok, err = svc.ValidatePassword(ctx, "evict@test.local", "password-123")
	require.NoError(t, err)
	assert.False(t, ok, "password login must fail after the erasure")
	_, err = svc.GetUserByEmail(ctx, "evict@test.local")
	assert.Error(t, err)
	_, err = svc.GetUserByID(ctx, user.ID)
	assert.Error(t, err)
	_, err = svc.ValidateS3Request(ctx, tenant.AccessKey)
	assert.Error(t, err, "the tenant's access key is gone")
	_, err = svc.ValidateAPIKey(ctx, key.Key, key.Secret)
	assert.Error(t, err, "the user's API key is gone")
	enabled, _ := svc.IsMFAEnabled(ctx, user.ID)
	assert.False(t, enabled)

	// Evicting again, or an unknown account, is a no-op.
	svc.Evict(user.ID, tenant.ID)
	svc.Evict("ghost", "ghost")
}
