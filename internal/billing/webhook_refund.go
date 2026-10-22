package billing

import (
	"context"
	"errors"
	"fmt"

	"github.com/FairForge/vaultaire/internal/payments"
	"github.com/stripe/stripe-go/v75"
	"github.com/stripe/stripe-go/v75/charge"
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

// chargeFetcher fetches a charge (tests replace it; nil = the API):
// charge.refund.updated carries a refund, whose charge names the customer
// and the invoice the ledger keys the charge by.
type chargeFetcher interface {
	Get(ctx context.Context, id string) (*stripe.Charge, error)
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
// takes paid time away is the operator's call (the admin refund form, which
// names the refund this records — RP28-1). Counted under the charge's
// provider (RecordRefund, RP24-3); audited billing.refund_recorded with
// actor stripe-webhook.
//
// A refund is not lost to delivery order or to being pending (RP28-2): a
// charge the ledger does not know yet has its refunds recorded when its
// invoice's payment settles (recordInvoicePaid), and a refund pending here
// is recorded by the charge.refund.updated that reports it succeeded.
func (h *WebhookHandler) handleChargeRefunded(ctx context.Context, event stripe.Event) (string, error) {
	var ch stripe.Charge
	if !h.unmarshal(event, "charge", &ch) {
		return "", nil
	}
	return h.applyChargeRefunds(ctx, event, &ch)
}

// handleRefundUpdated is charge.refund.updated (RP28-2): a refund that
// succeeded is recorded through the same path as charge.refunded, from the
// charge fetched from Stripe and its refunds as Stripe lists them now
// (never the payload's status); any other status is a log line.
// Idempotent on the re_ id (ErrRefundRecorded).
func (h *WebhookHandler) handleRefundUpdated(ctx context.Context, event stripe.Event) (string, error) {
	var r stripe.Refund
	if !h.unmarshal(event, "refund", &r) {
		return "", nil
	}
	if r.Status != stripe.RefundStatusSucceeded {
		h.logger.Info("stripe webhook: refund updated, not succeeded — nothing recorded",
			zap.String("id", event.ID), zap.String("refund", r.ID), zap.String("status", string(r.Status)))
		return "", nil
	}
	if r.Charge == nil || r.Charge.ID == "" || h.stripe.db == nil {
		return "", nil
	}
	ch, err := h.fetchCharge(ctx, r.Charge.ID)
	if stripeMissing(err) {
		h.logger.Warn("stripe webhook: updated refund names a charge Stripe does not have — nothing recorded",
			zap.String("id", event.ID), zap.String("refund", r.ID), zap.String("charge", r.Charge.ID))
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("fetch charge %s of refund %s: %w", r.Charge.ID, r.ID, err)
	}
	ch.Refunds = nil // listed as Stripe has them now
	return h.applyChargeRefunds(ctx, event, ch)
}

func (h *WebhookHandler) fetchCharge(ctx context.Context, id string) (*stripe.Charge, error) {
	if h.charges != nil {
		return h.charges.Get(ctx, id)
	}
	params := &stripe.ChargeParams{}
	params.Context = ctx
	return charge.Get(id, params)
}

func (h *WebhookHandler) refundSource() refundLister {
	if h.refunds != nil {
		return h.refunds
	}
	return stripeRefunds{}
}

// applyChargeRefunds records the succeeded refunds of ch (its own list, else
// Stripe's) against the ledger's row of ch's invoice.
func (h *WebhookHandler) applyChargeRefunds(ctx context.Context, event stripe.Event, ch *stripe.Charge) (string, error) {
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
		h.logger.Warn("stripe webhook: refunded charge the ledger does not know yet — its refunds are recorded when its invoice's payment lands",
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
		if refunds, err = h.refundSource().ForCharge(ctx, ch.ID); err != nil {
			return tenantID, err
		}
	}
	return tenantID, h.recordRefunds(ctx, tenantID, paymentID, ch.ID, refunds)
}

// recordRefunds records each succeeded refund of the charge chargeID (the
// ledger row paymentID), keyed by its re_ id.
func (h *WebhookHandler) recordRefunds(ctx context.Context, tenantID, paymentID, chargeID string, refunds []*stripe.Refund) error {
	for _, r := range refunds {
		if r == nil || r.ID == "" || r.Amount <= 0 || r.Status != stripe.RefundStatusSucceeded {
			continue
		}
		res, err := payments.RecordRefund(ctx, h.stripe.db, payments.Refund{TenantID: tenantID, PaymentID: paymentID,
			AmountCents: r.Amount, ProviderRef: r.ID, FromProvider: true, Actor: webhookActor, Reason: refundReason,
			Note: "charge " + chargeID})
		switch {
		case errors.Is(err, payments.ErrRefundRecorded):
			h.logger.Info("stripe webhook: refund already in the ledger",
				zap.String("tenant", tenantID), zap.String("refund", r.ID))
		case errors.Is(err, payments.ErrRefundExceeds), errors.Is(err, payments.ErrRefundMismatch):
			if errors.Is(err, payments.ErrRefundMismatch) {
				// RP30-7: the ledger's refund under this id (an operator's,
				// usually) says another amount or another charge.
				payments.CountPayment(payments.ProviderStripe, payments.KindRefund, payments.OutcomeMismatch)
			}
			h.logger.Error("stripe webhook: refund disagrees with the ledger — acknowledged, not recorded",
				zap.String("tenant", tenantID), zap.String("refund", r.ID), zap.String("charge", chargeID), zap.Error(err))
		case err != nil:
			return fmt.Errorf("record refund %s of charge %s: %w", r.ID, chargeID, err)
		default:
			h.logger.Info("refund recorded",
				zap.String("tenant", tenantID), zap.String("refund", r.ID), zap.Int64("amount_cents", r.Amount),
				zap.String("credit_note", res.InvoiceNumber), zap.Bool("fully_refunded", res.FullyRefunded))
		}
	}
	return nil
}
