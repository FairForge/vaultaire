package payments

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Ledger helpers of a prepaid provider (WP-PAY-4, BTCPay — docs/PAYMENTS.md
// § 3.3–3.4): the tenant's credit balance, the crypto side of a settled
// row, and the date one more period reaches.

// CreditBalance is what the ledger holds for the tenant (an overpayment, a
// partial payment of an invoice that expired — docs/PAYMENTS.md § 3.4):
// the settled credit rows minus the credit settled charges consumed
// (payments.credit_cents of a charge = the credit it was reduced by). Never
// negative.
func CreditBalance(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, tenantID string) (int64, error) {
	var bal int64
	if err := q.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(CASE WHEN kind = 'credit' THEN amount_cents ELSE -credit_cents END), 0)
		  FROM payments
		 WHERE tenant_id = $1 AND status IN ('settled', 'refunded') AND kind IN ('credit', 'charge')`,
		tenantID).Scan(&bal); err != nil {
		return 0, fmt.Errorf("credit balance of tenant %s: %w", tenantID, err)
	}
	if bal < 0 {
		bal = 0
	}
	return bal, nil
}

// CryptoDetail is the on-chain side of a payment: what arrived, at which
// rate, to which addresses, in which transactions. Addresses and txids go
// into the metadata keys the erasure drops (MetaAddresses, MetaTxids).
type CryptoDetail struct {
	// Amount (decimal string, e.g. "0.00012345"), Currency ("BTC"), Rate
	// (decimal string, USD per unit).
	Amount, Currency, Rate string
	Addresses, Txids       []string
}

// RecordCrypto writes d onto the row (provider, providerID), in tx.
func RecordCrypto(ctx context.Context, tx *sql.Tx, provider, providerID string, d CryptoDetail) error {
	meta := map[string]any{}
	if len(d.Addresses) > 0 {
		meta[MetaAddresses] = d.Addresses
	}
	if len(d.Txids) > 0 {
		meta[MetaTxids] = d.Txids
	}
	raw, err := json.Marshal(meta)
	if err != nil {
		return fmt.Errorf("crypto detail of %s/%s: %w", provider, providerID, err)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE payments
		   SET crypto_amount = COALESCE(NULLIF($3, '')::numeric, crypto_amount),
		       crypto_currency = COALESCE(NULLIF($4, ''), crypto_currency),
		       rate = COALESCE(NULLIF($5, '')::numeric, rate),
		       metadata = COALESCE(metadata, '{}'::jsonb) || $6::jsonb
		 WHERE provider = $1 AND provider_id = $2`,
		provider, providerID, d.Amount, d.Currency, d.Rate, string(raw)); err != nil {
		return fmt.Errorf("crypto detail of %s/%s: %w", provider, providerID, err)
	}
	return nil
}

// PaidThroughAfter is the date one more period would take the tenant's
// house to — Extend's arithmetic (GREATEST(paid_through, now) + the period,
// in UTC; the month-end rule of Postgres) without writing: what a prepaid
// provider's settled payment grants (with Grant, which also names the
// provider and the house). A tenant with no row, or a lapsed one, counts
// from now. The caller holds the entitlement lock.
func PaidThroughAfter(ctx context.Context, tx *sql.Tx, tenantID string, by Period) (time.Time, error) {
	if !by.Valid() {
		return time.Time{}, fmt.Errorf("paid through after: unknown period %q", by)
	}
	var t time.Time
	err := tx.QueryRowContext(ctx, `
		SELECT ((GREATEST(COALESCE((SELECT paid_through FROM entitlements WHERE tenant_id = $1 AND lapsed_at IS NULL), NOW()), NOW())
		         AT TIME ZONE 'UTC') + $2::interval) AT TIME ZONE 'UTC'`, tenantID, by.interval()).Scan(&t)
	if err != nil {
		return time.Time{}, fmt.Errorf("paid through after for tenant %s: %w", tenantID, err)
	}
	return t, nil
}

