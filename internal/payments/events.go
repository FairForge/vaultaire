package payments

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
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
	// EventRenewalStop: a subscription of another provider whose renewal
	// must stop because the entitlement no longer names it (WP-PAY-5): a
	// BTCPay payment took over a house Stripe renewed by itself (RP30-2's
	// implicit switch — status pending until Stripe confirms
	// cancel_at_period_end), or a switch the customer asked for settled
	// (status stopped at once: the renewal was stopped when they asked). The
	// deletion runner cancels every subscription such a row names, the
	// Stripe webhook never lets one take the house back, and the
	// renewal_stops job retries a pending one. Kept by the retention job
	// until the account's erasure anonymises it, like invalid_house.
	EventRenewalStop = "renewal_stop"
)

// Renewal stop statuses and reasons (payment_events.data of an
// EventRenewalStop row).
const (
	RenewalStopPending = "pending" // the provider has not confirmed the stop yet
	RenewalStopStopped = "stopped" // the provider confirmed it (cancel_at_period_end)
	RenewalStopGone    = "gone"    // the provider no longer has the subscription (or it is cancelled)

	// ReasonImplicitSwitch: a prepaid payment settled over a house another
	// provider renewed by itself (RP30-2).
	ReasonImplicitSwitch = "implicit_switch"
	// ReasonSwitch: the switch the customer asked for (SwitchToPrepaid)
	// settled.
	ReasonSwitch = "switch"
	// ReasonPortalUncancel: the customer re-enabled a stopped renewal in
	// the provider's portal (docs/PAYMENTS.md § 4.1 step 5) — the pending
	// switch is withdrawn and its open prepaid invoices are marked Invalid.
	ReasonPortalUncancel = "portal_uncancel"
)

// RenewalStop is one EventRenewalStop row.
type RenewalStop struct {
	TenantID, Provider, Ref string
	Reason, Status          string
	// Attempts counts the provider calls made for it; LastError is the
	// last one's failure.
	Attempts  int
	LastError string
	At        time.Time
}

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

// RecordRenewalStop writes (or re-opens) the renewal_stop row of ref in tx
// — the caller's transaction, so the row commits with the grant that made
// it necessary. status is RenewalStopPending (a provider call is due) or
// RenewalStopStopped (already confirmed). A row that exists is updated: a
// pending status re-opens a stopped one (the stop is asserted again); a
// stopped status never closes a pending one (RP33-3: a settle after a
// switch whose stop did not finish must not mark Stripe stopped — only the
// provider's answer closes a row, MarkRenewalStop).
func RecordRenewalStop(ctx context.Context, tx *sql.Tx, tenantID, provider, ref, reason, status string, detail map[string]any) error {
	if ref == "" {
		return errors.New("renewal stop: no reference")
	}
	data := map[string]any{"ref": ref, "reason": reason, "status": status, "attempts": 0}
	for k, v := range detail {
		data[k] = v
	}
	raw, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("renewal stop %s: %w", ref, err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO payment_events (provider, event_id, event_type, tenant_id, data)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (provider, event_id) DO UPDATE
		   SET tenant_id = EXCLUDED.tenant_id, received_at = NOW(),
		       data = payment_events.data || jsonb_build_object(
		           'status', CASE WHEN payment_events.data->>'status' = 'pending' THEN payment_events.data->'status'
		                          ELSE EXCLUDED.data->'status' END,
		           'reason', EXCLUDED.data->'reason')`,
		provider, eventID(EventRenewalStop, ref), EventRenewalStop, tenantID, raw); err != nil {
		return fmt.Errorf("record renewal stop %s for tenant %s: %w", ref, tenantID, err)
	}
	return nil
}

// WithdrawRenewalStop deletes the PENDING renewal_stop row of ref in tx:
// the switch that owed it was put back (revertSwitch) or withdrawn by the
// customer re-enabling the renewal (the Stripe webhook's portal race). A
// stopped or gone row is history and stays.
func WithdrawRenewalStop(ctx context.Context, tx *sql.Tx, tenantID, provider, ref string) error {
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM payment_events
		 WHERE provider = $1 AND event_id = $2 AND tenant_id = $3 AND data->>'status' = 'pending'`,
		provider, eventID(EventRenewalStop, ref), tenantID); err != nil {
		return fmt.Errorf("withdraw renewal stop %s of tenant %s: %w", ref, tenantID, err)
	}
	return nil
}

