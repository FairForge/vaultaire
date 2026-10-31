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
// Lock order (every path; TestLedger_LockOrder_* in this package stage the
// deadlocks and walk every statement of the repository): the key's
// advisory lock (lockHeadKey) → the head row (FOR UPDATE) → the key's
// version lock and rows → the upload row (multipart_uploads, FOR UPDATE)
// → tenant_quotas (a pool's OWNER row first, then the tenant's own —
// WP-PLAT-1, pool.go) → tenant_floor_quotas (a pool's owner floor row
// first, then the tenant's own — RP82-1; standard, then vault). The ledger
// statements are the LAST of the transaction: the tenant's quota row is
// held from its UPDATE to COMMIT only, and nothing that holds it waits for
// a key, row or upload lock — the reservation (CheckAndReserveFloor), the
// reconcile, SetHouse, ClearHouse and the account erasure take the quota
// rows alone, in the same order. A tenant_floor_quotas row is locked only
// while the tenant's tenant_quotas row is held.
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
	// total or the floor named here past its limit — and a holder past its
	// cap (WP-KEY-1: a *CapError naming it).
	Guard string
	// Holders: the key and bucket counters this change moves beside the
	// tenant's (holders.go, WP-KEY-1), moved after the tenant's rows.
	Holders []HolderMove
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
	for _, h := range c.Holders {
		if h.Bytes != 0 {
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
	// The tenant's rows first, then the holders' (holders.go). Only holders
	// moving (a write whose reservation equals its size, over a row another
	// key wrote) locks no tenant row.
	if c.Total != 0 || floorsMove(c.Floors) || (c.Guard != "" && c.Floors[c.Guard] > 0) {
		if err := applyTenantLedgerTx(ctx, tx, tenantID, c); err != nil {
			return err
		}
	}
	return applyHolders(ctx, tx, tenantID, c.Holders, c.Guard != "")
}

// floorsMove reports a non-zero floor move.
func floorsMove(f map[string]int64) bool {
	for _, d := range f {
		if d != 0 {
			return true
		}
	}
	return false
}

// applyTenantLedgerTx is ApplyLedgerTx's tenant half: the total, the pool's
// counter and the floors.
func applyTenantLedgerTx(ctx context.Context, tx *sql.Tx, tenantID string, c LedgerChange) error {
	// The total's row is UPDATEd only when the total moves: a new key whose
	// reservation equals its size changes no ledger at all and takes no
	// lock — the quota row is the tenant's one hot row, and every commit of
	// the tenant would otherwise queue on it. A floor-only change (a write
	// whose reservation predates a floor row SetHouse created, a part held
	// on another floor) still LOCKS it first: a floor row is locked only by
	// a transaction holding the total, or two floors locked in either order
	// deadlock against the reconcile or ClearHouse (Stage 1a Prompt 7,
	// Part 0).
	// A guarded increase is checked against the POOL (WP-PLAT-1; its floors
	// since RP82-1): the pool's owner row is locked FIRST — before this
	// tenant's own row, the order every reservation takes (lockPool) — so a
	// settle and a reservation of two children never wait for each other in
	// turn. The floor guard needs the owner too, total moving or not: a
	// child's own floor rows have limit 0 and never decide.
	// A total that moves moves the pool's counter too (RP82-3): the root's
	// row is locked first whether or not the change is guarded.
	totalGuard := c.Guard != "" && c.Total > 0
	floorGuard := c.Guard != "" && c.Floors[c.Guard] > 0
	pool := poolOf{owner: tenantID}
	if c.Total != 0 || floorGuard {
		p, err := lockPoolOwner(ctx, tx, tenantID)
		switch {
		case err == nil:
			pool = p
		case !totalGuard && errors.Is(err, sql.ErrNoRows):
			if c.Total != 0 {
				return nil // no quota row: nothing to change on the total
			}
		default:
			return err
		}
	}
	if c.Total == 0 {
		if _, err := tx.ExecContext(ctx, `SELECT 1 FROM tenant_quotas WHERE tenant_id = $1 FOR UPDATE`, tenantID); err != nil {
			return fmt.Errorf("ledger: lock tenant %s: %w", tenantID, err)
		}
	}
	if c.Total != 0 {
		used, limit, poolUsed, found, err := moveUsed(ctx, tx, tenantID, pool.owner, c.Total)
		switch {
		case err != nil:
			return err
		case !found:
			return nil
		}
		if totalGuard && pool.pooled {
			// A capped child (its own limit > 0) is held to min(cap, pool).
			if pool.owner != tenantID && limit > 0 && used > limit {
				return fmt.Errorf("%w: tenant %s past its cap %d > %d", ErrOverQuota, tenantID, used, limit)
			}
			used, limit = poolUsed, pool.limit
		}
		if totalGuard && used > limit {
			return fmt.Errorf("%w: tenant %s total %d > %d", ErrOverQuota, tenantID, used, limit)
		}
	}
	// The pool's floor: the owner's row, locked before this tenant's own.
	var poolFloorLimit int64
	poolFloor := false
	if floorGuard && pool.pooled {
		var err error
		if poolFloorLimit, poolFloor, err = lockPoolFloor(ctx, tx, pool.owner, c.Guard); err != nil {
			return err
		}
	}
	floors := make([]string, 0, len(c.Floors))
	for f, d := range c.Floors {
		if d != 0 {
			floors = append(floors, f)
		}
	}
	sort.Strings(floors) // standard before vault: one order for every path
	for _, f := range floors {
		guardHere := f == c.Guard && c.Floors[f] > 0
		var fUsed, fLimit int64
		err := tx.QueryRowContext(ctx, `
			UPDATE tenant_floor_quotas
			   SET storage_used_bytes = GREATEST(0, storage_used_bytes + $1), updated_at = NOW()
			 WHERE tenant_id = $2 AND floor = $3
			RETURNING storage_used_bytes, storage_limit_bytes`, c.Floors[f], tenantID, f).Scan(&fUsed, &fLimit)
		missing := errors.Is(err, sql.ErrNoRows)
		if err != nil && !missing {
			return fmt.Errorf("ledger: tenant %s floor %s: %w", tenantID, f, err)
		}
		switch {
		case !guardHere:
		case pool.pooled:
			if !poolFloor {
				continue // the owner has no such floor row: nothing guards it
			}
			used, err := poolFloorUsed(ctx, tx, pool.owner, f)
			if err != nil {
				return err
			}
			if missing {
				used += c.Floors[f] // a child with no counting row yet: its bytes still count
			}
			if used > poolFloorLimit {
				return fmt.Errorf("%w: tenant %s pool %s floor %s %d > %d", ErrOverQuota, tenantID, pool.owner, f, used, poolFloorLimit)
			}
		case !missing && fUsed > fLimit:
			return fmt.Errorf("%w: tenant %s floor %s %d > %d", ErrOverQuota, tenantID, f, fUsed, fLimit)
		}
	}
	return nil
}
