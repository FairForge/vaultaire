// internal/usage/floor.go
//
// Per-floor quotas (dashboard plan, Phase 1). The public site sells storage
// as a house: downstairs = Standard, attic = Vault, whole TB per floor.
// A tenant who bought a house carries one tenant_floor_quotas row per floor
// (migration 066) and every write is checked against the floor it lands on
// as well as the total. Tenants without floor rows — free tier, bench and
// admin accounts, anything provisioned by hand — are enforced exactly as
// before, on the single total in tenant_quotas.
//
// Invariants, per tenant, in logical bytes:
//
//	tenant_quotas.storage_used_bytes        == SUM(object_head_cache.size_bytes)
//	tenant_floor_quotas[f].storage_used_bytes == SUM(... WHERE floor = f)
//	tenant_quotas.storage_limit_bytes       == SUM(tenant_floor_quotas.storage_limit_bytes)   (house tenants)
//
// ReconcileStorageUsage rewrites all three from object_head_cache.
package usage

import (
	"context"
	"database/sql"
	"fmt"
)

// Floors. The internal ids stay standard/vault; customers read
// "downstairs" and "attic" (docs/DESIGN.md §1).
const (
	FloorStandard = "standard"
	FloorVault    = "vault"
)

// Byte units. A "TB" sold on the site is 2^40 bytes — the generous reading,
// and the one every existing size string in the dashboard already uses.
const (
	MB int64 = 1 << 20
	GB int64 = 1 << 30
	TB int64 = 1 << 40
)

// FloorOf maps the storage class resolved at write time onto the floor the
// object is billed on: the archive classes are the attic, everything else —
// STANDARD, PUBLIC (R2), RESILIENT (Lyve), the local dev classes — is
// downstairs. Where the Smart tier parks a downstairs object later is our
// business, not the customer's, so demotion never changes a floor.
func FloorOf(storageClass string) string {
	switch storageClass {
	case "GLACIER", "DEEP_ARCHIVE":
		return FloorVault
	}
	return FloorStandard
}

// House is what a tenant bought: whole TB per floor plus the pin-hot add-on
// (bytes that must never demote), all in bytes.
type House struct {
	StdBytes    int64
	VaultBytes  int64
	PinHotBytes int64
}

// HouseFromTB builds a House from whole-TB quantities (what Stripe items carry).
func HouseFromTB(std, vault, pinHot int) House {
	return House{StdBytes: int64(std) * TB, VaultBytes: int64(vault) * TB, PinHotBytes: int64(pinHot) * TB}
}

func (h House) StdTB() int        { return int(h.StdBytes / TB) }
func (h House) VaultTB() int      { return int(h.VaultBytes / TB) }
func (h House) PinHotTB() int     { return int(h.PinHotBytes / TB) }
func (h House) TotalBytes() int64 { return h.StdBytes + h.VaultBytes }
func (h House) Empty() bool       { return h.StdBytes == 0 && h.VaultBytes == 0 }
func (h House) Tier() string {
	if h.StdBytes > 0 {
		return "standard"
	}
	return "vault"
}

// FloorQuota is one tenant_floor_quotas row.
type FloorQuota struct {
	Floor      string
	LimitBytes int64
	UsedBytes  int64
}

