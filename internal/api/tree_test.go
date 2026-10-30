package api

// WP-ACCT-1 (Stage 1a Prompt 53, PR A): the account tree. A platform's
// child hangs off it by an edge (child_kind): managed, or app — an app may
// have managed children of its own, depth 2, the only depth beyond 1. Every
// tenant of the tree names the pool's root (pool_root_id), so a reservation
// at any depth is the root's pool; a child's own caps sit inside it.

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FairForge/vaultaire/internal/usage"
)

// cleanupDescendants removes the platform's grandchildren (registered after
// the fixture, so it runs before cleanupPlatform removes the children).
func cleanupDescendants(f *platformFixture) {
	f.t.Cleanup(func() {
		for _, q := range []string{
			`DELETE FROM api_keys WHERE tenant_id IN (SELECT g.id FROM tenants g JOIN tenants c ON c.id = g.parent_tenant_id WHERE c.parent_tenant_id = $1)`,
			`DELETE FROM tenant_floor_quotas WHERE tenant_id IN (SELECT g.id FROM tenants g JOIN tenants c ON c.id = g.parent_tenant_id WHERE c.parent_tenant_id = $1)`,
			`DELETE FROM tenant_quotas WHERE tenant_id IN (SELECT g.id FROM tenants g JOIN tenants c ON c.id = g.parent_tenant_id WHERE c.parent_tenant_id = $1)`,
			`DELETE FROM audit_logs WHERE tenant_id IN (SELECT g.id FROM tenants g JOIN tenants c ON c.id = g.parent_tenant_id WHERE c.parent_tenant_id = $1)`,
			`DELETE FROM tenants WHERE parent_tenant_id IN (SELECT c.id FROM tenants c WHERE c.parent_tenant_id = $1)`,
		} {
			_, _ = f.db.Exec(q, f.user.TenantID)
		}
	})
}

func TestTree_PoolAtDepthTwo(t *testing.T) {
	f := setupPlatformFixture(t).withManagement()
	f.grant()
	cleanupDescendants(f)
	ctx := context.Background()
	create := func(body map[string]any) (int, map[string]any) {
		return f.call(http.MethodPost, "/api/v1/platform/tenants", f.jwt, body)
	}

	code, out := create(map[string]any{"label": "an app", "kind": "app"})
	require.Equal(t, http.StatusCreated, code, out)
	app := out["id"].(string)
	assert.Equal(t, "app", out["kind"])
	code, out = create(map[string]any{"label": "a customer"})
	require.Equal(t, http.StatusCreated, code, out)
	managed := out["id"].(string)
	assert.Equal(t, "managed", out["kind"], "managed is the default edge")

	code, out = create(map[string]any{"label": "the app's user", "parent_id": app})
	require.Equal(t, http.StatusCreated, code, out)
	user := out["id"].(string)
	assert.Equal(t, app, out["parent_id"])
	var root, kind string
	require.NoError(t, f.db.QueryRow(`SELECT pool_root_id, child_kind FROM tenants WHERE id = $1`, user).Scan(&root, &kind))
	assert.Equal(t, f.user.TenantID, root, "depth 2 names the root")
	assert.Equal(t, "managed", kind)

	code, out = create(map[string]any{"label": "no", "parent_id": managed})
	assert.Equal(t, http.StatusBadRequest, code, out)
	assert.Equal(t, "child_of_child", errCode(out), "a managed child has no children")
	code, out = create(map[string]any{"label": "no", "parent_id": app, "kind": "app"})
	assert.Equal(t, http.StatusBadRequest, code, out)
	assert.Equal(t, "child_of_child", errCode(out), "depth 2 is the deepest")
	code, out = create(map[string]any{"label": "no", "kind": "sponsored"})
	assert.Equal(t, http.StatusBadRequest, code, out)
	assert.Equal(t, "invalid_kind", errCode(out), "a sponsored edge is made by a family invitation, never here")

	// The platform reaches its grandchild: read, keys.
	code, out = f.call(http.MethodGet, "/api/v1/platform/tenants/"+user, f.jwt, nil)
	assert.Equal(t, http.StatusOK, code, out)
	code, out = f.call(http.MethodPost, "/api/v1/platform/tenants/"+user+"/keys", f.jwt, map[string]any{"name": "app user key"})
	assert.Equal(t, http.StatusCreated, code, out)

	// One pool at any depth: the grandchild's bytes are the root's pool's.
	_, err := f.db.Exec(`UPDATE tenant_quotas SET storage_limit_bytes = 1000 WHERE tenant_id = $1`, f.user.TenantID)
	require.NoError(t, err)
	qm := usage.NewQuotaManager(f.db)
	res, err := qm.ReserveOnFloor(ctx, user, usage.FloorStandard, 600)
	require.NoError(t, err)
	assert.True(t, res.OK)
	res, err = qm.ReserveOnFloor(ctx, managed, usage.FloorStandard, 500)
	require.NoError(t, err)
	assert.False(t, res.OK, "600 at depth 2 + 500 > the root's 1000")
	var counter int64
	require.NoError(t, f.db.QueryRow(`SELECT pool_used_bytes FROM tenant_quotas WHERE tenant_id = $1`, f.user.TenantID).Scan(&counter))
	assert.Equal(t, int64(600), counter)
}

