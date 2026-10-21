package payments

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/FairForge/vaultaire/internal/audit"
	"github.com/FairForge/vaultaire/internal/usage"
	"github.com/google/uuid"
)

// The admin money operations (WP-PAY-3 item 4, docs/PAYMENTS.md § 7): a
// payment made outside any provider (a wire, a support credit), a date or
// house set by hand, a refund made outside. Each is one transaction under
// the tenant's entitlement lock, audited after the commit with the actor,
// the subject tenant and the reason — the reason is required (ErrReason
// Required). Called by the admin API (internal/api/admin_payments.go) and
// the dashboard's tenant page; neither re-implements a write.

var (
	// ErrReasonRequired: every admin money operation says why.
	ErrReasonRequired = errors.New("reason required")
	// ErrOrderRequired: a manual payment on a tenant with no entitlement
	// row must name the house it buys.
	ErrOrderRequired = errors.New("the tenant has no house yet: the payment must name one (std_tb / vault_tb / pin_hot_tb)")
	// ErrRefundExceeds: the refunds of a payment would exceed what was paid.
	ErrRefundExceeds = errors.New("refund exceeds the amount paid")
	// ErrRefundNoPeriod: the payment has no period on record, so the date
	// cannot be shortened proportionally — adjust it explicitly.
	ErrRefundNoPeriod = errors.New("the payment records no period: shorten paid_through with the entitlement adjustment instead")
	// ErrNoSuchPayment: no settled charge of that id for the tenant.
	ErrNoSuchPayment = errors.New("no such payment")
	// ErrRefundRecorded: a refund with this provider reference is already
	// in the ledger (a webhook redelivery — answered as handled).
	ErrRefundRecorded = errors.New("refund already recorded")
)

// ManualPayment is a payment recorded by an operator.
type ManualPayment struct {
	TenantID    string
	AmountCents int64
	// Period is what the money buys: one month or one year on the date.
	Period Period
	// Order is required when the tenant has no entitlement row (the house
	// the payment buys); ignored otherwise (the row's house is kept —
	// AdjustEntitlement changes a house).
	Order  *Order
	Note   string
	Actor  string
	Reason string
}

// ManualPaymentResult is what was written.
type ManualPaymentResult struct {
	PaymentID     string
	InvoiceNumber string
	PaidThrough   time.Time
	// Granted: the tenant had no row and got one (else the row was extended).
	Granted bool
}

// RecordManualPayment settles a 'manual' ledger row (numbered) and grants
// the house (no row — Order required) or extends the row by one Period, in
// one transaction; audits billing.payment_recorded_manual and
// billing.house_granted after the commit.
func RecordManualPayment(ctx context.Context, db *sql.DB, p ManualPayment) (ManualPaymentResult, error) {
	var res ManualPaymentResult
	if strings.TrimSpace(p.Reason) == "" {
		return res, ErrReasonRequired
	}
	if !p.Period.Valid() {
		return res, fmt.Errorf("manual payment: unknown period %q", p.Period)
	}
	if p.AmountCents < 0 {
		return res, errors.New("manual payment: negative amount")
	}
	now := time.Now().UTC()
	var granted Granted
	var order Order
	err := InTx(ctx, db, func(tx *sql.Tx) error {
		if err := Lock(ctx, tx, p.TenantID); err != nil {
			return err
		}
		e, err := Get(ctx, tx, p.TenantID)
		var start, end time.Time
		switch {
		case errors.Is(err, ErrNoEntitlement):
			if p.Order == nil {
				return ErrOrderRequired
			}
			order = *p.Order
			order.Period = p.Period
			start, end = now, addPeriod(now, p.Period)
			granted, err = GrantRecorded(ctx, tx, p.TenantID, order, end, ProviderManual, "", RenewalPrepaid)
			if err != nil {
				return err
			}
			res.Granted = true
		case err != nil:
			return err
		default:
			order = e.Order
			start = e.PaidThrough
			if e.LapsedAt != nil || start.Before(now) {
				start = now
			}
			end, err = Extend(ctx, tx, p.TenantID, p.Period)
			if err != nil {
				return err
			}
			// The audit reports what the row IS (RP24-3): a wire recorded on
			// a Stripe `auto` row extends that row, it does not re-provider it.
			granted = Granted{Changed: true, PaidThrough: end, Order: order, Provider: e.Provider, Renewal: e.Renewal, Previous: &e}
		}
		res.PaidThrough = end
		o := order
		settled, err := Settle(ctx, tx, Payment{
			TenantID: p.TenantID, Provider: ProviderManual, ProviderID: "manual:" + uuid.New().String(),
			Kind: KindCharge, Purpose: PurposeManual, AmountCents: p.AmountCents,
			PeriodStart: start, PeriodEnd: end, Order: &o, Actor: p.Actor, Note: p.Note,
		}, now)
		if err != nil {
			return err
		}
		res.PaymentID, res.InvoiceNumber = settled.ID, settled.InvoiceNumber
		return nil
	})
	if err != nil {
		return res, err
	}
	CountPayment(ProviderManual, KindCharge, OutcomeSettled)
	audit.Record(ctx, db, audit.Entry{Actor: p.Actor, TenantID: p.TenantID, EventType: "billing", Action: "billing.payment_recorded_manual",
		Resource: "payment:" + res.PaymentID, Metadata: map[string]any{
			"tenant": p.TenantID, "actor": p.Actor, "payment_id": res.PaymentID, "invoice_number": res.InvoiceNumber, "amount_cents": p.AmountCents,
			"period": string(p.Period), "paid_through": res.PaidThrough.UTC().Format(time.RFC3339), "granted": res.Granted,
			"reason": p.Reason, "note": p.Note}})
	AuditGranted(ctx, db, p.TenantID, granted, p.Actor)
	return res, nil
}

