// internal/usage/pool.go
//
// The quota pool (WP-PLAT-1, Stage 1a Prompt 51; plan § WP-STRIPE). A
// platform's child tenant (tenants.parent_tenant_id, migration 117) has a
// tenant_quotas row of limit 0 that counts ITS bytes; what it may store is
// decided by its POOL: the owner's (the parent's) storage_limit_bytes
// against the sum of the owner's and every child's storage_used_bytes. The
// parent's own writes draw from the same pool. One level only; a sandbox
// (tenants.sandbox_of) is never in a pool — it is no child, and its row is
// its own local-disk cap.
//
// The walk is one statement that resolves the owner and locks its row
// (FOR UPDATE), then one that sums the pool: the sum is read AFTER the
// lock, so it sees every reservation committed by a transaction that held
// the lock before (under READ COMMITTED a statement's snapshot is taken at
// its start — a sum inside the locking statement would keep the snapshot of
// before the wait). Every pool reservation takes the owner's lock, so the
// pool's reservations are serialised on it.
package usage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// poolOf is the pool a tenant reserves in.
type poolOf struct {
	owner  string
	limit  int64
	used   int64 // the owner's row alone (a tenant in no pool: its own)
	pooled bool  // a child, or a platform: the pool's sum decides
}

// lockPoolOwner resolves tenantID's pool and locks the owner's quota row.
// A tenant with no tenants row (a quota row alone: tests, tools) is its own
// pool.
func lockPoolOwner(ctx context.Context, tx *sql.Tx, tenantID string) (poolOf, error) {
	p := poolOf{}
	err := tx.QueryRowContext(ctx, `
		SELECT o.tenant_id, o.storage_limit_bytes, o.storage_used_bytes, (t.parent_tenant_id IS NOT NULL OR t.platform)
		  FROM tenants t JOIN tenant_quotas o ON o.tenant_id = COALESCE(t.parent_tenant_id, t.id)
		 WHERE t.id = $1
		   FOR UPDATE OF o`, tenantID).Scan(&p.owner, &p.limit, &p.used, &p.pooled)
	if errors.Is(err, sql.ErrNoRows) {
		err = tx.QueryRowContext(ctx, `
			SELECT tenant_id, storage_limit_bytes, storage_used_bytes FROM tenant_quotas
			 WHERE tenant_id = $1 FOR UPDATE`, tenantID).Scan(&p.owner, &p.limit, &p.used)
	}
	if err != nil {
		return poolOf{}, fmt.Errorf("checking quota for tenant %s: %w", tenantID, err)
	}
	return p, nil
}

// poolUsed sums the pool of owner: its own counter and every child's.
func poolUsed(ctx context.Context, tx *sql.Tx, owner string) (int64, error) {
	var used int64
	if err := tx.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(q.storage_used_bytes), 0)
		  FROM tenant_quotas q
		 WHERE q.tenant_id = $1
		    OR q.tenant_id IN (SELECT c.id FROM tenants c WHERE c.parent_tenant_id = $1)`, owner).Scan(&used); err != nil {
		return 0, fmt.Errorf("summing the pool of %s: %w", owner, err)
	}
	return used, nil
}

// lockPool is the reservation's view: the pool the tenant reserves in, with
// the owner's row locked — and, for a child, the child's own row (the row
// the reservation then charges), after the owner's — and what the pool
// holds (p.used: the pool's sum for a pooled tenant, its own row else).
func lockPool(ctx context.Context, tx *sql.Tx, tenantID string) (poolOf, error) {
	p, err := lockPoolOwner(ctx, tx, tenantID)
	if err != nil {
		return poolOf{}, err
	}
	if !p.pooled {
		return p, nil
	}
	if p.owner != tenantID {
		if _, err := tx.ExecContext(ctx, `SELECT 1 FROM tenant_quotas WHERE tenant_id = $1 FOR UPDATE`, tenantID); err != nil {
			return poolOf{}, fmt.Errorf("checking quota for tenant %s: %w", tenantID, err)
		}
	}
	p.used, err = poolUsed(ctx, tx, p.owner)
	return p, err
}

// The pool's floors (RP82-1, Stage 1a Prompt 52 Part 0). Floors are pooled
// exactly like the total: what a pool may hold on a floor is the OWNER's
// tenant_floor_quotas limit, against the sum of the owner's and every
// child's row for that floor. A child's floor rows are counting rows
// (limit 0), written with the child when its owner has a house
// (auth.CreateChildTenant) and for existing children by SetHouse. Lock
// order: owner total → child total (lockPool) → owner floor
// (lockPoolFloor) → child floor; every pooled floor check holds the
// owner's total, so the sum read after the owner floor's lock is the
// pool's.

// poolHasFloors reports whether the pool's owner bought a house.
func poolHasFloors(ctx context.Context, tx *sql.Tx, owner string) (bool, error) {
	var n int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM tenant_floor_quotas WHERE tenant_id = $1`, owner).Scan(&n); err != nil {
		return false, fmt.Errorf("checking floors of the pool of %s: %w", owner, err)
	}
	return n > 0, nil
}

