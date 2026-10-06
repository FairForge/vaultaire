package auth

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"
)

// Two app instances overlap during a zero-downtime deploy (#605): the new
// slot boots (LoadFromDB) while the old one still serves. Whatever the old
// one writes after that boot — a registration, a password change, an MFA
// change, an erasure — must decide the next sign-in on the new one, with no
// reload. These tests play the two instances against one database: `old`
// writes, `fresh` booted before the write and must still see it.

func twoInstances(t *testing.T) (fresh, old *AuthService) {
	t.Helper()
	db := setupTestDB(t)
	t.Cleanup(func() { _ = db.Close() })
	fresh = NewAuthService(nil, db)
	fresh.SetJWTSecret("overlap-test-secret") // both slots share JWT_SECRET
	require.NoError(t, fresh.LoadFromDB(context.Background()))
	require.NoError(t, fresh.LoadMFAFromDB(context.Background()))
	old = NewAuthService(nil, db)
	old.SetJWTSecret("overlap-test-secret")
	return fresh, old
}

func TestUsersFromDB_RegisteredElsewhereCanSignIn(t *testing.T) {
	ctx := context.Background()
	fresh, old := twoInstances(t)
	const email = "overlap-new@stored.ge"

	// Arrange: the old instance registers the user after fresh booted.
	user, tenant, _, err := old.CreateUserWithTenant(ctx, email, "first-password", "Overlap Co")
	require.NoError(t, err)

	// Act + Assert
	ok, err := fresh.ValidatePassword(ctx, email, "first-password")
	require.NoError(t, err)
	assert.True(t, ok, "a user registered on the other instance signs in")

	got, err := fresh.GetUserByEmail(ctx, email)
	require.NoError(t, err)
	assert.Equal(t, user.ID, got.ID)
	assert.Equal(t, tenant.ID, got.TenantID, "the tenant is linked")

	byID, err := fresh.GetUserByID(ctx, user.ID)
	require.NoError(t, err)
	assert.Equal(t, email, byID.Email)

	// A JWT minted by the other instance is accepted here.
	tok, err := old.GenerateJWT(user)
	require.NoError(t, err)
	_, err = fresh.ValidateJWT(tok)
	require.NoError(t, err)

	// The same e-mail cannot be registered twice through the fresh instance.
	_, _, _, err = fresh.CreateUserWithTenant(ctx, email, "another-password", "Dup Co")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "already exists")
}

func TestUsersFromDB_PasswordChangedInDB(t *testing.T) {
	ctx := context.Background()
	fresh, old := twoInstances(t)
	const email = "overlap-pw@stored.ge"
	user, _, _, err := old.CreateUserWithTenant(ctx, email, "old-password", "")
	require.NoError(t, err)
	ok, err := fresh.ValidatePassword(ctx, email, "old-password") // cached now
	require.NoError(t, err)
	require.True(t, ok)

	// Act: the hash changes in the database only.
	hash, err := bcrypt.GenerateFromPassword([]byte("new-password"), bcrypt.MinCost)
	require.NoError(t, err)
	_, err = fresh.sqlDB.Exec(`UPDATE users SET password_hash = $1 WHERE id = $2`, string(hash), user.ID)
	require.NoError(t, err)

	// Assert: no reload, the database decides.
	ok, err = fresh.ValidatePassword(ctx, email, "old-password")
	require.NoError(t, err)
	assert.False(t, ok, "the old password stops working at once")
	ok, err = fresh.ValidatePassword(ctx, email, "new-password")
	require.NoError(t, err)
	assert.True(t, ok, "the new password works at once")

	// A change made through the other instance's ChangePassword also lands,
	// and ChangePassword here checks the current password against the row.
	require.NoError(t, old.ChangePassword(ctx, user.ID, "new-password", "third-password"))
	err = fresh.ChangePassword(ctx, user.ID, "new-password", "fourth-password")
	require.Error(t, err, "a stale current password is refused")
	require.NoError(t, fresh.ChangePassword(ctx, user.ID, "third-password", "fourth-password"))
}

