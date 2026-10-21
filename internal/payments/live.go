package payments

import (
	"context"
	"database/sql"
	"fmt"
)

// The live houses (WP-PAY-7, the contract of docs/PAYMENTS.md § 1.6 step 4):
// a house is live when its entitlement row is granted — lapsed_at IS NULL,
// in grace or not. This is the one replacement for every read of
// tenants.{plan = 'house', subscription_status, house_period}: a status the
// provider reported is not what the tenant holds, the record is.

// LiveHouseTenantsSQL selects the tenant id of every granted entitlement —
// for a reader that joins or counts in SQL (`WHERE t.id IN (` + it + `)`).
const LiveHouseTenantsSQL = `SELECT tenant_id FROM entitlements WHERE lapsed_at IS NULL`

// LiveHouses reads every granted entitlement, ordered by tenant id.
func LiveHouses(ctx context.Context, db *sql.DB) ([]Entitlement, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT tenant_id, std_tb, vault_tb, pin_hot_tb, period, paid_through, renewal, provider,
		       COALESCE(provider_ref, ''), created_at, updated_at
		  FROM entitlements WHERE lapsed_at IS NULL
		 ORDER BY tenant_id`)
	if err != nil {
		return nil, fmt.Errorf("read live houses: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []Entitlement
	for rows.Next() {
		var e Entitlement
		var period, renewal string
		if err := rows.Scan(&e.TenantID, &e.Order.Std, &e.Order.Vault, &e.Order.PinHot, &period, &e.PaidThrough,
			&renewal, &e.Provider, &e.ProviderRef, &e.CreatedAt, &e.UpdatedAt); err != nil {
			return nil, fmt.Errorf("read live houses: %w", err)
		}
		e.Order.Period, e.Renewal = Period(period), Renewal(renewal)
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read live houses: %w", err)
	}
	return out, nil
}

// CountLiveHouses is the number of granted entitlements (the admin pages'
// "active subscriptions").
func CountLiveHouses(ctx context.Context, db *sql.DB) (int, error) {
	var n int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM (`+LiveHouseTenantsSQL+`) l`).Scan(&n); err != nil {
		return 0, fmt.Errorf("count live houses: %w", err)
	}
	return n, nil
}

// HouseMonth is the month's movement of houses from the record: granted
// for the first time this month, lapsed this month, and live at the
// month's start (the churn rate's denominator). UTC months.
type HouseMonth struct {
	New, Lapsed, LiveAtStart int
}

// HouseMonthOf reads the movement of the UTC month that holds now (the
// database's clock).
func HouseMonthOf(ctx context.Context, db *sql.DB) (HouseMonth, error) {
	var m HouseMonth
	err := db.QueryRowContext(ctx, `
		WITH b AS (SELECT date_trunc('month', NOW() AT TIME ZONE 'UTC') AT TIME ZONE 'UTC' AS start)
		SELECT COUNT(*) FILTER (WHERE e.created_at >= b.start),
		       COUNT(*) FILTER (WHERE e.lapsed_at >= b.start),
		       COUNT(*) FILTER (WHERE e.created_at < b.start AND (e.lapsed_at IS NULL OR e.lapsed_at >= b.start))
		  FROM entitlements e, b`).Scan(&m.New, &m.Lapsed, &m.LiveAtStart)
	if err != nil {
		return m, fmt.Errorf("read house month: %w", err)
	}
	return m, nil
}
