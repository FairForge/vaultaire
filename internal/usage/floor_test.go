// internal/usage/floor_test.go
//
// Per-floor quotas (dashboard plan, Phase 1). The house sells whole TB per
// floor: downstairs = standard, attic = vault. Tenants who bought a house
// carry one tenant_floor_quotas row per floor; everyone else keeps the
// single total quota and is enforced exactly as before.
package usage

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFloorOf(t *testing.T) {
	cases := map[string]string{
		"GLACIER":            FloorVault,
		"DEEP_ARCHIVE":       FloorVault,
		"":                   FloorStandard,
		"STANDARD":           FloorStandard,
		"PUBLIC":             FloorStandard,
		"RESILIENT":          FloorStandard,
		"REDUCED_REDUNDANCY": FloorStandard,
	}
	for class, want := range cases {
		assert.Equal(t, want, FloorOf(class), "class %q", class)
	}
}

func insertHead(t *testing.T, tenantID, key, floor string, size int64) {
	t.Helper()
	db := setupTestDB(t)
	_, err := db.Exec(`INSERT INTO object_head_cache (tenant_id, bucket, object_key, size_bytes, etag, floor)
		VALUES ($1, 'b', $2, $3, 'etag', $4)`, tenantID, key, size, floor)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM object_head_cache WHERE tenant_id = $1`, tenantID)
	})
}

func TestSetHouse_CreatesFloorRowsFromExistingObjects(t *testing.T) {
	db := setupTestDB(t)
	m := NewQuotaManager(db)
	ctx := context.Background()
	tenantID := newTestTenant(t, db, m, "house-set", "free", FreeTierLimits.StorageBytes)
	insertHead(t, tenantID, "down", FloorStandard, 50)
	insertHead(t, tenantID, "up", FloorVault, 100)
	require.NoError(t, m.ReleaseQuota(ctx, tenantID, -150)) // total already accounted

	require.NoError(t, m.SetHouse(ctx, tenantID, House{StdBytes: 6 * TB, VaultBytes: 1 * TB, PinHotBytes: 1 * TB}))

	floors, err := m.GetFloors(ctx, tenantID)
	require.NoError(t, err)
	require.Len(t, floors, 2)
	assert.Equal(t, FloorQuota{Floor: FloorStandard, LimitBytes: 6 * TB, UsedBytes: 50}, floors[0])
	assert.Equal(t, FloorQuota{Floor: FloorVault, LimitBytes: 1 * TB, UsedBytes: 100}, floors[1])

	used, limit, err := m.GetUsage(ctx, tenantID)
	require.NoError(t, err)
	assert.Equal(t, int64(7*TB), limit, "total limit is the sum of the floors")
	assert.Equal(t, int64(150), used, "total used is untouched")
	tier, err := m.GetTier(ctx, tenantID)
	require.NoError(t, err)
	assert.Equal(t, "standard", tier)

	var pin int64
	require.NoError(t, db.QueryRow(`SELECT pin_hot_bytes FROM tenant_quotas WHERE tenant_id = $1`, tenantID).Scan(&pin))
	assert.Equal(t, int64(1*TB), pin)

	// Resizing keeps the used bytes and rewrites the limits; attic-only
	// houses are a real product (decision 2026-09-27) and report tier vault.
	require.NoError(t, m.SetHouse(ctx, tenantID, House{StdBytes: 0, VaultBytes: 2 * TB}))
	floors, err = m.GetFloors(ctx, tenantID)
	require.NoError(t, err)
	assert.Equal(t, FloorQuota{Floor: FloorStandard, LimitBytes: 0, UsedBytes: 50}, floors[0])
	assert.Equal(t, FloorQuota{Floor: FloorVault, LimitBytes: 2 * TB, UsedBytes: 100}, floors[1])
	_, limit, _ = m.GetUsage(ctx, tenantID)
	assert.Equal(t, int64(2*TB), limit)
	tier, _ = m.GetTier(ctx, tenantID)
	assert.Equal(t, "vault", tier)
	require.NoError(t, db.QueryRow(`SELECT pin_hot_bytes FROM tenant_quotas WHERE tenant_id = $1`, tenantID).Scan(&pin))
	assert.Equal(t, int64(0), pin)
}

func TestClearHouse_BackToFreeTierWithoutTouchingData(t *testing.T) {
	db := setupTestDB(t)
	m := NewQuotaManager(db)
	ctx := context.Background()
	tenantID := newTestTenant(t, db, m, "house-clear", "free", FreeTierLimits.StorageBytes)
	require.NoError(t, m.SetHouse(ctx, tenantID, House{StdBytes: 1 * TB, PinHotBytes: 1 * TB}))
	ok, err := m.CheckAndReserveFloor(ctx, tenantID, FloorStandard, 10*GB)
	require.NoError(t, err)
	require.True(t, ok)

	require.NoError(t, m.ClearHouse(ctx, tenantID))

	floors, err := m.GetFloors(ctx, tenantID)
	require.NoError(t, err)
	assert.Empty(t, floors, "no house = no floor rows")
	used, limit, err := m.GetUsage(ctx, tenantID)
	require.NoError(t, err)
	assert.Equal(t, FreeTierLimits.StorageBytes, limit)
	assert.Equal(t, int64(10*GB), used, "billing never deletes: the bytes stay accounted")
	tier, _ := m.GetTier(ctx, tenantID)
	assert.Equal(t, "free", tier)
	var pin int64
	require.NoError(t, db.QueryRow(`SELECT pin_hot_bytes FROM tenant_quotas WHERE tenant_id = $1`, tenantID).Scan(&pin))
	assert.Equal(t, int64(0), pin)

	// Clearing twice is harmless (webhook retries).
	require.NoError(t, m.ClearHouse(ctx, tenantID))
}

func TestCheckAndReserveFloor_EnforcesEachFloor(t *testing.T) {
	db := setupTestDB(t)
	m := NewQuotaManager(db)
	ctx := context.Background()
	tenantID := newTestTenant(t, db, m, "house-floor", "free", FreeTierLimits.StorageBytes)
	require.NoError(t, m.SetHouse(ctx, tenantID, House{StdBytes: 1 * TB, VaultBytes: 1 * TB}))

	ok, err := m.CheckAndReserveFloor(ctx, tenantID, FloorStandard, 600*GB)
	require.NoError(t, err)
	assert.True(t, ok)

	// The total (2 TB) has room, the floor (1 TB) does not.
	ok, err = m.CheckAndReserveFloor(ctx, tenantID, FloorStandard, 600*GB)
	require.NoError(t, err)
	assert.False(t, ok, "downstairs is full")

	ok, err = m.CheckAndReserveFloor(ctx, tenantID, FloorVault, 600*GB)
	require.NoError(t, err)
	assert.True(t, ok, "the attic still has room")

	floors, err := m.GetFloors(ctx, tenantID)
	require.NoError(t, err)
	assert.Equal(t, int64(600*GB), floors[0].UsedBytes)
	assert.Equal(t, int64(600*GB), floors[1].UsedBytes)
	used, _, _ := m.GetUsage(ctx, tenantID)
	assert.Equal(t, int64(1200*GB), used, "total used == sum of the floors")

	var events int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM quota_usage_events WHERE tenant_id = $1 AND operation = 'RESERVE'`, tenantID).Scan(&events))
	assert.Equal(t, 2, events)
}

