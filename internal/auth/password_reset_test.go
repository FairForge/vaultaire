package auth

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"
)

func TestPasswordReset_RequestReset(t *testing.T) {
	svc := NewAuthService(nil, nil)
	svc.SetVerifySecret("test-secret-key")

	_, _, _, err := svc.CreateUserWithTenant(context.Background(), "reset@stored.ge", "OldPass123!", "")
	require.NoError(t, err)

	token, err := svc.RequestPasswordReset(context.Background(), "reset@stored.ge")
	require.NoError(t, err)
	require.NotEmpty(t, token)
}

func TestPasswordReset_RequestUnknownEmail(t *testing.T) {
	svc := NewAuthService(nil, nil)
	svc.SetVerifySecret("test-secret-key")

	_, err := svc.RequestPasswordReset(context.Background(), "ghost@stored.ge")
	assert.Error(t, err)
}

func TestPasswordReset_CompleteReset(t *testing.T) {
	svc := NewAuthService(nil, nil)
	svc.SetVerifySecret("test-secret-key")

	user, _, _, err := svc.CreateUserWithTenant(context.Background(), "complete@stored.ge", "OldPass123!", "")
	require.NoError(t, err)
	originalHash := user.PasswordHash

	token, err := svc.RequestPasswordReset(context.Background(), "complete@stored.ge")
	require.NoError(t, err)

	gotUserID, err := svc.CompletePasswordReset(context.Background(), token, "NewPass456!")
	require.NoError(t, err)
	assert.Equal(t, user.ID, gotUserID)

	// Password hash should be updated.
	assert.NotEqual(t, originalHash, user.PasswordHash)

	// New password should validate.
	valid, err := svc.ValidatePassword(context.Background(), "complete@stored.ge", "NewPass456!")
	require.NoError(t, err)
	assert.True(t, valid)

	// Old password should no longer work.
	valid, _ = svc.ValidatePassword(context.Background(), "complete@stored.ge", "OldPass123!")
	assert.False(t, valid)

	// Hash should be a valid bcrypt hash.
	require.NoError(t, bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte("NewPass456!")))
}

func TestPasswordReset_InvalidToken(t *testing.T) {
	svc := NewAuthService(nil, nil)
	svc.SetVerifySecret("test-secret-key")

	_, _, _, err := svc.CreateUserWithTenant(context.Background(), "u@stored.ge", "OldPass123!", "")
	require.NoError(t, err)

	_, err = svc.CompletePasswordReset(context.Background(), "bogus-token", "NewPass456!")
	assert.Error(t, err)
}

func TestPasswordReset_TokenSingleUse(t *testing.T) {
	svc := NewAuthService(nil, nil)
	svc.SetVerifySecret("test-secret-key")

	_, _, _, err := svc.CreateUserWithTenant(context.Background(), "single@stored.ge", "OldPass123!", "")
	require.NoError(t, err)

	token, err := svc.RequestPasswordReset(context.Background(), "single@stored.ge")
	require.NoError(t, err)

	_, err = svc.CompletePasswordReset(context.Background(), token, "NewPass456!")
	require.NoError(t, err)

	// Token should not be in resetTokens map anymore.
	svc.resetMu.Lock()
	_, exists := svc.resetTokens[token]
	svc.resetMu.Unlock()
	assert.False(t, exists, "token should be cleared after use")
}

func TestPasswordReset_TokenWithWrongSecret(t *testing.T) {
	svc1 := NewAuthService(nil, nil)
	svc1.SetVerifySecret("secret-one")
	_, _, _, _ = svc1.CreateUserWithTenant(context.Background(), "x@stored.ge", "OldPass123!", "")
	token, _ := svc1.RequestPasswordReset(context.Background(), "x@stored.ge")

	svc2 := NewAuthService(nil, nil)
	svc2.SetVerifySecret("secret-two")
	_, _, _, _ = svc2.CreateUserWithTenant(context.Background(), "x@stored.ge", "OldPass123!", "")

	_, err := svc2.CompletePasswordReset(context.Background(), token, "NewPass456!")
	assert.Error(t, err)
}

func TestPasswordReset_TokenNotInterchangeableWithEmailVerify(t *testing.T) {
	svc := NewAuthService(nil, nil)
	svc.SetVerifySecret("test-secret-key")

	user, _, _, err := svc.CreateUserWithTenant(context.Background(), "swap@stored.ge", "OldPass123!", "")
	require.NoError(t, err)

	// An email verify token should not work as a reset token.
	verifyToken, err := svc.GenerateEmailVerifyToken(context.Background(), user.ID)
	require.NoError(t, err)
	_, err = svc.CompletePasswordReset(context.Background(), verifyToken, "NewPass456!")
	assert.Error(t, err, "email verify token must not be valid as a reset token")

	// And a reset token should not work as an email verify token.
	resetToken, err := svc.RequestPasswordReset(context.Background(), "swap@stored.ge")
	require.NoError(t, err)
	err = svc.VerifyEmail(context.Background(), resetToken)
	assert.Error(t, err, "reset token must not be valid as an email verify token")
}

func TestPasswordReset_RateLimit(t *testing.T) {
	svc := NewAuthService(nil, nil)
	svc.SetVerifySecret("test-secret-key")

	_, _, _, err := svc.CreateUserWithTenant(context.Background(), "rl@stored.ge", "OldPass123!", "")
	require.NoError(t, err)

	// First 3 requests should succeed.
	for i := 0; i < 3; i++ {
		_, err := svc.RequestPasswordReset(context.Background(), "rl@stored.ge")
		require.NoError(t, err, "request %d should succeed", i+1)
	}

	// 4th request should be rate-limited.
	_, err = svc.RequestPasswordReset(context.Background(), "rl@stored.ge")
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrResetRateLimited), "expected ErrResetRateLimited, got %v", err)
}

func TestPasswordReset_RejectsShortPassword(t *testing.T) {
	svc := NewAuthService(nil, nil)
	svc.SetVerifySecret("test-secret-key")

	_, _, _, err := svc.CreateUserWithTenant(context.Background(), "short@stored.ge", "OldPass123!", "")
	require.NoError(t, err)
	token, err := svc.RequestPasswordReset(context.Background(), "short@stored.ge")
	require.NoError(t, err)

	_, err = svc.CompletePasswordReset(context.Background(), token, "short")
	assert.Error(t, err)
}
