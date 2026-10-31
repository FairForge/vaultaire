package api

// WP-KEY-1 (Stage 1a Prompt 54, PR A; D-STRIPE-29): the management key on
// /api/v1/manage and /api/v1/platform. A key has no member and no role: its
// scopes are its authority (manage:write acts as a developer, manage:read as
// a viewer, platform as an admin on the platform routes, apps as an app's
// admin over its own children); it never does what needs a person
// (members, the export, the erasure, minting management keys); its S3
// ceiling bounds what it mints; the audit trail names it.

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FairForge/vaultaire/internal/account"
	"github.com/FairForge/vaultaire/internal/audit"
	"github.com/FairForge/vaultaire/internal/auth"
)

// mgmtKey mints a management key of the fixture's tenant directly (the
// owner's), returning its secret and row.
func (f *platformFixture) mgmtKey(ceiling *auth.S3Ceiling, scopes ...string) (string, *auth.ManagementKey) {
	f.t.Helper()
	k, secret, err := auth.MintManagementKey(context.Background(), f.db, f.user.TenantID,
		auth.ManagementKeyOptions{Name: "test server", Scopes: scopes, Ceiling: ceiling, CreatedBy: f.user.ID})
	require.NoError(f.t, err)
	f.t.Cleanup(func() { _, _ = f.db.Exec(`DELETE FROM management_keys WHERE tenant_id = $1`, f.user.TenantID) })
	return secret, k
}

func TestManagementKey_ScopesAreItsAuthority(t *testing.T) {
	f := setupPlatformFixture(t).withManagement()
	t.Cleanup(func() { _, _ = f.db.Exec(`DELETE FROM buckets WHERE tenant_id = $1`, f.user.TenantID) })
	read, _ := f.mgmtKey(nil, auth.ScopeManageRead)
	write, _ := f.mgmtKey(nil, auth.ScopeManageWrite)

	code, out := f.call(http.MethodGet, "/api/v1/manage/buckets", read, nil)
	assert.Equal(t, http.StatusOK, code, out)
	code, out = f.call(http.MethodPost, "/api/v1/manage/buckets", read, map[string]any{"name": "mk-read-" + f.user.TenantID[7:15]})
	assert.Equal(t, http.StatusForbidden, code, out)
	assert.Equal(t, "scope_forbidden", errCode(out), "a manage:read key is 403 on a write")

	code, out = f.call(http.MethodPost, "/api/v1/manage/buckets", write, map[string]any{"name": "mk-write-" + f.user.TenantID[7:15]})
	assert.Equal(t, http.StatusCreated, code, out)
	code, out = f.call(http.MethodPost, "/api/v1/manage/keys", write, map[string]any{"name": "minted by a server"})
	require.Equal(t, http.StatusCreated, code, out)
	var maker string
	require.NoError(t, f.db.QueryRow(`SELECT user_id::text FROM api_keys WHERE id = $1`, out["id"]).Scan(&maker))
	assert.Equal(t, f.user.ID, maker, "a key's S3 key hangs on the account's owner")

	code, out = f.call(http.MethodGet, "/api/v1/manage/buckets", "sk_live_notakeyatall", nil)
	assert.Equal(t, http.StatusUnauthorized, code, out)
	assert.Equal(t, "invalid_management_key", errCode(out))
	code, out = f.call(http.MethodGet, "/api/v1/manage/buckets", f.jwt, nil)
	assert.Equal(t, http.StatusOK, code, "a JWT is still a JWT: %v", out)
}

