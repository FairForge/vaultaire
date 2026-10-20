package payments

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Receipts (WP-PAY-3 item 1, docs/PAYMENTS.md § 3.8): a receipt is RENDERED
// from the ledger row, never stored twice. ReadReceipt answers only the
// tenant's own rows (the tenant comes from the session, never from the URL
// alone — ErrNoReceipt for anyone else's id).

// ErrNoReceipt: no settled row of that id for the tenant.
var ErrNoReceipt = errors.New("no such receipt")

// Receipt is one settled money event as the receipt page prints it.
type Receipt struct {
	ID            string
	InvoiceNumber string
	Status        string
	Kind          string // charge | refund | credit
	Purpose       string
	Provider      string
	ProviderID    string
	AmountCents   int64
	// CryptoAmount / CryptoCurrency / Rate: present for a crypto payment
	// (empty today — WP-PAY-4).
	CryptoAmount, CryptoCurrency, Rate string
	PeriodStart, PeriodEnd             time.Time
	PaidAt                             time.Time
	// Order is the house the payment bought (nil for a row without one).
	Order *Order
	// Lines are the receipt lines from the house snapshot (QuoteHouse), or
	// one line for a payment without a house.
	Lines []QuoteLine
	// Email is the tenant's e-mail as the ledger's tenant row has it.
	Email    string
	TenantID string
	RefundOf string
}

// Amount formats the amount ("$53.88").
func (r Receipt) Amount() string { return money(r.AmountCents) }

// PeriodWords is "20 Oct 2026 – 20 Oct 2027", "" when unknown.
func (r Receipt) PeriodWords() string {
	if r.PeriodStart.IsZero() || r.PeriodEnd.IsZero() {
		return ""
	}
	return r.PeriodStart.UTC().Format("2 Jan 2006") + " – " + r.PeriodEnd.UTC().Format("2 Jan 2006")
}

const receiptColumns = `
	SELECT p.id::text, COALESCE(p.invoice_number, ''), p.status, p.kind, p.purpose, p.provider, p.provider_id, p.amount_cents,
	       COALESCE(p.crypto_amount::text, ''), COALESCE(p.crypto_currency, ''), COALESCE(p.rate::text, ''),
	       p.period_start, p.period_end, p.settled_at, p.std_tb, p.vault_tb, p.pin_hot_tb, p.period,
	       COALESCE(p.refund_of::text, ''), COALESCE(t.email, ''), p.tenant_id
	  FROM payments p JOIN tenants t ON t.id = p.tenant_id`

func scanReceipt(row interface{ Scan(...any) error }) (Receipt, error) {
	var r Receipt
	var start, end, paid sql.NullTime
	var std, vault, pin sql.NullInt64
	var period sql.NullString
	if err := row.Scan(&r.ID, &r.InvoiceNumber, &r.Status, &r.Kind, &r.Purpose, &r.Provider, &r.ProviderID, &r.AmountCents,
		&r.CryptoAmount, &r.CryptoCurrency, &r.Rate, &start, &end, &paid, &std, &vault, &pin, &period,
		&r.RefundOf, &r.Email, &r.TenantID); err != nil {
		return r, err
	}
	r.PeriodStart, r.PeriodEnd, r.PaidAt = start.Time, end.Time, paid.Time
	if std.Valid || vault.Valid || pin.Valid {
		o := Order{Std: int(std.Int64), Vault: int(vault.Int64), PinHot: int(pin.Int64), Period: Period(period.String)}
		r.Order = &o
		q := QuoteHouse(o.Std, o.Vault, o.PinHot, o.Period)
		r.Lines = q.Lines
	}
	if len(r.Lines) == 0 {
		label := "Payment"
		if r.Kind == KindRefund {
			label = "Refund"
		}
		r.Lines = []QuoteLine{{Label: label, Cents: r.AmountCents}}
	}
	return r, nil
}

// ReadReceipt reads one settled or refunded row of the tenant.
func ReadReceipt(ctx context.Context, db *sql.DB, tenantID, paymentID string) (Receipt, error) {
	r, err := scanReceipt(db.QueryRowContext(ctx, receiptColumns+`
		 WHERE p.id::text = $2 AND p.tenant_id = $1 AND p.status IN ('settled', 'refunded')`, tenantID, paymentID))
	if errors.Is(err, sql.ErrNoRows) {
		return r, ErrNoReceipt
	}
	if err != nil {
		return r, fmt.Errorf("read receipt %s of tenant %s: %w", paymentID, tenantID, err)
	}
	return r, nil
}

// ListReceipts lists the tenant's settled rows, newest first.
func ListReceipts(ctx context.Context, db *sql.DB, tenantID string, limit int) ([]Receipt, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := db.QueryContext(ctx, receiptColumns+`
		 WHERE p.tenant_id = $1 AND p.status IN ('settled', 'refunded') AND p.invoice_number IS NOT NULL
		 ORDER BY p.settled_at DESC, p.id::text LIMIT $2`, tenantID, limit)
	if err != nil {
		return nil, fmt.Errorf("list receipts of tenant %s: %w", tenantID, err)
	}
	defer func() { _ = rows.Close() }()
	var out []Receipt
	for rows.Next() {
		r, err := scanReceipt(rows)
		if err != nil {
			return nil, fmt.Errorf("list receipts of tenant %s: %w", tenantID, err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list receipts of tenant %s: %w", tenantID, err)
	}
	return out, nil
}

// ListLedger lists every row of the tenant's ledger (any status), newest
// first — the admin view.
func ListLedger(ctx context.Context, db *sql.DB, tenantID string, limit int) ([]Receipt, error) {
	if limit <= 0 {
		limit = 200
	}
	rows, err := db.QueryContext(ctx, receiptColumns+`
		 WHERE p.tenant_id = $1
		 ORDER BY p.created_at DESC, p.id::text LIMIT $2`, tenantID, limit)
	if err != nil {
		return nil, fmt.Errorf("list ledger of tenant %s: %w", tenantID, err)
	}
	defer func() { _ = rows.Close() }()
	var out []Receipt
	for rows.Next() {
		r, err := scanReceipt(rows)
		if err != nil {
			return nil, fmt.Errorf("list ledger of tenant %s: %w", tenantID, err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list ledger of tenant %s: %w", tenantID, err)
	}
	return out, nil
}