// A platform's erasure reaches depth 2: the app's children first, then the
// app, then the platform.
func TestTree_AnAppsChildrenAreErasedWithIt(t *testing.T) {
	f := setupDeletionFixture(t)
	f.seed(time.Now().Add(-time.Hour))
	f.exec(`UPDATE tenants SET platform = TRUE WHERE id = $1`, f.tenantID)
	app, _ := f.seedChild(nil)
	f.exec(`UPDATE tenants SET child_kind = 'app' WHERE id = $1`, app)
	grand := "tenant-gc-" + uuid.NewString()[:12]
	f.exec(`INSERT INTO tenants (id, name, email, parent_tenant_id, label) VALUES ($1, 'g', $2, $3, 'g')`, grand, "platform+"+grand+"@platform.invalid", app)
	f.exec(`INSERT INTO tenant_quotas (tenant_id, storage_limit_bytes) VALUES ($1, 0)`, grand)
	gb := "grand-bkt-" + grand[10:16]
	f.exec(`INSERT INTO buckets (tenant_id, name) VALUES ($1, $2)`, grand, gb)
	blob := filepath.Join(f.primary, grand+"_"+gb, "o.txt")
	require.NoError(t, os.MkdirAll(filepath.Dir(blob), 0o750))
	require.NoError(t, os.WriteFile(blob, []byte("grandchild bytes"), 0o600))
	f.exec(`INSERT INTO object_head_cache (tenant_id, bucket, object_key, size_bytes, etag, backend_name) VALUES ($1, $2, 'o.txt', 16, 'e', 'local')`, grand, gb)
	t.Cleanup(func() {
		for _, q := range []string{`DELETE FROM object_head_cache WHERE tenant_id = $1`, `DELETE FROM buckets WHERE tenant_id = $1`,
			`DELETE FROM tenant_quotas WHERE tenant_id = $1`, `DELETE FROM audit_logs WHERE tenant_id = $1`, `DELETE FROM tenants WHERE id = $1`} {
			_, _ = f.db.Exec(q, grand)
		}
	})

	res, err := f.runner.RunOnce(context.Background())
	require.NoError(t, err, "%+v", res)
	require.Equal(t, 1, res.Erased, "%+v", res)
	assert.Equal(t, outcomeErased, res.Tenants[0].Outcome, "%+v", res.Tenants[0])
	for _, id := range []string{grand, app, f.tenantID} {
		assert.Zero(t, f.count(`SELECT COUNT(*) FROM tenants WHERE id = $1`, id), id)
	}
	assert.False(t, fileExists(blob), "the grandchild's bytes are gone")
}

// Caps inside the pool: a child's own storage cap (> 0) is min(cap, pool),
// and its egress cap paces it while the pool still has room.
func TestTree_ChildCapInsideThePool(t *testing.T) {
	t.Run("storage", func(t *testing.T) {
		f := setupPlatformFixture(t).withManagement()
		f.grant()
		_, err := f.db.Exec(`UPDATE tenant_quotas SET storage_limit_bytes = $2 WHERE tenant_id = $1`, f.user.TenantID, 1<<20)
		require.NoError(t, err)
		code, out := f.call(http.MethodPost, "/api/v1/platform/tenants", f.jwt, map[string]any{"label": "capped", "storage_cap_bytes": 10 << 10})
		require.Equal(t, http.StatusCreated, code, out)
		child := out["id"].(string)
		caps, _ := out["caps"].(map[string]any)
		assert.EqualValues(t, 10<<10, caps["storage_cap_bytes"])

		qm := usage.NewQuotaManager(f.db)
		ctx := context.Background()
		res, err := qm.ReserveOnFloor(ctx, child, usage.FloorStandard, 10<<10)
		require.NoError(t, err)
		assert.True(t, res.OK, "up to its cap")
		res, err = qm.ReserveOnFloor(ctx, child, usage.FloorStandard, 1)
		require.NoError(t, err)
		assert.False(t, res.OK, "one byte past its cap, with the pool far from full")
		res, err = qm.ReserveOnFloor(ctx, f.user.TenantID, usage.FloorStandard, 1)
		require.NoError(t, err)
		assert.True(t, res.OK, "the pool still has room for the others")

		code, out = f.call(http.MethodPatch, "/api/v1/platform/tenants/"+child, f.jwt, map[string]any{"storage_cap_bytes": 0})
		require.Equal(t, http.StatusOK, code, out)
		res, err = qm.ReserveOnFloor(ctx, child, usage.FloorStandard, 1)
		require.NoError(t, err)
		assert.True(t, res.OK, "cap 0: the pool decides")
		code, out = f.call(http.MethodPatch, "/api/v1/platform/tenants/"+child, f.jwt, map[string]any{"egress_cap_bytes": -1})
		assert.Equal(t, http.StatusBadRequest, code, out)
	})

	t.Run("egress", func(t *testing.T) {
		// A platform with a 10 MiB allowance; child A capped at 2 MiB of
		// egress, child B with none.
		f := setupEgressFixture(t, 20*mib, testThrottle(), nil)
		_, err := f.db.Exec(`UPDATE tenants SET platform = TRUE WHERE id = $1`, f.tenantID)
		require.NoError(t, err)
		a, b := f.addChild("A"), f.addChild("B")
		_, err = f.db.Exec(`UPDATE tenant_quotas SET egress_cap_bytes = $2 WHERE tenant_id = $1`, a, 2*mib)
		require.NoError(t, err)
		f.putAs(a, "a.bin", 3*mib)
		f.putAs(b, "b.bin", 3*mib)
		f.enforced.Store(true)

		f.getAs(a, "a.bin") // crosses A's cap (3 MiB > 2 MiB)
		capped := f.getAs(a, "a.bin")
		free := f.getAs(b, "b.bin")

		require.Equal(t, http.StatusOK, capped.status)
		assert.GreaterOrEqual(t, capped.elapsed, 400*time.Millisecond, "A past its own cap is paced: (3 − 1 burst) MiB at 4 MiB/s")
		require.Equal(t, http.StatusOK, free.status)
		assert.Less(t, free.elapsed, egressFastBound, "B is not: the pool (6 of 10 MiB) has room")
	})
}