// addPeriod adds one period in UTC, Go's way (the month-end rule matches
// Postgres's for the dates a payment lands on).
func addPeriod(t time.Time, p Period) time.Time {
	if p == PeriodAnnual {
		return t.UTC().AddDate(1, 0, 0)
	}
	return t.UTC().AddDate(0, 1, 0)
}

// Adjustment sets paid_through and/or the house by hand.
type Adjustment struct {
	TenantID    string
	PaidThrough *time.Time
	Order       *Order
	Actor       string
	Reason      string
}

// AdjustEntitlement is one of the two writers that may SHORTEN a date
// (every grant takes the later date; the other is RecordRefund, whose
// proportional shortening is its own guarded UPDATE under the same
// entitlement lock — RP24-4). It sets paid_through as given and/or the house's lines
// (floors written in the same transaction); a lapsed row given a future
// date is granted again. Audits billing.paid_through_adjusted.
func AdjustEntitlement(ctx context.Context, db *sql.DB, a Adjustment) (Entitlement, error) {
	if strings.TrimSpace(a.Reason) == "" {
		return Entitlement{}, ErrReasonRequired
	}
	if a.PaidThrough == nil && a.Order == nil {
		return Entitlement{}, errors.New("adjustment: nothing to change (paid_through or order)")
	}
	var before, after Entitlement
	err := InTx(ctx, db, func(tx *sql.Tx) error {
		if err := Lock(ctx, tx, a.TenantID); err != nil {
			return err
		}
		e, err := Get(ctx, tx, a.TenantID)
		if err != nil {
			return err
		}
		before = e
		if a.Order != nil {
			o := *a.Order
			if o.Period == "" {
				o.Period = e.Order.Period
			}
			if err := o.Validate(); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `
				UPDATE entitlements SET std_tb = $2, vault_tb = $3, pin_hot_tb = $4, period = $5, updated_at = NOW()
				 WHERE tenant_id = $1`, a.TenantID, o.Std, o.Vault, o.PinHot, string(o.Period)); err != nil {
				return fmt.Errorf("adjust house of tenant %s: %w", a.TenantID, err)
			}
			e.Order = o
		}
		if a.PaidThrough != nil {
			if _, err := tx.ExecContext(ctx, `
				UPDATE entitlements SET paid_through = $2, lapsed_at = CASE WHEN $2 > NOW() THEN NULL ELSE lapsed_at END, updated_at = NOW()
				 WHERE tenant_id = $1`, a.TenantID, a.PaidThrough.UTC()); err != nil {
				return fmt.Errorf("adjust paid_through of tenant %s: %w", a.TenantID, err)
			}
		}
		after, err = Get(ctx, tx, a.TenantID)
		if err != nil {
			return err
		}
		if after.LapsedAt == nil {
			return usageSetHouse(ctx, tx, a.TenantID, after.Order)
		}
		return nil
	})
	if err != nil {
		return after, err
	}
	meta := map[string]any{"tenant": a.TenantID, "actor": a.Actor, "reason": a.Reason,
		"paid_through_before": before.PaidThrough.UTC().Format(time.RFC3339), "paid_through": after.PaidThrough.UTC().Format(time.RFC3339),
		"std": after.Order.Std, "vault": after.Order.Vault, "pin_hot": after.Order.PinHot, "period": string(after.Order.Period)}
	if a.PaidThrough != nil && a.PaidThrough.Before(before.PaidThrough) {
		meta["shortened"] = true
	}
	audit.Record(ctx, db, audit.Entry{Actor: a.Actor, TenantID: a.TenantID, EventType: "billing", Action: "billing.paid_through_adjusted",
		Resource: "tenant:" + a.TenantID, Metadata: meta})
	return after, nil
}