func TestManagementKey_ReachesThePlatformRoutes(t *testing.T) {
	f := setupPlatformFixture(t).withManagement()
	f.grant()
	plat, _ := f.mgmtKey(nil, auth.ScopePlatform)
	write, _ := f.mgmtKey(nil, auth.ScopeManageWrite)

	code, out := f.call(http.MethodPost, "/api/v1/platform/tenants", plat, map[string]any{"label": "made by a server"})
	require.Equal(t, http.StatusCreated, code, out)
	child := out["id"].(string)
	code, out = f.call(http.MethodPost, "/api/v1/platform/tenants/"+child+"/keys", plat, map[string]any{"name": "its key"})
	require.Equal(t, http.StatusCreated, code, out)
	assert.NotEmpty(t, out["secret"], "the child's key, with no person in the loop")
	code, out = f.call(http.MethodGet, "/api/v1/platform/tenants", plat, nil)
	require.Equal(t, http.StatusOK, code, out)

	code, out = f.call(http.MethodPost, "/api/v1/platform/tenants", write, map[string]any{"label": "no"})
	assert.Equal(t, http.StatusForbidden, code, out)
	assert.Equal(t, "scope_forbidden", errCode(out), "manage:write is not the platform scope")
	code, out = f.call(http.MethodPost, "/api/v1/manage/buckets", plat, map[string]any{"name": "mk-plat-" + f.user.TenantID[7:15]})
	assert.Equal(t, http.StatusForbidden, code, out)
	assert.Equal(t, "scope_forbidden", errCode(out), "platform is not a manage scope")
}

func TestManagementKey_AnAppsKeyReachesOnlyItsSubtree(t *testing.T) {
	f := setupPlatformFixture(t).withManagement()
	f.grant()
	cleanupDescendants(f)
	plat, _ := f.mgmtKey(nil, auth.ScopePlatform)
	t.Cleanup(func() {
		_, _ = f.db.Exec(`DELETE FROM management_keys WHERE tenant_id IN (SELECT id FROM tenants WHERE parent_tenant_id = $1)`, f.user.TenantID)
	})

	code, out := f.call(http.MethodPost, "/api/v1/platform/tenants", plat, map[string]any{"label": "an app", "kind": "app"})
	require.Equal(t, http.StatusCreated, code, out)
	app := out["id"].(string)
	code, out = f.call(http.MethodPost, "/api/v1/platform/tenants", plat, map[string]any{"label": "a customer"})
	require.Equal(t, http.StatusCreated, code, out)
	other := out["id"].(string)

	code, out = f.call(http.MethodPost, "/api/v1/platform/tenants/"+other+"/management-keys", plat, map[string]any{"scopes": []string{"apps"}})
	assert.Equal(t, http.StatusBadRequest, code, out)
	assert.Equal(t, "not_an_app", errCode(out))
	code, out = f.call(http.MethodPost, "/api/v1/platform/tenants/"+app+"/management-keys", plat, map[string]any{"name": "the app's backend", "scopes": []string{"apps", "platform"}})
	assert.Equal(t, http.StatusBadRequest, code, out, "an app's key is never a platform key")
	code, out = f.call(http.MethodPost, "/api/v1/platform/tenants/"+app+"/management-keys", plat, map[string]any{"name": "the app's backend", "scopes": []string{"apps"}})
	require.Equal(t, http.StatusCreated, code, out)
	appKey := out["secret"].(string)

	code, out = f.call(http.MethodPost, "/api/v1/platform/tenants", appKey, map[string]any{"label": "an end user"})
	require.Equal(t, http.StatusCreated, code, out)
	user := out["id"].(string)
	assert.Equal(t, app, out["parent_id"], "the app's key makes the app's own children")
	code, out = f.call(http.MethodPost, "/api/v1/platform/tenants/"+user+"/keys", appKey, map[string]any{"name": "end user key"})
	require.Equal(t, http.StatusCreated, code, out)

	code, out = f.call(http.MethodGet, "/api/v1/platform/tenants", appKey, nil)
	require.Equal(t, http.StatusOK, code, out)
	items, _ := out["data"].([]any)
	require.Len(t, items, 1, "only its subtree: %v", out)
	assert.Equal(t, user, items[0].(map[string]any)["id"])
	code, out = f.call(http.MethodGet, "/api/v1/platform/tenants/"+other, appKey, nil)
	assert.Equal(t, http.StatusNotFound, code, "the platform's other child is not the app's: %v", out)
	code, out = f.call(http.MethodGet, "/api/v1/platform/tenants/"+app, appKey, nil)
	assert.Equal(t, http.StatusNotFound, code, "nor is the app itself: %v", out)

	code, out = f.call(http.MethodPost, "/api/v1/platform/tenants", appKey, map[string]any{"label": "an app's app", "kind": "app"})
	assert.Equal(t, http.StatusBadRequest, code, out)
	assert.Equal(t, "child_of_child", errCode(out), "depth 2 stays the deepest")
	code, out = f.call(http.MethodPost, "/api/v1/platform/tenants", appKey, map[string]any{"label": "deeper", "parent_id": user})
	assert.Equal(t, http.StatusNotFound, code, out)
	code, out = f.call(http.MethodPost, "/api/v1/platform/tenants/"+user+"/management-keys", appKey, map[string]any{"scopes": []string{"apps"}})
	assert.Equal(t, http.StatusBadRequest, code, out)
	assert.Equal(t, "not_an_app", errCode(out))
}