func TestUsersFromDB_DeletedInDBStopsAuthenticating(t *testing.T) {
	ctx := context.Background()
	fresh, old := twoInstances(t)
	const email = "overlap-del@stored.ge"
	user, tenant, _, err := old.CreateUserWithTenant(ctx, email, "the-password", "")
	require.NoError(t, err)
	ok, err := fresh.ValidatePassword(ctx, email, "the-password") // cached now
	require.NoError(t, err)
	require.True(t, ok)

	// Act: the account is erased in the database (the other instance's
	// deletion runner; its Evict only reaches its own process).
	_, err = fresh.sqlDB.Exec(`DELETE FROM api_keys WHERE user_id = $1`, user.ID)
	require.NoError(t, err)
	_, err = fresh.sqlDB.Exec(`DELETE FROM tenants WHERE id = $1`, tenant.ID)
	require.NoError(t, err)
	_, err = fresh.sqlDB.Exec(`DELETE FROM users WHERE id = $1`, user.ID)
	require.NoError(t, err)

	// Assert
	ok, err = fresh.ValidatePassword(ctx, email, "the-password")
	require.NoError(t, err)
	assert.False(t, ok, "an erased user cannot sign in")
	_, err = fresh.GetUserByEmail(ctx, email)
	require.Error(t, err)
	_, err = fresh.GetUserByID(ctx, user.ID)
	require.Error(t, err)

	fresh.cacheMu.RLock()
	_, byEmail := fresh.users[email]
	_, byID := fresh.userIndex[user.ID]
	fresh.cacheMu.RUnlock()
	assert.False(t, byEmail, "dropped from the e-mail map")
	assert.False(t, byID, "dropped from the id map")
}

func TestUsersFromDB_MFAChangedElsewhere(t *testing.T) {
	ctx := context.Background()
	fresh, old := twoInstances(t)
	const email = "overlap-mfa@stored.ge"
	user, _, _, err := old.CreateUserWithTenant(ctx, email, "the-password", "")
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = fresh.sqlDB.Exec(`DELETE FROM user_mfa WHERE user_id = $1`, user.ID) })

	on, err := fresh.IsMFAEnabled(ctx, user.ID)
	require.NoError(t, err)
	require.False(t, on)

	// Act: MFA is enabled on the other instance.
	require.NoError(t, old.EnableMFA(ctx, user.ID, "JBSWY3DPEHPK3PXP", []string{"BACKUP-ONE", "BACKUP-TWO"}))

	// Assert: sign-in here asks for the second factor.
	on, err = fresh.IsMFAEnabled(ctx, user.ID)
	require.NoError(t, err)
	assert.True(t, on, "MFA enabled elsewhere is enforced here")
	secret, err := fresh.GetMFASecret(ctx, user.ID)
	require.NoError(t, err)
	assert.Equal(t, "JBSWY3DPEHPK3PXP", secret)

	// A backup code consumed on one instance is gone on the other.
	ok, err := old.ValidateBackupCode(ctx, user.ID, "BACKUP-ONE")
	require.NoError(t, err)
	require.True(t, ok)
	ok, err = fresh.ValidateBackupCode(ctx, user.ID, "BACKUP-ONE")
	require.NoError(t, err)
	assert.False(t, ok, "a backup code is single-use across instances")
	ok, err = fresh.ValidateBackupCode(ctx, user.ID, "BACKUP-TWO")
	require.NoError(t, err)
	assert.True(t, ok)

	// Disabled elsewhere → no longer asked for here.
	require.NoError(t, old.DisableMFA(ctx, user.ID))
	on, err = fresh.IsMFAEnabled(ctx, user.ID)
	require.NoError(t, err)
	assert.False(t, on)
}
