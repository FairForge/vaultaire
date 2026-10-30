// internal/usage/pool.go
//
// The quota pool (WP-PLAT-1, Stage 1a Prompt 51; plan § WP-STRIPE; the
// tree: WP-ACCT-1, Stage 1a Prompt 53, docs/ACCOUNTS.md). A child tenant
// (tenants.parent_tenant_id, migration 117) has a tenant_quotas row that
// counts ITS bytes; what it may store is decided by its POOL: the ROOT's
// storage_limit_bytes against the pool's used bytes — and, when the child
// has its own cap (storage_limit_bytes > 0), that cap against its own row:
// min(cap, pool). The root's own writes draw from the same pool. The pool
// is every tenant whose tenants.pool_root_id names the root (migration
// 120, denormalised at any depth: an app's children are the root's too); a
// sandbox (tenants.sandbox_of) is its own root — its row is its own
// local-disk cap.
//
// THE ONE RESOLVER: pool_root_id is read by this file's functions only
// (PoolRoot, lockPoolOwner, poolSum, lockPoolFloor, poolFloorUsed) —
// TestTree_PoolRootIsTheOneResolver walks the repository.
//
// The pool's used bytes are ONE counter (RP82-3): pool_used_bytes on the
// root's row, moved by moveUsed in the statement that moves the member's
// storage_used_bytes, under the root's lock (lockPoolOwner, FOR UPDATE —
// every pool reservation takes it first, so the pool's reservations are
// serialised on it). A pooled check reads the counter; poolSum (the SUM
// over the tree) is the reconcile's and the drift metric's, never a
// write's.
package usage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// poolOf is the pool a tenant reserves in.
type poolOf struct {
	owner  string // the pool's root
	limit  int64  // the root's storage_limit_bytes
	used   int64  // the pool's counter for a pooled tenant; the tenant's own row else
	pooled bool   // in a tree (a child, or a root with children or the platform grant): the pool decides
}

// lockPoolOwner resolves tenantID's pool and locks the root's quota row.
// A tenant with no tenants row (a quota row alone: tests, tools) is its own
// pool.
func lockPoolOwner(ctx context.Context, tx *sql.Tx, tenantID string) (poolOf, error) {
	p := poolOf{}
	var own, counter int64
	err := tx.QueryRowContext(ctx, `
		SELECT o.tenant_id, o.storage_limit_bytes, o.storage_used_bytes, o.pool_used_bytes,
		       (t.pool_root_id <> t.id OR t.platform
		        OR EXISTS (SELECT 1 FROM tenants c WHERE c.pool_root_id = t.id AND c.id <> t.id))
		  FROM tenants t JOIN tenant_quotas o ON o.tenant_id = t.pool_root_id
		 WHERE t.id = $1
		   FOR UPDATE OF o`, tenantID).Scan(&p.owner, &p.limit, &own, &counter, &p.pooled)
	if errors.Is(err, sql.ErrNoRows) {
		err = tx.QueryRowContext(ctx, `
			SELECT tenant_id, storage_limit_bytes, storage_used_bytes FROM tenant_quotas
			 WHERE tenant_id = $1 FOR UPDATE`, tenantID).Scan(&p.owner, &p.limit, &own)
	}
	if err != nil {
		return poolOf{}, fmt.Errorf("checking quota for tenant %s: %w", tenantID, err)
	}
	p.used = own
	if p.pooled {
		p.used = counter
	}
	return p, nil
}

