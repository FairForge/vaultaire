package billing

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"

	"github.com/FairForge/vaultaire/internal/payments"
	"github.com/stripe/stripe-go/v75"
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
// payments.ErrRefGone. Never an immediate cancel — the period it was paid
// for runs out.
func (s *StripeService) StopRenewalRef(ctx context.Context, tenantID, ref string) error {
	if !s.configured {
		return payments.ErrProviderUnconfigured
	}
	sub, err := s.subs().Get(ctx, ref)
	if stripeMissing(err) {
		return fmt.Errorf("subscription %s of tenant %s: %w", ref, tenantID, payments.ErrRefGone)
	}
	if err != nil {
		return fmt.Errorf("read subscription %s: %w", ref, err)
	}
	switch {
	case sub.Status == stripe.SubscriptionStatusCanceled || sub.Status == stripe.SubscriptionStatusIncompleteExpired:
		return fmt.Errorf("subscription %s is %s: %w", ref, sub.Status, payments.ErrRefGone)
	case sub.CancelAtPeriodEnd:
		return nil
	}
	if _, err := subscription.Update(ref, renewalParams(ctx, true)); err != nil {
		return fmt.Errorf("set cancel_at_period_end on %s: %w", ref, err)
	}
	return nil
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
