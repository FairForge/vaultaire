package api

// WP-PLAT-1 PR B: a platform's children draw from its POOL — the parent's
// limit, the sum of the parent's and every child's counter — in the one
// reservation every write path takes (usage.ReserveTx), the edge admit's
// included. A sandbox is never in the pool.

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FairForge/vaultaire/internal/auth"
	"github.com/FairForge/vaultaire/internal/usage"
)

// poolChild is a child tenant of the fixture's tenant with one scoped key
// and one bucket.
type poolChild struct {
	id, ak, sk, bucket string
}

func (f *requestLogFixture) makePlatform(limit int64) {
	f.t.Helper()
	f.exec(`UPDATE tenants SET platform = TRUE WHERE id = $1`, f.tenantID)
	f.exec(`UPDATE tenant_quotas SET storage_limit_bytes = $2, storage_used_bytes = 0 WHERE tenant_id = $1`, f.tenantID, limit)
	f.t.Cleanup(func() { cleanupPlatform(f.db, f.tenantID) })
}

func (f *requestLogFixture) addChild(name string) poolChild {
	f.t.Helper()
	id := uuid.NewString()
	c := poolChild{id: "tenant-pool-" + id[:12], ak: "VKCH" + strings.ToUpper(id[:12]), sk: "SKCH" + id, bucket: "pool-" + name + "-" + id[:6]}
	f.exec(`INSERT INTO tenants (id, name, email, parent_tenant_id, label) VALUES ($1, $2, $3, $4, $5)`, c.id, name, "platform+"+c.id+"@platform.invalid", f.tenantID, name)
	f.exec(`INSERT INTO tenant_quotas (tenant_id, storage_limit_bytes) VALUES ($1, 0)`, c.id)
	f.exec(`INSERT INTO api_keys (id, user_id, tenant_id, name, key_id, secret_hash, secret_key) VALUES ($1, $2, $3, 'child', $4, 'h', $5)`,
		uuid.NewString(), f.userID, c.id, c.ak, c.sk)
	f.exec(`INSERT INTO buckets (tenant_id, name, visibility) VALUES ($1, $2, 'private')`, c.id, c.bucket)
	return c
}

func (f *requestLogFixture) usedOf(id string) int64 {
	f.t.Helper()
	var n int64
	require.NoError(f.t, f.db.QueryRow(`SELECT storage_used_bytes FROM tenant_quotas WHERE tenant_id = $1`, id).Scan(&n))
	return n
}

// Parent limit 10 KiB: child A's 6 KiB fits; child B's 6 KiB and the
// parent's own 6 KiB do not — the pool is spent; the edge admit's
// reservation is the same rule. Each child's bytes are its own counter's.
func TestPlatform_ChildQuotaDrawsFromTheParentsPool(t *testing.T) {
	f := setupRequestLogFixture(t)
	f.makePlatform(10 << 10)
	a, b := f.addChild("a"), f.addChild("b")
	six := []byte(strings.Repeat("6", 6<<10))

	w := f.do(http.MethodPut, "/"+a.bucket+"/six.bin", six, a.ak, a.sk, nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, int64(6<<10), f.usedOf(a.id), "the child's own counter")
	assert.Zero(t, f.usedOf(f.tenantID), "never the parent's row")

	w = f.do(http.MethodPut, "/"+b.bucket+"/six.bin", six, b.ak, b.sk, nil)
	assert.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "QuotaExceeded")
	w = f.do(http.MethodPut, "/"+f.bucket+"/six.bin", six, f.ak, f.sk, nil)
	assert.Equal(t, http.StatusForbidden, w.Code, "the parent's own write draws from the same pool")
	assert.Contains(t, w.Body.String(), "QuotaExceeded")

	// The edge admit reserves through the same rule.
	require.NoError(t, f.fl.Set(context.Background(), flagEdgeIngest, b.id, true, "test"))
	require.NoError(t, f.fl.Refresh(context.Background()))
	t.Cleanup(func() { _, _ = f.db.Exec(`DELETE FROM feature_flags WHERE tenant_id = $1`, b.id) })
	oak, osk := f.ak, f.sk
	f.ak, f.sk = b.ak, b.sk
	adm := f.admit(f.signed("/"+b.bucket+"/edge.bin", nil, six, nil, ""))
	f.ak, f.sk = oak, osk
	require.NotNil(t, adm.Refusal, "refused at the admit: %+v", adm)
	assert.Equal(t, http.StatusForbidden, adm.Refusal.Status)
	assert.Contains(t, adm.Refusal.Body, "QuotaExceeded")

	// 4 KiB is left in the pool: a child's 4 KiB fits.
	four := []byte(strings.Repeat("4", 4<<10))
	w = f.do(http.MethodPut, "/"+b.bucket+"/four.bin", four, b.ak, b.sk, nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, int64(4<<10), f.usedOf(b.id))

	// The billed view: each child under its own id, the pool under the owner.
	qm := usage.NewQuotaManager(f.db)
	billedA, _, err := qm.BilledBytes(context.Background(), a.id)
	require.NoError(t, err)
	assert.Equal(t, int64(6<<10), billedA)
	pool, err := qm.PoolBilledBytes(context.Background(), f.tenantID)
	require.NoError(t, err)
	assert.Equal(t, int64(10<<10), pool)

	// A delete frees the pool for everyone.
	w = f.do(http.MethodDelete, "/"+a.bucket+"/six.bin", nil, a.ak, a.sk, nil)
	require.Equal(t, http.StatusNoContent, w.Code)
	w = f.do(http.MethodPut, "/"+f.bucket+"/six.bin", six, f.ak, f.sk, nil)
	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
}