// poolSum sums the pool of root over the tree: every tenant whose
// pool_root_id names it, and a quota row of that id with no tenants row.
// The reconcile's and the drift metric's — never a write's (RP82-3).
func poolSum(ctx context.Context, q RowQuerier, root string) (int64, error) {
	var used int64
	if err := q.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(q.storage_used_bytes), 0)
		  FROM tenant_quotas q
		 WHERE q.tenant_id = $1
		    OR q.tenant_id IN (SELECT c.id FROM tenants c WHERE c.pool_root_id = $1)`, root).Scan(&used); err != nil {
		return 0, fmt.Errorf("summing the pool of %s: %w", root, err)
	}
	return used, nil
}

// lockPool is the reservation's view: the pool the tenant reserves in, with
// the root's row locked — and, for a child, the child's own row (the row
// the reservation then charges), after the root's — and what the pool
// holds (p.used: the pool's counter for a pooled tenant, its own row else).
func lockPool(ctx context.Context, tx *sql.Tx, tenantID string) (poolOf, error) {
	p, err := lockPoolOwner(ctx, tx, tenantID)
	if err != nil {
		return poolOf{}, err
	}
	if !p.pooled || p.owner == tenantID {
		return p, nil
	}
	if _, err := tx.ExecContext(ctx, `SELECT 1 FROM tenant_quotas WHERE tenant_id = $1 FOR UPDATE`, tenantID); err != nil {
		return poolOf{}, fmt.Errorf("checking quota for tenant %s: %w", tenantID, err)
	}
	return p, nil
}

// fits reports whether n more bytes fit the pool.
func (p poolOf) fits(n int64) bool {
	return p.used+n <= p.limit
}

// moveUsed moves tenantID's storage_used_bytes by delta (clamped at zero)
// and its pool root's pool_used_bytes by what was applied, in ONE statement
// (RP82-3). The caller holds the root's row (lockPoolOwner) when root is
// not the tenant itself; the tenant's row is locked here, after it.
// found=false: the tenant has no quota row. poolUsed is the root's counter
// after the move (0 when the root has no row).
func moveUsed(ctx context.Context, tx *sql.Tx, tenantID, root string, delta int64) (used, limit, poolUsed int64, found bool, err error) {
	if root == "" || root == tenantID {
		err = tx.QueryRowContext(ctx, `
			UPDATE tenant_quotas
			   SET storage_used_bytes = GREATEST(0, storage_used_bytes + $1),
			       pool_used_bytes = pool_used_bytes + (GREATEST(0, storage_used_bytes + $1) - storage_used_bytes),
			       updated_at = NOW()
			 WHERE tenant_id = $2
			RETURNING storage_used_bytes, storage_limit_bytes, pool_used_bytes`, delta, tenantID).Scan(&used, &limit, &poolUsed)
		if errors.Is(err, sql.ErrNoRows) {
			return 0, 0, 0, false, nil
		}
		if err != nil {
			return 0, 0, 0, false, fmt.Errorf("ledger: tenant %s: %w", tenantID, err)
		}
		return used, limit, poolUsed, true, nil
	}
	var old int64
	err = tx.QueryRowContext(ctx, `SELECT storage_used_bytes FROM tenant_quotas WHERE tenant_id = $1 FOR UPDATE`, tenantID).Scan(&old)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, 0, 0, false, nil
	}
	if err != nil {
		return 0, 0, 0, false, fmt.Errorf("ledger: tenant %s: %w", tenantID, err)
	}
	applied := max(0, old+delta) - old
	rows, err := tx.QueryContext(ctx, `
		UPDATE tenant_quotas
		   SET storage_used_bytes = CASE WHEN tenant_id = $1 THEN storage_used_bytes + $3 ELSE storage_used_bytes END,
		       pool_used_bytes = CASE WHEN tenant_id = $2 THEN pool_used_bytes + $3 ELSE pool_used_bytes END,
		       updated_at = NOW()
		 WHERE tenant_id IN ($1, $2)
		RETURNING tenant_id, storage_used_bytes, storage_limit_bytes, pool_used_bytes`, tenantID, root, applied)
	if err != nil {
		return 0, 0, 0, false, fmt.Errorf("ledger: tenant %s (pool %s): %w", tenantID, root, err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id string
		var u, l, pu int64
		if err := rows.Scan(&id, &u, &l, &pu); err != nil {
			return 0, 0, 0, false, fmt.Errorf("ledger: tenant %s (pool %s): %w", tenantID, root, err)
		}
		if id == tenantID {
			used, limit, found = u, l, true
		} else {
			poolUsed = pu
		}
	}
	if err := rows.Err(); err != nil {
		return 0, 0, 0, false, fmt.Errorf("ledger: tenant %s (pool %s): %w", tenantID, root, err)
	}
	return used, limit, poolUsed, found, nil
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
// tenant of its tree's.
func poolFloorUsed(ctx context.Context, tx *sql.Tx, owner, floor string) (int64, error) {
	var used int64
	if err := tx.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(f.storage_used_bytes), 0)
		  FROM tenant_floor_quotas f
		 WHERE f.floor = $2
		   AND (f.tenant_id = $1 OR f.tenant_id IN (SELECT c.id FROM tenants c WHERE c.pool_root_id = $1))`,
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
// spend (D-STRIPE-31): the root of its pool, tenants.pool_root_id (WP-ACCT-1;
// before migration 120 the parent's id), the same rule lockPoolOwner locks
// by. A sandbox is its own root (sandbox_of is no pool edge). A tenant with
// no tenants row (a quota row alone: tests, tools) is its own pool. The ONE
// resolver of the egress side.
func PoolRoot(ctx context.Context, q RowQuerier, tenantID string) (string, error) {
	var root string
	err := q.QueryRowContext(ctx, `SELECT pool_root_id FROM tenants WHERE id = $1`, tenantID).Scan(&root)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return tenantID, nil
	case err != nil:
		return "", fmt.Errorf("pool root of %s: %w", tenantID, err)
	}
	return root, nil
}