// SetHouse writes the floor limits a subscription grants. The total quota
// becomes the sum of the floors and the tier follows the house (standard
// when there is a downstairs, vault for an attic-only house). Floor rows are
// created on first use with their used bytes summed from object_head_cache,
// so a free-tier tenant's existing objects are counted on the right floor;
// on resize the used bytes are kept. Idempotent: the webhook may replay it.
func (m *QuotaManager) SetHouse(ctx context.Context, tenantID string, h House) error {
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("set house for tenant %s: begin: %w", tenantID, err)
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx,
		`UPDATE tenant_quotas
		    SET storage_limit_bytes = $1, tier = $2, pin_hot_bytes = $3, updated_at = NOW()
		  WHERE tenant_id = $4`,
		h.TotalBytes(), h.Tier(), h.PinHotBytes, tenantID)
	if err != nil {
		return fmt.Errorf("set house for tenant %s: total: %w", tenantID, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("set house for tenant %s: no tenant_quotas row", tenantID)
	}

	for _, f := range []struct {
		floor string
		limit int64
	}{{FloorStandard, h.StdBytes}, {FloorVault, h.VaultBytes}} {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO tenant_floor_quotas (tenant_id, floor, storage_limit_bytes, storage_used_bytes)
			 VALUES ($1, $2, $3, COALESCE(
			     (SELECT SUM(size_bytes) FROM object_head_cache WHERE tenant_id = $1 AND floor = $2), 0))
			 ON CONFLICT (tenant_id, floor) DO UPDATE
			   SET storage_limit_bytes = EXCLUDED.storage_limit_bytes, updated_at = NOW()`,
			tenantID, f.floor, f.limit); err != nil {
			return fmt.Errorf("set house for tenant %s: floor %s: %w", tenantID, f.floor, err)
		}
	}
	return tx.Commit()
}

// ClearHouse removes the floor rows and returns the tenant to the free tier
// limits. Used bytes are never touched: billing does not delete data, the
// tenant simply cannot upload past the free tier until they buy again or
// delete something.
func (m *QuotaManager) ClearHouse(ctx context.Context, tenantID string) error {
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("clear house for tenant %s: begin: %w", tenantID, err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM tenant_floor_quotas WHERE tenant_id = $1`, tenantID); err != nil {
		return fmt.Errorf("clear house for tenant %s: floors: %w", tenantID, err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE tenant_quotas
		    SET storage_limit_bytes = $1, tier = 'free', pin_hot_bytes = 0, updated_at = NOW()
		  WHERE tenant_id = $2`,
		FreeTierLimits.StorageBytes, tenantID); err != nil {
		return fmt.Errorf("clear house for tenant %s: total: %w", tenantID, err)
	}
	return tx.Commit()
}

// GetFloors returns the tenant's floor rows, standard first; empty when the
// tenant has no house.
func (m *QuotaManager) GetFloors(ctx context.Context, tenantID string) ([]FloorQuota, error) {
	rows, err := m.db.QueryContext(ctx,
		`SELECT floor, storage_limit_bytes, storage_used_bytes
		   FROM tenant_floor_quotas WHERE tenant_id = $1
		  ORDER BY CASE floor WHEN 'standard' THEN 0 ELSE 1 END, floor`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("get floors for tenant %s: %w", tenantID, err)
	}
	defer func() { _ = rows.Close() }()
	var out []FloorQuota
	for rows.Next() {
		var f FloorQuota
		if err := rows.Scan(&f.Floor, &f.LimitBytes, &f.UsedBytes); err != nil {
			return nil, fmt.Errorf("get floors for tenant %s: scan: %w", tenantID, err)
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// GetHouse returns the house the floor rows describe, and false when the
// tenant has none.
func (m *QuotaManager) GetHouse(ctx context.Context, tenantID string) (House, bool, error) {
	floors, err := m.GetFloors(ctx, tenantID)
	if err != nil || len(floors) == 0 {
		return House{}, false, err
	}
	var h House
	for _, f := range floors {
		switch f.Floor {
		case FloorStandard:
			h.StdBytes = f.LimitBytes
		case FloorVault:
			h.VaultBytes = f.LimitBytes
		}
	}
	err = m.db.QueryRowContext(ctx,
		`SELECT pin_hot_bytes FROM tenant_quotas WHERE tenant_id = $1`, tenantID).Scan(&h.PinHotBytes)
	if err != nil {
		return House{}, false, fmt.Errorf("get house for tenant %s: %w", tenantID, err)
	}
	return h, true, nil
}

// CheckAndReserveFloor is CheckAndReserve for a write that lands on a known
// floor: the total is checked and reserved exactly as before, and when the
// tenant has a house the floor is checked and reserved in the same
// transaction. A house without that floor (no attic bought) refuses the
// write. Returns false (no error) when either ledger has no room.
func (m *QuotaManager) CheckAndReserveFloor(ctx context.Context, tenantID, floor string, bytes int64) (bool, error) {
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("beginning transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var used, limit int64
	if err := tx.QueryRowContext(ctx,
		`SELECT storage_used_bytes, storage_limit_bytes FROM tenant_quotas
		  WHERE tenant_id = $1 FOR UPDATE`, tenantID).Scan(&used, &limit); err != nil {
		return false, fmt.Errorf("checking quota for tenant %s: %w", tenantID, err)
	}
	if used+bytes > limit {
		return false, nil
	}

	// The floor ledger, when the tenant has one. The total row lock above
	// serialises every floor write for the tenant, so the floor read is
	// consistent without its own FOR UPDATE.
	var floorRows int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM tenant_floor_quotas WHERE tenant_id = $1`, tenantID).Scan(&floorRows); err != nil {
		return false, fmt.Errorf("checking floors for tenant %s: %w", tenantID, err)
	}
	if floorRows > 0 {
		var fUsed, fLimit int64
		err := tx.QueryRowContext(ctx,
			`SELECT storage_used_bytes, storage_limit_bytes FROM tenant_floor_quotas
			  WHERE tenant_id = $1 AND floor = $2`, tenantID, floor).Scan(&fUsed, &fLimit)
		if err == sql.ErrNoRows {
			return false, nil // the house has no such floor
		}
		if err != nil {
			return false, fmt.Errorf("checking floor %s for tenant %s: %w", floor, tenantID, err)
		}
		if fUsed+bytes > fLimit {
			return false, nil
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE tenant_floor_quotas
			    SET storage_used_bytes = storage_used_bytes + $1, updated_at = NOW()
			  WHERE tenant_id = $2 AND floor = $3`, bytes, tenantID, floor); err != nil {
			return false, fmt.Errorf("reserving on floor %s: %w", floor, err)
		}
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE tenant_quotas
		    SET storage_used_bytes = storage_used_bytes + $1, updated_at = NOW()
		  WHERE tenant_id = $2`, bytes, tenantID); err != nil {
		return false, fmt.Errorf("updating quota: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO quota_usage_events (tenant_id, operation, bytes_delta, object_key)
		 VALUES ($1, 'RESERVE', $2, '')`, tenantID, bytes); err != nil {
		return false, fmt.Errorf("recording event: %w", err)
	}
	return true, tx.Commit()
}

// ReleaseFloor is ReleaseQuota for bytes known to sit on a floor: the total
// and (when present) the floor row move together, clamped at zero. A
// negative value adds unconditionally on both ledgers — bytes that are
// already durably stored must be accounted even past the limit.
func (m *QuotaManager) ReleaseFloor(ctx context.Context, tenantID, floor string, bytes int64) error {
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("releasing quota for tenant %s: begin: %w", tenantID, err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx,
		`UPDATE tenant_quotas
		    SET storage_used_bytes = GREATEST(0, storage_used_bytes - $1), updated_at = NOW()
		  WHERE tenant_id = $2`, bytes, tenantID); err != nil {
		return fmt.Errorf("releasing quota for tenant %s: %w", tenantID, err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE tenant_floor_quotas
		    SET storage_used_bytes = GREATEST(0, storage_used_bytes - $1), updated_at = NOW()
		  WHERE tenant_id = $2 AND floor = $3`, bytes, tenantID, floor); err != nil {
		return fmt.Errorf("releasing floor %s for tenant %s: %w", floor, tenantID, err)
	}
	return tx.Commit()
}
