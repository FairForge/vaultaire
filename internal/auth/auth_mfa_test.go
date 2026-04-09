package auth

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestAuthWithUser(t *testing.T) (*AuthService, *User) {
	t.Helper()
	svc := NewAuthService(nil, nil)
	user, _, _, err := svc.CreateUserWithTenant(context.Background(), "mfa@stored.ge", "password123", "Test")
	require.NoError(t, err)
	return svc, user
}

func TestEnableMFA(t *testing.T) {
	t.Run("enables MFA for existing user", func(t *testing.T) {
		svc, user := newTestAuthWithUser(t)
		ctx := context.Background()

		err := svc.EnableMFA(ctx, user.ID, "JBSWY3DPEHPK3PXP", []string{"CODE1111", "CODE2222"})
		require.NoError(t, err)

		enabled, err := svc.IsMFAEnabled(ctx, user.ID)
		require.NoError(t, err)
		assert.True(t, enabled)
	})

	t.Run("rejects unknown user", func(t *testing.T) {
		svc := NewAuthService(nil, nil)
		err := svc.EnableMFA(context.Background(), "nonexistent", "secret", nil)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "user not found")
	})
}

func TestDisableMFA(t *testing.T) {
	t.Run("disables MFA", func(t *testing.T) {
		svc, user := newTestAuthWithUser(t)
		ctx := context.Background()

		require.NoError(t, svc.EnableMFA(ctx, user.ID, "SECRET", []string{"BACKUP1"}))

		err := svc.DisableMFA(ctx, user.ID)
		require.NoError(t, err)

		enabled, err := svc.IsMFAEnabled(ctx, user.ID)
		require.NoError(t, err)
		assert.False(t, enabled)
	})
}

func TestIsMFAEnabled(t *testing.T) {
	t.Run("returns false when not configured", func(t *testing.T) {
		svc, user := newTestAuthWithUser(t)

		enabled, err := svc.IsMFAEnabled(context.Background(), user.ID)
		require.NoError(t, err)
		assert.False(t, enabled)
	})
}

func TestGetMFASecret(t *testing.T) {
	t.Run("returns secret when enabled", func(t *testing.T) {
		svc, user := newTestAuthWithUser(t)
		ctx := context.Background()

		require.NoError(t, svc.EnableMFA(ctx, user.ID, "MYSECRET", nil))

		secret, err := svc.GetMFASecret(ctx, user.ID)
		require.NoError(t, err)
		assert.Equal(t, "MYSECRET", secret)
	})

	t.Run("errors when not enabled", func(t *testing.T) {
		svc, user := newTestAuthWithUser(t)
		_, err := svc.GetMFASecret(context.Background(), user.ID)
		assert.Error(t, err)
	})
}

func TestValidateBackupCode(t *testing.T) {
	t.Run("consumes backup code once", func(t *testing.T) {
		svc, user := newTestAuthWithUser(t)
		ctx := context.Background()

		codes := []string{"AAAA1111", "BBBB2222", "CCCC3333"}
		require.NoError(t, svc.EnableMFA(ctx, user.ID, "SECRET", codes))

		// First use succeeds.
		valid, err := svc.ValidateBackupCode(ctx, user.ID, "AAAA1111")
		require.NoError(t, err)
		assert.True(t, valid)

		// Second use of same code fails.
		valid, err = svc.ValidateBackupCode(ctx, user.ID, "AAAA1111")
		require.NoError(t, err)
		assert.False(t, valid)

		// Other codes still work.
		valid, err = svc.ValidateBackupCode(ctx, user.ID, "BBBB2222")
		require.NoError(t, err)
		assert.True(t, valid)
	})

	t.Run("rejects invalid code", func(t *testing.T) {
		svc, user := newTestAuthWithUser(t)
		ctx := context.Background()

		require.NoError(t, svc.EnableMFA(ctx, user.ID, "SECRET", []string{"REAL1111"}))

		valid, err := svc.ValidateBackupCode(ctx, user.ID, "WRONG999")
		require.NoError(t, err)
		assert.False(t, valid)
	})

	t.Run("returns false when MFA not enabled", func(t *testing.T) {
		svc, user := newTestAuthWithUser(t)
		valid, err := svc.ValidateBackupCode(context.Background(), user.ID, "CODE")
		require.NoError(t, err)
		assert.False(t, valid)
	})
}
