package billing

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"

	"github.com/FairForge/vaultaire/internal/payments"
	"github.com/stripe/stripe-go/v75"
	"github.com/stripe/stripe-go/v75/invoice"
	"github.com/stripe/stripe-go/v75/subscription"
	"go.uber.org/zap"
)

// StripeService is the Stripe payments.Provider (WP-PAY-2, docs/PAYMENTS.md
// § 2.2): the dashboard and the deletion runner reach Stripe only through
// this interface, via payments.Registry.

var (
	_ payments.Provider          = (*StripeService)(nil)
	_ payments.RefRenewalStopper = (*StripeService)(nil)
)

// NewUnconfiguredStripe is the provider registered when STRIPE_SECRET_KEY
// is not set: never Ready (vaultaire_payment_provider_ready{provider=
// "stripe"} 0), every call payments.ErrProviderUnconfigured — so a page,
// a flag and the deletion runner can name Stripe on a box without a key.
func NewUnconfiguredStripe(logger *zap.Logger) *StripeService {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &StripeService{logger: logger}
}

// SetWebhook attaches the webhook handler the provider exposes (nil while
// STRIPE_WEBHOOK_SECRET is unset: no route).
func (s *StripeService) SetWebhook(h http.Handler) { s.webhook = h }

// Name is "stripe".
func (s *StripeService) Name() string { return payments.ProviderStripe }

// Renewal: a Stripe subscription renews by itself.
func (s *StripeService) Renewal() payments.Renewal { return payments.RenewalAuto }

// Ready = the key is set ∧ the six house prices match prices.json;
// Pending while the key is set and no verification has completed (RP22-1);
// Unready with no key, no price ids, a mismatch or a missing price.
func (s *StripeService) Ready(context.Context) (payments.Readiness, string) {
	if !s.configured {
		return payments.Unready, "STRIPE_SECRET_KEY is not set"
	}
	return s.HouseReadiness()
}

// StartCheckout opens a Stripe Checkout for the house: the customer is
// created here, lazily, the first time the tenant buys (the ONLY creation
// site — 43.2: no third party knows a tenant until it buys; idempotent on
// the tenant for Stripe's 24 h window), then HasLiveSubscription refuses a
// second subscription (R10-05, payments.ErrLiveSubscription), then the
// session (the purpose in its metadata).
func (s *StripeService) StartCheckout(ctx context.Context, c payments.Checkout) (string, error) {
	if !s.configured {
		return "", payments.ErrProviderUnconfigured
	}
	if err := ValidateHouseOrder(c.Order); err != nil {
		return "", fmt.Errorf("house order: %w", err)
	}
	customerID, err := s.GetCustomerID(ctx, c.TenantID)
	if errors.Is(err, errNoCustomer) {
		customerID, err = s.createCustomer(ctx, c.Email, c.TenantID)
	}
	if err != nil {
		return "", err
	}
	live, err := s.HasLiveSubscription(ctx, customerID)
	if err != nil {
		return "", err
	}
	if live {
		return "", fmt.Errorf("stripe customer %s: %w", customerID, payments.ErrLiveSubscription)
	}
	purpose := c.Purpose
	if purpose == "" {
		purpose = payments.PurposeNew
	}
	return s.CreateHouseCheckout(ctx, customerID, c.TenantID, c.Order, purpose, c.StartAt, c.ReturnURL, c.CancelURL)
}

// currentSubscription is the Stripe subscription that bills the tenant's
// house: the entitlement row's provider_ref, and nothing else (WP-PAY-7 —
// rows exist for every granted house since 097; "" = none).
func (s *StripeService) currentSubscription(ctx context.Context, tenantID string) (string, error) {
	if s.db == nil {
		return "", errors.New("no database")
	}
	var ref string
	err := s.db.QueryRowContext(ctx, `
		SELECT COALESCE(provider_ref, '') FROM entitlements
		 WHERE tenant_id = $1 AND provider = 'stripe'`, tenantID).Scan(&ref)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read subscription of tenant %s: %w", tenantID, err)
	}
	return ref, nil
}

// Resize changes the house's items in place (prorated); Applied is the
// house the subscription now bills, which the Registry records.
func (s *StripeService) Resize(ctx context.Context, tenantID string, o payments.Order) (payments.ResizeOutcome, error) {
	if !s.configured {
		return payments.ResizeOutcome{}, payments.ErrProviderUnconfigured
	}
	subID, err := s.currentSubscription(ctx, tenantID)
	if err != nil {
		return payments.ResizeOutcome{}, err
	}
	if subID == "" {
		return payments.ResizeOutcome{}, fmt.Errorf("tenant %s has no Stripe subscription to resize", tenantID)
	}
	sub, err := s.UpdateHouseSubscription(ctx, subID, o)
	if err != nil {
		return payments.ResizeOutcome{}, err
	}
	if got, ok := s.HouseFromSubscription(sub); ok {
		return payments.ResizeOutcome{Applied: &got}, nil
	}
	return payments.ResizeOutcome{}, nil
}

