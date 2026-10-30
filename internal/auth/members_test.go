package auth

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// WP-TEAM-1 (Stage 1a Prompt 52, PR A): registration writes the owner
// member in its one transaction — the four-table rule becomes five.
func TestRegister_CreatesTheOwnerMember(t *testing.T) {
	db := setupTestDB(t)
	t.Cleanup(func() { _ = db.Close() })
	email := "member-owner-" + uuid.NewString()[:8] + "@stored.ge"
	svc := NewAuthService(nil, db)

	user, tenant, _, err := svc.CreateUserWithTenant(context.Background(), email, "correct horse battery", "Members Co")
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM tenant_members WHERE tenant_id = $1`, tenant.ID)
		_, _ = db.Exec(`DELETE FROM api_keys WHERE tenant_id = $1`, tenant.ID)
		_, _ = db.Exec(`DELETE FROM tenant_quotas WHERE tenant_id = $1`, tenant.ID)
		_, _ = db.Exec(`DELETE FROM tenants WHERE id = $1`, tenant.ID)
		_, _ = db.Exec(`DELETE FROM users WHERE id = $1`, user.ID)
	})

	var role string
	var invitedBy *string
	require.NoError(t, db.QueryRow(`SELECT role, invited_by::text FROM tenant_members WHERE tenant_id = $1 AND user_id = $2`,
		tenant.ID, user.ID).Scan(&role, &invitedBy))
	assert.Equal(t, "owner", role)
	assert.Nil(t, invitedBy, "nobody invited the registering user")
	r, err := MemberRole(context.Background(), db, tenant.ID, user.ID)
	require.NoError(t, err)
	assert.Equal(t, RoleOwner, r)
}

// MemberRole: the row decides; with no row only the tenant's registering
// user (the e-mail rule — an account the previous build registered during
// the deploy overlap) is its owner; anyone else is no member.
func TestMemberRole_RowThenRegistrationElseNotMember(t *testing.T) {
	db := setupTestDB(t)
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	owner, other := uuid.NewString(), uuid.NewString()
	tenantID := "tenant-mr-" + owner[:8]
	ownerEmail, otherEmail := "mr-"+owner[:8]+"@stored.ge", "mr-"+other[:8]+"@stored.ge"
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO users (id, email, password_hash) VALUES ($1, $2, '')`, []any{owner, ownerEmail}},
		{`INSERT INTO users (id, email, password_hash) VALUES ($1, $2, '')`, []any{other, otherEmail}},
		{`INSERT INTO tenants (id, name, email) VALUES ($1, 'mr', $2)`, []any{tenantID, ownerEmail}},
	} {
		_, err := db.Exec(q.sql, q.args...)
		require.NoError(t, err)
	}
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM tenant_members WHERE tenant_id = $1`, tenantID)
		_, _ = db.Exec(`DELETE FROM tenants WHERE id = $1`, tenantID)
		_, _ = db.Exec(`DELETE FROM users WHERE id IN ($1, $2)`, owner, other)
	})

	r, err := MemberRole(ctx, db, tenantID, owner)
	require.NoError(t, err)
	assert.Equal(t, RoleOwner, r, "no row: the registering user owns it")
	_, err = MemberRole(ctx, db, tenantID, other)
	assert.ErrorIs(t, err, ErrNotMember)

	_, err = db.Exec(`INSERT INTO tenant_members (tenant_id, user_id, role) VALUES ($1, $2, 'viewer')`, tenantID, other)
	require.NoError(t, err)
	r, err = MemberRole(ctx, db, tenantID, other)
	require.NoError(t, err)
	assert.Equal(t, RoleViewer, r)
}

// The action table: a viewer reads; a developer makes keys and buckets and
// a platform child's keys; an admin also manages members and children; the
// owner alone deletes the account, exports it and changes billing.
func TestCan_TheActionTable(t *testing.T) {
	want := map[Role][]Action{
		RoleViewer:    {ActionRead},
		RoleDeveloper: {ActionRead, ActionBucketWrite, ActionKeyWrite, ActionWebhookWrite, ActionSandboxReset, ActionChildKey},
		RoleAdmin:     {ActionRead, ActionBucketWrite, ActionKeyWrite, ActionWebhookWrite, ActionSandboxReset, ActionChildKey, ActionChildWrite, ActionMemberManage},
		RoleOwner:     Actions(),
	}
	for role, allowed := range want {
		set := map[Action]bool{}
		for _, a := range allowed {
			set[a] = true
		}
		for _, a := range Actions() {
			assert.Equal(t, set[a], Can(role, a), "%s may %s", role, a)
		}
	}
	assert.False(t, Can("", ActionRead), "no role, no action")
	assert.False(t, Can("superuser", ActionRead))
}

// RP84-3: the registration's company becomes tenants.name, which the
// invitation's subject carries; control characters (CR/LF above all) are
// stripped before either row is written.
func TestRegister_TenantNameHasNoControlCharacters(t *testing.T) {
	db := setupTestDB(t)
	t.Cleanup(func() { _ = db.Close() })
	email := "ctl-name-" + uuid.NewString()[:8] + "@stored.ge"
	svc := NewAuthService(nil, db)

	user, tenant, _, err := svc.CreateUserWithTenant(context.Background(), email, "correct horse battery",
		"Acme\r\nReply-To: x@evil.example\tInc\x00\x7f")
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM tenant_members WHERE tenant_id = $1`, tenant.ID)
		_, _ = db.Exec(`DELETE FROM api_keys WHERE tenant_id = $1`, tenant.ID)
		_, _ = db.Exec(`DELETE FROM tenant_quotas WHERE tenant_id = $1`, tenant.ID)
		_, _ = db.Exec(`DELETE FROM tenants WHERE id = $1`, tenant.ID)
		_, _ = db.Exec(`DELETE FROM users WHERE id = $1`, user.ID)
	})
	var name, company string
	require.NoError(t, db.QueryRow(`SELECT t.name, u.company FROM tenants t JOIN users u ON u.email = t.email WHERE t.id = $1`,
		tenant.ID).Scan(&name, &company))
	assert.Equal(t, "AcmeReply-To: x@evil.example Inc", name, "control characters stripped, a tab made a space")
	assert.Equal(t, name, company)
}
