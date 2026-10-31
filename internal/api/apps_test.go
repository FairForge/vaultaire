package api

// WP-APP-1 PR C (docs/ACCOUNTS.md § Apps; F-43): any verified, paying
// account makes app children with no platform grant — up to 25 — each with
// its own management key (scope apps) for its backend, which makes the
// app's end users (depth 2) and stores within the builder's pool; deleting
// the app cascades; an app's bucket is never Public until its root is
// verified.

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FairForge/vaultaire/internal/auth"
	"github.com/FairForge/vaultaire/internal/modes"
	"github.com/FairForge/vaultaire/internal/usage"
)

// withApps makes the fixture's account a verified, paying one with the
// apps flag on.
func (f *platformFixture) withApps(verified, paying bool) *platformFixture {
	f.t.Helper()
	f.withAgents() // the flags service
	require.NoError(f.t, f.s.flags.Set(context.Background(), flagApps, f.user.TenantID, true, "test"))
	require.NoError(f.t, f.s.flags.Refresh(context.Background()))
	tier := "free"
	if paying {
		tier = "standard"
	}
	_, err := f.db.Exec(`UPDATE tenant_quotas SET tier = $2 WHERE tenant_id = $1`, f.user.TenantID, tier)
	require.NoError(f.t, err)
	_, err = f.db.Exec(`UPDATE users SET email_verified = $2 WHERE id = $1`, f.user.ID, verified)
	require.NoError(f.t, err)
	cleanupDescendants(f)
	return f
}

func (f *platformFixture) makeApp(label string) (string, string) {
	f.t.Helper()
	code, out := f.call(http.MethodPost, "/api/v1/manage/apps", f.jwt, map[string]any{"label": label})
	require.Equal(f.t, http.StatusCreated, code, out)
	key := out["management_key"].(map[string]any)["secret"].(string)
	return out["id"].(string), key
}

func TestApps_BuilderCreatesWithoutAGrant(t *testing.T) {
	f := setupPlatformFixture(t).withManagement().withApps(false, true)
	code, out := f.call(http.MethodPost, "/api/v1/manage/apps", f.jwt, map[string]any{"label": "my app"})
	assert.Equal(t, http.StatusForbidden, code, out)
	assert.Equal(t, "apps_need_verified_account", errCode(out))
	f.withApps(true, false)
	code, out = f.call(http.MethodPost, "/api/v1/manage/apps", f.jwt, map[string]any{"label": "my app"})
	assert.Equal(t, http.StatusForbidden, code, out)
	assert.Equal(t, "apps_need_paying_account", errCode(out))
	f.withApps(true, true)

	var platform bool
	require.NoError(t, f.db.QueryRow(`SELECT platform FROM tenants WHERE id = $1`, f.user.TenantID).Scan(&platform))
	require.False(t, platform, "no platform grant")
	app, key := f.makeApp("my app")
	assert.True(t, strings.HasPrefix(key, auth.ManagementKeyLivePrefix))
	var kind, parent string
	require.NoError(t, f.db.QueryRow(`SELECT child_kind, parent_tenant_id FROM tenants WHERE id = $1`, app).Scan(&kind, &parent))
	assert.Equal(t, "app", kind)
	assert.Equal(t, f.user.TenantID, parent)

	code, out = f.call(http.MethodGet, "/api/v1/manage/apps", f.jwt, nil)
	require.Equal(t, http.StatusOK, code, out)
	require.Len(t, out["data"], 1)

	// The builder itself still has no platform routes.
	code, _ = f.call(http.MethodGet, "/api/v1/platform/tenants", f.jwt, nil)
	assert.Equal(t, http.StatusNotFound, code)

	// Up to the limit.
	prev := auth.MaxSelfServeApps
	auth.MaxSelfServeApps = 2
	t.Cleanup(func() { auth.MaxSelfServeApps = prev })
	f.makeApp("second")
	code, out = f.call(http.MethodPost, "/api/v1/manage/apps", f.jwt, map[string]any{"label": "third"})
	assert.Equal(t, http.StatusConflict, code, out)
	assert.Equal(t, "app_limit_reached", errCode(out))

	// A developer may not make one.
	_, dev := f.addMember(auth.RoleDeveloper)
	code, out = f.call(http.MethodPost, "/api/v1/manage/apps", dev, map[string]any{"label": "dev's"})
	assert.Equal(t, http.StatusForbidden, code, out)
}