func TestManagementKey_RevokedIsRefusedAtOnceThroughTheAPI(t *testing.T) {
	f := setupPlatformFixture(t).withManagement()
	t.Cleanup(func() { _, _ = f.db.Exec(`DELETE FROM management_keys WHERE tenant_id = $1`, f.user.TenantID) })

	code, out := f.call(http.MethodPost, "/api/v1/manage/management-keys", f.jwt, map[string]any{"name": "ci", "scopes": []string{"manage:read"}})
	require.Equal(t, http.StatusCreated, code, out)
	id, secret := out["id"].(string), out["secret"].(string)
	assert.Regexp(t, `^sk_live_[0-9A-Za-z]{43}$`, secret)
	code, out = f.call(http.MethodGet, "/api/v1/manage/management-keys", f.jwt, nil)
	require.Equal(t, http.StatusOK, code, out)
	listed := out["data"].([]any)[0].(map[string]any)
	assert.Equal(t, id, listed["id"])
	assert.NotContains(t, listed, "secret", "shown once")

	code, _ = f.call(http.MethodGet, "/api/v1/manage/buckets", secret, nil)
	require.Equal(t, http.StatusOK, code)
	code, out = f.call(http.MethodDelete, "/api/v1/manage/management-keys/"+id, f.jwt, nil)
	require.Equal(t, http.StatusOK, code, out)
	code, out = f.call(http.MethodGet, "/api/v1/manage/buckets", secret, nil)
	assert.Equal(t, http.StatusUnauthorized, code, "refused on the next request: %v", out)

	_, devJWT := f.addMember(auth.RoleDeveloper)
	code, out = f.call(http.MethodPost, "/api/v1/manage/management-keys", devJWT, map[string]any{"scopes": []string{"manage:read"}})
	assert.Equal(t, http.StatusForbidden, code, out)
	assert.Equal(t, "role_forbidden", errCode(out), "an admin's or the owner's")
}

// RP82-6: the ceiling of what a key mints is its own field, never a bucket
// scope of the key's: it bounds the S3 keys the key makes, and not the
// buckets the key itself manages.
func TestManagementKey_CeilingIsItsOwnField(t *testing.T) {
	f := setupPlatformFixture(t).withManagement()
	f.grant()
	t.Cleanup(func() { _, _ = f.db.Exec(`DELETE FROM buckets WHERE tenant_id = $1`, f.user.TenantID) })
	ceiling := &auth.S3Ceiling{Permissions: []string{"GetObject", "ListObjects"}, BucketScope: []string{"photos"}}
	key, _ := f.mgmtKey(ceiling, auth.ScopePlatform, auth.ScopeManageWrite)
	code, out := f.call(http.MethodPost, "/api/v1/platform/tenants", key, map[string]any{"label": "c"})
	require.Equal(t, http.StatusCreated, code, out)
	child := out["id"].(string)

	code, out = f.call(http.MethodPost, "/api/v1/platform/tenants/"+child+"/keys", key, map[string]any{"permissions": []string{"PutObject"}})
	assert.Equal(t, http.StatusBadRequest, code, out)
	assert.Equal(t, "scope_exceeds_parent", errCode(out))
	code, out = f.call(http.MethodPost, "/api/v1/platform/tenants/"+child+"/keys", key, map[string]any{"permissions": []string{"GetObject"}})
	require.Equal(t, http.StatusCreated, code, out)
	assert.ElementsMatch(t, []any{"photos"}, out["bucket_scope"], "what the mint leaves unset is the ceiling's")
	code, out = f.call(http.MethodPost, "/api/v1/manage/keys", key, map[string]any{"permissions": []string{"PutObject"}})
	assert.Equal(t, http.StatusBadRequest, code, out)
	assert.Equal(t, "scope_exceeds_parent", errCode(out), "the tenant's own keys too")

	code, out = f.call(http.MethodPost, "/api/v1/manage/buckets", key, map[string]any{"name": "mk-other-" + f.user.TenantID[7:15]})
	assert.Equal(t, http.StatusCreated, code, "the ceiling is not the key's own bucket scope: %v", out)
}

