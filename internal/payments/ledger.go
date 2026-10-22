package payments

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// The payments ledger (migration 097): one row per money event from any
// provider, keyed by (provider, provider_id). A row is an accounting record:
// it is never deleted — an account erasure drops its tenant link, its crypto
// addresses and transaction ids and its note (account.EraseRows), and the
// retention job leaves it alone.
//
// Metadata keys (fixed now so the erasure knows what to drop): "ids" — the
// provider's own ids (a Stripe subscription, a pull payment), kept;
// "addresses" — crypto addresses, dropped at erasure; "txids" — on-chain
// transaction ids, dropped at erasure.

// Payment kinds, purposes and statuses (the CHECKs of migration 097).
const (
	KindCharge     = "charge"
	KindRefund     = "refund"
	KindCredit     = "credit"
	KindAdjustment = "adjustment"

	PurposeNew    = "new"
	PurposeRenew  = "renew"
	PurposeResize = "resize"
	PurposeSwitch = "switch"
	PurposeManual = "manual"

	StatusPending        = "pending"
	StatusProcessing     = "processing"
	StatusSettled        = "settled"
	StatusFailed         = "failed"
	StatusExpired        = "expired"
	StatusExpiredPartial = "expired_partial"
	StatusInvalid        = "invalid"
	StatusRefunded       = "refunded"
)

// Metadata keys.
const (
	MetaIDs       = "ids"
	MetaAddresses = "addresses"
	MetaTxids     = "txids"
)

// Payment is what a provider's notification says about one money event.
type Payment struct {
	TenantID    string
	Provider    string
	ProviderID  string
	Kind        string
	Purpose     string
	AmountCents int64
	CreditCents int64
	// PeriodStart/PeriodEnd: what the payment bought (zero = unknown).
	PeriodStart, PeriodEnd time.Time
	// Order is the house it bought (nil = not a house payment).
	Order    *Order
	Actor    string
	Note     string
	Metadata map[string]any
	// Unnumbered settles the row without a receipt number (RP24-1: the $0
	// subscription_create invoice of a trialing Stripe subscription — the
	// gapless counter must not spend a number on it, and no receipt is
	// rendered or mailed for it).
	Unnumbered bool
}

// Settled is the outcome of Settle.
type Settled struct {
	ID            string
	InvoiceNumber string
	// Newly is false when the row was already settled (a replay).
	Newly bool
	// DoublePeriod is set when this newly settled charge bought dates
	// another provider's settled charge also bought (WP-PAY-5 item 3,
	// docs/PAYMENTS.md § 2.5 point 5): the caller reports it after its
	// commit (ReportDoublePeriod). The settle itself stands — paid is paid.
	DoublePeriod *DoublePeriod
}

// DoublePeriod names the two charges whose periods overlap.
type DoublePeriod struct {
	ThisID, ThisProvider, ThisProviderID    string
	ThisStart, ThisEnd                      time.Time
	OtherID, OtherProvider, OtherProviderID string
	OtherStart, OtherEnd                    time.Time
}

// InvoiceNumber formats our receipt number: ST-<year>-<six digits>.
func InvoiceNumber(year, n int) string { return fmt.Sprintf("ST-%d-%06d", year, n) }

// nextInvoiceNumber takes the year's next number under the counter row's
// lock, held to the end of tx: concurrent settles are numbered one after
// the other, and a settle whose transaction rolls back gives its number
// back — the sequence has no gap.
func nextInvoiceNumber(ctx context.Context, tx *sql.Tx, year int) (string, error) {
	var n int
	if err := tx.QueryRowContext(ctx, `
		INSERT INTO invoice_counters (year, last) VALUES ($1, 1)
		ON CONFLICT (year) DO UPDATE SET last = invoice_counters.last + 1
		RETURNING last`, year).Scan(&n); err != nil {
		return "", fmt.Errorf("next invoice number of %d: %w", year, err)
	}
	return InvoiceNumber(year, n), nil
}

func (p Payment) check() error {
	if p.Provider == "" || p.ProviderID == "" {
		return errors.New("payment without provider or provider id")
	}
	if p.Kind == "" || p.Purpose == "" {
		return errors.New("payment without kind or purpose")
	}
	return nil
}

func nullTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UTC()
}