func TestCheckAndReserveFloor_HouseWithoutAtticRefusesVault(t *testing.T) {
	db := setupTestDB(t)
	m := NewQuotaManager(db)
	ctx := context.Background()
	tenantID := newTestTenant(t, db, m, "house-noattic", "free", FreeTierLimits.StorageBytes)
	require.NoError(t, m.SetHouse(ctx, tenantID, House{StdBytes: 1 * TB}))

	ok, err := m.CheckAndReserveFloor(ctx, tenantID, FloorVault, 1)
	require.NoError(t, err)
	assert.False(t, ok, "no attic was bought")
	used, _, _ := m.GetUsage(ctx, tenantID)
	assert.Equal(t, int64(0), used)
}

func TestCheckAndReserveFloor_NoHouseIsTotalOnly(t *testing.T) {
	db := setupTestDB(t)
	m := NewQuotaManager(db)
	ctx := context.Background()
	tenantID := newTestTenant(t, db, m, "legacy-floor", "starter", 1*GB)

	ok, err := m.CheckAndReserveFloor(ctx, tenantID, FloorVault, 500*MB)
	require.NoError(t, err)
	assert.True(t, ok, "a legacy tenant may use any floor")
	ok, err = m.CheckAndReserveFloor(ctx, tenantID, FloorStandard, 600*MB)
	require.NoError(t, err)
	assert.False(t, ok, "the single total quota still applies")

	floors, err := m.GetFloors(ctx, tenantID)
	require.NoError(t, err)
	assert.Empty(t, floors)
	used, _, _ := m.GetUsage(ctx, tenantID)
	assert.Equal(t, int64(500*MB), used)
}