func TestManagementKey_AuditNamesThePrincipal(t *testing.T) {
	f := setupPlatformFixture(t).withManagement()
	f.grant()
	key, k := f.mgmtKey(nil, auth.ScopePlatform)
	code, out := f.call(http.MethodPost, "/api/v1/platform/tenants", key, map[string]any{"label": "by key"})
	require.Equal(t, http.StatusCreated, code, out)
	byKey := out["id"].(string)
	code, out = f.call(http.MethodPost, "/api/v1/platform/tenants", f.jwt, map[string]any{"label": "by person"})
	require.Equal(t, http.StatusCreated, code, out)
	byPerson := out["id"].(string)

	page, err := audit.List(context.Background(), f.db, audit.Filter{TenantID: f.user.TenantID, Action: "platform.tenant_created", Limit: 10})
	require.NoError(t, err)
	got := map[string]audit.Row{}
	for _, r := range page.Rows {
		got[r.Resource] = r
	}
	require.Contains(t, got, "tenant:"+byKey)
	assert.Equal(t, "mk:"+k.ID, got["tenant:"+byKey].Principal, "the key, not the system")
	assert.Empty(t, got["tenant:"+byKey].PerformedBy, "no person acted")
	require.Contains(t, got, "tenant:"+byPerson)
	assert.Equal(t, f.user.ID, got["tenant:"+byPerson].Principal)
	assert.Equal(t, f.user.ID, got["tenant:"+byPerson].PerformedBy)
}

// RP86: a management key never manages members, exports or erases the
// account, or mints management keys — a leaked server key must not make
// its holder a member.
func TestManagementKey_NeverManagesMembers(t *testing.T) {
	f := setupPlatformFixture(t).withManagement()
	key, _ := f.mgmtKey(nil, auth.ScopeManageWrite, auth.ScopeManageRead)
	member, _ := f.addMember(auth.RoleViewer)
	for _, c := range []struct{ method, path string }{
		{http.MethodPost, "/api/v1/manage/members"},
		{http.MethodPatch, "/api/v1/manage/members/" + member.ID},
		{http.MethodDelete, "/api/v1/manage/members/" + member.ID},
		{http.MethodDelete, "/api/v1/manage/invitations/00000000-0000-0000-0000-000000000000"},
		{http.MethodPost, "/api/v1/manage/invitations/sometoken/accept"},
		{http.MethodPost, "/api/v1/manage/account/export"},
		{http.MethodDelete, "/api/v1/manage/account"},
		{http.MethodPost, "/api/v1/manage/account/cancel-deletion"},
		{http.MethodPost, "/api/v1/manage/management-keys"},
		{http.MethodDelete, "/api/v1/manage/management-keys/00000000-0000-0000-0000-000000000000"},
	} {
		code, out := f.call(c.method, c.path, key, map[string]any{"email": "x@stored.ge", "role": "admin", "scopes": []string{"manage:read"}})
		assert.Equal(t, http.StatusForbidden, code, "%s %s: %v", c.method, c.path, out)
		assert.Equal(t, "person_required", errCode(out), "%s %s", c.method, c.path)
	}
	assert.Equal(t, 1, countRows(t, f.db, `SELECT COUNT(*) FROM tenant_members WHERE user_id = $1 AND role = 'viewer'`, member.ID))
}

