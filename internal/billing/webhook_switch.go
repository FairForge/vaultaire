package billing

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/FairForge/vaultaire/internal/audit"
	"github.com/FairForge/vaultaire/internal/payments"
	"github.com/stripe/stripe-go/v75"
	"go.uber.org/zap"
)

// Switching providers at the Stripe webhook (WP-PAY-5, docs/PAYMENTS.md
// § 4.1): a subscription a switch took the house from never takes it back,
// and the portal race withdraws a pending switch.

// grantStripe is the webhook's grant of a Stripe house (GrantRecorded) in
// tx, and the portal race: a grant that turns the entitlement's own
// subscription from renewal 'prepaid' back to 'auto' — the customer
// re-enabled it in Stripe's portal — withdraws a pending switch
// (pending_provider cleared in the same transaction); uncancelled tells the
// caller to invalidate the tenant's open prepaid invoices after the commit
// (afterUncancel).
func grantStripe(ctx context.Context, tx *sql.Tx, tenantID string, o payments.Order, paidThrough time.Time, subID string,
	renewal payments.Renewal) (g payments.Granted, uncancelled bool, err error) {
	g, err = payments.GrantRecorded(ctx, tx, tenantID, o, paidThrough, payments.ProviderStripe, subID, renewal)
	if err != nil {
		return g, false, err
	}
	prev := g.Previous
	if !g.Changed || prev == nil || prev.LapsedAt != nil || prev.Provider != payments.ProviderStripe || prev.ProviderRef != subID ||
		prev.Renewal != payments.RenewalPrepaid || renewal != payments.RenewalAuto {
		return g, false, nil
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE entitlements SET pending_provider = NULL WHERE tenant_id = $1 AND pending_provider IS NOT NULL`, tenantID); err != nil {
		return g, false, fmt.Errorf("withdraw the pending switch of tenant %s: %w", tenantID, err)
	}
	return g, true, nil
}

// afterUncancel marks the tenant's open prepaid invoices Invalid after the
// portal race's grant committed (docs/PAYMENTS.md § 4.1 step 5). Best
// effort: a refused or failed mark is the invalidator's Error + counter
// (RP30-4) — never a 500 here; an invoice left open expires in 30 minutes,
// and one paid anyway is the double-period detector's.
func (h *WebhookHandler) afterUncancel(ctx context.Context, tenantID, subID string) {
	h.logger.Warn("stripe webhook: the renewal was re-enabled at Stripe — a pending switch is withdrawn",
		zap.String("tenant", tenantID), zap.String("subscription", subID))
	if h.invoices == nil {
		return
	}
	mctx, cancel := payments.Detached(ctx)
	defer cancel()
	n, err := h.invoices.InvalidateOpenInvoices(mctx, tenantID, payments.ReasonPortalUncancel)
	if err != nil {
		h.logger.Error("stripe webhook: open prepaid invoices not invalidated after the portal un-cancel — mark them Invalid by hand",
			zap.String("tenant", tenantID), zap.Error(err))
		return
	}
	if n > 0 {
		h.logger.Info("stripe webhook: open prepaid invoices marked Invalid", zap.String("tenant", tenantID), zap.Int("marked", n))
	}
}

func (h *WebhookHandler) renewalStopper() payments.RefRenewalStopper {
	if h.stops != nil {
		return h.stops
	}
	return h.stripe
}

// switchedAway reports a subscription a provider switch took the house from
// (a renewal_stop row names it): a granting event of it never takes the
// house back (RP30-2 — the entitlement is another provider's). One that
// renews again (un-cancelled in the portal) has its stop asserted again:
// the row re-opened (pending) and the stop asked of Stripe now, audited
// billing.renewal_stop_reasserted — the period the customer prepaid is not
// billed twice (a renewal that lands anyway is the double-period
// detector's). A provider call that fails leaves the row pending for the
// renewal_stops job; never a 500.
func (h *WebhookHandler) switchedAway(ctx context.Context, tenantID string, sub *stripe.Subscription) (bool, error) {
	if h.stripe.db == nil {
		return false, nil
	}
	st, found, err := payments.RenewalStopOf(ctx, h.stripe.db, tenantID, payments.ProviderStripe, sub.ID)
	if err != nil || !found {
		return false, err
	}
	if sub.CancelAtPeriodEnd || st.Status == payments.RenewalStopGone {
		h.logger.Info("stripe webhook: event of a subscription a switch took the house from — nothing applied",
			zap.String("tenant", tenantID), zap.String("subscription", sub.ID), zap.String("status", string(sub.Status)))
		return true, nil
	}
	if err := payments.InTx(ctx, h.stripe.db, func(tx *sql.Tx) error {
		return payments.RecordRenewalStop(ctx, tx, tenantID, payments.ProviderStripe, sub.ID, st.Reason, payments.RenewalStopPending,
			map[string]any{"reasserted_at": time.Now().UTC().Format(time.RFC3339)})
	}); err != nil {
		return true, err
	}
	audit.Record(ctx, h.stripe.db, audit.Entry{TenantID: tenantID, EventType: "billing", Action: "billing.renewal_stop_reasserted",
		Resource: "tenant:" + tenantID, Metadata: map[string]any{"tenant": tenantID, "actor": webhookActor, "provider": payments.ProviderStripe,
			"subscription": sub.ID, "status": string(sub.Status), "reason": st.Reason}})
	h.logger.Warn("stripe webhook: a subscription a switch took the house from renews again — its stop is asserted again",
		zap.String("tenant", tenantID), zap.String("subscription", sub.ID))
	cerr := h.renewalStopper().StopRenewalRef(ctx, tenantID, sub.ID)
	status, last := payments.RenewalStopStopped, ""
	switch {
	case cerr == nil:
	case errors.Is(cerr, payments.ErrRefGone):
		status = payments.RenewalStopGone
	default:
		status, last = payments.RenewalStopPending, cerr.Error()
		h.logger.Warn("stripe webhook: the stop could not be asserted now — the renewal_stops job retries",
			zap.String("tenant", tenantID), zap.String("subscription", sub.ID), zap.Error(cerr))
	}
	if err := payments.MarkRenewalStop(ctx, h.stripe.db, payments.ProviderStripe, sub.ID, status, last); err != nil {
		h.logger.Warn("stripe webhook: renewal stop outcome not recorded", zap.String("subscription", sub.ID), zap.Error(err))
	}
	return true, nil
}
