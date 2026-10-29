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

// lockPool is the reservation's view: what the pool holds and may hold,
// with the owner's row locked — and, for a child, the child's own row
// (the row the reservation then charges), after the owner's.
func lockPool(ctx context.Context, tx *sql.Tx, tenantID string) (used, limit int64, err error) {
	p, err := lockPoolOwner(ctx, tx, tenantID)
	if err != nil {
		return 0, 0, err
	}
	if !p.pooled {
		return p.used, p.limit, nil
	}
	if p.owner != tenantID {
		if _, err := tx.ExecContext(ctx, `SELECT 1 FROM tenant_quotas WHERE tenant_id = $1 FOR UPDATE`, tenantID); err != nil {
			return 0, 0, fmt.Errorf("checking quota for tenant %s: %w", tenantID, err)
		}
	}
	used, err = poolUsed(ctx, tx, p.owner)
	return used, p.limit, err
}
