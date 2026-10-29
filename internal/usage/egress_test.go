// internal/usage/egress_test.go
//
// The egress allowance (WP-R10-9): one definition, derived at read time from
// what the tenant holds, read by the throttle, the alerter, the overview, the
// admin tenant page and /api/v1/user/usage.
package usage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEgressRatios_ComeFromThePriceFile(t *testing.T) {
	// Arrange: the price file as it is on disk, parsed independently.
	raw, err := os.ReadFile("../api/landing/prices.json")
	require.NoError(t, err)
	var file struct {
		Egress struct {
			Std   float64 `json:"standard_free_ratio"`
			Vault float64 `json:"vault_restore_free_ratio"`
		} `json:"egress"`
	}
	require.NoError(t, json.Unmarshal(raw, &file))

	// Act
	std, vault := EgressRatios()

	// Assert: the code sells what the site prints, and the site prints what
	// was decided (2× downstairs + 1× attic since 2026-10-27 — the edge plane took transit off SLC, iDrive's free pool is 3× stored, Lyve egress is $0; it was 0.5× from SMART_TIER_DESIGN 2026-09-21).
	assert.Equal(t, file.Egress.Std, std)
	assert.Equal(t, file.Egress.Vault, vault)
	assert.Equal(t, 2.0, std)
	assert.Equal(t, 1.0, vault)
	assert.Equal(t, int64(float64(TB)*file.Egress.Std)+int64(float64(TB)*file.Egress.Vault),
		PlanEgressAllowance(TB, TB))
}

func TestEgressAllowance_Precedence(t *testing.T) {
	db := setupTestDB(t)
	m := NewQuotaManager(db)
	ctx := context.Background()

	t.Run("house 6+1 is twice the downstairs plus the whole attic", func(t *testing.T) {
		tenantID := newTestTenant(t, db, m, "egress-house", "free", FreeTierLimits.StorageBytes)
		require.NoError(t, m.SetHouse(ctx, tenantID, HouseFromTB(6, 1, 0)))

		a, err := m.EgressAllowance(ctx, tenantID)

		require.NoError(t, err)
		assert.Equal(t, 13*TB, a.Bytes)
		assert.Equal(t, 13*TB, a.PlanBytes)
		assert.Equal(t, int64(0), a.OverrideBytes)
		assert.Equal(t, EgressSourceHouse, a.Source)
	})

	t.Run("attic-only house is one times the attic", func(t *testing.T) {
		tenantID := newTestTenant(t, db, m, "egress-attic", "free", FreeTierLimits.StorageBytes)
		require.NoError(t, m.SetHouse(ctx, tenantID, HouseFromTB(0, 2, 0)))

		a, err := m.EgressAllowance(ctx, tenantID)

		require.NoError(t, err)
		assert.Equal(t, 2*TB, a.Bytes)
		assert.Equal(t, EgressSourceHouse, a.Source)
	})

	t.Run("no house is twice the single quota", func(t *testing.T) {
		tenantID := newTestTenant(t, db, m, "egress-legacy", "vault18", 18*TB)

		a, err := m.EgressAllowance(ctx, tenantID)

		require.NoError(t, err)
		assert.Equal(t, 36*TB, a.Bytes)
		assert.Equal(t, EgressSourceQuota, a.Source)
	})

	t.Run("free tier is 10 GiB", func(t *testing.T) {
		tenantID := newTestTenant(t, db, m, "egress-free", "free", FreeTierLimits.StorageBytes)

		a, err := m.EgressAllowance(ctx, tenantID)

		require.NoError(t, err)
		assert.Equal(t, 10*GB, a.Bytes)
		assert.Equal(t, EgressSourceQuota, a.Source)
	})

	t.Run("admin override wins and the plan number stays readable", func(t *testing.T) {
		tenantID := newTestTenant(t, db, m, "egress-override", "free", FreeTierLimits.StorageBytes)
		require.NoError(t, m.SetHouse(ctx, tenantID, HouseFromTB(6, 1, 0)))
		setOverride(t, db, tenantID, 7*GB)

		a, err := m.EgressAllowance(ctx, tenantID)

		require.NoError(t, err)
		assert.Equal(t, 7*GB, a.Bytes)
		assert.Equal(t, 7*GB, a.OverrideBytes)
		assert.Equal(t, 13*TB, a.PlanBytes)
		assert.Equal(t, EgressSourceOverride, a.Source)
	})

	t.Run("override of zero or NULL means the plan's allowance", func(t *testing.T) {
		tenantID := newTestTenant(t, db, m, "egress-zero", "free", FreeTierLimits.StorageBytes)
		setOverride(t, db, tenantID, 0)

		a, err := m.EgressAllowance(ctx, tenantID)

		require.NoError(t, err)
		assert.Equal(t, 10*GB, a.Bytes)
		assert.Equal(t, EgressSourceQuota, a.Source)
	})

	t.Run("unknown tenant is an error, not an allowance", func(t *testing.T) {
		_, err := m.EgressAllowance(ctx, "egress-no-such-tenant")

		require.Error(t, err)
		assert.True(t, errors.Is(err, sql.ErrNoRows))
	})
}