// RP86 (RP84-1's lesson): a management key is the tenant's. A member's
// removal names the management keys it minted (keys_made) and revokes them
// only with revoke_keys; its erasure forgets the maker, never the key.
func TestErase_AMembersErasureKeepsThePlatformsManagementKey(t *testing.T) {
	f := setupPlatformFixture(t).withManagement()
	t.Cleanup(func() { _, _ = f.db.Exec(`DELETE FROM management_keys WHERE tenant_id = $1`, f.user.TenantID) })
	u, uJWT := f.addMember(auth.RoleAdmin)
	code, out := f.call(http.MethodPost, "/api/v1/manage/management-keys", uJWT, map[string]any{"name": "server", "scopes": []string{"manage:read"}})
	require.Equal(t, http.StatusCreated, code, out)
	id, secret := out["id"].(string), out["secret"].(string)

	code, out = f.call(http.MethodDelete, "/api/v1/manage/members/"+u.ID, f.jwt, nil)
	require.Equal(t, http.StatusOK, code, out)
	made, _ := out["keys_made"].([]any)
	require.Len(t, made, 1, "%v", out)
	assert.Equal(t, id, made[0].(map[string]any)["id"])
	assert.Equal(t, "management", made[0].(map[string]any)["kind"])

	_, err := f.db.Exec(`UPDATE users SET status = 'pending_deletion', deletion_scheduled_at = NOW() - INTERVAL '1 hour' WHERE id = $1`, u.ID)
	require.NoError(t, err)
	_, err = account.NewService(f.db, nil).EraseRows(context.Background(), u.ID, "", u.Email, time.Now())
	require.NoError(t, err)
	assert.Zero(t, countRows(t, f.db, `SELECT COUNT(*) FROM users WHERE id = $1`, u.ID))
	assert.Equal(t, 1, countRows(t, f.db, `SELECT COUNT(*) FROM management_keys WHERE id = $1 AND revoked_at IS NULL AND created_by IS NULL`, id),
		"the key stays live, its maker forgotten")
	code, out = f.call(http.MethodGet, "/api/v1/manage/buckets", secret, nil)
	assert.Equal(t, http.StatusOK, code, "and still works: %v", out)

	// revoke_keys revokes a removed member's management keys too.
	u2, u2JWT := f.addMember(auth.RoleAdmin)
	code, out = f.call(http.MethodPost, "/api/v1/manage/management-keys", u2JWT, map[string]any{"scopes": []string{"manage:read"}})
	require.Equal(t, http.StatusCreated, code, out)
	id2 := out["id"].(string)
	code, out = f.call(http.MethodDelete, "/api/v1/manage/members/"+u2.ID, f.jwt, map[string]any{"revoke_keys": true})
	require.Equal(t, http.StatusOK, code, out)
	assert.Equal(t, 1, countRows(t, f.db, `SELECT COUNT(*) FROM management_keys WHERE id = $1 AND revoked_at IS NOT NULL`, id2))
}

