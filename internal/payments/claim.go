package payments

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ClaimEvent is the webhook contract of every provider but Stripe
// (docs/PAYMENTS.md § 2.4; Stripe keeps stripe_events and its own
// claimEvent, internal/billing/webhook.go — the same contract on another
// table). A delivery is claimed BEFORE its handler runs: the row
// (provider, eventID) is inserted in a transaction that stays open while the
// handler runs and is committed only after it succeeded, with the tenant it
// resolved. So:
//
//   - a delivery of an id already committed is a duplicate (answer 200);
//   - a concurrent delivery of the same id waits on the uncommitted row (at
//     most lock_timeout, 5 s — then an error: answer 5xx, the provider
//     retries) and is then a duplicate, or takes the event over if the first
//     one rolled back;
//   - a handler that fails rolls the claim back (Rollback) and answers 5xx,
//     so the provider's redelivery is handled as a fresh event; a process
//     that dies mid-handler takes its connection — and so its claim — with it.
//
// The body is kept in data (JSONB; payment_events are the processor's
// records, 90 days — the retention job — and their tenant link is nulled at
// erasure).

// Claim is a held claim of one delivery.
type Claim struct {
	tx                *sql.Tx
	provider, eventID string
}

// ClaimEvent claims (provider, eventID). duplicate = the id was already
// committed (or was committed by the delivery this one waited on): the
// returned claim is nil and nothing is held. body must be JSON.
func ClaimEvent(ctx context.Context, db *sql.DB, provider, eventID, eventType string, body []byte) (*Claim, bool, error) {
	if provider == "" || eventID == "" {
		return nil, false, errors.New("claim: provider and event id are required")
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, fmt.Errorf("begin claim of %s/%s: %w", provider, eventID, err)
	}
	// How long a delivery waits on another delivery of the same id before
	// it answers 5xx and lets the provider redeliver later.
	if _, err := tx.ExecContext(ctx, `SET LOCAL lock_timeout = '5s'`); err != nil {
		_ = tx.Rollback()
		return nil, false, fmt.Errorf("claim of %s/%s: lock timeout: %w", provider, eventID, err)
	}
	var claimed string
	err = tx.QueryRowContext(ctx, `
		INSERT INTO payment_events (provider, event_id, event_type, data)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (provider, event_id) DO NOTHING
		RETURNING event_id`, provider, eventID, eventType, string(body)).Scan(&claimed)
	if errors.Is(err, sql.ErrNoRows) {
		_ = tx.Rollback()
		return nil, true, nil
	}
	if err != nil {
		_ = tx.Rollback()
		return nil, false, fmt.Errorf("claim %s/%s: %w", provider, eventID, err)
	}
	return &Claim{tx: tx, provider: provider, eventID: eventID}, false, nil
}

// Tx is the claim's transaction (held open; a handler may lock in it, e.g.
// the tenant's entitlement lock, so one tenant's events apply one at a time).
func (c *Claim) Tx() *sql.Tx { return c.tx }

// Commit writes the tenant the handler resolved ("" = none, not ours) onto
// the row and commits the claim: the delivery is handled.
func (c *Claim) Commit(ctx context.Context, tenantID string) error {
	if tenantID != "" {
		if _, err := c.tx.ExecContext(ctx,
			`UPDATE payment_events SET tenant_id = $3 WHERE provider = $1 AND event_id = $2`,
			c.provider, c.eventID, tenantID); err != nil {
			_ = c.tx.Rollback()
			return fmt.Errorf("claim %s/%s: write tenant: %w", c.provider, c.eventID, err)
		}
	}
	if err := c.tx.Commit(); err != nil {
		return fmt.Errorf("commit claim %s/%s: %w", c.provider, c.eventID, err)
	}
	return nil
}

// Rollback releases the claim: the provider's redelivery is a fresh event.
func (c *Claim) Rollback() { _ = c.tx.Rollback() }