// The hard requirement of WP-R10-9: an admin override survives a webhook
// replay of SetHouse and a ClearHouse. The allowance is derived at read time
// and SetHouse/ClearHouse never write bandwidth_limit_bytes.
func TestEgressAllowance_OverrideSurvivesSetHouseReplayAndClearHouse(t *testing.T) {
	db := setupTestDB(t)
	m := NewQuotaManager(db)
	ctx := context.Background()
	tenantID := newTestTenant(t, db, m, "egress-survive", "free", FreeTierLimits.StorageBytes)
	require.NoError(t, m.SetHouse(ctx, tenantID, HouseFromTB(2, 0, 0)))
	setOverride(t, db, tenantID, 3*TB)

	// Act 1: the webhook replays the same house, then a resize arrives.
	require.NoError(t, m.SetHouse(ctx, tenantID, HouseFromTB(2, 0, 0)))
	require.NoError(t, m.SetHouse(ctx, tenantID, HouseFromTB(4, 1, 0)))

	// Assert 1: the override still wins; the plan number followed the resize.
	a, err := m.EgressAllowance(ctx, tenantID)
	require.NoError(t, err)
	assert.Equal(t, 3*TB, a.Bytes)
	assert.Equal(t, 9*TB, a.PlanBytes)
	assert.Equal(t, EgressSourceOverride, a.Source)

	// Act 2: the subscription ends.
	require.NoError(t, m.ClearHouse(ctx, tenantID))

	// Assert 2: still the override; under it the plan is the free tier's.
	a, err = m.EgressAllowance(ctx, tenantID)
	require.NoError(t, err)
	assert.Equal(t, 3*TB, a.Bytes)
	assert.Equal(t, 10*GB, a.PlanBytes)

	// Act 3: the admin clears the override.
	setOverride(t, db, tenantID, 0)

	// Assert 3: back on the plan, nothing left behind.
	a, err = m.EgressAllowance(ctx, tenantID)
	require.NoError(t, err)
	assert.Equal(t, 10*GB, a.Bytes)
	assert.Equal(t, EgressSourceQuota, a.Source)
}

func TestEgressMonth_IsTheUTCCalendarMonth(t *testing.T) {
	// Arrange: 18:30 on 31 October in Denver is already 1 November in UTC.
	denver := time.FixedZone("MDT", -6*3600)
	evening := time.Date(2026, 10, 31, 18, 30, 0, 0, denver)

	// Act
	start := EgressMonthStart(evening)
	reset := EgressResetAt(evening)

	// Assert
	assert.Equal(t, time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), start)
	assert.Equal(t, time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC), reset)
	assert.Equal(t, time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC),
		EgressResetAt(time.Date(2026, 12, 31, 23, 59, 59, 0, time.UTC)))
}