// lockPoolFloor locks the owner's row for floor and returns its limit;
// ok=false when the owner has no row for it.
func lockPoolFloor(ctx context.Context, tx *sql.Tx, owner, floor string) (limit int64, ok bool, err error) {
	err = tx.QueryRowContext(ctx, `
		SELECT storage_limit_bytes FROM tenant_floor_quotas
		 WHERE tenant_id = $1 AND floor = $2 FOR UPDATE`, owner, floor).Scan(&limit)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return 0, false, nil
	case err != nil:
		return 0, false, fmt.Errorf("checking floor %s of the pool of %s: %w", floor, owner, err)
	}
	return limit, true, nil
}

// poolFloorUsed sums the pool of owner on floor: its own row and every
// child's.
func poolFloorUsed(ctx context.Context, tx *sql.Tx, owner, floor string) (int64, error) {
	var used int64
	if err := tx.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(f.storage_used_bytes), 0)
		  FROM tenant_floor_quotas f
		 WHERE f.floor = $2
		   AND (f.tenant_id = $1 OR f.tenant_id IN (SELECT c.id FROM tenants c WHERE c.parent_tenant_id = $1))`,
		owner, floor).Scan(&used); err != nil {
		return 0, fmt.Errorf("summing floor %s of the pool of %s: %w", floor, owner, err)
	}
	return used, nil
}

// ensureChildFloorRow writes a child's counting row for floor when it has
// none (a child made before its owner bought the house and before SetHouse
// reached it), seeded from its billed bytes on that floor — the seeding
// SetHouse does. Called by a reservation, before its write: the head row
// it will flip does not exist yet, so nothing is counted twice.
func ensureChildFloorRow(ctx context.Context, tx *sql.Tx, child, floor string) error {
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO tenant_floor_quotas (tenant_id, floor, storage_limit_bytes, storage_used_bytes)
		 VALUES ($1, $2, 0, `+billedBytesOfParamsFloor+`)
		 ON CONFLICT (tenant_id, floor) DO NOTHING`, child, floor); err != nil {
		return fmt.Errorf("floor %s row of child %s: %w", floor, child, err)
	}
	return nil
}

// RowQuerier is what PoolRoot reads through: a *sql.DB or a *sql.Tx.
type RowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// PoolRoot is the tenant whose allowance and month a tenant's downloads
// spend (D-STRIPE-31): a child's platform, else the tenant itself — the
// owner COALESCE(parent_tenant_id, id), the same rule lockPoolOwner locks
// by. A sandbox is its own root (sandbox_of is no pool edge). A tenant with
// no tenants row (a quota row alone: tests, tools) is its own pool. The ONE
// resolver of the egress side; WP-ACCT-1 swaps its body for pool_root_id.
func PoolRoot(ctx context.Context, q RowQuerier, tenantID string) (string, error) {
	var root string
	err := q.QueryRowContext(ctx, `SELECT COALESCE(parent_tenant_id, id) FROM tenants WHERE id = $1`, tenantID).Scan(&root)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return tenantID, nil
	case err != nil:
		return "", fmt.Errorf("pool root of %s: %w", tenantID, err)
	}
	return root, nil
}
