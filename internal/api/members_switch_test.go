package api

// WP-TEAM-1 (Stage 1a Prompt 52, PR C): a user in several tenants. The
// sign-in lists them and mints a JWT for one; every management list is the
// active tenant's; keys are the tenant's, whoever made them; the audit row
// names the member who acted and the tenant acted on.

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FairForge/vaultaire/internal/auth"
)

const memberPassword = "Str0ngPassw0rd!"

// login signs in through POST /auth/login, optionally choosing tenant_id.
func (f *platformFixture) login(email, tenantID string) (int, map[string]any) {
	f.t.Helper()
	body := map[string]any{"email": email, "password": memberPassword}
	if tenantID != "" {
		body["tenant_id"] = tenantID
	}
	return f.call(http.MethodPost, "/auth/login", "", body)
}

func bucketNames(out map[string]any) []string {
	var names []string
	data, _ := out["data"].([]any)
	for _, d := range data {
		if m, ok := d.(map[string]any); ok {
			names = append(names, m["name"].(string))
		}
	}
	return names
}

func keyTenants(t *testing.T, f *platformFixture, out map[string]any) []string {
	t.Helper()
	var tenants []string
	data, _ := out["data"].([]any)
	for _, d := range data {
		m := d.(map[string]any)
		var tenant string
		require.NoError(t, f.db.QueryRow(`SELECT tenant_id FROM api_keys WHERE id = $1`, m["id"]).Scan(&tenant))
		tenants = append(tenants, tenant)
	}
	return tenants
}

func TestMembers_AUserInTwoTenantsSeesTheRightOne(t *testing.T) {
	f := setupPlatformFixture(t).withManagement()
	f.s.router.Post("/auth/login", f.s.handleLogin)
	ctx := context.Background()
	// Tenant A is the fixture user's own; tenant B is another account's,
	// which the fixture user joins as a viewer.
	other, tenantB, _, err := f.s.auth.CreateUserWithTenant(ctx, "other-"+uuid.NewString()[:8]+"@stored.ge", memberPassword, "B")
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = f.db.Exec(`DELETE FROM buckets WHERE tenant_id = $1`, tenantB.ID)
		_, _ = f.db.Exec(`DELETE FROM tenant_members WHERE tenant_id = $1`, tenantB.ID)
		_, _ = f.db.Exec(`DELETE FROM audit_logs WHERE tenant_id = $1`, tenantB.ID)
		cleanupPlatform(f.db, tenantB.ID)
		_, _ = f.db.Exec(`DELETE FROM api_keys WHERE tenant_id = $1`, tenantB.ID)
		_, _ = f.db.Exec(`DELETE FROM tenant_quotas WHERE tenant_id = $1`, tenantB.ID)
		_, _ = f.db.Exec(`DELETE FROM tenants WHERE id = $1`, tenantB.ID)
		_, _ = f.db.Exec(`DELETE FROM users WHERE id = $1`, other.ID)
	})
	_, err = f.db.Exec(`INSERT INTO tenant_members (tenant_id, user_id, role) VALUES ($1, $2, 'viewer')`, tenantB.ID, f.user.ID)
	require.NoError(t, err)
	bucketA, bucketB := "two-a-"+uuid.NewString()[:8], "two-b-"+uuid.NewString()[:8]
	_, err = f.db.Exec(`INSERT INTO buckets (tenant_id, name, visibility) VALUES ($1, $2, 'private'), ($3, $4, 'private')`,
		f.user.TenantID, bucketA, tenantB.ID, bucketB)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = f.db.Exec(`DELETE FROM buckets WHERE tenant_id = $1`, f.user.TenantID) })

	// The sign-in lists both tenants and defaults to the owned one.
	code, out := f.login(f.user.Email, "")
	require.Equal(t, http.StatusOK, code, out)
	assert.Equal(t, f.user.TenantID, out["tenant_id"], "the owned tenant by default")
	assert.Equal(t, "owner", out["role"])
	tenants, _ := out["tenants"].([]any)
	require.Len(t, tenants, 2)
	jwtA, _ := out["token"].(string)

	code, out = f.call(http.MethodGet, "/api/v1/manage/buckets", jwtA, nil)
	require.Equal(t, http.StatusOK, code, out)
	assert.Contains(t, bucketNames(out), bucketA)
	assert.NotContains(t, bucketNames(out), bucketB, "a JWT for A never lists B's buckets")
	code, out = f.call(http.MethodGet, "/api/v1/manage/keys", jwtA, nil)
	require.Equal(t, http.StatusOK, code, out)
	for _, tn := range keyTenants(t, f, out) {
		assert.Equal(t, f.user.TenantID, tn, "a JWT for A lists A's keys only")
	}

	// Switching mints B's: B's buckets and keys, and the viewer's role.
	code, out = f.login(f.user.Email, tenantB.ID)
	require.Equal(t, http.StatusOK, code, out)
	assert.Equal(t, tenantB.ID, out["tenant_id"])
	assert.Equal(t, "viewer", out["role"])
	jwtB, _ := out["token"].(string)
	code, out = f.call(http.MethodGet, "/api/v1/manage/buckets", jwtB, nil)
	require.Equal(t, http.StatusOK, code, out)
	assert.Contains(t, bucketNames(out), bucketB)
	assert.NotContains(t, bucketNames(out), bucketA)
	code, out = f.call(http.MethodGet, "/api/v1/manage/keys", jwtB, nil)
	require.Equal(t, http.StatusOK, code, out)
	bKeys := keyTenants(t, f, out)
	require.NotEmpty(t, bKeys, "B's primary key, made by B's owner, is in B's list")
	for _, tn := range bKeys {
		assert.Equal(t, tenantB.ID, tn)
	}
	code, out = f.call(http.MethodPost, "/api/v1/manage/keys", jwtB, map[string]any{"name": "no"})
	assert.Equal(t, http.StatusForbidden, code, "a viewer of B: %v", out)

	// A tenant the user is no member of is refused at the sign-in.
	code, out = f.login(f.user.Email, "tenant-not-mine-"+uuid.NewString()[:8])
	assert.Equal(t, http.StatusForbidden, code, out)
	assert.Equal(t, "not_a_member", errCode(out))
}

