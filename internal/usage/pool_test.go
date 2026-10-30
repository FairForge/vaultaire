package usage

// WP-PLAT-1 PR B: the pool. A platform's child (tenants.parent_tenant_id)
// has a quota row of limit 0 that counts its bytes; every reservation and
// every guarded ledger change checks the POOL — the owner's limit against
// the sum of the owner's and its children's counters.

import (
	"context"
	"database/sql"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FairForge/vaultaire/internal/testutil"
)

func poolDB(t *testing.T) (*sql.DB, string, string) {
	t.Helper()
	db, err := sql.Open("postgres", testutil.DSN())
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	owner, child := "pool-owner-"+uuid.NewString()[:8], "pool-child-"+uuid.NewString()[:8]
	for _, q := range []string{
		`INSERT INTO tenants (id, name, email, platform) VALUES ('` + owner + `', 'o', '` + owner + `@x.invalid', TRUE)`,
		`INSERT INTO tenants (id, name, email, parent_tenant_id) VALUES ('` + child + `', 'c', '` + child + `@x.invalid', '` + owner + `')`,
		`INSERT INTO tenant_quotas (tenant_id, storage_limit_bytes) VALUES ('` + owner + `', 1000)`,
		`INSERT INTO tenant_quotas (tenant_id, storage_limit_bytes) VALUES ('` + child + `', 0)`,
	} {
		_, err := db.Exec(q)
		require.NoError(t, err, q)
	}
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM tenant_quotas WHERE tenant_id IN ($1, $2)`, owner, child)
		_, _ = db.Exec(`DELETE FROM tenants WHERE id = $1`, child)
		_, _ = db.Exec(`DELETE FROM tenants WHERE id = $1`, owner)
	})
	return db, owner, child
}

func TestPool_ReserveChecksThePoolNotTheChildsRow(t *testing.T) {
	db, owner, child := poolDB(t)
	m := NewQuotaManager(db)
	ctx := context.Background()
	res, err := m.ReserveOnFloor(ctx, child, FloorStandard, 600)
	require.NoError(t, err)
	assert.True(t, res.OK, "the child's own limit 0 does not decide")
	res, err = m.ReserveOnFloor(ctx, owner, FloorStandard, 600)
	require.NoError(t, err)
	assert.False(t, res.OK, "600 + 600 > the owner's 1000")
	res, err = m.ReserveOnFloor(ctx, owner, FloorStandard, 400)
	require.NoError(t, err)
	assert.True(t, res.OK)
	used, _, err := m.GetUsage(ctx, child)
	require.NoError(t, err)
	assert.Equal(t, int64(600), used)
	pool, err := m.PoolBilledBytes(ctx, owner)
	require.NoError(t, err)
	assert.Zero(t, pool, "billed = rows, none written yet")
}

// A guarded ledger change (a net reservation's settle) of a child is
// checked against the pool, never the child's limit of 0.
func TestPool_GuardChecksThePool(t *testing.T) {
	db, owner, child := poolDB(t)
	ctx := context.Background()
	apply := func(tenant string, n int64) error {
		tx, err := db.BeginTx(ctx, nil)
		require.NoError(t, err)
		defer func() { _ = tx.Rollback() }()
		if err := ApplyLedgerTx(ctx, tx, tenant, LedgerChange{Total: n, Floors: map[string]int64{FloorStandard: n}, Guard: FloorStandard}); err != nil {
			return err
		}
		return tx.Commit()
	}
	require.NoError(t, apply(child, 700), "within the pool")
	require.ErrorIs(t, apply(child, 400), ErrOverQuota, "past the pool")
	require.ErrorIs(t, apply(owner, 400), ErrOverQuota, "the owner's own change too")
	require.NoError(t, apply(owner, 300))
}

// RP82-1 (Stage 1a Prompt 52, Part 0): floors are pooled like the total. A
// child of an owner with a house draws from the OWNER's floors: its
// counting floor rows (limit 0) hold its bytes, and the check sums the
// pool's rows for the floor against the owner's floor limit.
func TestPool_ChildFloorsDrawFromTheOwnersFloors(t *testing.T) {
	db, owner, child := poolDB(t)
	m := NewQuotaManager(db)
	ctx := context.Background()
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM tenant_floor_quotas WHERE tenant_id IN ($1, $2)`, owner, child) })
	require.NoError(t, m.SetHouse(ctx, owner, House{StdBytes: 10 << 10, VaultBytes: 100 << 10}))

	res, err := m.ReserveOnFloor(ctx, child, FloorStandard, 12<<10)
	require.NoError(t, err)
	assert.False(t, res.OK, "12 KiB on Standard is past the owner's 10 KiB Standard floor")
	res, err = m.ReserveOnFloor(ctx, child, FloorVault, 12<<10)
	require.NoError(t, err)
	assert.True(t, res.OK, "the Vault floor has room")
	assert.True(t, res.Floor, "the child's own floor row was charged")

	res, err = m.ReserveOnFloor(ctx, owner, FloorStandard, 6<<10)
	require.NoError(t, err)
	require.True(t, res.OK)
	res, err = m.ReserveOnFloor(ctx, child, FloorStandard, 6<<10)
	require.NoError(t, err)
	assert.False(t, res.OK, "the owner's own 6 KiB counts against the same 10 KiB")
	res, err = m.ReserveOnFloor(ctx, child, FloorStandard, 4<<10)
	require.NoError(t, err)
	assert.True(t, res.OK)

	floors, err := m.GetFloors(ctx, child)
	require.NoError(t, err)
	got := map[string]FloorQuota{}
	for _, f := range floors {
		got[f.Floor] = f
	}
	assert.Equal(t, FloorQuota{Floor: FloorStandard, LimitBytes: 0, UsedBytes: 4 << 10}, got[FloorStandard])
	assert.Equal(t, FloorQuota{Floor: FloorVault, LimitBytes: 0, UsedBytes: 12 << 10}, got[FloorVault])

	// A guarded ledger change (a settle) of the child is checked the same way.
	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	err = ApplyLedgerTx(ctx, tx, child, LedgerChange{Total: 1 << 10, Floors: map[string]int64{FloorStandard: 1 << 10}, Guard: FloorStandard})
	_ = tx.Rollback()
	require.ErrorIs(t, err, ErrOverQuota, "6 + 4 + 1 KiB > the owner's 10 KiB Standard floor")
}