// PendingOf finds the open (pending / processing) or settled ledger row the
// provider knows as providerID: its id, tenant, status, purpose and the
// house and credit it was opened with (ErrNoSuchPayment when there is
// none). The webhook of a prepaid provider resolves the tenant from it —
// never from the provider's payload.
func PendingOf(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, provider, providerID string) (Opened, error) {
	var o Opened
	var tenant sql.NullString
	var std, vault, pin sql.NullInt64
	var period sql.NullString
	err := q.QueryRowContext(ctx, `
		SELECT id::text, tenant_id, status, purpose, kind, amount_cents, credit_cents, std_tb, vault_tb, pin_hot_tb, period
		  FROM payments WHERE provider = $1 AND provider_id = $2`, provider, providerID).
		Scan(&o.ID, &tenant, &o.Status, &o.Purpose, &o.Kind, &o.AmountCents, &o.CreditCents, &std, &vault, &pin, &period)
	if errors.Is(err, sql.ErrNoRows) {
		return o, ErrNoSuchPayment
	}
	if err != nil {
		return o, fmt.Errorf("find payment %s/%s: %w", provider, providerID, err)
	}
	o.TenantID = tenant.String
	if period.Valid {
		o.Order = &Order{Std: int(std.Int64), Vault: int(vault.Int64), PinHot: int(pin.Int64), Period: Period(period.String)}
	}
	return o, nil
}

// Opened is a ledger row as a prepaid provider opened it.
type Opened struct {
	ID, TenantID, Status, Purpose, Kind string
	AmountCents, CreditCents            int64
	Order                               *Order
}

// MarkProcessing moves an open row to 'processing' (a payment seen, not
// yet confirmed); false when the row is not pending.
func MarkProcessing(ctx context.Context, tx *sql.Tx, provider, providerID string) (bool, error) {
	res, err := tx.ExecContext(ctx, `
		UPDATE payments SET status = 'processing' WHERE provider = $1 AND provider_id = $2 AND status = 'pending'`,
		provider, providerID)
	if err != nil {
		return false, fmt.Errorf("payment %s/%s → processing: %w", provider, providerID, err)
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// SetCredit upserts the credit row (provider, providerID) of tenantID to
// cents (≥ 0): a credit that tracks what arrived on an invoice that never
// settled (it grows with a late partial payment, and goes to 0 when the
// invoice settles after all — docs/PAYMENTS.md § 3.4). Unnumbered, like
// every credit. Returns the change in the tenant's balance.
func SetCredit(ctx context.Context, tx *sql.Tx, tenantID, provider, providerID string, cents int64, note string, at time.Time) (int64, error) {
	if cents < 0 {
		cents = 0
	}
	var before sql.NullInt64
	if err := tx.QueryRowContext(ctx,
		`SELECT amount_cents FROM payments WHERE provider = $1 AND provider_id = $2 AND kind = 'credit' FOR UPDATE`,
		provider, providerID).Scan(&before); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("read credit %s/%s: %w", provider, providerID, err)
	}
	if !before.Valid && cents == 0 {
		return 0, nil
	}
	if !before.Valid {
		if _, err := Credit(ctx, tx, Payment{TenantID: tenantID, Provider: provider, ProviderID: providerID,
			AmountCents: cents, Note: note}, at); err != nil {
			return 0, err
		}
		return cents, nil
	}
	if before.Int64 == cents {
		return 0, nil
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE payments SET amount_cents = $3 WHERE provider = $1 AND provider_id = $2 AND kind = 'credit'`,
		provider, providerID, cents); err != nil {
		return 0, fmt.Errorf("set credit %s/%s: %w", provider, providerID, err)
	}
	return cents - before.Int64, nil
}

// AdoptPending renames a pending row's provisional id (pending:<uuid>,
// written before the provider's invoice existed) to the provider's id.
func AdoptPending(ctx context.Context, tx interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}, provider, provisional, providerID string) error {
	if _, err := tx.ExecContext(ctx,
		`UPDATE payments SET provider_id = $3 WHERE provider = $1 AND provider_id = $2 AND status = 'pending'`,
		provider, provisional, providerID); err != nil {
		return fmt.Errorf("adopt %s/%s as %s: %w", provider, provisional, providerID, err)
	}
	return nil
}