func TestMonthEgressBytes_EgressOnlyThisUTCMonth(t *testing.T) {
	db := setupTestDB(t)
	m := NewQuotaManager(db)
	ctx := context.Background()
	tenantID := newTestTenant(t, db, m, "egress-month", "free", FreeTierLimits.StorageBytes)
	insertTenantRow(t, db, tenantID)
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	for _, row := range []struct {
		day             string
		ingress, egress int64
	}{
		{"2026-09-30", 1 << 30, 1 << 30}, // last month
		{"2026-10-01", 500, 100},
		{"2026-10-14", 700, 23},
	} {
		_, err := db.Exec(`INSERT INTO bandwidth_usage_daily (tenant_id, date, ingress_bytes, egress_bytes, requests_count)
			VALUES ($1, $2, $3, $4, 1)`, tenantID, row.day, row.ingress, row.egress)
		require.NoError(t, err)
	}

	// Act
	used, err := MonthEgressBytes(ctx, db, tenantID, EgressMonthStart(now))

	// Assert: ingress never counts (R13-16), last month never counts.
	require.NoError(t, err)
	assert.Equal(t, int64(123), used)
}

func TestEgressThrottle_Rate(t *testing.T) {
	c := DefaultEgressThrottle()

	// The pace that spends one more allowance in a 30-day month.
	assert.Equal(t, 4*TB/EgressMonthSeconds, c.Rate(4*TB))
	// Never below the floor: the free tier's 2.5 GiB would be ~1 KiB/s.
	assert.Equal(t, int64(65536), c.Rate(5*GB/2))
	assert.Equal(t, int64(65536), c.Rate(0))
	// The factor scales the pace; the floor still holds.
	c.Factor = 2
	assert.Equal(t, 2*(4*TB/EgressMonthSeconds), c.Rate(4*TB))
	assert.Equal(t, 16, c.MaxStreams)
}

func setOverride(t *testing.T, db *sql.DB, tenantID string, bytes int64) {
	t.Helper()
	_, err := db.Exec(`UPDATE tenant_quotas SET bandwidth_limit_bytes = $1 WHERE tenant_id = $2`, bytes, tenantID)
	require.NoError(t, err)
}

// insertTenantRow adds the tenants row bandwidth_usage_daily's FK needs.
func insertTenantRow(t *testing.T, db *sql.DB, tenantID string) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO tenants (id, name, email, access_key, secret_key)
		VALUES ($1, 'egress-test', $2, $3, $4)`,
		tenantID, tenantID+"@test.local", "AK"+tenantID, "SK"+tenantID)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM tenants WHERE id = $1`, tenantID) })
}

// RP82-2 (Stage 1a Prompt 52, Part 0): a child's own quota row has limit 0
// (the pool decides). Its allowance is never read from that row — that
// would be 0, "nothing to download, never over", an unpaced child. It is
// the pool root's, and its month is the pool's.
func TestEgress_ZeroAllowanceIsNotUnlimitedForAChild(t *testing.T) {
	db, owner, child := poolDB(t)
	ctx := context.Background()
	setOverride(t, db, owner, 10<<20)
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM bandwidth_usage_daily WHERE tenant_id IN ($1, $2)`, owner, child) })

	a, err := NewQuotaManager(db).EgressAllowance(ctx, child)
	require.NoError(t, err)
	assert.Equal(t, int64(10<<20), a.Bytes, "the child's allowance is its pool root's, never its own row's 0")
	assert.Equal(t, owner, a.Root)

	now := time.Now()
	day := EgressMonthStart(now).Format("2006-01-02")
	for _, id := range []string{owner, child} {
		_, err := db.Exec(`INSERT INTO bandwidth_usage_daily (tenant_id, date, egress_bytes, requests_count) VALUES ($1, $2, $3, 1)`, id, day, 6<<20)
		require.NoError(t, err)
	}
	st, err := EgressStatusFromDB(ctx, db, child, now)
	require.NoError(t, err)
	assert.Equal(t, int64(12<<20), st.UsedBytes, "the month is the pool's")
	assert.True(t, st.Over, "a child past its pool's allowance is over")

	root, err := PoolRoot(ctx, db, child)
	require.NoError(t, err)
	assert.Equal(t, owner, root)
	root, err = PoolRoot(ctx, db, owner)
	require.NoError(t, err)
	assert.Equal(t, owner, root)
	root, err = PoolRoot(ctx, db, "no-such-tenant-"+owner)
	require.NoError(t, err)
	assert.Equal(t, "no-such-tenant-"+owner, root, "a tenant with no tenants row is its own pool")
}
