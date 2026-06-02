package auth

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSignupsEnabled_DefaultTrue(t *testing.T) {
	svc := NewAuthService(nil, nil)
	assert.True(t, svc.SignupsEnabled(), "signups should default to enabled")
}

func TestCreateUserWithTenant_BlockedWhenSignupsDisabled(t *testing.T) {
	svc := NewAuthService(nil, nil)
	svc.SetSignupsEnabled(false)

	user, tenant, key, err := svc.CreateUserWithTenant(
		context.Background(), "blocked@example.com", "pw-123456", "Co")

	require.Error(t, err)
	assert.ErrorIs(t, err, ErrSignupsDisabled)
	assert.Nil(t, user)
	assert.Nil(t, tenant)
	assert.Nil(t, key)

	// Nothing leaked into the in-memory maps.
	_, exists := svc.users["blocked@example.com"]
	assert.False(t, exists, "no user should be created when signups are disabled")
}

func TestCreateUserFromOAuth_BlockedWhenSignupsDisabled(t *testing.T) {
	svc := NewAuthService(nil, nil)
	svc.SetSignupsEnabled(false)

	// OAuth signup funnels through CreateUserWithTenant, so it is blocked at the
	// same chokepoint; the wrapped error still matches via errors.Is.
	user, tenant, err := svc.CreateUserFromOAuth(
		context.Background(), "oauth@example.com", "Name", "google", "g-123")

	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrSignupsDisabled),
		"OAuth signup must be blocked via the chokepoint")
	assert.Nil(t, user)
	assert.Nil(t, tenant)
}

func TestCreateUserWithTenant_AllowedWhenEnabled(t *testing.T) {
	svc := NewAuthService(nil, nil) // default enabled

	user, _, _, err := svc.CreateUserWithTenant(
		context.Background(), "ok@example.com", "pw-123456", "Co")
	require.NoError(t, err)
	require.NotNil(t, user)

	// Toggling back on after a disable works.
	svc.SetSignupsEnabled(false)
	svc.SetSignupsEnabled(true)
	user2, _, _, err := svc.CreateUserWithTenant(
		context.Background(), "ok2@example.com", "pw-123456", "Co")
	require.NoError(t, err)
	require.NotNil(t, user2)
}
