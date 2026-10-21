package billing

import (
	"context"
	"errors"
	"fmt"

	"github.com/FairForge/vaultaire/internal/payments"
	"github.com/stripe/stripe-go/v75"
	"github.com/stripe/stripe-go/v75/refund"
	"go.uber.org/zap"
)

// refundLister lists a charge's refunds (tests replace it; nil = the API).
// Since API version 2022-11-15 a charge object carries no refunds unless
// expanded, so the webhook's charge usually names none and they are asked
// of Stripe.
type refundLister interface {
	ForCharge(ctx context.Context, chargeID string) ([]*stripe.Refund, error)
}

type stripeRefunds struct{}

func (stripeRefunds) ForCharge(ctx context.Context, chargeID string) ([]*stripe.Refund, error) {
	params := &stripe.RefundListParams{Charge: stripe.String(chargeID)}
	params.Context = ctx
	params.Limit = stripe.Int64(100)
	var out []*stripe.Refund
	it := refund.List(params)
	for it.Next() {
		out = append(out, it.Refund())
	}
	if err := it.Err(); err != nil {
		return nil, fmt.Errorf("list refunds of charge %s: %w", chargeID, err)
	}
	return out, nil
}

// refundReason is the ledger's reason for a refund the webhook records.
const refundReason = "stripe charge.refunded"

// handleChargeRefunded records the refunds of a charge in the ledger
// (RP23-4, docs/PAYMENTS.md § 7): the tenant from the customer, the charge
// row from the charge's invoice (the ledger keys a Stripe charge by its
// invoice id), and one payments.RecordRefund per succeeded refund, keyed by
// the refund id — a redelivery, a refund already recorded by hand under
// the same id, a charge the ledger does not know or an amount past the
// charge are acknowledged with a log line (200), never a 500 Stripe would
// retry for three days. paid_through is not shortened: whether a refund
// takes paid time away is the operator's call (the admin refund form).
// Counted under the charge's provider (RecordRefund, RP24-3); audited
// billing.refund_recorded with actor stripe-webhook.
func (h *WebhookHandler) handleChargeRefunded(ctx context.Context, event stripe.Event) (string, error) {
	var ch stripe.Charge
	if !h.unmarshal(event, "charge", &ch) {
		return "", nil
	}
	customerID := ""
	if ch.Customer != nil {
		customerID = ch.Customer.ID
	}
	if customerID == "" || h.stripe.db == nil {
		return "", nil
	}
	tenantID, ours, err := h.resolveTenant(ctx, event, customerID, ch.Metadata["tenant_id"])
	if err != nil {
		return tenantID, fmt.Errorf("lookup tenant for refunded charge %s: %w", ch.ID, err)
	}
	if !ours {
		return tenantID, nil
	}
	invoiceID := ""
	if ch.Invoice != nil {
		invoiceID = ch.Invoice.ID
	}
	if invoiceID == "" {
		h.logger.Warn("stripe webhook: refunded charge names no invoice — not in the ledger, nothing recorded",
			zap.String("id", event.ID), zap.String("tenant", tenantID), zap.String("charge", ch.ID))
		return tenantID, nil
	}
	paymentID, err := payments.ChargeOf(ctx, h.stripe.db, tenantID, payments.ProviderStripe, invoiceID)
	if errors.Is(err, payments.ErrNoSuchPayment) {
		h.logger.Warn("stripe webhook: refunded charge the ledger does not know — acknowledged, nothing recorded",
			zap.String("id", event.ID), zap.String("tenant", tenantID), zap.String("charge", ch.ID), zap.String("invoice", invoiceID))
		return tenantID, nil
	}
	if err != nil {
		return tenantID, err
	}

	refunds := []*stripe.Refund(nil)
	if ch.Refunds != nil {
		refunds = ch.Refunds.Data
	}
	if len(refunds) == 0 {
		lister := h.refunds
		if lister == nil {
			lister = stripeRefunds{}
		}
		if refunds, err = lister.ForCharge(ctx, ch.ID); err != nil {
			return tenantID, err
		}
	}
	for _, r := range refunds {
		if r == nil || r.ID == "" || r.Amount <= 0 || r.Status != stripe.RefundStatusSucceeded {
			continue
		}
		res, err := payments.RecordRefund(ctx, h.stripe.db, payments.Refund{TenantID: tenantID, PaymentID: paymentID,
			AmountCents: r.Amount, ProviderRef: r.ID, Actor: webhookActor, Reason: refundReason,
			Note: "charge " + ch.ID})
		switch {
		case errors.Is(err, payments.ErrRefundRecorded):
			h.logger.Info("stripe webhook: refund already in the ledger",
				zap.String("tenant", tenantID), zap.String("refund", r.ID))
		case errors.Is(err, payments.ErrRefundExceeds):
			h.logger.Error("stripe webhook: refund exceeds what the ledger says was paid — acknowledged, not recorded",
				zap.String("tenant", tenantID), zap.String("refund", r.ID), zap.String("charge", ch.ID), zap.Error(err))
		case err != nil:
			return tenantID, fmt.Errorf("record refund %s of charge %s: %w", r.ID, ch.ID, err)
		default:
			h.logger.Info("refund recorded",
				zap.String("tenant", tenantID), zap.String("refund", r.ID), zap.Int64("amount_cents", r.Amount),
				zap.String("credit_note", res.InvoiceNumber), zap.Bool("fully_refunded", res.FullyRefunded))
		}
	}
	return tenantID, nil
}