// SetHouse on an owner gives each existing child counting floor rows,
// seeded from the child's billed bytes, so its objects count on the right
// floor of the pool from that moment.
func TestPool_SetHouseGivesChildrenCountingFloors(t *testing.T) {
	db, owner, child := poolDB(t)
	m := NewQuotaManager(db)
	ctx := context.Background()
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM tenant_floor_quotas WHERE tenant_id IN ($1, $2)`, owner, child) })
	insertHead(t, child, "kept", FloorStandard, 5<<10)

	require.NoError(t, m.SetHouse(ctx, owner, House{StdBytes: 10 << 10, VaultBytes: 100 << 10}))

	floors, err := m.GetFloors(ctx, child)
	require.NoError(t, err)
	require.Len(t, floors, 2)
	assert.Equal(t, FloorQuota{Floor: FloorStandard, LimitBytes: 0, UsedBytes: 5 << 10}, floors[0])
	assert.Equal(t, FloorQuota{Floor: FloorVault, LimitBytes: 0, UsedBytes: 0}, floors[1])
	res, err := m.ReserveOnFloor(ctx, owner, FloorStandard, 6<<10)
	require.NoError(t, err)
	assert.False(t, res.OK, "the child's 5 KiB already sit on the owner's 10 KiB Standard floor")

	// A replay keeps what the child's rows hold; the reconcile rewrites the
	// child's floor rows from its head rows as it does the owner's.
	require.NoError(t, m.SetHouse(ctx, owner, House{StdBytes: 10 << 10, VaultBytes: 100 << 10}))
	_, err = db.Exec(`UPDATE tenant_floor_quotas SET storage_used_bytes = 1 WHERE tenant_id = $1 AND floor = 'standard'`, child)
	require.NoError(t, err)
	require.NoError(t, m.ReconcileTenantStorageUsage(ctx, child))
	_, floorsBilled, err := m.BilledBytes(ctx, child)
	require.NoError(t, err)
	floors, err = m.GetFloors(ctx, child)
	require.NoError(t, err)
	assert.Equal(t, floorsBilled[FloorStandard], floors[0].UsedBytes)
	assert.Equal(t, int64(5<<10), floors[0].UsedBytes)
}

// RP82-3 (Stage 1a Prompt 53, Part 0b): the pool's used bytes are one
// counter on the root's row (tenant_quotas.pool_used_bytes), moved in the
// statement that moves the member's — the check reads it, not a SUM over
// every child. After every write, delete and reconcile it equals the sum;
// a drift is reported (PoolDrift → vaultaire_pool_used_drift_bytes), never
// rewritten except by the operator's explicit reseed.
func TestPool_UsedCounterEqualsTheSum(t *testing.T) {
	db, owner, child := poolDB(t)
	child2 := "pool-child2-" + uuid.NewString()[:8]
	for _, q := range []string{
		`INSERT INTO tenants (id, name, email, parent_tenant_id) VALUES ('` + child2 + `', 'c2', '` + child2 + `@x.invalid', '` + owner + `')`,
		`INSERT INTO tenant_quotas (tenant_id, storage_limit_bytes) VALUES ('` + child2 + `', 0)`,
	} {
		_, err := db.Exec(q)
		require.NoError(t, err, q)
	}
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM tenant_quotas WHERE tenant_id = $1`, child2)
		_, _ = db.Exec(`DELETE FROM tenants WHERE id = $1`, child2)
	})
	m := NewQuotaManager(db)
	ctx := context.Background()
	counter := func() int64 {
		var n int64
		require.NoError(t, db.QueryRow(`SELECT pool_used_bytes FROM tenant_quotas WHERE tenant_id = $1`, owner).Scan(&n))
		return n
	}
	sum := func() int64 {
		var n int64
		require.NoError(t, db.QueryRow(`SELECT COALESCE(SUM(q.storage_used_bytes), 0) FROM tenant_quotas q JOIN tenants t ON t.id = q.tenant_id
			WHERE t.pool_root_id = $1`, owner).Scan(&n))
		return n
	}
	apply := func(tenant string, c LedgerChange) {
		tx, err := db.BeginTx(ctx, nil)
		require.NoError(t, err)
		defer func() { _ = tx.Rollback() }()
		require.NoError(t, ApplyLedgerTx(ctx, tx, tenant, c))
		require.NoError(t, tx.Commit())
	}
	step := func(what string) {
		t.Helper()
		assert.Equal(t, sum(), counter(), "after %s", what)
	}

	res, err := m.ReserveOnFloor(ctx, child, FloorStandard, 300)
	require.NoError(t, err)
	require.True(t, res.OK)
	step("child 1 reserves 300")
	res, err = m.ReserveOnFloor(ctx, child2, FloorStandard, 200)
	require.NoError(t, err)
	require.True(t, res.OK)
	step("child 2 reserves 200")
	apply(child, LedgerChange{Total: -100, Floors: map[string]int64{FloorStandard: -100}})
	step("child 1 deletes 100")
	require.NoError(t, m.ReleaseFloor(ctx, child2, FloorStandard, 50))
	step("child 2 releases 50")
	apply(owner, LedgerChange{Total: 50, Floors: map[string]int64{FloorStandard: 50}, Guard: FloorStandard})
	step("the owner writes 50")
	apply(child2, LedgerChange{Total: -1000})
	step("child 2 deletes past zero (clamped)")
	require.NoError(t, m.ReconcileTenantStorageUsage(ctx, child))
	step("child 1 reconciled to its rows")
	assert.Equal(t, int64(50), counter(), "the owner's 50 is all the pool holds")

	// The check reads the counter: a counter that drifted decides, and the
	// drift is reported, not repaired.
	_, err = db.Exec(`UPDATE tenant_quotas SET pool_used_bytes = pool_used_bytes + 900 WHERE tenant_id = $1`, owner)
	require.NoError(t, err)
	res, err = m.ReserveOnFloor(ctx, child, FloorStandard, 51)
	require.NoError(t, err)
	assert.False(t, res.OK, "950 + 51 > 1000 by the counter")
	drift, err := PoolDrift(ctx, db)
	require.NoError(t, err)
	assert.Equal(t, int64(900), drift[owner])
	require.NoError(t, m.ReconcileTenantStorageUsage(ctx, child2))
	drift, err = PoolDrift(ctx, db)
	require.NoError(t, err)
	assert.Equal(t, int64(900), drift[owner], "a reconcile never rewrites the drift away")
	n, err := m.ReseedPoolUsed(ctx, owner)
	require.NoError(t, err)
	assert.Equal(t, int64(900), n, "the operator's reseed answers the drift it removed")
	step("reseeded")
}