// RP88-1: "test": true mints the key ON the tenant's sandbox (made on first
// use) — the one way to an sk_test_ key — and the key acts on the sandbox.
// RP88-2: the mint body takes an optional expires_at, and the list shows it.
func TestManagementKey_TestKeyActsOnTheSandbox(t *testing.T) {
	f := setupPlatformFixture(t).withManagement()
	t.Cleanup(func() { _, _ = f.db.Exec(`DELETE FROM management_keys WHERE tenant_id = $1`, f.user.TenantID) })

	code, out := f.call(http.MethodPost, "/api/v1/manage/management-keys", f.jwt,
		map[string]any{"name": "ci", "scopes": []string{"manage:write"}, "test": true})
	require.Equal(t, http.StatusCreated, code, out)
	secret := out["secret"].(string)
	assert.Regexp(t, `^sk_test_[0-9A-Za-z]{43}$`, secret)
	sandbox, err := auth.SandboxOf(context.Background(), f.db, f.user.TenantID)
	require.NoError(t, err)
	require.NotEmpty(t, sandbox, "the sandbox is made on first use")
	assert.Equal(t, sandbox, out["tenant_id"])
	assert.Equal(t, false, out["livemode"])

	name := "mk-test-" + f.user.TenantID[7:15]
	code, out = f.call(http.MethodPost, "/api/v1/manage/buckets", secret, map[string]any{"name": name})
	require.Equal(t, http.StatusCreated, code, out)
	assert.Equal(t, 1, countRows(t, f.db, `SELECT COUNT(*) FROM buckets WHERE tenant_id = $1 AND name = $2`, sandbox, name), "the bucket is the sandbox's")
	assert.Zero(t, countRows(t, f.db, `SELECT COUNT(*) FROM buckets WHERE tenant_id = $1 AND name = $2`, f.user.TenantID, name))

	code, out = f.call(http.MethodGet, "/api/v1/manage/management-keys", f.jwt, nil)
	require.Equal(t, http.StatusOK, code, out)
	require.Len(t, out["data"].([]any), 1, "the tenant lists its sandbox's keys")

	// RP88-2 through the API.
	code, out = f.call(http.MethodPost, "/api/v1/manage/management-keys", f.jwt,
		map[string]any{"scopes": []string{"manage:read"}, "expires_at": time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)})
	assert.Equal(t, http.StatusBadRequest, code, out)
	assert.Equal(t, "invalid_expiry", errCode(out))
	at := time.Now().Add(24 * time.Hour).UTC().Truncate(time.Second)
	code, out = f.call(http.MethodPost, "/api/v1/manage/management-keys", f.jwt,
		map[string]any{"scopes": []string{"manage:read"}, "expires_at": at.Format(time.RFC3339)})
	require.Equal(t, http.StatusCreated, code, out)
	got, err := time.Parse(time.RFC3339, out["expires_at"].(string))
	require.NoError(t, err)
	assert.True(t, at.Equal(got), "%v vs %v", at, got)
}

// RP88-5 (RP88 Part 2): a test management key mints the sandbox's S3 keys —
// test-shaped (VLT_TEST_, mode test, the sandbox's), hanging on the account's
// owner as a person's test key does. It answered 401 missing_user: the
// lookup resolved no actor for a sandbox (its e-mail is
// sandbox+…@sandbox.invalid, it has no member and no parent), so the one
// rehearsal step a server integration needs was closed.
func TestManagementKey_TestKeyMintsTheSandboxsS3Key(t *testing.T) {
	f := setupPlatformFixture(t).withManagement()
	t.Cleanup(func() { _, _ = f.db.Exec(`DELETE FROM management_keys WHERE tenant_id = $1`, f.user.TenantID) })

	code, out := f.call(http.MethodPost, "/api/v1/manage/management-keys", f.jwt,
		map[string]any{"name": "ci", "scopes": []string{"manage:write"}, "test": true})
	require.Equal(t, http.StatusCreated, code, out)
	secret := out["secret"].(string)
	sandbox := out["tenant_id"].(string)
	t.Cleanup(func() { _, _ = f.db.Exec(`DELETE FROM api_keys WHERE tenant_id = $1`, sandbox) })

	code, out = f.call(http.MethodPost, "/api/v1/manage/keys", secret, map[string]any{"name": "rehearsal"})
	require.Equal(t, http.StatusCreated, code, out)
	accessKey, _ := out["key"].(string)
	assert.True(t, strings.HasPrefix(accessKey, auth.TestKeyPrefix), "a sandbox key is test-shaped: %q", accessKey)
	var tenantID, userID string
	require.NoError(t, f.db.QueryRow(`SELECT tenant_id, user_id::text FROM api_keys WHERE key_id = $1`, accessKey).Scan(&tenantID, &userID))
	assert.Equal(t, sandbox, tenantID, "the key is the sandbox's")
	assert.Equal(t, f.user.ID, userID, "it hangs on the account's owner")

	// "test": true from a key that IS the sandbox's is the same mint — never
	// sandbox_of_sandbox.
	code, out = f.call(http.MethodPost, "/api/v1/manage/keys", secret, map[string]any{"name": "rehearsal-2", "test": true})
	require.Equal(t, http.StatusCreated, code, out)
	assert.Equal(t, 2, countRows(t, f.db, `SELECT COUNT(*) FROM api_keys WHERE tenant_id = $1 AND revoked_at IS NULL`, sandbox))
}