// renewalParams sets cancel_at_period_end: true stops the renewal (the
// house runs to the period's end), false resumes it.
func renewalParams(ctx context.Context, stop bool) *stripe.SubscriptionParams {
	params := &stripe.SubscriptionParams{CancelAtPeriodEnd: stripe.Bool(stop)}
	params.Context = ctx
	return params
}

func (s *StripeService) setRenewal(ctx context.Context, tenantID string, stop bool) error {
	if !s.configured {
		return payments.ErrProviderUnconfigured
	}
	subID, err := s.currentSubscription(ctx, tenantID)
	if err != nil {
		return err
	}
	if subID == "" {
		return fmt.Errorf("tenant %s has no Stripe subscription", tenantID)
	}
	if _, err := subscription.Update(subID, renewalParams(ctx, stop)); err != nil {
		return fmt.Errorf("set cancel_at_period_end=%t on %s: %w", stop, subID, err)
	}
	// The customer.subscription.updated that follows writes the
	// entitlement's renewal (applyHouse reads cancel_at_period_end).
	return nil
}

// StopRenewal sets cancel_at_period_end = true.
func (s *StripeService) StopRenewal(ctx context.Context, tenantID string) error {
	return s.setRenewal(ctx, tenantID, true)
}

// ResumeRenewal sets cancel_at_period_end = false.
func (s *StripeService) ResumeRenewal(ctx context.Context, tenantID string) error {
	return s.setRenewal(ctx, tenantID, false)
}

// StopRenewalRef sets cancel_at_period_end on the named subscription
// (payments.RefRenewalStopper, WP-PAY-5): one the entitlement no longer
// names because a BTCPay payment took the house. Read first, so a repeat is
// free: already stopping = nil; missing, cancelled or expired =
// payments.ErrRefGone. A paid period is never cancelled — it runs out.
//
// A subscription whose current period was never paid — past_due, unpaid,
// incomplete — is cancelled NOW instead (RP31-4): cancel_at_period_end
// would leave Stripe's dunning retrying the card, and a later success would
// bill the period the customer paid another way. No proration and no final
// invoice (stripe-go v75.11.0 subscription.go:633-643
// SubscriptionCancelParams.InvoiceNow / .Prorate; DELETE
// /v1/subscriptions/{id}, subscription/client.go:68-77); answered
// payments.ErrRefGone, so the renewal_stop row is closed gone.
func (s *StripeService) StopRenewalRef(ctx context.Context, tenantID, ref string) error {
	if !s.configured {
		return payments.ErrProviderUnconfigured
	}
	sub, err := s.subs().Get(ctx, ref)
	if stripeMissing(err) {
		return fmt.Errorf("subscription %s of tenant %s: %w", ref, tenantID, payments.ErrRefGone)
	}
	if err != nil {
		return fmt.Errorf("read subscription %s: %w", ref, refusedOr(err))
	}
	switch {
	case sub.Status == stripe.SubscriptionStatusCanceled:
		// A cancel-now whose void did not finish (the process died between
		// them, a Stripe 5xx) is finished by the next pass: the open invoices
		// it left are voided before the row is closed gone.
		if err := s.voidOpenInvoices(ctx, tenantID, ref); err != nil {
			return err
		}
		return fmt.Errorf("subscription %s is %s: %w", ref, sub.Status, payments.ErrRefGone)
	case sub.Status == stripe.SubscriptionStatusIncompleteExpired:
		return fmt.Errorf("subscription %s is %s: %w", ref, sub.Status, payments.ErrRefGone)
	case sub.Status == stripe.SubscriptionStatusPastDue || sub.Status == stripe.SubscriptionStatusUnpaid ||
		sub.Status == stripe.SubscriptionStatusIncomplete:
		if _, err := subscription.Cancel(ref, &stripe.SubscriptionCancelParams{Params: stripe.Params{Context: ctx},
			InvoiceNow: stripe.Bool(false), Prorate: stripe.Bool(false)}); err != nil {
			return fmt.Errorf("cancel unpaid subscription %s (%s): %w", ref, sub.Status, refusedOr(err))
		}
		s.logger.Warn("stripe: a subscription whose period was never paid is cancelled now — the house is paid another way",
			zap.String("tenant", tenantID), zap.String("subscription", ref), zap.String("status", string(sub.Status)))
		if err := s.voidOpenInvoices(ctx, tenantID, ref); err != nil {
			return err
		}
		return fmt.Errorf("subscription %s was %s, cancelled now: %w", ref, sub.Status, payments.ErrRefGone)
	case sub.CancelAtPeriodEnd:
		return nil
	}
	if _, err := subscription.Update(ref, renewalParams(ctx, true)); err != nil {
		return fmt.Errorf("set cancel_at_period_end on %s: %w", ref, refusedOr(err))
	}
	return nil
}

