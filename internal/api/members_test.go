package api

// WP-TEAM-1 (Stage 1a Prompt 52, PR A): members and roles. requireJWT reads
// the acting member's role from tenant_members on every request; every
// write of the management and platform APIs names its action and is
// refused (403 role_forbidden) to a role auth.Can does not allow.

import (
	"net/http"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FairForge/vaultaire/internal/auth"
)

// addMember makes a user that is a member of the fixture's tenant with
// role, and returns a JWT for that tenant.
func (f *platformFixture) addMember(role auth.Role) (*auth.User, string) {
	f.t.Helper()
	u := &auth.User{ID: uuid.NewString(), Email: "member-" + uuid.NewString()[:8] + "@stored.ge", TenantID: f.user.TenantID}
	_, err := f.db.Exec(`INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, u.ID, u.Email)
	require.NoError(f.t, err)
	_, err = f.db.Exec(`INSERT INTO tenant_members (tenant_id, user_id, role, invited_by) VALUES ($1, $2, $3, $4)`,
		f.user.TenantID, u.ID, string(role), f.user.ID)
	require.NoError(f.t, err)
	f.t.Cleanup(func() {
		_, _ = f.db.Exec(`DELETE FROM tenant_members WHERE user_id = $1`, u.ID)
		_, _ = f.db.Exec(`DELETE FROM api_keys WHERE user_id = $1`, u.ID)
		_, _ = f.db.Exec(`DELETE FROM audit_logs WHERE user_id = $1 OR performed_by = $1`, u.ID)
		_, _ = f.db.Exec(`DELETE FROM users WHERE id = $1`, u.ID)
	})
	jwt, err := f.s.auth.GenerateJWTForTenant(u, f.user.TenantID, role)
	require.NoError(f.t, err)
	return u, jwt
}

func (f *platformFixture) withManagement() *platformFixture {
	f.s.registerManagementRoutes()
	f.t.Cleanup(func() {
		_, _ = f.db.Exec(`DELETE FROM api_keys WHERE tenant_id = $1 AND NOT is_primary`, f.user.TenantID)
		_, _ = f.db.Exec(`DELETE FROM tenant_members WHERE tenant_id = $1`, f.user.TenantID)
	})
	return f
}

func TestMembers_ViewerCannotCreateAKey(t *testing.T) {
	f := setupPlatformFixture(t).withManagement()
	_, viewer := f.addMember(auth.RoleViewer)

	code, out := f.call(http.MethodPost, "/api/v1/manage/keys", viewer, map[string]any{"name": "nope"})
	assert.Equal(t, http.StatusForbidden, code, out)
	assert.Equal(t, "role_forbidden", errCode(out))
	code, out = f.call(http.MethodGet, "/api/v1/manage/buckets", viewer, nil)
	assert.Equal(t, http.StatusOK, code, "a viewer reads: %v", out)

	code, out = f.call(http.MethodPost, "/api/v1/manage/keys", f.jwt, map[string]any{"name": "owner's"})
	assert.Equal(t, http.StatusCreated, code, "the owner makes keys: %v", out)
}

func TestMembers_OnlyTheOwnerDeletesTheAccount(t *testing.T) {
	f := setupPlatformFixture(t).withManagement()
	t.Cleanup(func() {
		_, _ = f.db.Exec(`UPDATE users SET deletion_scheduled_at = NULL, status = 'active' WHERE id = $1`, f.user.ID)
	})
	for _, role := range []auth.Role{auth.RoleViewer, auth.RoleDeveloper, auth.RoleAdmin} {
		_, jwt := f.addMember(role)
		code, out := f.call(http.MethodDelete, "/api/v1/manage/account", jwt, map[string]any{"reason": "leaving"})
		assert.Equal(t, http.StatusForbidden, code, "%s: %v", role, out)
		assert.Equal(t, "role_forbidden", errCode(out), role)
	}
	var scheduled bool
	require.NoError(t, f.db.QueryRow(`SELECT deletion_scheduled_at IS NOT NULL FROM users WHERE id = $1`, f.user.ID).Scan(&scheduled))
	assert.False(t, scheduled, "no member scheduled the owner's account")

	code, out := f.call(http.MethodDelete, "/api/v1/manage/account", f.jwt, map[string]any{"reason": "done"})
	assert.Equal(t, http.StatusOK, code, out)
	require.NoError(t, f.db.QueryRow(`SELECT deletion_scheduled_at IS NOT NULL FROM users WHERE id = $1`, f.user.ID).Scan(&scheduled))
	assert.True(t, scheduled, "the owner did")
}

func TestMembers_APlatformsMemberActsOnItsChildren(t *testing.T) {
	f := setupPlatformFixture(t).withManagement()
	f.grant()
	code, out := f.call(http.MethodPost, "/api/v1/platform/tenants", f.jwt, map[string]any{"label": "a customer"})
	require.Equal(t, http.StatusCreated, code, out)
	child, _ := out["id"].(string)
	require.NotEmpty(t, child)
	_, developer := f.addMember(auth.RoleDeveloper)
	_, viewer := f.addMember(auth.RoleViewer)

	code, out = f.call(http.MethodPost, "/api/v1/platform/tenants/"+child+"/keys", developer, map[string]any{"name": "dev's"})
	assert.Equal(t, http.StatusCreated, code, "a developer of the platform mints a child's key: %v", out)
	code, out = f.call(http.MethodPost, "/api/v1/platform/tenants/"+child+"/keys", viewer, map[string]any{"name": "viewer's"})
	assert.Equal(t, http.StatusForbidden, code, out)
	assert.Equal(t, "role_forbidden", errCode(out))
	code, _ = f.call(http.MethodGet, "/api/v1/platform/tenants/"+child, viewer, nil)
	assert.Equal(t, http.StatusOK, code, "a viewer reads the child")
	code, out = f.call(http.MethodPost, "/api/v1/platform/tenants", developer, map[string]any{"label": "no"})
	assert.Equal(t, http.StatusForbidden, code, "creating a child is an admin's: %v", out)
}

// A user who is no member of the JWT's tenant is refused on every route of
// the tenant surfaces.
func TestMembers_ANonMemberIsRefused(t *testing.T) {
	f := setupPlatformFixture(t).withManagement()
	u, _ := f.addMember(auth.RoleViewer)
	_, err := f.db.Exec(`DELETE FROM tenant_members WHERE user_id = $1`, u.ID)
	require.NoError(t, err)
	jwt, err := f.s.auth.GenerateJWTForTenant(u, f.user.TenantID, auth.RoleViewer)
	require.NoError(t, err)
	code, out := f.call(http.MethodGet, "/api/v1/manage/buckets", jwt, nil)
	assert.Equal(t, http.StatusForbidden, code, out)
	assert.Equal(t, "not_a_member", errCode(out), "a removed member loses the tenant at once, whatever its token says")
}

// memberWritesOpenToAnyMember are the writes no role check guards, and why.
var memberWritesOpenToAnyMember = map[string]string{}

// Every write route of /api/v1/manage and /api/v1/platform names a write
// action: a viewer — who may only read — is refused by role on each.
func TestManagementRoutes_EveryWriteNamesItsAction(t *testing.T) {
	f := setupPlatformFixture(t).withManagement()
	f.grant()
	_, viewer := f.addMember(auth.RoleViewer)
	rl, _ := f.s.jsonAPIMiddleware()
	rl.mu.Lock()
	rl.rps, rl.burst = 1000, 1000 // the walk sends every write at once
	rl.mu.Unlock()
	writes := 0
	require.NoError(t, chi.Walk(f.s.router, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		if method == http.MethodGet || method == http.MethodHead || method == http.MethodOptions {
			return nil
		}
		if !strings.HasPrefix(route, "/api/v1/manage/") && !strings.HasPrefix(route, "/api/v1/platform/") {
			return nil
		}
		if why, open := memberWritesOpenToAnyMember[method+" "+route]; open {
			assert.NotEmpty(t, why)
			return nil
		}
		writes++
		path := strings.NewReplacer("{name}", "b", "{id}", "x", "{key}", "k", "{user}", "u").Replace(route)
		code, out := f.call(method, path, viewer, map[string]any{})
		assert.Equal(t, http.StatusForbidden, code, "%s %s: %v", method, route, out)
		assert.Equal(t, "role_forbidden", errCode(out), "%s %s names no write action", method, route)
		return nil
	}))
	assert.Greater(t, writes, 15, "the walk found the write routes")
}