// renewalStopColumns reads an EventRenewalStop row.
const renewalStopColumns = `tenant_id, provider, data->>'ref', COALESCE(data->>'reason', ''), COALESCE(data->>'status', ''),
	COALESCE((data->>'attempts')::int, 0), COALESCE(data->>'last_error', ''), received_at`

func scanRenewalStop(sc interface{ Scan(...any) error }) (RenewalStop, error) {
	var s RenewalStop
	var tenant sql.NullString
	err := sc.Scan(&tenant, &s.Provider, &s.Ref, &s.Reason, &s.Status, &s.Attempts, &s.LastError, &s.At)
	s.TenantID = tenant.String
	return s, err
}

// RenewalStops lists the tenant's renewal_stop rows (every status), oldest
// first — the deletion runner cancels each.
func RenewalStops(ctx context.Context, q interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, tenantID string) ([]RenewalStop, error) {
	return queryRenewalStops(ctx, q, `
		SELECT `+renewalStopColumns+` FROM payment_events
		 WHERE event_type = $1 AND tenant_id = $2 ORDER BY received_at, event_id`, EventRenewalStop, tenantID)
}

// PendingRenewalStops lists up to limit rows whose provider has not
// confirmed the stop, oldest first (the renewal_stops job; tenantID ""
// = every tenant).
func PendingRenewalStops(ctx context.Context, db *sql.DB, tenantID string, limit int) ([]RenewalStop, error) {
	return queryRenewalStops(ctx, db, `
		SELECT `+renewalStopColumns+` FROM payment_events
		 WHERE event_type = $1 AND data->>'status' = 'pending' AND tenant_id IS NOT NULL
		   AND ($2 = '' OR tenant_id = $2)
		 ORDER BY received_at, event_id LIMIT $3`, EventRenewalStop, tenantID, limit)
}

func queryRenewalStops(ctx context.Context, q interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, query string, args ...any) ([]RenewalStop, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("read renewal stops: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []RenewalStop
	for rows.Next() {
		s, err := scanRenewalStop(rows)
		if err != nil {
			return nil, fmt.Errorf("read renewal stops: %w", err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read renewal stops: %w", err)
	}
	return out, nil
}

// RenewalStopOf is the tenant's renewal_stop row of (provider, ref), found
// false when there is none — the Stripe webhook asks it before a granting
// event may take the house.
func RenewalStopOf(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, tenantID, provider, ref string) (RenewalStop, bool, error) {
	s, err := scanRenewalStop(q.QueryRowContext(ctx, `
		SELECT `+renewalStopColumns+` FROM payment_events
		 WHERE provider = $1 AND event_id = $2 AND tenant_id = $3`, provider, eventID(EventRenewalStop, ref), tenantID))
	if errors.Is(err, sql.ErrNoRows) {
		return RenewalStop{}, false, nil
	}
	if err != nil {
		return RenewalStop{}, false, fmt.Errorf("read renewal stop %s of tenant %s: %w", ref, tenantID, err)
	}
	return s, true, nil
}

// MarkRenewalStop records a provider call's outcome on a row: status
// stopped / gone, or pending with the call's error (attempts counted).
func MarkRenewalStop(ctx context.Context, db *sql.DB, provider, ref, status, lastError string) error {
	if _, err := db.ExecContext(ctx, `
		UPDATE payment_events
		   SET data = data || jsonb_build_object('status', $3::text, 'last_error', $4::text,
		                                         'attempts', COALESCE((data->>'attempts')::int, 0) + 1,
		                                         'checked_at', NOW())
		 WHERE provider = $1 AND event_id = $2`, provider, eventID(EventRenewalStop, ref), status, lastError); err != nil {
		return fmt.Errorf("mark renewal stop %s: %w", ref, err)
	}
	return nil
}
