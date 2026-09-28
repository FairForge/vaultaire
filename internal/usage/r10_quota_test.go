package usage

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ReleaseQuota runs on every delete and overwrite; its "negative delta adds
// unconditionally" contract is what settlePutQuota relies on to account
// bytes that are already durably stored (WP-R0-8 / R0-14).
func TestReleaseQuota_Contract(t *testing.T) {
	db := setupTestDB(t)
	m := NewQuotaManager(db)
	ctx := context.Background()
	tenantID := newTestTenant(t, db, m, "release", "free", 10*MB)

	ok, err := m.CheckAndReserve(ctx, tenantID, 6*MB)
	require.NoError(t, err)
	require.True(t, ok)

	// Decrement.
	require.NoError(t, m.ReleaseQuota(ctx, tenantID, 2*MB))
	used, _, err := m.GetUsage(ctx, tenantID)
	require.NoError(t, err)
	assert.Equal(t, 4*MB, used)

	// Clamped at zero: releasing more than is used never goes negative.
	require.NoError(t, m.ReleaseQuota(ctx, tenantID, 9*MB))
	used, _, _ = m.GetUsage(ctx, tenantID)
	assert.Equal(t, int64(0), used)

	// A negative delta adds, and does so past the limit (the bytes exist).
	require.NoError(t, m.ReleaseQuota(ctx, tenantID, -12*MB))
	used, limit, _ := m.GetUsage(ctx, tenantID)
	assert.Equal(t, 12*MB, used)
	assert.Greater(t, used, limit, "force-accounting ignores the limit")

	// An unknown tenant is a no-op, not an error.
	require.NoError(t, m.ReleaseQuota(ctx, "no-such-tenant", 1*MB))
}

// ReleaseFloor moves the total and the floor together and honours the same
// contract on both ledgers (066).
func TestReleaseFloor_Contract(t *testing.T) {
	db := setupTestDB(t)
	m := NewQuotaManager(db)
	ctx := context.Background()
	tenantID := newTestTenant(t, db, m, "release-floor", "free", FreeTierLimits.StorageBytes)
	require.NoError(t, m.SetHouse(ctx, tenantID, House{StdBytes: 10 * MB, VaultBytes: 10 * MB}))
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM tenant_floor_quotas WHERE tenant_id = $1`, tenantID) })

	ok, err := m.CheckAndReserveFloor(ctx, tenantID, FloorVault, 6*MB)
	require.NoError(t, err)
	require.True(t, ok)

	require.NoError(t, m.ReleaseFloor(ctx, tenantID, FloorVault, 8*MB))
	used, _, _ := m.GetUsage(ctx, tenantID)
	assert.Equal(t, int64(0), used, "total clamped")
	floors, err := m.GetFloors(ctx, tenantID)
	require.NoError(t, err)
	assert.Equal(t, int64(0), floors[1].UsedBytes, "attic clamped")

	require.NoError(t, m.ReleaseFloor(ctx, tenantID, FloorStandard, -3*MB))
	used, _, _ = m.GetUsage(ctx, tenantID)
	floors, _ = m.GetFloors(ctx, tenantID)
	assert.Equal(t, 3*MB, used)
	assert.Equal(t, 3*MB, floors[0].UsedBytes, "downstairs force-accounted")
}

// Two writers that each fit but not together: the tenant row lock in
// CheckAndReserveFloor serialises them, so exactly as many succeed as the
// floor holds and both ledgers end equal to what was granted (R10 area 2).
func TestCheckAndReserveFloor_ConcurrentWritersSerialize(t *testing.T) {
	db := setupTestDB(t)
	m := NewQuotaManager(db)
	ctx := context.Background()
	tenantID := newTestTenant(t, db, m, "race", "free", FreeTierLimits.StorageBytes)
	require.NoError(t, m.SetHouse(ctx, tenantID, House{StdBytes: 16 * MB, VaultBytes: 100 * MB}))
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM tenant_floor_quotas WHERE tenant_id = $1`, tenantID) })

	const writers = 32
	var wg sync.WaitGroup
	results := make(chan bool, writers)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, err := m.CheckAndReserveFloor(ctx, tenantID, FloorStandard, 1*MB)
			assert.NoError(t, err)
			results <- ok
		}()
	}
	wg.Wait()
	close(results)
	granted := 0
	for ok := range results {
		if ok {
			granted++
		}
	}
	assert.Equal(t, 16, granted, "the downstairs holds exactly 16 MiB")
	used, _, err := m.GetUsage(ctx, tenantID)
	require.NoError(t, err)
	assert.Equal(t, 16*MB, used)
	floors, err := m.GetFloors(ctx, tenantID)
	require.NoError(t, err)
	assert.Equal(t, 16*MB, floors[0].UsedBytes)
	assert.Equal(t, int64(0), floors[1].UsedBytes)
}
