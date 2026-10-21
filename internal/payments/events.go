package payments

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
)

// Operational rows this service writes into payment_events beside the
// providers' webhook dedup rows (WP-PAY-7: the tenants billing columns
// they replace are dropped). Their event ids carry a prefix no provider
// delivery id has.
const (
	// EventDeletionCancel: the deletion runner cancelled provider_ref with
	// its provider (the stamp that was tenants.deletion_stripe_cancelled_at).
	// Written only after the provider answered; one row per (provider, ref).
	EventDeletionCancel = "deletion_cancel"
	// EventInvalidHouse: the Stripe webhook acknowledged a subscription that
	// is no valid house (RP20-4) — Stripe still bills it and no entitlement
	// names it, so the deletion runner reads this row to cancel it (RP22-3,
	// D-P20-3; was tenants.stripe_subscription_id). Kept by the retention
	// job until the account's erasure anonymises it.
	EventInvalidHouse = "invalid_house"
)

func eventID(kind, ref string) string { return kind + ":" + ref }

// recordEvent inserts an operational row; a repeat refreshes received_at
// and data (idempotent).
func recordEvent(ctx context.Context, db *sql.DB, provider, kind, ref, tenantID string, data map[string]any) error {
	raw, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("record %s %s: %w", kind, ref, err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO payment_events (provider, event_id, event_type, tenant_id, data)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (provider, event_id) DO UPDATE
		   SET tenant_id = EXCLUDED.tenant_id, data = EXCLUDED.data, received_at = NOW()`,
		provider, eventID(kind, ref), kind, tenantID, raw); err != nil {
		return fmt.Errorf("record %s %s for tenant %s: %w", kind, ref, tenantID, err)
	}
	return nil
}

// MarkDeletionCancelled records that the deletion runner cancelled ref with
// provider for tenantID. Call it only after the provider answered.
func MarkDeletionCancelled(ctx context.Context, db *sql.DB, tenantID, provider, ref string) error {
	return recordEvent(ctx, db, provider, EventDeletionCancel, ref, tenantID, map[string]any{"provider_ref": ref})
}

// DeletionCancelled reports whether the runner already cancelled ref with
// provider for tenantID (a new subscription has a new ref, so a stamp never
// skips it).
func DeletionCancelled(ctx context.Context, db *sql.DB, tenantID, provider, ref string) (bool, error) {
	var ok bool
	if err := db.QueryRowContext(ctx, `
		SELECT EXISTS (SELECT 1 FROM payment_events
		                WHERE provider = $1 AND event_id = $2 AND tenant_id = $3)`,
		provider, eventID(EventDeletionCancel, ref), tenantID).Scan(&ok); err != nil {
		return false, fmt.Errorf("read deletion cancel of %s for tenant %s: %w", ref, tenantID, err)
	}
	return ok, nil
}

// RecordInvalidHouse remembers a Stripe subscription the webhook
// acknowledged as no valid house, so the deletion runner cancels it.
func RecordInvalidHouse(ctx context.Context, db *sql.DB, tenantID, subscriptionID, status string) error {
	return recordEvent(ctx, db, ProviderStripe, EventInvalidHouse, subscriptionID, tenantID,
		map[string]any{"subscription": subscriptionID, "status": status})
}

// InvalidHouseRefs lists the Stripe subscriptions recorded as invalid
// houses for tenantID.
func InvalidHouseRefs(ctx context.Context, db *sql.DB, tenantID string) ([]string, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT data->>'subscription' FROM payment_events
		 WHERE provider = $1 AND event_type = $2 AND tenant_id = $3 AND data->>'subscription' <> ''
		 ORDER BY received_at`, ProviderStripe, EventInvalidHouse, tenantID)
	if err != nil {
		return nil, fmt.Errorf("read invalid houses of tenant %s: %w", tenantID, err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var ref string
		if err := rows.Scan(&ref); err != nil {
			return nil, fmt.Errorf("read invalid houses of tenant %s: %w", tenantID, err)
		}
		out = append(out, ref)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read invalid houses of tenant %s: %w", tenantID, err)
	}
	return out, nil
}