func TestReleaseFloor_ClampsAndForceAccounts(t *testing.T) {
	db := setupTestDB(t)
	m := NewQuotaManager(db)
	ctx := context.Background()
	tenantID := newTestTenant(t, db, m, "house-release", "free", FreeTierLimits.StorageBytes)
	require.NoError(t, m.SetHouse(ctx, tenantID, House{StdBytes: 1 * TB, VaultBytes: 1 * TB}))
	ok, err := m.CheckAndReserveFloor(ctx, tenantID, FloorVault, 100)
	require.NoError(t, err)
	require.True(t, ok)

	require.NoError(t, m.ReleaseFloor(ctx, tenantID, FloorVault, 40))
	floors, _ := m.GetFloors(ctx, tenantID)
	assert.Equal(t, int64(60), floors[1].UsedBytes)
	used, _, _ := m.GetUsage(ctx, tenantID)
	assert.Equal(t, int64(60), used)

	// Over-release clamps at zero on both ledgers.
	require.NoError(t, m.ReleaseFloor(ctx, tenantID, FloorVault, 1000))
	floors, _ = m.GetFloors(ctx, tenantID)
	assert.Equal(t, int64(0), floors[1].UsedBytes)
	used, _, _ = m.GetUsage(ctx, tenantID)
	assert.Equal(t, int64(0), used)

	// A negative delta force-accounts bytes that are already durable, even
	// past the floor's limit.
	require.NoError(t, m.ReleaseFloor(ctx, tenantID, FloorStandard, -(2*TB)))
	floors, _ = m.GetFloors(ctx, tenantID)
	assert.Equal(t, int64(2*TB), floors[0].UsedBytes)
	used, _, _ = m.GetUsage(ctx, tenantID)
	assert.Equal(t, int64(2*TB), used)

	// A legacy tenant's release only touches the total.
	legacy := newTestTenant(t, db, m, "legacy-release", "starter", 1*GB)
	require.NoError(t, m.ReleaseFloor(ctx, legacy, FloorStandard, -7))
	used, _, _ = m.GetUsage(ctx, legacy)
	assert.Equal(t, int64(7), used)
}

func TestReconcileStorageUsage_PerFloor(t *testing.T) {
	db := setupTestDB(t)
	m := NewQuotaManager(db)
	ctx := context.Background()
	tenantID := newTestTenant(t, db, m, "house-reconcile", "free", FreeTierLimits.StorageBytes)
	require.NoError(t, m.SetHouse(ctx, tenantID, House{StdBytes: 1 * TB, VaultBytes: 1 * TB}))
	insertHead(t, tenantID, "a", FloorStandard, 10)
	insertHead(t, tenantID, "b", FloorStandard, 20)
	insertHead(t, tenantID, "c", FloorVault, 300)
	// Drift the ledgers on purpose.
	_, err := db.Exec(`UPDATE tenant_floor_quotas SET storage_used_bytes = 999 WHERE tenant_id = $1`, tenantID)
	require.NoError(t, err)

	_, err = m.ReconcileStorageUsage(ctx)
	require.NoError(t, err)

	floors, err := m.GetFloors(ctx, tenantID)
	require.NoError(t, err)
	assert.Equal(t, int64(30), floors[0].UsedBytes)
	assert.Equal(t, int64(300), floors[1].UsedBytes)
	used, _, _ := m.GetUsage(ctx, tenantID)
	assert.Equal(t, int64(330), used)
}

// House.Total and the byte helpers are what the billing page prints.
func TestHouse_Helpers(t *testing.T) {
	h := House{StdBytes: 6 * TB, VaultBytes: 1 * TB}
	assert.Equal(t, 6, h.StdTB())
	assert.Equal(t, 1, h.VaultTB())
	assert.Equal(t, int64(7*TB), h.TotalBytes())
	assert.False(t, h.Empty())
	assert.True(t, House{}.Empty())
	assert.Equal(t, fmt.Sprintf("%d", 3), fmt.Sprintf("%d", HouseFromTB(3, 0, 0).StdTB()))
	_ = time.Now
}
