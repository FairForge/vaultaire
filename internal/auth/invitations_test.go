package auth

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// WP-TEAM-1 PR B: a person with no account registers through the link and
// becomes a member — with NO tenant of its own — once.
func TestRegisterInvitedUser_JoinsWithoutATenantOfItsOwn(t *testing.T) {
	db := setupTestDB(t)
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	svc := NewAuthService(nil, db)
	owner, tenant, _, err := svc.CreateUserWithTenant(ctx, "inv-owner-"+uuid.NewString()[:8]+"@stored.ge", "correct horse battery", "Inv Co")
	require.NoError(t, err)
	address := "inv-new-" + uuid.NewString()[:8] + "@stored.ge"
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM audit_logs WHERE tenant_id = $1`, tenant.ID)
		_, _ = db.Exec(`DELETE FROM tenant_invitations WHERE tenant_id = $1`, tenant.ID)
		_, _ = db.Exec(`DELETE FROM tenant_members WHERE tenant_id = $1`, tenant.ID)
		_, _ = db.Exec(`DELETE FROM api_keys WHERE tenant_id = $1`, tenant.ID)
		_, _ = db.Exec(`DELETE FROM tenant_quotas WHERE tenant_id = $1`, tenant.ID)
		_, _ = db.Exec(`DELETE FROM tenants WHERE id = $1`, tenant.ID)
		_, _ = db.Exec(`DELETE FROM users WHERE email IN ($1, $2)`, owner.Email, address)
	})
	now := time.Now()
	_, token, err := CreateInvitation(ctx, db, tenant.ID, owner.ID, address, RoleDeveloper, now)
	require.NoError(t, err)

	_, _, err = svc.RegisterInvitedUser(ctx, token, "short", now)
	require.ErrorIs(t, err, ErrPasswordTooShort)
	user, inv, err := svc.RegisterInvitedUser(ctx, token, "correct horse battery", now)
	require.NoError(t, err)
	assert.Equal(t, tenant.ID, inv.TenantID)

	role, err := MemberRole(ctx, db, tenant.ID, user.ID)
	require.NoError(t, err)
	assert.Equal(t, RoleDeveloper, role)
	var own int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM tenants WHERE email = $1`, address).Scan(&own))
	assert.Zero(t, own, "no tenant of its own")
	ok, err := svc.ValidatePassword(ctx, address, "correct horse battery")
	require.NoError(t, err)
	assert.True(t, ok, "it signs in")

	_, _, err = svc.RegisterInvitedUser(ctx, token, "correct horse battery", now)
	assert.Error(t, err, "single use")
	_, err = AcceptInvitation(ctx, db, token, user.ID, address, now)
	assert.ErrorIs(t, err, ErrInvitationUsed)
}
