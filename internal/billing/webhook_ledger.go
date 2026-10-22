package billing

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/FairForge/vaultaire/internal/payments"
	"github.com/stripe/stripe-go/v75"
	"go.uber.org/zap"
)

// The payments ledger from Stripe's invoice events (WP-PAY-1,
// docs/PAYMENTS.md § 1.4). One row per invoice, keyed by its id:
// invoice.payment_failed records it 'failed' (informational — no
// entitlement change), invoice.payment_succeeded settles it (our gapless
// receipt number) and, for a house subscription, grants the house through
// the subscription's current period end — fetched, never the payload's
// (R10-15), idempotent with the customer.subscription.updated that Stripe
// sends for the same renewal. The amounts and the period are the
// invoice's own: a paid invoice is final at Stripe, so the payload of its
// payment event cannot be stale about them (design § 1.4 said "fetch the
// invoice"; the subscription is what can be stale, and it is fetched).

// invoicePurpose maps Stripe's billing_reason onto the ledger's purpose.
func invoicePurpose(r stripe.InvoiceBillingReason) string {
	switch r {
	case stripe.InvoiceBillingReasonSubscriptionCreate:
		return payments.PurposeNew
	case stripe.InvoiceBillingReasonSubscriptionUpdate:
		return payments.PurposeResize
	case stripe.InvoiceBillingReasonManual:
		return payments.PurposeManual
	}
	return payments.PurposeRenew
}

// invoicePeriod is what the invoice bought: the earliest line start to the
// latest line end (zero when no line carries one).
func invoicePeriod(inv *stripe.Invoice) (start, end time.Time) {
	if inv.Lines == nil {
		return
	}
	for _, l := range inv.Lines.Data {
		if l == nil || l.Period == nil {
			continue
		}
		if s := time.Unix(l.Period.Start, 0).UTC(); l.Period.Start > 0 && (start.IsZero() || s.Before(start)) {
			start = s
		}
		if e := time.Unix(l.Period.End, 0).UTC(); l.Period.End > 0 && e.After(end) {
			end = e
		}
	}
	return
}

func invoiceSubscriptionID(inv *stripe.Invoice) string {
	if inv.Subscription == nil {
		return ""
	}
	return inv.Subscription.ID
}

// invoicePayment is the ledger row an invoice event writes.
func invoicePayment(tenantID string, inv *stripe.Invoice, amount int64) payments.Payment {
	start, end := invoicePeriod(inv)
	ids := map[string]any{}
	if subID := invoiceSubscriptionID(inv); subID != "" {
		ids["subscription"] = subID
	}
	if inv.Customer != nil && inv.Customer.ID != "" {
		ids["customer"] = inv.Customer.ID
	}
	return payments.Payment{
		TenantID: tenantID, Provider: payments.ProviderStripe, ProviderID: inv.ID,
		Kind: payments.KindCharge, Purpose: invoicePurpose(inv.BillingReason), AmountCents: amount,
		PeriodStart: start, PeriodEnd: end, Metadata: map[string]any{payments.MetaIDs: ids},
	}
}

