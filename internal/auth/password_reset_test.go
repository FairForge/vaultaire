package auth

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPasswordReset_RequestReset(t *testing.T) {
	// Test that requesting a password reset generates a token
	service := NewAuthService(nil)

	// Create a user first
	_, _, _, err := service.CreateUserWithTenant(context.TODO(), "test@stored.ge", "OldPass123!", "")
	require.NoError(t, err)

	// Request password reset
	token, err := service.RequestPasswordReset(context.TODO(), "test@stored.ge")
	require.NoError(t, err)
	require.NotEmpty(t, token)
}

func TestPasswordReset_CompleteReset(t *testing.T) {
	// Test completing a password reset with token
	// TODO: Implement
}