func TestApps_EndUsersAtDepthTwoDrawFromTheBuildersPool(t *testing.T) {
	f := setupPlatformFixture(t).withManagement().withApps(true, true)
	app, key := f.makeApp("photos")

	code, out := f.call(http.MethodPost, "/api/v1/platform/tenants", key, map[string]any{"label": "end user 1"})
	require.Equal(t, http.StatusCreated, code, out)
	user := out["id"].(string)
	assert.Equal(t, app, out["parent_id"], "the app's own end user")
	assert.Equal(t, "managed", out["kind"])
	code, out = f.call(http.MethodPost, "/api/v1/platform/tenants/"+user+"/keys", key, map[string]any{"name": "end user key"})
	require.Equal(t, http.StatusCreated, code, out)
	code, out = f.call(http.MethodPost, "/api/v1/platform/tenants", key, map[string]any{"label": "deeper", "kind": "app"})
	assert.Equal(t, http.StatusBadRequest, code, out)
	assert.Equal(t, "child_of_child", errCode(out), "depth 2 is the deepest")

	var root string
	require.NoError(t, f.db.QueryRow(`SELECT pool_root_id FROM tenants WHERE id = $1`, user).Scan(&root))
	assert.Equal(t, f.user.TenantID, root, "the builder's pool")
	_, err := f.db.Exec(`UPDATE tenant_quotas SET storage_limit_bytes = 1000 WHERE tenant_id = $1`, f.user.TenantID)
	require.NoError(t, err)
	qm := usage.NewQuotaManager(f.db)
	res, err := qm.ReserveOnFloor(context.Background(), user, usage.FloorStandard, 700)
	require.NoError(t, err)
	assert.True(t, res.OK)
	res, err = qm.ReserveOnFloor(context.Background(), f.user.TenantID, usage.FloorStandard, 400)
	require.NoError(t, err)
	assert.False(t, res.OK, "the end user's 700 count in the builder's 1000")
}

func TestApps_DeleteCascades(t *testing.T) {
	f := setupPlatformFixture(t).withManagement().withApps(true, true)
	app, key := f.makeApp("to delete")
	code, out := f.call(http.MethodPost, "/api/v1/platform/tenants", key, map[string]any{"label": "its user"})
	require.Equal(t, http.StatusCreated, code, out)
	user := out["id"].(string)

	code, out = f.call(http.MethodDelete, "/api/v1/manage/apps/"+app, f.jwt, nil)
	require.Equal(t, http.StatusOK, code, out)
	for _, id := range []string{app, user} {
		var due bool
		require.NoError(t, f.db.QueryRow(`SELECT deletion_scheduled_at IS NOT NULL FROM tenants WHERE id = $1`, id).Scan(&due))
		assert.True(t, due, "%s is scheduled with its app", id)
	}
	var revoked bool
	require.NoError(t, f.db.QueryRow(`SELECT bool_and(revoked_at IS NOT NULL) FROM management_keys WHERE tenant_id = $1`, app).Scan(&revoked))
	assert.True(t, revoked, "the app's backend key stops at once")
	code, _ = f.call(http.MethodGet, "/api/v1/platform/tenants", key, nil)
	assert.Equal(t, http.StatusUnauthorized, code)
	code, out = f.call(http.MethodDelete, "/api/v1/manage/apps/"+user, f.jwt, nil)
	assert.Equal(t, http.StatusNotFound, code, "an end user is no app of the builder's: %v", out)
}

func TestApps_NeverPublicUntilVerified(t *testing.T) {
	f := setupPlatformFixture(t).withManagement().withApps(true, true)
	app, _ := f.makeApp("site")
	_, err := f.db.Exec(`INSERT INTO buckets (tenant_id, name) VALUES ($1, 'site-assets')`, app)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = f.db.Exec(`DELETE FROM buckets WHERE tenant_id = $1`, app) })
	public := modes.VisibilityPublic

	_, err = f.db.Exec(`UPDATE users SET email_verified = FALSE WHERE id = $1`, f.user.ID)
	require.NoError(t, err)
	_, err = modes.SetBucket(context.Background(), f.db, app, "site-assets", modes.Change{Visibility: &public})
	assert.True(t, errors.Is(err, modes.ErrUnverifiedNeverPublic), "%v", err)

	_, err = f.db.Exec(`UPDATE users SET email_verified = TRUE WHERE id = $1`, f.user.ID)
	require.NoError(t, err)
	mode, err := modes.SetBucket(context.Background(), f.db, app, "site-assets", modes.Change{Visibility: &public})
	require.NoError(t, err)
	assert.Equal(t, modes.Public, mode)
}