func nullText(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func (p Payment) house() (std, vault, pin, period any) {
	if p.Order == nil {
		return nil, nil, nil, nil
	}
	return p.Order.Std, p.Order.Vault, p.Order.PinHot, string(p.Order.Period)
}

func (p Payment) metadata() (any, error) {
	if len(p.Metadata) == 0 {
		return nil, nil
	}
	b, err := json.Marshal(p.Metadata)
	if err != nil {
		return nil, fmt.Errorf("payment metadata: %w", err)
	}
	return string(b), nil
}

// ensureRow inserts the payment as status if (provider, provider_id) has no
// row yet, then locks the row and returns its id and status. A concurrent
// first insert of the same id waits on the other's uncommitted row.
func ensureRow(ctx context.Context, tx *sql.Tx, p Payment, status string) (id, current string, err error) {
	if err := p.check(); err != nil {
		return "", "", err
	}
	meta, err := p.metadata()
	if err != nil {
		return "", "", err
	}
	std, vault, pin, period := p.house()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO payments (tenant_id, provider, provider_id, kind, purpose, status, amount_cents, credit_cents,
		                      period_start, period_end, std_tb, vault_tb, pin_hot_tb, period, actor, note, metadata)
		VALUES (NULLIF($1, ''), $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17)
		ON CONFLICT (provider, provider_id) DO NOTHING`,
		p.TenantID, p.Provider, p.ProviderID, p.Kind, p.Purpose, status, p.AmountCents, p.CreditCents,
		nullTime(p.PeriodStart), nullTime(p.PeriodEnd), std, vault, pin, period,
		nullText(p.Actor), nullText(p.Note), meta); err != nil {
		return "", "", fmt.Errorf("record payment %s/%s: %w", p.Provider, p.ProviderID, err)
	}
	if err := tx.QueryRowContext(ctx,
		`SELECT id::text, status FROM payments WHERE provider = $1 AND provider_id = $2 FOR UPDATE`,
		p.Provider, p.ProviderID).Scan(&id, &current); err != nil {
		return "", "", fmt.Errorf("lock payment %s/%s: %w", p.Provider, p.ProviderID, err)
	}
	return id, current, nil
}

// RecordPending records an invoice or checkout that was opened: status
// 'pending' (a row that exists is left as it is). Returns the row's id.
func RecordPending(ctx context.Context, tx *sql.Tx, p Payment) (string, error) {
	id, _, err := ensureRow(ctx, tx, p, StatusPending)
	return id, err
}

// Settle records that the payment was received at `at`: the row (created if
// absent) becomes 'settled' with the amounts and period of p, and gets our
// invoice number from the counter of at's UTC year. A row already settled
// (a replayed notification) is returned unchanged, Newly false.
func Settle(ctx context.Context, tx *sql.Tx, p Payment, at time.Time) (Settled, error) {
	id, current, err := ensureRow(ctx, tx, p, StatusPending)
	if err != nil {
		return Settled{}, err
	}
	if current == StatusSettled || current == StatusRefunded {
		var number sql.NullString
		if err := tx.QueryRowContext(ctx, `SELECT invoice_number FROM payments WHERE id = $1`, id).Scan(&number); err != nil {
			return Settled{}, fmt.Errorf("read settled payment %s: %w", id, err)
		}
		return Settled{ID: id, InvoiceNumber: number.String}, nil
	}
	at = at.UTC()
	var number string
	if !p.Unnumbered {
		if number, err = nextInvoiceNumber(ctx, tx, at.Year()); err != nil {
			return Settled{}, err
		}
	}
	std, vault, pin, period := p.house()
	if _, err := tx.ExecContext(ctx, `
		UPDATE payments
		   SET status = 'settled', settled_at = $2, invoice_number = NULLIF($3, ''),
		       tenant_id = COALESCE(tenant_id, NULLIF($4, '')),
		       amount_cents = $5, credit_cents = $6,
		       period_start = COALESCE($7, period_start), period_end = COALESCE($8, period_end),
		       std_tb = COALESCE($9, std_tb), vault_tb = COALESCE($10, vault_tb),
		       pin_hot_tb = COALESCE($11, pin_hot_tb), period = COALESCE($12, period)
		 WHERE id = $1`,
		id, at, number, p.TenantID, p.AmountCents, p.CreditCents,
		nullTime(p.PeriodStart), nullTime(p.PeriodEnd), std, vault, pin, period); err != nil {
		return Settled{}, fmt.Errorf("settle payment %s: %w", id, err)
	}
	out := Settled{ID: id, InvoiceNumber: number, Newly: true}
	if p.Kind == KindCharge && p.AmountCents > 0 {
		if out.DoublePeriod, err = doublePeriodOf(ctx, tx, id); err != nil {
			return Settled{}, err
		}
	}
	return out, nil
}

// doublePeriodOf is the double-period detector (WP-PAY-5 item 3): another
// provider's settled charge of the same tenant whose period overlaps the
// charge id just settled — both bounds (other.end > this.start AND
// other.start < this.end; the design's one-sided "end > start" would flag a
// later prepaid period that does not overlap). $0 rows on either side (a
// trialing subscription's first invoice, a credit-covered order) paid
// nothing twice and are never a match.
func doublePeriodOf(ctx context.Context, tx *sql.Tx, id string) (*DoublePeriod, error) {
	var d DoublePeriod
	err := tx.QueryRowContext(ctx, `
		SELECT t.id::text, t.provider, t.provider_id, t.period_start, t.period_end,
		       o.id::text, o.provider, o.provider_id, o.period_start, o.period_end
		  FROM payments t
		  JOIN payments o ON o.tenant_id = t.tenant_id AND o.provider <> t.provider AND o.kind = 'charge'
		                 AND o.status = 'settled' AND o.amount_cents > 0
		                 AND o.period_end > t.period_start AND o.period_start < t.period_end
		 WHERE t.id = $1::uuid AND t.tenant_id IS NOT NULL AND t.period_start IS NOT NULL AND t.period_end IS NOT NULL
		 ORDER BY o.period_end DESC LIMIT 1`, id).
		Scan(&d.ThisID, &d.ThisProvider, &d.ThisProviderID, &d.ThisStart, &d.ThisEnd,
			&d.OtherID, &d.OtherProvider, &d.OtherProviderID, &d.OtherStart, &d.OtherEnd)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("double-period check of payment %s: %w", id, err)
	}
	return &d, nil
}

// Fail records a payment attempt that failed (a Stripe invoice whose charge
// was declined): informational, no entitlement change. A settled or
// refunded row is never moved back.
func Fail(ctx context.Context, tx *sql.Tx, p Payment) (string, error) {
	id, current, err := ensureRow(ctx, tx, p, StatusFailed)
	if err != nil {
		return "", err
	}
	if current == StatusPending || current == StatusProcessing {
		if _, err := tx.ExecContext(ctx, `UPDATE payments SET status = 'failed' WHERE id = $1`, id); err != nil {
			return "", fmt.Errorf("fail payment %s: %w", id, err)
		}
	}
	return id, nil
}

// transition moves an open (pending/processing) row to status; false when
// there is no such open row.
func transition(ctx context.Context, tx *sql.Tx, provider, providerID, status string) (bool, error) {
	res, err := tx.ExecContext(ctx, `
		UPDATE payments SET status = $3
		 WHERE provider = $1 AND provider_id = $2 AND status IN ('pending', 'processing')`,
		provider, providerID, status)
	if err != nil {
		return false, fmt.Errorf("payment %s/%s → %s: %w", provider, providerID, status, err)
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// Expire records an invoice that expired unpaid (partial: something was
// received — WP-PAY-4 keeps it as credit).
func Expire(ctx context.Context, tx *sql.Tx, provider, providerID string, partial bool) (bool, error) {
	status := StatusExpired
	if partial {
		status = StatusExpiredPartial
	}
	return transition(ctx, tx, provider, providerID, status)
}

// Invalidate records an invoice the provider marked invalid (a double
// spend, a payment that never confirmed).
func Invalidate(ctx context.Context, tx *sql.Tx, provider, providerID string) (bool, error) {
	return transition(ctx, tx, provider, providerID, StatusInvalid)
}

// Credit records a credit (an overpayment kept against the next invoice, an
// operator's goodwill): a settled 'credit' row with no invoice number of
// its own. Idempotent on (provider, provider_id).
func Credit(ctx context.Context, tx *sql.Tx, p Payment, at time.Time) (string, error) {
	p.Kind = KindCredit
	if p.Purpose == "" {
		p.Purpose = PurposeManual
	}
	id, current, err := ensureRow(ctx, tx, p, StatusSettled)
	if err != nil {
		return "", err
	}
	if current != StatusSettled {
		return "", fmt.Errorf("credit %s/%s: the id names a %s row", p.Provider, p.ProviderID, current)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE payments SET settled_at = COALESCE(settled_at, $2) WHERE id = $1`, id, at.UTC()); err != nil {
		return "", fmt.Errorf("credit %s: %w", id, err)
	}
	return id, nil
}
