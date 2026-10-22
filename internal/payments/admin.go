package payments

import (
	"context"
	"database/sql"
	"encoding/json"
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
	// ErrRefundProviderRef: the charge's provider records its own refunds
	// (Stripe's charge.refunded): an operator may record one only under the
	// provider's refund id (re_…), so the webhook and the form meet on one
	// row (RP28-1).
	ErrRefundProviderRef = errors.New("this charge's provider records its refunds itself: give the provider's refund id (re_…) — the webhook records it under that id")
	// ErrRefundShortened: this refund already took its share off
	// paid_through (a refund shortens the date once).
	ErrRefundShortened = errors.New("this refund already shortened paid_through")
	// ErrRefundMismatch: the refund named is in the ledger but does not
	// match the call (another charge, another amount).
	ErrRefundMismatch = errors.New("the refund in the ledger does not match")
)

// RecordsOwnRefunds reports whether provider tells us of its refunds itself
// (Stripe: charge.refunded / charge.refund.updated). A refund of such a
// provider's charge is created by its webhook, or by an operator under the
// provider's refund id; never under a generated reference (RP28-1). Manual
// and BTCPay refunds are made by the operator and recorded only by hand.
func RecordsOwnRefunds(provider string) bool { return provider == ProviderStripe }

// stripeRefundPrefix is the prefix of a Stripe refund id.
const stripeRefundPrefix = "re_"

// MetaShortened is the metadata key a refund row carries once it took its
// share off paid_through: {"seconds": <shortened by>, "at": <RFC 3339>,
// "actor": <who>}. A refund shortens the date once (ErrRefundShortened).
// Kept at erasure (no personal data).
const MetaShortened = "shortened"

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
	var manualSettled Settled
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
		manualSettled = settled
		return nil
	})
	if err != nil {
		return res, err
	}
	CountPayment(ProviderManual, KindCharge, OutcomeSettled)
	// A wire over dates another provider was paid for: counted and audited
	// (the operator recording it is the one to see the page).
	_ = ReportDoublePeriod(ctx, db, nil, p.TenantID, manualSettled)
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
	// FromProvider: the provider's own notice of the refund (the Stripe
	// webhook). Only it may create the refund of a charge whose provider
	// RecordsOwnRefunds without the provider's refund id, and a refund
	// already in the ledger is ErrRefundRecorded for it — for an operator
	// it is an idempotent hit (or the shortening of that refund).
	FromProvider bool
}

// RefundResult is what was written.
type RefundResult struct {
	PaymentID     string
	InvoiceNumber string
	ShortenedBy   time.Duration
	PaidThrough   time.Time
	// FullyRefunded: the charge is now 'refunded'.
	FullyRefunded bool
	// Existing: the refund was already in the ledger (recorded by the
	// provider's webhook or an earlier call); nothing new was written but,
	// when asked, the shortening.
	Existing bool
}