// A platform's sandbox is never in its pool: the sandbox's bytes count on
// its own row only (its local-disk cap), and the pool's sum leaves them out.
func TestPlatform_ASandboxIsNeverInThePool(t *testing.T) {
	f := setupRequestLogFixture(t)
	f.makePlatform(10 << 10)
	sb := "sandbox-pool-" + uuid.NewString()[:8]
	f.exec(`INSERT INTO tenants (id, name, email, sandbox_of) VALUES ($1, 'test data', $2, $3)`, sb, "sandbox+"+sb+"@sandbox.invalid", f.tenantID)
	f.exec(`INSERT INTO tenant_quotas (tenant_id, storage_used_bytes) VALUES ($1, $2)`, sb, 9<<10)
	f.t.Cleanup(func() {
		_, _ = f.db.Exec(`DELETE FROM tenant_quotas WHERE tenant_id = $1`, sb)
		_, _ = f.db.Exec(`DELETE FROM tenants WHERE id = $1`, sb)
	})
	six := []byte(strings.Repeat("6", 6<<10))
	w := f.do(http.MethodPut, "/"+f.bucket+"/six.bin", six, f.ak, f.sk, nil)
	require.Equal(t, http.StatusOK, w.Code, "the sandbox's 9 KiB are not the pool's: %s", w.Body.String())
	pool, err := usage.NewQuotaManager(f.db).PoolBilledBytes(context.Background(), f.tenantID)
	require.NoError(t, err)
	assert.Equal(t, int64(6<<10), pool)
	res, err := usage.NewQuotaManager(f.db).ReserveOnFloor(context.Background(), sb, usage.FloorStandard, 1<<10)
	require.NoError(t, err)
	assert.True(t, res.OK, "the sandbox reserves against its own cap, not the pool")
}

// RP82-1 (Stage 1a Prompt 52, Part 0): a platform's children draw from its
// FLOORS, not only its total. Owner floors 10 KiB Standard + 100 KiB Vault:
// a child made after the house (auth.CreateChildTenant writes its counting
// floor rows) is refused a 12 KiB Standard PUT and stores a 12 KiB Vault
// PUT; the owner's own Standard bytes count against the same 10 KiB.
func TestPool_ChildFloorsDrawFromTheOwnersFloors(t *testing.T) {
	f := setupRequestLogFixture(t)
	f.makePlatform(10 << 10)
	ctx := context.Background()
	require.NoError(t, usage.NewQuotaManager(f.db).SetHouse(ctx, f.tenantID, usage.House{StdBytes: 10 << 10, VaultBytes: 100 << 10}))
	t.Cleanup(func() { _, _ = f.db.Exec(`DELETE FROM tenant_floor_quotas WHERE tenant_id = $1`, f.tenantID) })
	child, err := auth.CreateChildTenant(ctx, f.db, f.tenantID, "floors", "")
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = f.db.Exec(`DELETE FROM tenant_floor_quotas WHERE tenant_id = $1`, child.ID) })
	var rows, limits int64
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(storage_limit_bytes), 0) FROM tenant_floor_quotas WHERE tenant_id = $1`, child.ID).Scan(&rows, &limits))
	assert.Equal(t, int64(2), rows, "a counting row per floor the owner has")
	assert.Zero(t, limits, "a child's floor rows have limit 0: the owner's floors decide")
	id := uuid.NewString()
	c := poolChild{id: child.ID, ak: "VKCF" + strings.ToUpper(id[:12]), sk: "SKCF" + id, bucket: "floors-" + id[:6]}
	f.exec(`INSERT INTO api_keys (id, user_id, tenant_id, name, key_id, secret_hash, secret_key) VALUES ($1, $2, $3, 'child', $4, 'h', $5)`,
		uuid.NewString(), f.userID, c.id, c.ak, c.sk)
	f.exec(`INSERT INTO buckets (tenant_id, name, visibility) VALUES ($1, $2, 'private')`, c.id, c.bucket)
	twelve := []byte(strings.Repeat("c", 12<<10))
	six := []byte(strings.Repeat("6", 6<<10))

	w := f.do(http.MethodPut, "/"+c.bucket+"/std.bin", twelve, c.ak, c.sk, nil)
	assert.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "QuotaExceeded")
	w = f.do(http.MethodPut, "/"+c.bucket+"/vault.bin", twelve, c.ak, c.sk, map[string]string{"x-amz-storage-class": "GLACIER"})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	w = f.do(http.MethodPut, "/"+f.bucket+"/six.bin", six, f.ak, f.sk, nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	w = f.do(http.MethodPut, "/"+c.bucket+"/six.bin", six, c.ak, c.sk, nil)
	assert.Equal(t, http.StatusForbidden, w.Code, "the owner's 6 KiB and the child's 6 KiB are past the 10 KiB Standard floor")
	assert.Contains(t, w.Body.String(), "QuotaExceeded")
}
