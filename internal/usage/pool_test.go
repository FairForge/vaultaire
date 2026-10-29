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