// PoolDrift is pool_used_bytes − the pool's sum, per pooled root (a root
// with children or the platform grant) whose counter differs: what the
// drift metric (vaultaire_pool_used_drift_bytes{root}) exports. Never
// repaired here (RP82-3: a mismatch is a metric, not a silent fix).
func PoolDrift(ctx context.Context, db *sql.DB) (map[string]int64, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT r.tenant_id, r.pool_used_bytes - COALESCE(SUM(m.storage_used_bytes), 0)
		  FROM tenants rt
		  JOIN tenant_quotas r ON r.tenant_id = rt.id
		  JOIN tenants mt ON mt.pool_root_id = rt.id
		  JOIN tenant_quotas m ON m.tenant_id = mt.id
		 WHERE rt.pool_root_id = rt.id
		   AND (rt.platform OR EXISTS (SELECT 1 FROM tenants c WHERE c.pool_root_id = rt.id AND c.id <> rt.id))
		 GROUP BY r.tenant_id, r.pool_used_bytes
		HAVING r.pool_used_bytes <> COALESCE(SUM(m.storage_used_bytes), 0)`)
	if err != nil {
		return nil, fmt.Errorf("pool drift: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]int64{}
	for rows.Next() {
		var root string
		var d int64
		if err := rows.Scan(&root, &d); err != nil {
			return nil, fmt.Errorf("pool drift: %w", err)
		}
		out[root] = d
	}
	return out, rows.Err()
}

// ReseedPoolUsed rewrites root's pool_used_bytes to the pool's sum and
// answers the drift it removed (counter − sum). The OPERATOR's explicit fix
// (POST /api/v1/admin/quota/reconcile?tenant_id=<root>&pool=reseed,
// audited): nothing calls it on its own.
func (m *QuotaManager) ReseedPoolUsed(ctx context.Context, root string) (int64, error) {
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("reseed the pool of %s: %w", root, err)
	}
	defer func() { _ = tx.Rollback() }()
	var counter int64
	if err := tx.QueryRowContext(ctx, `SELECT pool_used_bytes FROM tenant_quotas WHERE tenant_id = $1 FOR UPDATE`, root).Scan(&counter); err != nil {
		return 0, fmt.Errorf("reseed the pool of %s: %w", root, err)
	}
	// Every member's row, after the root's: the sum read under them is the
	// pool's.
	if _, err := tx.ExecContext(ctx, `
		SELECT 1 FROM tenant_quotas WHERE tenant_id IN (SELECT c.id FROM tenants c WHERE c.pool_root_id = $1 AND c.id <> $1)
		 ORDER BY tenant_id FOR UPDATE`, root); err != nil {
		return 0, fmt.Errorf("reseed the pool of %s: %w", root, err)
	}
	sum, err := poolSum(ctx, tx, root)
	if err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE tenant_quotas SET pool_used_bytes = $2, updated_at = NOW() WHERE tenant_id = $1`, root, sum); err != nil {
		return 0, fmt.Errorf("reseed the pool of %s: %w", root, err)
	}
	return counter - sum, tx.Commit()
}
