// internal/usage/ledger_tx.go
//
// The ledger inside the transaction that makes it true (WP-R10-8 Part A,
// R10-17, R10-43). Every write and every delete changes tenant_quotas and
// tenant_floor_quotas in the same transaction as the head/version rows it
// flips or removes — before COMMIT — so the ledger is true at every
// instant, not after the next reconcile: a crash, a lock wait or a cut
// client between the row change and a separate quota statement used to
// leave the bytes counted (or uncounted) until reconcile.
//
// Lock order (every path, asserted by TestLedger_LockOrder_*): the key's
// advisory lock (lockHeadKey) → the head row (FOR UPDATE) → the key's
// version lock and rows → the upload row (multipart_uploads, FOR UPDATE)
// → tenant_quotas → tenant_floor_quotas (standard, then vault). The ledger
// statements are the LAST of the transaction: the tenant's quota row is
// held from its UPDATE to COMMIT only, and nothing that holds it waits for
// a key, row or upload lock — the reservation (CheckAndReserveFloor), the
// reconcile and SetHouse take the quota rows alone, in the same order.
package usage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
)

// ErrOverQuota is a ledger change refused because it leaves the tenant past
// a limit (LedgerChange.Guard).
var ErrOverQuota = errors.New("quota exceeded")

// LedgerChange is a signed change of a tenant's billed bytes: on the total
// and per floor (they differ when a reservation reached the total before the
// tenant had a floor row).
type LedgerChange struct {
	Total  int64
	Floors map[string]int64
	// Guard, when set, refuses (ErrOverQuota) a change that increases the
	// total or the floor named here past its limit.
	Guard string
}

// Zero reports a change that moves nothing.
func (c LedgerChange) Zero() bool {
	if c.Total != 0 {
		return false
	}
	for _, d := range c.Floors {
		if d != 0 {
			return false
		}
	}
	return true
}

// ApplyLedgerTx applies c to the tenant's ledgers inside tx. Used bytes are
// clamped at zero. A tenant with no tenant_quotas row (or no row for a
// floor) has nothing to change there. A zero change takes no lock.
func ApplyLedgerTx(ctx context.Context, tx *sql.Tx, tenantID string, c LedgerChange) error {
	if c.Zero() {
		return nil
	}
	var used, limit int64
	err := tx.QueryRowContext(ctx, `
		UPDATE tenant_quotas
		   SET storage_used_bytes = GREATEST(0, storage_used_bytes + $1), updated_at = NOW()
		 WHERE tenant_id = $2
		RETURNING storage_used_bytes, storage_limit_bytes`, c.Total, tenantID).Scan(&used, &limit)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil
	case err != nil:
		return fmt.Errorf("ledger: tenant %s: %w", tenantID, err)
	}
	if c.Guard != "" && c.Total > 0 && used > limit {
		return fmt.Errorf("%w: tenant %s total %d > %d", ErrOverQuota, tenantID, used, limit)
	}
	floors := make([]string, 0, len(c.Floors))
	for f, d := range c.Floors {
		if d != 0 {
			floors = append(floors, f)
		}
	}
	sort.Strings(floors) // standard before vault: one order for every path
	for _, f := range floors {
		var fUsed, fLimit int64
		err := tx.QueryRowContext(ctx, `
			UPDATE tenant_floor_quotas
			   SET storage_used_bytes = GREATEST(0, storage_used_bytes + $1), updated_at = NOW()
			 WHERE tenant_id = $2 AND floor = $3
			RETURNING storage_used_bytes, storage_limit_bytes`, c.Floors[f], tenantID, f).Scan(&fUsed, &fLimit)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			continue
		case err != nil:
			return fmt.Errorf("ledger: tenant %s floor %s: %w", tenantID, f, err)
		}
		if f == c.Guard && c.Floors[f] > 0 && fUsed > fLimit {
			return fmt.Errorf("%w: tenant %s floor %s %d > %d", ErrOverQuota, tenantID, f, fUsed, fLimit)
		}
	}
	return nil
}
