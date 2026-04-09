package auth

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGenerateVerifyToken(t *testing.T) {
	svc := NewAuthService(nil, nil)
	svc.SetVerifySecret("test-secret-key")

	user, _, _, err := svc.CreateUserWithTenant(context.Background(), "verify@stored.ge", "password123", "Test")
	require.NoError(t, err)

	token, err := svc.GenerateEmailVerifyToken(context.Background(), user.ID)
	require.NoError(t, err)
	assert.NotEmpty(t, token)
	assert.False(t, user.EmailVerified)
}

func TestVerifyEmail_ValidToken(t *testing.T) {
	svc := NewAuthService(nil, nil)
	svc.SetVerifySecret("test-secret-key")

	user, _, _, err := svc.CreateUserWithTenant(context.Background(), "verify@stored.ge", "password123", "Test")
	require.NoError(t, err)

	token, err := svc.GenerateEmailVerifyToken(context.Background(), user.ID)
	require.NoError(t, err)

	err = svc.VerifyEmail(context.Background(), token)
	require.NoError(t, err)

	assert.True(t, user.EmailVerified)
}

func TestVerifyEmail_InvalidToken(t *testing.T) {
	svc := NewAuthService(nil, nil)
	svc.SetVerifySecret("test-secret-key")

	err := svc.VerifyEmail(context.Background(), "bogus-token")
	assert.Error(t, err)
}

func TestVerifyEmail_ExpiredToken(t *testing.T) {
	svc := NewAuthService(nil, nil)
	svc.SetVerifySecret("test-secret-key")

	// Token with wrong secret won't verify.
	svc2 := NewAuthService(nil, nil)
	svc2.SetVerifySecret("different-secret")
	user, _, _, _ := svc2.CreateUserWithTenant(context.Background(), "other@stored.ge", "pass", "X")
	token, _ := svc2.GenerateEmailVerifyToken(context.Background(), user.ID)

	err := svc.VerifyEmail(context.Background(), token)
	assert.Error(t, err)
}

func TestIsEmailVerified(t *testing.T) {
	svc := NewAuthService(nil, nil)
	svc.SetVerifySecret("test-secret-key")

	user, _, _, _ := svc.CreateUserWithTenant(context.Background(), "check@stored.ge", "pass", "X")

	assert.False(t, svc.IsEmailVerified(context.Background(), user.ID))

	token, _ := svc.GenerateEmailVerifyToken(context.Background(), user.ID)
	_ = svc.VerifyEmail(context.Background(), token)

	assert.True(t, svc.IsEmailVerified(context.Background(), user.ID))
}

func TestOAuthUsersAutoVerified(t *testing.T) {
	svc := NewAuthService(nil, nil)
	svc.SetVerifySecret("test-secret-key")

	// OAuth users (empty password) should be auto-verified.
	user, _, _, _ := svc.CreateUserWithTenant(context.Background(), "oauth@stored.ge", "", "OAuth")
	assert.True(t, user.EmailVerified)
}
