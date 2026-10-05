package auth

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// WP-R12-15: regenerating backup codes replaces the set in the database and
// in memory, kills every old code, leaves the authenticator alone and writes
// its audit row.
func TestRegenerateBackupCodes_PersistsAndAudits(t *testing.T) {
	a := newLifecycleAccount(t, "mfa")
	ctx := context.Background()

	err := a.svc.RegenerateBackupCodes(ctx, a.user.ID, []string{"NEWCODE1"})
	require.ErrorIs(t, err, ErrMFANotEnabled, "nothing to attach codes to")

	require.NoError(t, a.svc.EnableMFA(ctx, a.user.ID, "JBSWY3DPEHPK3PXP", []string{"OLDCODE1", "OLDCODE2"}))
	t.Cleanup(func() { _, _ = a.db.Exec(`DELETE FROM user_mfa WHERE user_id::text = $1`, a.user.ID) })

	require.NoError(t, a.svc.RegenerateBackupCodes(ctx, a.user.ID, []string{"NEWCODE1", "NEWCODE2", "NEWCODE3"}))

	ok, _ := a.svc.ValidateBackupCode(ctx, a.user.ID, "OLDCODE1")
	assert.False(t, ok, "an old code is dead")
	ok, _ = a.svc.ValidateBackupCode(ctx, a.user.ID, "NEWCODE2")
	assert.True(t, ok, "a new code works")
	secret, err := a.svc.GetMFASecret(ctx, a.user.ID)
	require.NoError(t, err)
	assert.Equal(t, "JBSWY3DPEHPK3PXP", secret, "the authenticator is untouched")

	// The row is the truth: a fresh service loading from the database sees
	// the new set (minus the one just consumed) and none of the old.
	fresh := NewAuthService(nil, a.db)
	require.NoError(t, fresh.LoadFromDB(ctx))
	require.NoError(t, fresh.LoadMFAFromDB(ctx))
	ok, _ = fresh.ValidateBackupCode(ctx, a.user.ID, "OLDCODE2")
	assert.False(t, ok)
	ok, _ = fresh.ValidateBackupCode(ctx, a.user.ID, "NEWCODE3")
	assert.True(t, ok)

	meta := a.auditMetadata("mfa.backup_codes_regenerated")
	assert.EqualValues(t, 3, meta["backup_codes"])
}