// Refund records money given back outside the system (a Stripe dashboard
// refund, a wire back).
type Refund struct {
	TenantID    string
	PaymentID   string
	AmountCents int64
	// ProviderRef is the provider's own id of the refund (re_…); empty =
	// a generated one.
	ProviderRef string
	// ShortenPaidThrough takes the refunded share of the payment's period
	// off the date (the payment must record a period — ErrRefundNoPeriod).
	ShortenPaidThrough bool
	Note               string
	Actor              string
	Reason             string
}

// RefundResult is what was written.
type RefundResult struct {
	PaymentID     string
	InvoiceNumber string
	ShortenedBy   time.Duration
	PaidThrough   time.Time
	// FullyRefunded: the charge is now 'refunded'.
	FullyRefunded bool
}

// RecordRefund writes a settled 'refund' row with refund_of = the charge
// (the charge's provider; a credit-note number from the same counter),
// marks the charge 'refunded' once its refunds reach its amount, and —
// when asked — shortens paid_through by the refunded share of the period
// the charge bought. Audits billing.refund_recorded.
func RecordRefund(ctx context.Context, db *sql.DB, r Refund) (RefundResult, error) {
	var res RefundResult
	if strings.TrimSpace(r.Reason) == "" {
		return res, ErrReasonRequired
	}
	if r.AmountCents <= 0 {
		return res, errors.New("refund: the amount must be positive")
	}
	now := time.Now().UTC()
	var provider string
	err := InTx(ctx, db, func(tx *sql.Tx) error {
		if err := Lock(ctx, tx, r.TenantID); err != nil {
			return err
		}
		var providerID string
		var amount int64
		var start, end sql.NullTime
		err := tx.QueryRowContext(ctx, `
			SELECT provider, provider_id, amount_cents, period_start, period_end FROM payments
			 WHERE id::text = $1 AND tenant_id = $2 AND kind = 'charge' AND status IN ('settled', 'refunded')
			 FOR UPDATE`, r.PaymentID, r.TenantID).Scan(&provider, &providerID, &amount, &start, &end)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNoSuchPayment
		}
		if err != nil {
			return fmt.Errorf("read payment %s: %w", r.PaymentID, err)
		}
		var refunded int64
		if err := tx.QueryRowContext(ctx, `
			SELECT COALESCE(SUM(amount_cents), 0) FROM payments WHERE refund_of::text = $1 AND kind = 'refund' AND status = 'settled'`,
			r.PaymentID).Scan(&refunded); err != nil {
			return fmt.Errorf("sum refunds of %s: %w", r.PaymentID, err)
		}
		if refunded+r.AmountCents > amount {
			return fmt.Errorf("%w: %d + %d > %d cents", ErrRefundExceeds, refunded, r.AmountCents, amount)
		}
		ref := r.ProviderRef
		if ref == "" {
			ref = "refund:" + uuid.New().String()
		}
		settled, err := Settle(ctx, tx, Payment{
			TenantID: r.TenantID, Provider: provider, ProviderID: ref, Kind: KindRefund, Purpose: PurposeManual,
			AmountCents: r.AmountCents, PeriodStart: start.Time, PeriodEnd: end.Time, Actor: r.Actor, Note: r.Note,
			Metadata: map[string]any{MetaIDs: map[string]any{"refund_of": providerID}},
		}, now)
		if err != nil {
			return err
		}
		if !settled.Newly {
			return fmt.Errorf("%w: %s/%s", ErrRefundRecorded, provider, ref)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE payments SET refund_of = $2::uuid WHERE id = $1::uuid`, settled.ID, r.PaymentID); err != nil {
			return fmt.Errorf("link refund %s: %w", settled.ID, err)
		}
		res.PaymentID, res.InvoiceNumber = settled.ID, settled.InvoiceNumber
		if refunded+r.AmountCents == amount {
			if _, err := tx.ExecContext(ctx, `UPDATE payments SET status = 'refunded' WHERE id::text = $1`, r.PaymentID); err != nil {
				return fmt.Errorf("mark %s refunded: %w", r.PaymentID, err)
			}
			res.FullyRefunded = true
		}
		if r.ShortenPaidThrough {
			if !start.Valid || !end.Valid || !end.Time.After(start.Time) || amount <= 0 {
				return ErrRefundNoPeriod
			}
			share := float64(r.AmountCents) / float64(amount)
			res.ShortenedBy = time.Duration(math.Round(float64(end.Time.Sub(start.Time)) * share))
			err := tx.QueryRowContext(ctx, `
				UPDATE entitlements SET paid_through = paid_through - ($2 * INTERVAL '1 second'), updated_at = NOW()
				 WHERE tenant_id = $1 RETURNING paid_through`, r.TenantID, res.ShortenedBy.Seconds()).Scan(&res.PaidThrough)
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNoEntitlement
			}
			if err != nil {
				return fmt.Errorf("shorten paid_through of tenant %s: %w", r.TenantID, err)
			}
		}
		return nil
	})
	if err != nil {
		return res, err
	}
	// Labelled by the CHARGE's provider (RP24-3): a refund of a Stripe
	// charge recorded here by hand is still Stripe money.
	CountPayment(provider, KindRefund, OutcomeSettled)
	meta := map[string]any{"tenant": r.TenantID, "actor": r.Actor, "refund_of": r.PaymentID, "payment_id": res.PaymentID, "invoice_number": res.InvoiceNumber,
		"amount_cents": r.AmountCents, "provider_ref": r.ProviderRef, "reason": r.Reason, "note": r.Note, "fully_refunded": res.FullyRefunded}
	if r.ShortenPaidThrough {
		meta["shortened_by"] = res.ShortenedBy.String()
		meta["paid_through"] = res.PaidThrough.UTC().Format(time.RFC3339)
	}
	audit.Record(ctx, db, audit.Entry{Actor: r.Actor, TenantID: r.TenantID, EventType: "billing", Action: "billing.refund_recorded",
		Resource: "payment:" + res.PaymentID, Metadata: meta})
	return res, nil
}

// ChargeOf finds the settled (or refunded) charge row the provider knows as
// providerID (a Stripe invoice id) for tenantID: its ledger id
// (ErrNoSuchPayment when the ledger has none).
func ChargeOf(ctx context.Context, db *sql.DB, tenantID, provider, providerID string) (string, error) {
	var id string
	err := db.QueryRowContext(ctx, `
		SELECT id::text FROM payments
		 WHERE provider = $1 AND provider_id = $2 AND tenant_id = $3 AND kind = 'charge' AND status IN ('settled', 'refunded')`,
		provider, providerID, tenantID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNoSuchPayment
	}
	if err != nil {
		return "", fmt.Errorf("find charge %s/%s: %w", provider, providerID, err)
	}
	return id, nil
}

// usageSetHouse writes the floors of a house (the adjustment's).
func usageSetHouse(ctx context.Context, tx *sql.Tx, tenantID string, o Order) error {
	return usage.SetHouseTx(ctx, tx, tenantID, o.House())
}
