package auth

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Post-merge review of #584: the primary pair could be given an expiry
// (SetAPIKeyExpiration checked RevokedAt, not IsPrimary) and RotateAPIKey
// copied ExpiresAt onto the successor — so an account could expire its one
// way in, and rotating gave it another dead key. The primary is never
// expired (ErrPrimaryKeyExpire, the ErrPrimaryKeyRevoke shape), and an
// expired key — primary or scoped — is not rotated into a successor that
// is born dead (ErrKeyExpired).

func newExpiryAccount(t *testing.T) (*AuthService, *User, *APIKey) {
	t.Helper()
	svc := NewAuthService(nil, nil)
	user, _, key, err := svc.CreateUserWithTenant(context.Background(), "expiry-"+GenerateID()[:8]+"@stored.ge", "Str0ngPassw0rd!", "x")
	require.NoError(t, err)
	require.True(t, key.IsPrimary)
	return svc, user, key
}

// forceExpiry stamps an expiry straight onto the cached row — the state a
// row written before this fix (or by hand) would be in.
func forceExpiry(svc *AuthService, accessKey string, at time.Time) {
	svc.cacheMu.Lock()
	defer svc.cacheMu.Unlock()
	svc.apiKeys[accessKey].ExpiresAt = &at
}

func TestPrimaryKey_ExpiryIsRefused(t *testing.T) {
	svc, user, key := newExpiryAccount(t)
	ctx := context.Background()

	err := svc.SetAPIKeyExpiration(ctx, user.ID, key.ID, time.Now().Add(24*time.Hour))
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrPrimaryKeyExpire), "got %v", err)
	assert.False(t, errors.Is(err, ErrPrimaryKeyRevoke), "its own sentinel: the message must not say 'revoked'")

	got, err := svc.GetOwnedAPIKey(ctx, user.ID, key.ID)
	require.NoError(t, err)
	assert.Nil(t, got.ExpiresAt, "the primary keeps no expiry")

	// Rotating first does not open a way around it: the successor is the
	// primary too.
	successor, err := svc.RotateAPIKey(ctx, user.ID, key.ID)
	require.NoError(t, err)
	require.True(t, successor.IsPrimary)
	err = svc.SetAPIKeyExpiration(ctx, user.ID, successor.ID, time.Now().Add(24*time.Hour))
	assert.True(t, errors.Is(err, ErrPrimaryKeyExpire), "got %v", err)

	// A scoped key still takes an expiry.
	scoped, err := svc.GenerateAPIKey(ctx, user.ID, "scoped", &KeyCreateOptions{Permissions: []string{OpGetObject}})
	require.NoError(t, err)
	require.NoError(t, svc.SetAPIKeyExpiration(ctx, user.ID, scoped.ID, time.Now().Add(24*time.Hour)))
}

func TestRotateAPIKey_ExpiredKeyIsRefused(t *testing.T) {
	svc, user, key := newExpiryAccount(t)
	ctx := context.Background()

	scoped, err := svc.GenerateAPIKey(ctx, user.ID, "scoped", &KeyCreateOptions{Permissions: []string{OpGetObject}})
	require.NoError(t, err)
	require.NoError(t, svc.SetAPIKeyExpiration(ctx, user.ID, scoped.ID, time.Now().Add(-time.Minute)))

	before, err := svc.ListAPIKeys(ctx, user.ID)
	require.NoError(t, err)

	_, err = svc.RotateAPIKey(ctx, user.ID, scoped.ID)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrKeyExpired), "got %v", err)

	// The primary in the same state (a row from before the fix): refused
	// the same way, and it is still the live primary — never revoked by a
	// refused rotation.
	forceExpiry(svc, key.Key, time.Now().Add(-time.Minute))
	_, err = svc.RotateAPIKey(ctx, user.ID, key.ID)
	assert.True(t, errors.Is(err, ErrKeyExpired), "got %v", err)

	after, err := svc.ListAPIKeys(ctx, user.ID)
	require.NoError(t, err)
	assert.Len(t, after, len(before), "a refused rotation mints no successor")
	for _, k := range after {
		assert.Nil(t, k.RevokedAt, "a refused rotation revokes nothing: %s", k.Name)
	}
}