func TestMembers_AuditRowNamesTheMember(t *testing.T) {
	f := setupPlatformFixture(t).withManagement()
	dev, devJWT := f.addMember(auth.RoleDeveloper)

	code, out := f.call(http.MethodPost, "/api/v1/manage/keys", devJWT, map[string]any{"name": "dev's key"})
	require.Equal(t, http.StatusCreated, code, out)
	keyID, _ := out["id"].(string)
	var keyTenant, keyUser string
	require.NoError(t, f.db.QueryRow(`SELECT tenant_id, user_id::text FROM api_keys WHERE id = $1`, keyID).Scan(&keyTenant, &keyUser))
	assert.Equal(t, f.user.TenantID, keyTenant, "the key is the active tenant's")
	assert.Equal(t, dev.ID, keyUser, "made by the member")

	var actor, tenant string
	require.NoError(t, f.db.QueryRow(`SELECT performed_by::text, tenant_id FROM audit_logs
		WHERE action = 'key.created' AND resource = $1`, "key:"+keyID).Scan(&actor, &tenant))
	assert.Equal(t, dev.ID, actor, "the audit row names the member who acted")
	assert.Equal(t, f.user.TenantID, tenant, "and the tenant it acted on")

	// The owner revokes the developer's key: a key is the tenant's.
	code, out = f.call(http.MethodDelete, "/api/v1/manage/keys/"+keyID, f.jwt, nil)
	require.Equal(t, http.StatusOK, code, out)
	require.NoError(t, f.db.QueryRow(`SELECT performed_by::text, tenant_id FROM audit_logs
		WHERE action = 'key.revoked' AND resource = $1`, "key:"+keyID).Scan(&actor, &tenant))
	assert.Equal(t, f.user.ID, actor)
	assert.Equal(t, f.user.TenantID, tenant)
}
