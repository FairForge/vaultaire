package api

// RP92-2 / RP92-3 / RP92-6 (Stage 1a Prompt 57 Part 0): a family is a pool
// of PERSONAL accounts. A team never joins it (its members' writes would
// draw on the organizer's pool under one label); the join attaches the
// person's own root, never whatever tenant they are acting on; a join whose
// bytes do not fit the pool is refused (the whole family would go read-only,
// D-STRIPE-97); and the family list is the billing role's, never a viewer's.

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FairForge/vaultaire/internal/auth"
)

// otherMember makes a user that is a member of tenant with role.
func (f *platformFixture) otherMember(tenant string, role auth.Role) *auth.User {
	f.t.Helper()
	u := &auth.User{ID: uuid.NewString(), Email: "teammate-" + uuid.NewString()[:8] + "@family.example"}
	_, err := f.db.Exec(`INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, u.ID, u.Email)
	require.NoError(f.t, err)
	_, err = f.db.Exec(`INSERT INTO tenant_members (tenant_id, user_id, role) VALUES ($1, $2, $3)`, tenant, u.ID, string(role))
	require.NoError(f.t, err)
	f.t.Cleanup(func() {
		_, _ = f.db.Exec(`DELETE FROM tenant_members WHERE user_id = $1`, u.ID)
		_, _ = f.db.Exec(`DELETE FROM users WHERE id = $1`, u.ID)
	})
	return u
}

func (f *platformFixture) childKind(tenant string) string {
	f.t.Helper()
	var k *string
	require.NoError(f.t, f.db.QueryRow(`SELECT child_kind FROM tenants WHERE id = $1`, tenant).Scan(&k))
	if k == nil {
		return ""
	}
	return *k
}

func TestFamily_ATeamNeverJoins(t *testing.T) {
	f := setupPlatformFixture(t).withManagement().withFamily()
	p := f.person("famteam")
	tok, err := f.invite(p.user.Email)
	require.NoError(t, err)

	// A teammate on the person's account: a team, not a personal account.
	mate := f.otherMember(p.user.TenantID, auth.RoleViewer)
	code, out := f.join(p, tok)
	assert.Equal(t, http.StatusConflict, code, out)
	assert.Equal(t, "cannot_join_family", errCode(out))
	assert.Empty(t, f.childKind(p.user.TenantID), "not attached")

	// A pending member invitation: a team about to be.
	_, err = f.db.Exec(`DELETE FROM tenant_members WHERE user_id = $1`, mate.ID)
	require.NoError(t, err)
	_, err = f.db.Exec(`INSERT INTO tenant_invitations (tenant_id, kind, email, role, token_hash, expires_at)
		VALUES ($1, 'member', 'soon@family.example', 'viewer', $2, $3)`, p.user.TenantID, uuid.NewString(), time.Now().Add(24*time.Hour))
	require.NoError(t, err)
	code, out = f.join(p, tok)
	assert.Equal(t, http.StatusConflict, code, out)
	assert.Equal(t, "cannot_join_family", errCode(out))
	assert.Empty(t, f.childKind(p.user.TenantID))

	// An expired one is no team: the personal account joins.
	_, err = f.db.Exec(`UPDATE tenant_invitations SET expires_at = NOW() - interval '1 hour' WHERE tenant_id = $1 AND kind = 'member'`, p.user.TenantID)
	require.NoError(t, err)
	code, out = f.join(p, tok)
	require.Equal(t, http.StatusOK, code, out)
	assert.Equal(t, "sponsored", f.childKind(p.user.TenantID))
}

func TestFamily_JoinAttachesTheOwnRootNotTheActiveTenant(t *testing.T) {
	f := setupPlatformFixture(t).withManagement().withFamily()
	p := f.person("famown")
	team := f.person("famteamowner")
	// The person is a viewer of someone else's team, and acts on it.
	_, err := f.db.Exec(`INSERT INTO tenant_members (tenant_id, user_id, role) VALUES ($1, $2, 'viewer')`, team.user.TenantID, p.user.ID)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = f.db.Exec(`DELETE FROM tenant_members WHERE tenant_id = $1 AND user_id = $2`, team.user.TenantID, p.user.ID)
	})
	onTeam, err := f.s.auth.GenerateJWTForTenant(p.user, team.user.TenantID, auth.RoleViewer)
	require.NoError(t, err)
	tok, err := f.invite(p.user.Email)
	require.NoError(t, err)

	code, out := f.call(http.MethodPost, "/api/v1/manage/family/join/"+tok, onTeam, nil)
	require.Equal(t, http.StatusOK, code, "the join is the person's, whatever tenant they act on: %v", out)
	assert.Equal(t, "sponsored", f.childKind(p.user.TenantID), "their OWN account joined")
	assert.Empty(t, f.childKind(team.user.TenantID), "the active tenant (a team) did not")
	var parent string
	require.NoError(t, f.db.QueryRow(`SELECT parent_tenant_id FROM tenants WHERE id = $1`, p.user.TenantID).Scan(&parent))
	assert.Equal(t, f.user.TenantID, parent)
}

func TestFamily_JoinRefusedWhenThePoolWouldOverflow(t *testing.T) {
	f := setupPlatformFixture(t).withManagement().withFamily()
	var limit, poolBefore int64
	require.NoError(t, f.db.QueryRow(`SELECT storage_limit_bytes, pool_used_bytes FROM tenant_quotas WHERE tenant_id = $1`, f.user.TenantID).
		Scan(&limit, &poolBefore))
	p := f.person("famfull")
	// A lapsed plan's account: read-only above the free tier, never deleted.
	big := limit - poolBefore + 1
	_, err := f.db.Exec(`UPDATE tenant_quotas SET storage_used_bytes = $2, pool_used_bytes = $2 WHERE tenant_id = $1`, p.user.TenantID, big)
	require.NoError(t, err)
	tok, err := f.invite(p.user.Email)
	require.NoError(t, err)

	code, out := f.join(p, tok)
	assert.Equal(t, http.StatusConflict, code, out)
	assert.Equal(t, "family_pool_full", errCode(out))
	assert.Empty(t, f.childKind(p.user.TenantID), "nothing moved")
	assert.EqualValues(t, poolBefore, countRows(t, f.db, `SELECT pool_used_bytes FROM tenant_quotas WHERE tenant_id = $1`, f.user.TenantID), "the pool untouched")
	assert.EqualValues(t, big, countRows(t, f.db, `SELECT storage_used_bytes FROM tenant_quotas WHERE tenant_id = $1`, p.user.TenantID))
	assert.Zero(t, countRows(t, f.db, `SELECT COUNT(*) FROM tenant_invitations WHERE tenant_id = $1 AND accepted_at IS NOT NULL`, f.user.TenantID), "the invitation unspent")

	// Exactly what fits joins.
	_, err = f.db.Exec(`UPDATE tenant_quotas SET storage_used_bytes = $2, pool_used_bytes = $2 WHERE tenant_id = $1`, p.user.TenantID, big-1)
	require.NoError(t, err)
	code, out = f.join(p, tok)
	require.Equal(t, http.StatusOK, code, out)
	assert.Equal(t, limit, int64(countRows(t, f.db, `SELECT pool_used_bytes FROM tenant_quotas WHERE tenant_id = $1`, f.user.TenantID)))
}

func TestFamily_ViewerSeesNoFamily(t *testing.T) {
	f := setupPlatformFixture(t).withManagement().withFamily()
	m := f.familyOf(4096)
	_, viewer := f.addMember(auth.RoleViewer)

	code, out := f.call(http.MethodGet, "/api/v1/manage/family", viewer, nil)
	assert.Equal(t, http.StatusForbidden, code, "a viewer of the organizer's team reads no member's address: %v", out)
	assert.Equal(t, "role_forbidden", errCode(out))
	assert.NotContains(t, fmt.Sprint(out), m.user.Email)

	code, out = f.call(http.MethodGet, "/api/v1/manage/family", f.jwt, nil)
	require.Equal(t, http.StatusOK, code, out)
	assert.Contains(t, fmt.Sprint(out), m.user.Email, "the owner does")
}
