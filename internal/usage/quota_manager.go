// internal/usage/quota_manager.go
package usage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

type QuotaManager struct {
	db *sql.DB
}

func NewQuotaManager(db *sql.DB) *QuotaManager {
	return &QuotaManager{db: db}
}

// The quota schema (tenant_quotas, quota_usage_events) is owned by the
// migration set — see internal/database/migrations/056_runtime_tables_and_deletion.sql.
// There is deliberately no Go DDL here (Review R9 / WP-R0-7): the previous
// InitializeSchema had drifted from the migrations (no spending_cap_cents, no
// ON DELETE CASCADE) and the tests that rebuilt from it corrupted every
// database they ran against.

func (m *QuotaManager) CreateTenant(ctx context.Context, tenantID, tier string, limitBytes int64) error {
	_, err := m.db.ExecContext(ctx,
		`INSERT INTO tenant_quotas (tenant_id, tier, storage_limit_bytes)
         VALUES ($1, $2, $3)
         ON CONFLICT (tenant_id) DO UPDATE
         SET tier = $2, storage_limit_bytes = $3, updated_at = NOW()`,
		tenantID, tier, limitBytes)

	if err != nil {
		return fmt.Errorf("creating tenant %s: %w", tenantID, err)
	}
	return nil
}

func (m *QuotaManager) CheckAndReserve(ctx context.Context, tenantID string, bytes int64) (bool, error) {
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("beginning transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// The pool's rule (the root's row first, its counter moved with the
	// total — RP82-3), as ReserveTx without the floors.
	p, err := lockPool(ctx, tx, tenantID)
	if err != nil {
		return false, err
	}
	if !p.fits(bytes) {
		return false, nil // Quota exceeded but not an error
	}
	if _, _, _, _, err := moveUsed(ctx, tx, tenantID, p.owner, bytes); err != nil {
		return false, fmt.Errorf("updating quota: %w", err)
	}
	return true, tx.Commit()
}

// ReleaseQuota subtracts bytes from the tenant's usage, clamped at zero.
// A negative value adds the bytes unconditionally (no limit check) — used to
// account data that is already durably stored.
func (m *QuotaManager) ReleaseQuota(ctx context.Context, tenantID string, bytes int64) error {
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("releasing quota for tenant %s: %w", tenantID, err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := ApplyLedgerTx(ctx, tx, tenantID, LedgerChange{Total: -bytes}); err != nil {
		return fmt.Errorf("releasing quota for tenant %s: %w", tenantID, err)
	}
	return tx.Commit()
}

// ReconcileStorageUsage rewrites every tenant's storage_used_bytes — and
// every floor ledger (066) — to the billed bytes: the logical sizes of the
// head rows plus the retained non-current versions (billed.go), the billing
// source of truth, in ONE transaction that
// first locks every tenant row in order. Returns the number of tenant rows
// updated. Run once before enabling metered billing (Gate C), and any time
// drift is suspected.
//
// Run only while writes are quiesced: an in-flight PUT's reservation is not
// yet reflected in object_head_cache, so reconciling during live traffic
// erases that reservation and under-counts until the next reconcile. The
// row locks mean a reservation is wholly before or wholly after the rewrite
// (the two ledgers can no longer disagree with each other — Review R10-18),
// but they cannot see a reservation whose head row is still in flight.
func (m *QuotaManager) ReconcileStorageUsage(ctx context.Context) (int64, error) {
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("reconciling storage usage: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx,
		`SELECT tenant_id FROM tenant_quotas ORDER BY tenant_id FOR UPDATE`); err != nil {
		return 0, fmt.Errorf("reconciling storage usage: lock tenants: %w", err)
	}
	// The pools' counters move by the members' corrections (RP82-3): a
	// drift the counter had stays, for the metric to show.
	before, err := poolTotals(ctx, tx)
	if err != nil {
		return 0, err
	}
	res, err := tx.ExecContext(ctx, `
		UPDATE tenant_quotas tq
		SET storage_used_bytes = `+billedBytesOfTQ+`,
		    updated_at = NOW()`)
	if err != nil {
		return 0, fmt.Errorf("reconciling storage usage: %w", err)
	}
	after, err := poolTotals(ctx, tx)
	if err != nil {
		return 0, err
	}
	for root, sum := range after {
		if d := sum - before[root]; d != 0 {
			if _, err := tx.ExecContext(ctx, `UPDATE tenant_quotas SET pool_used_bytes = pool_used_bytes + $2 WHERE tenant_id = $1`, root, d); err != nil {
				return 0, fmt.Errorf("reconciling the pool of %s: %w", root, err)
			}
		}
	}
	// The floor ledgers (066) follow the same source of truth, per floor.
	if _, err := tx.ExecContext(ctx, `
		UPDATE tenant_floor_quotas f
		SET storage_used_bytes = `+billedBytesOfF+`,
		    updated_at = NOW()`); err != nil {
		return 0, fmt.Errorf("reconciling floor usage: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("reconciling storage usage: rows: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("reconciling storage usage: commit: %w", err)
	}
	return n, nil
}

// ReconcileTenantStorageUsage is ReconcileStorageUsage for ONE tenant.
// Tests use this one: the global reconcile rewrites every tenant's ledger,
// and with `go test ./...` running packages in parallel against one
// database it erased other packages' in-flight reservations (a PUT reserves
// before its head row exists), which showed up as a CI-only flake.
func (m *QuotaManager) ReconcileTenantStorageUsage(ctx context.Context, tenantID string) error {
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("reconciling storage usage for tenant %s: begin: %w", tenantID, err)
	}
	defer func() { _ = tx.Rollback() }()
	// The pool's root first (the ledger's order), then the tenant's row;
	// the root's counter moves by the tenant's correction (RP82-3).
	root := tenantID
	if p, err := lockPoolOwner(ctx, tx, tenantID); err == nil {
		root = p.owner
	}
	var oldUsed int64
	err = tx.QueryRowContext(ctx, `SELECT storage_used_bytes FROM tenant_quotas WHERE tenant_id = $1 FOR UPDATE`, tenantID).Scan(&oldUsed)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("reconciling storage usage for tenant %s: %w", tenantID, err)
	}
	var newUsed int64
	if err := tx.QueryRowContext(ctx, `
		UPDATE tenant_quotas tq
		SET storage_used_bytes = `+billedBytesOfTQ+`,
		    updated_at = NOW()
		WHERE tq.tenant_id = $1
		RETURNING storage_used_bytes`, tenantID).Scan(&newUsed); err != nil {
		return fmt.Errorf("reconciling storage usage for tenant %s: %w", tenantID, err)
	}
	if d := newUsed - oldUsed; d != 0 {
		if _, err := tx.ExecContext(ctx, `UPDATE tenant_quotas SET pool_used_bytes = pool_used_bytes + $2 WHERE tenant_id = $1`, root, d); err != nil {
			return fmt.Errorf("reconciling the pool of %s: %w", root, err)
		}
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE tenant_floor_quotas f
		SET storage_used_bytes = `+billedBytesOfF+`,
		    updated_at = NOW()
		WHERE f.tenant_id = $1`, tenantID); err != nil {
		return fmt.Errorf("reconciling floor usage for tenant %s: %w", tenantID, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("reconciling storage usage for tenant %s: commit: %w", tenantID, err)
	}
	return nil
}

func (m *QuotaManager) GetUsage(ctx context.Context, tenantID string) (used, limit int64, err error) {
	err = m.db.QueryRowContext(ctx,
		`SELECT storage_used_bytes, storage_limit_bytes
         FROM tenant_quotas
         WHERE tenant_id = $1`,
		tenantID).Scan(&used, &limit)

	if err != nil {
		return 0, 0, fmt.Errorf("getting usage for tenant %s: %w", tenantID, err)
	}

	return used, limit, nil
}

// Fix ListQuotas to use correct column names:
func (qm *QuotaManager) ListQuotas(ctx context.Context) ([]map[string]interface{}, error) {
	query := `
        SELECT tenant_id, tier, storage_limit_bytes, storage_used_bytes, created_at
        FROM tenant_quotas
        ORDER BY created_at DESC`

	rows, err := qm.db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var quotas []map[string]interface{}
	for rows.Next() {
		q := make(map[string]interface{})
		var tenantID, tier string
		var storageLimit, storageUsed int64
		var createdAt time.Time

		err := rows.Scan(&tenantID, &tier, &storageLimit, &storageUsed, &createdAt)
		if err != nil {
			continue
		}

		q["tenant_id"] = tenantID
		q["plan"] = tier // Map tier to plan for API consistency
		q["storage_limit"] = storageLimit
		q["storage_used"] = storageUsed
		q["created_at"] = createdAt

		quotas = append(quotas, q)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate rows: %w", err)
	}

	return quotas, nil
}

func (qm *QuotaManager) DeleteQuota(ctx context.Context, tenantID string) error {
	query := `DELETE FROM tenant_quotas WHERE tenant_id = $1`
	_, err := qm.db.ExecContext(ctx, query, tenantID)
	return err
}

// GetTier returns the current tier for a tenant
func (m *QuotaManager) GetTier(ctx context.Context, tenantID string) (string, error) {
	var tier string
	err := m.db.QueryRowContext(ctx,
		"SELECT tier FROM tenant_quotas WHERE tenant_id = $1", tenantID).Scan(&tier)
	return tier, err
}

// GetUsageHistory is the tenant's daily traffic for the last days days,
// newest first: bytes uploaded and downloaded and requests served, from
// bandwidth_usage_daily (the metered counters, UTC days). It summed
// quota_usage_events, whose only writer was a 'RESERVE' row per reservation
// with no key — uploaded and deleted were always 0 (R10-25, WP-R10-8 Part D).
func (m *QuotaManager) GetUsageHistory(ctx context.Context, tenantID string, days int) ([]map[string]interface{}, error) {
	rows, err := m.db.QueryContext(ctx,
		`SELECT TO_CHAR(date, 'YYYY-MM-DD'), ingress_bytes, egress_bytes, requests_count
		 FROM bandwidth_usage_daily
		 WHERE tenant_id = $1 AND date > (NOW() AT TIME ZONE 'UTC')::date - $2::int
		 ORDER BY date DESC`, tenantID, days)
	if err != nil {
		return nil, fmt.Errorf("usage history for tenant %s: %w", tenantID, err)
	}
	defer func() { _ = rows.Close() }()

	history := []map[string]interface{}{}
	for rows.Next() {
		var date string
		var uploaded, downloaded, requests int64
		if err := rows.Scan(&date, &uploaded, &downloaded, &requests); err != nil {
			return nil, fmt.Errorf("usage history for tenant %s: scan: %w", tenantID, err)
		}
		history = append(history, map[string]interface{}{
			"date":       date,
			"uploaded":   uploaded,
			"downloaded": downloaded,
			"requests":   requests,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate rows: %w", err)
	}
	return history, nil
}

// poolTotals is every pool's sum of storage_used_bytes, by root (the
// reconcile's before/after: a counter moves by its members' corrections).
func poolTotals(ctx context.Context, tx *sql.Tx) (map[string]int64, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT COALESCE(t.pool_root_id, q.tenant_id), SUM(q.storage_used_bytes)
		  FROM tenant_quotas q LEFT JOIN tenants t ON t.id = q.tenant_id
		 GROUP BY 1`)
	if err != nil {
		return nil, fmt.Errorf("reconciling storage usage: pools: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]int64{}
	for rows.Next() {
		var root string
		var sum int64
		if err := rows.Scan(&root, &sum); err != nil {
			return nil, fmt.Errorf("reconciling storage usage: pools: %w", err)
		}
		out[root] = sum
	}
	return out, rows.Err()
}