// voidOpenInvoices voids every OPEN invoice of the subscription ref
// (RP37-3): a cancel only turns Stripe's automatic collection off — "When
// you cancel a subscription, all open and draft invoices for that
// subscription have their auto_advance property set to false … You can
// still manually attempt to collect payment" (docs.stripe.com/billing/
// subscriptions/cancel) — so the past_due invoice the dunning mails link
// to stays payable, and a card payment of it after the customer chose to
// pay another way is the double charge docs/PAYMENTS.md § 4.4 forbids. A
// void is final and keeps the paper trail. A failure is returned (never
// ErrRefGone), so the stop stays owed and the next pass — which finds the
// subscription canceled — voids what is left.
func (s *StripeService) voidOpenInvoices(ctx context.Context, tenantID, ref string) error {
	params := &stripe.InvoiceListParams{Subscription: stripe.String(ref), Status: stripe.String(string(stripe.InvoiceStatusOpen))}
	params.Context = ctx
	var open []string
	iter := invoice.List(params)
	for iter.Next() {
		open = append(open, iter.Invoice().ID)
	}
	if err := iter.Err(); err != nil {
		return fmt.Errorf("list the open invoices of %s: %w", ref, refusedOr(err))
	}
	for _, id := range open {
		if _, err := invoice.VoidInvoice(id, &stripe.InvoiceVoidInvoiceParams{Params: stripe.Params{Context: ctx}}); err != nil {
			return fmt.Errorf("void open invoice %s of %s: %w", id, ref, refusedOr(err))
		}
		s.logger.Warn("stripe: the open invoice of a subscription cancelled for a switch is voided — it can no longer be paid",
			zap.String("tenant", tenantID), zap.String("subscription", ref), zap.String("invoice", id))
	}
	return nil
}

// refusedOr marks a PERMANENT Stripe answer — a 4xx other than 404 (gone,
// handled by the caller), 408, 409 (an idempotency / lock conflict) and
// 429 — as payments.ErrProviderRefused (RP33-3: SwitchToPrepaid reverts
// only on it; a 5xx, a timeout or no answer leaves the stop to the
// renewal_stops job).
func refusedOr(err error) error {
	var se *stripe.Error
	if errors.As(err, &se) && se.HTTPStatusCode >= 400 && se.HTTPStatusCode < 500 {
		switch se.HTTPStatusCode {
		case http.StatusNotFound, http.StatusRequestTimeout, http.StatusConflict, http.StatusTooManyRequests:
		default:
			return fmt.Errorf("%w: %w", payments.ErrProviderRefused, err)
		}
	}
	return err
}

// Cancel cancels the tenant's subscription now (cancelRef: idempotent). A
// tenant with no subscription has nothing to cancel.
func (s *StripeService) Cancel(ctx context.Context, tenantID string) error {
	if !s.configured {
		return payments.ErrProviderUnconfigured
	}
	subID, err := s.currentSubscription(ctx, tenantID)
	if err != nil {
		return err
	}
	if subID == "" {
		return nil
	}
	return s.cancelRef(ctx, tenantID, subID)
}

// CancelRef cancels the named subscription now (payments.RefCanceller —
// the deletion runner names every subscription the record knows: the
// entitlement's and an invalid house's). Idempotent.
func (s *StripeService) CancelRef(ctx context.Context, tenantID, ref string) error {
	if !s.configured {
		return payments.ErrProviderUnconfigured
	}
	return s.cancelRef(ctx, tenantID, ref)
}

// ManageURL is a Billing Portal session.
func (s *StripeService) ManageURL(ctx context.Context, tenantID, returnURL string) (string, error) {
	if !s.configured {
		return "", payments.ErrProviderUnconfigured
	}
	return s.CreateBillingPortalSession(ctx, tenantID, returnURL)
}

// Webhook is the signed /webhook/stripe handler (nil without its secret).
func (s *StripeService) Webhook() http.Handler { return s.webhook }