// RecordRefund writes a settled 'refund' row with refund_of = the charge
// (the charge's provider; a credit-note number from the same counter),
// marks the charge 'refunded' once its refunds reach its amount, and —
// when asked — shortens paid_through by the refunded share of the period
// the charge bought. Audits billing.refund_recorded.
//
// One refund at a provider is one row (RP28-1): r.PaymentID may name the
// charge or a refund row already in the ledger, and r.ProviderRef a refund
// already recorded under that id. A refund found that way is not recorded
// again: for the provider (FromProvider) it is ErrRefundRecorded — or
// ErrRefundMismatch when its amount differs (RP30-7); for an
// operator it is an idempotent hit (Existing) — with ShortenPaidThrough,
// the date is shortened by that refund's share once (ErrRefundShortened the
// second time), audited billing.paid_through_adjusted. A NEW refund of a
// charge whose provider RecordsOwnRefunds needs the provider's refund id
// from an operator (ErrRefundProviderRef).
func RecordRefund(ctx context.Context, db *sql.DB, r Refund) (RefundResult, error) {
	var res RefundResult
	if strings.TrimSpace(r.Reason) == "" {
		return res, ErrReasonRequired
	}
	if r.AmountCents < 0 {
		return res, errors.New("refund: the amount must be positive")
	}
	now := time.Now().UTC()
	var provider string
	var before time.Time
	err := InTx(ctx, db, func(tx *sql.Tx) error {
		if err := Lock(ctx, tx, r.TenantID); err != nil {
			return err
		}
		chargeID, existing, err := refundTarget(ctx, tx, r)
		if err != nil {
			return err
		}
		var providerID string
		var amount int64
		var start, end sql.NullTime
		err = tx.QueryRowContext(ctx, `
			SELECT provider, provider_id, amount_cents, period_start, period_end FROM payments
			 WHERE id::text = $1 AND tenant_id = $2 AND kind = 'charge' AND status IN ('settled', 'refunded')
			 FOR UPDATE`, chargeID, r.TenantID).Scan(&provider, &providerID, &amount, &start, &end)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNoSuchPayment
		}
		if err != nil {
			return fmt.Errorf("read payment %s: %w", chargeID, err)
		}
		ref := r.ProviderRef
		if existing == nil && ref != "" {
			if existing, err = refundByRef(ctx, tx, provider, ref); err != nil {
				return err
			}
		}
		share := func(cents int64) (time.Duration, error) {
			if !start.Valid || !end.Valid || !end.Time.After(start.Time) || amount <= 0 {
				return 0, ErrRefundNoPeriod
			}
			return time.Duration(math.Round(float64(end.Time.Sub(start.Time)) * float64(cents) / float64(amount))), nil
		}

		if existing != nil {
			switch {
			case existing.refundOf != chargeID:
				return fmt.Errorf("%w: refund %s is of another charge", ErrRefundMismatch, existing.providerID)
			case r.FromProvider && r.AmountCents != 0 && r.AmountCents != existing.amount:
				// An operator recorded the refund first under its id with
				// another amount (RP30-7): the provider's notice never
				// silently confirms it.
				return fmt.Errorf("%w: refund %s is %d cents in the ledger, the provider says %d", ErrRefundMismatch,
					existing.providerID, existing.amount, r.AmountCents)
			case r.FromProvider:
				return fmt.Errorf("%w: %s/%s", ErrRefundRecorded, provider, existing.providerID)
			case r.AmountCents != 0 && r.AmountCents != existing.amount:
				return fmt.Errorf("%w: refund %s is %d cents, not %d", ErrRefundMismatch, existing.providerID, existing.amount, r.AmountCents)
			}
			res.PaymentID, res.InvoiceNumber, res.Existing = existing.id, existing.invoiceNumber, true
			r.AmountCents, r.ProviderRef = existing.amount, existing.providerID
			if !r.ShortenPaidThrough {
				return nil
			}
			if existing.shortened {
				return fmt.Errorf("%w: %s", ErrRefundShortened, existing.providerID)
			}
			by, err := share(existing.amount)
			if err != nil {
				return err
			}
			res.ShortenedBy = by
			before, res.PaidThrough, err = shortenPaidThrough(ctx, tx, r.TenantID, existing.id, by, r.Actor, now)
			return err
		}

		if r.AmountCents == 0 {
			return errors.New("refund: the amount must be positive")
		}
		if RecordsOwnRefunds(provider) && !r.FromProvider && !strings.HasPrefix(ref, stripeRefundPrefix) {
			return ErrRefundProviderRef
		}
		var refunded int64
		if err := tx.QueryRowContext(ctx, `
			SELECT COALESCE(SUM(amount_cents), 0) FROM payments WHERE refund_of::text = $1 AND kind = 'refund' AND status = 'settled'`,
			chargeID).Scan(&refunded); err != nil {
			return fmt.Errorf("sum refunds of %s: %w", chargeID, err)
		}
		if refunded+r.AmountCents > amount {
			return fmt.Errorf("%w: %d + %d > %d cents", ErrRefundExceeds, refunded, r.AmountCents, amount)
		}
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
		if _, err := tx.ExecContext(ctx, `UPDATE payments SET refund_of = $2::uuid WHERE id = $1::uuid`, settled.ID, chargeID); err != nil {
			return fmt.Errorf("link refund %s: %w", settled.ID, err)
		}
		res.PaymentID, res.InvoiceNumber = settled.ID, settled.InvoiceNumber
		if refunded+r.AmountCents == amount {
			if _, err := tx.ExecContext(ctx, `UPDATE payments SET status = 'refunded' WHERE id::text = $1`, chargeID); err != nil {
				return fmt.Errorf("mark %s refunded: %w", chargeID, err)
			}
			res.FullyRefunded = true
		}
		if r.ShortenPaidThrough {
			by, err := share(r.AmountCents)
			if err != nil {
				return err
			}
			res.ShortenedBy = by
			_, res.PaidThrough, err = shortenPaidThrough(ctx, tx, r.TenantID, settled.ID, by, r.Actor, now)
			return err
		}
		return nil
	})
	if err != nil {
		return res, err
	}
	if res.Existing {
		// Nothing new in the ledger; a shortening is a date change, audited
		// as one (the refund itself was audited when it was recorded).
		if r.ShortenPaidThrough {
			audit.Record(ctx, db, audit.Entry{Actor: r.Actor, TenantID: r.TenantID, EventType: "billing", Action: "billing.paid_through_adjusted",
				Resource: "tenant:" + r.TenantID, Metadata: map[string]any{"tenant": r.TenantID, "actor": r.Actor, "reason": r.Reason,
					"refund": res.PaymentID, "provider_ref": r.ProviderRef, "shortened": true, "shortened_by": res.ShortenedBy.String(),
					"paid_through_before": before.UTC().Format(time.RFC3339), "paid_through": res.PaidThrough.UTC().Format(time.RFC3339)}})
		}
		return res, nil
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

// recordedRefund is a refund row already in the ledger.
type recordedRefund struct {
	id, providerID, invoiceNumber, refundOf string
	amount                                  int64
	shortened                               bool
}

func scanRefund(row *sql.Row) (*recordedRefund, error) {
	var rr recordedRefund
	err := row.Scan(&rr.id, &rr.providerID, &rr.invoiceNumber, &rr.refundOf, &rr.amount, &rr.shortened)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &rr, nil
}

// refundTarget resolves r.PaymentID: the charge to refund, or — when it
// names one of the tenant's refund rows — that refund and its charge.
func refundTarget(ctx context.Context, tx *sql.Tx, r Refund) (chargeID string, existing *recordedRefund, err error) {
	existing, err = scanRefund(tx.QueryRowContext(ctx, `
		SELECT id::text, provider_id, COALESCE(invoice_number, ''), COALESCE(refund_of::text, ''), amount_cents, COALESCE(metadata ? 'shortened', false) FROM payments
		 WHERE id::text = $1 AND tenant_id = $2 AND kind = 'refund' AND status = 'settled' FOR UPDATE`, r.PaymentID, r.TenantID))
	if err != nil {
		return "", nil, fmt.Errorf("read payment %s: %w", r.PaymentID, err)
	}
	if existing != nil {
		return existing.refundOf, existing, nil
	}
	return r.PaymentID, nil, nil
}

// refundByRef finds the refund the provider knows as ref.
func refundByRef(ctx context.Context, tx *sql.Tx, provider, ref string) (*recordedRefund, error) {
	rr, err := scanRefund(tx.QueryRowContext(ctx, `
		SELECT id::text, provider_id, COALESCE(invoice_number, ''), COALESCE(refund_of::text, ''), amount_cents, COALESCE(metadata ? 'shortened', false) FROM payments
		 WHERE provider = $1 AND provider_id = $2 AND kind = 'refund' FOR UPDATE`, provider, ref))
	if err != nil {
		return nil, fmt.Errorf("find refund %s/%s: %w", provider, ref, err)
	}
	return rr, nil
}

// shortenPaidThrough takes `by` off the tenant's paid_through (the caller
// holds the entitlement lock) and stamps the refund row that did it
// (MetaShortened), so a refund shortens the date once. Returns the date
// before and after.
func shortenPaidThrough(ctx context.Context, tx *sql.Tx, tenantID, refundID string, by time.Duration, actor string, now time.Time) (before, after time.Time, err error) {
	err = tx.QueryRowContext(ctx, `SELECT paid_through FROM entitlements WHERE tenant_id = $1`, tenantID).Scan(&before)
	if errors.Is(err, sql.ErrNoRows) {
		return before, after, ErrNoEntitlement
	}
	if err != nil {
		return before, after, fmt.Errorf("read paid_through of tenant %s: %w", tenantID, err)
	}
	if err := tx.QueryRowContext(ctx, `
		UPDATE entitlements SET paid_through = paid_through - ($2 * INTERVAL '1 second'), updated_at = NOW()
		 WHERE tenant_id = $1 RETURNING paid_through`, tenantID, by.Seconds()).Scan(&after); err != nil {
		return before, after, fmt.Errorf("shorten paid_through of tenant %s: %w", tenantID, err)
	}
	stamp, err := json.Marshal(map[string]any{"seconds": by.Seconds(), "at": now.Format(time.RFC3339), "actor": actor})
	if err != nil {
		return before, after, fmt.Errorf("shortened stamp: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE payments SET metadata = jsonb_set(COALESCE(metadata, '{}'::jsonb), '{shortened}', $2::jsonb) WHERE id::text = $1`,
		refundID, string(stamp)); err != nil {
		return before, after, fmt.Errorf("stamp refund %s shortened: %w", refundID, err)
	}
	return before, after, nil
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