// recordInvoicePaid settles the invoice's ledger row and grants the house
// its subscription now describes, in one transaction.
func (h *WebhookHandler) recordInvoicePaid(ctx context.Context, event stripe.Event, tenantID string, inv *stripe.Invoice) error {
	if h.stripe.db == nil || inv.ID == "" {
		return nil
	}
	p := invoicePayment(tenantID, inv, inv.AmountPaid)
	// A $0 invoice (the subscription_create invoice of a trialing
	// subscription — a renew deferred with trial_end, RP24-1) is settled
	// for the record but spends no gapless receipt number and mails no
	// receipt: nothing was paid yet.
	p.Unnumbered = inv.AmountPaid == 0

	// The house the payment extends: the subscription as Stripe has it now.
	// An invalid house (RP20-4) is not granted; the ledger row is settled
	// all the same — the money was paid.
	var grant func(*sql.Tx) error
	var invalid error
	var invalidSub *stripe.Subscription
	var res stripeGrant
	var grantSub *stripe.Subscription
	if subID := invoiceSubscriptionID(inv); subID != "" && h.quotas != nil {
		sub, err := h.fetchSubscription(ctx, subID)
		switch {
		case stripeMissing(err):
			h.logger.Warn("stripe webhook: paid invoice names a subscription Stripe does not have — ledger only",
				zap.String("id", event.ID), zap.String("invoice", inv.ID), zap.String("subscription", subID))
		case err != nil:
			return fmt.Errorf("fetch subscription %s of paid invoice %s: %w", subID, inv.ID, err)
		default:
			if order, ok := h.stripe.HouseFromSubscription(sub); ok {
				o := order
				p.Order = &o
				// A subscription a switch took the house from never takes it
				// back (WP-PAY-5) — grantStripe reads that under the lock
				// (RP31-3): the payment is recorded — and the double-period
				// detector sees it — but nothing is granted.
				if sub.Status == stripe.SubscriptionStatusActive || sub.Status == stripe.SubscriptionStatusTrialing {
					paidThrough := h.paidThrough(sub)
					renewal := payments.RenewalAuto
					if sub.CancelAtPeriodEnd {
						renewal = payments.RenewalPrepaid
					}
					grantSub = sub
					grant = func(tx *sql.Tx) error {
						r, err := grantStripe(ctx, tx, tenantID, order, paidThrough, sub, renewal)
						if errors.Is(err, payments.ErrInvalidOrder) {
							// Refused before any statement but the lock: the
							// transaction is intact.
							invalid, invalidSub = err, sub
							return nil
						}
						res = r
						return err
					}
				}
			}
		}
	}

	// Refunds Stripe made before this payment reached the ledger (RP28-2:
	// charge.refunded delivered first was acknowledged with nothing to
	// attach to). Listed only while the row is not settled yet — one Stripe
	// call, and a listing that fails answers 500 before anything is written
	// — and recorded only when this delivery newly settles it.
	chargeID := ""
	if inv.Charge != nil {
		chargeID = inv.Charge.ID
	}
	var early []*stripe.Refund
	if chargeID != "" && inv.AmountPaid > 0 {
		_, err := payments.ChargeOf(ctx, h.stripe.db, tenantID, payments.ProviderStripe, inv.ID)
		switch {
		case errors.Is(err, payments.ErrNoSuchPayment):
			if early, err = h.refundSource().ForCharge(ctx, chargeID); err != nil {
				return fmt.Errorf("list refunds of charge %s of paid invoice %s: %w", chargeID, inv.ID, err)
			}
		case err != nil:
			return err
		}
	}

	at := time.Now()
	if inv.StatusTransitions != nil && inv.StatusTransitions.PaidAt > 0 {
		at = time.Unix(inv.StatusTransitions.PaidAt, 0)
	}
	var settled payments.Settled
	if err := payments.InTx(ctx, h.stripe.db, func(tx *sql.Tx) error {
		var err error
		if settled, err = payments.Settle(ctx, tx, p, at); err != nil {
			return err
		}
		if grant != nil {
			return grant(tx)
		}
		return nil
	}); err != nil {
		return fmt.Errorf("record paid invoice %s for tenant %s: %w", inv.ID, tenantID, err)
	}
	if invalid != nil {
		h.houseInvalid(ctx, tenantID, invalidSub, *p.Order, invalid)
	} else if grant != nil && res.away {
		h.afterSwitchedAway(ctx, tenantID, grantSub, res)
	} else if grant != nil {
		h.auditGrant(ctx, tenantID, res.Granted, invoiceSubscriptionID(inv))
		if res.uncancelled {
			h.afterUncancel(ctx, tenantID, invoiceSubscriptionID(inv))
		}
	}
	if settled.Newly && len(early) > 0 {
		// After the commit: a failure here is not retried by Stripe (the
		// redelivery is a replay), so it is an Error naming the refunds —
		// the operator records them with the admin form under their re_ ids.
		if err := h.recordRefunds(ctx, tenantID, settled.ID, chargeID, early); err != nil {
			h.logger.Error("stripe webhook: refunds of a newly settled charge not recorded — record them by hand",
				zap.String("tenant", tenantID), zap.String("invoice", inv.ID), zap.String("charge", chargeID), zap.Error(err))
		}
	}
	if err := payments.ReportDoublePeriod(ctx, h.stripe.db, h.receipts, tenantID, settled); err != nil {
		h.logger.Warn("stripe webhook: double period not mailed to the operator", zap.String("tenant", tenantID), zap.Error(err))
	}
	if settled.DoublePeriod != nil {
		h.logger.Error("stripe webhook: the customer paid twice for overlapping dates — granted; refund one by hand",
			zap.String("tenant", tenantID), zap.String("invoice", inv.ID), zap.String("other", settled.DoublePeriod.OtherProvider+":"+settled.DoublePeriod.OtherProviderID))
	}
	outcome := payments.OutcomeSettled
	if !settled.Newly {
		outcome = payments.OutcomeReplayed
	}
	payments.CountPayment(payments.ProviderStripe, payments.KindCharge, outcome)
	// The receipt of a NEWLY settled, NUMBERED row (a replay or a $0 trial
	// invoice sends nothing): after the commit, best effort — a mail that
	// fails is one Warn, never a 500.
	if settled.Newly && settled.InvoiceNumber != "" && h.receipts != nil {
		mctx, cancel := payments.Detached(ctx)
		if err := h.receipts.SendReceipt(mctx, tenantID, settled.ID); err != nil {
			h.logger.Warn("stripe webhook: receipt not sent", zap.String("tenant", tenantID),
				zap.String("invoice_number", settled.InvoiceNumber), zap.Error(err))
		}
		cancel()
	}
	h.logger.Info("payment recorded",
		zap.String("tenant", tenantID), zap.String("invoice", inv.ID),
		zap.String("invoice_number", settled.InvoiceNumber), zap.Bool("replay", !settled.Newly),
		zap.Int64("amount_cents", inv.AmountPaid), zap.Bool("house_extended", grant != nil && invalid == nil))
	return nil
}

// recordInvoiceFailed records the invoice's failed charge (no entitlement
// change: past_due keeps the date where the last payment left it).
func (h *WebhookHandler) recordInvoiceFailed(ctx context.Context, tenantID string, inv *stripe.Invoice) error {
	if h.stripe.db == nil || inv.ID == "" {
		return nil
	}
	if err := payments.InTx(ctx, h.stripe.db, func(tx *sql.Tx) error {
		_, err := payments.Fail(ctx, tx, invoicePayment(tenantID, inv, inv.AmountDue))
		return err
	}); err != nil {
		return fmt.Errorf("record failed invoice %s for tenant %s: %w", inv.ID, tenantID, err)
	}
	payments.CountPayment(payments.ProviderStripe, payments.KindCharge, payments.OutcomeFailed)
	return nil
}
