package billing

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/stripe/stripe-go/v75"
	"go.uber.org/zap"
)

// Stage 1a Prompt 27 Part 0 — RP33-9 (note): a Stripe invoice with
// billing_reason subscription_update is filed as a resize and skipped by
// the double-period detector. That is true only while the customer portal
// cannot change a plan or an interval, so the hourly price assert asserts
// the portal's default configuration too: features.subscription_update
// enabled = checkout closed, like a price that does not match.

// portalPrices is a price source that also answers the portal's default
// configuration (stripePrices does both).
type portalPrices struct {
	fakePrices
	cfg *stripe.BillingPortalConfiguration
	err error
}

func (p portalPrices) DefaultPortalConfiguration(context.Context) (*stripe.BillingPortalConfiguration, error) {
	return p.cfg, p.err
}

func portalWithUpdates(enabled bool) *stripe.BillingPortalConfiguration {
	return &stripe.BillingPortalConfiguration{ID: "bpc_test", IsDefault: true, Features: &stripe.BillingPortalConfigurationFeatures{
		SubscriptionUpdate: &stripe.BillingPortalConfigurationFeaturesSubscriptionUpdate{Enabled: enabled}}}
}

func TestVerifyHousePrices_PortalAllowsPlanChanges_Unready(t *testing.T) {
	// Arrange: the prices match; the portal lets a customer switch plans.
	svc := NewStripeService("sk_test_fake", nil, zap.NewNop())
	svc.ConfigureHouse(testIDs, portalPrices{fakePrices: goodPrices(), cfg: portalWithUpdates(true)})

	// Act
	err := svc.VerifyHousePrices(context.Background())

	// Assert
	require.Error(t, err)
	assert.Contains(t, err.Error(), "subscription_update")
	ready, why := svc.HouseCheckoutReady()
	assert.False(t, ready)
	assert.Contains(t, why, "bpc_test")

	// The portal fixed: ready again.
	svc.ConfigureHouse(testIDs, portalPrices{fakePrices: goodPrices(), cfg: portalWithUpdates(false)})
	require.NoError(t, svc.VerifyHousePrices(context.Background()))
	ready, _ = svc.HouseCheckoutReady()
	assert.True(t, ready)
}
