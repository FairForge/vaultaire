package billing

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestNewStripeService(t *testing.T) {
	svc := NewStripeService("sk_test_fake", nil, zap.NewNop())
	require.NotNil(t, svc)
	assert.NotNil(t, svc.plans)
}

func TestRegisterAndGetPlan(t *testing.T) {
	svc := NewStripeService("sk_test_fake", nil, zap.NewNop())

	svc.RegisterPlan(Plan{
		ID:        "vault3",
		PriceID:   "price_vault3_test",
		Name:      "Vault 3TB",
		PriceFmt:  "$2.99/mo",
		StorageTB: 3,
	})
	svc.RegisterPlan(Plan{
		ID:        "standard",
		PriceID:   "price_standard_test",
		Name:      "Standard",
		PriceFmt:  "$3.99/TB/mo",
		StorageTB: 0,
	})

	p, ok := svc.GetPlan("vault3")
	assert.True(t, ok)
	assert.Equal(t, "price_vault3_test", p.PriceID)
	assert.Equal(t, int64(3), p.StorageTB)

	_, ok = svc.GetPlan("nonexistent")
	assert.False(t, ok)

	plans := svc.Plans()
	assert.Len(t, plans, 2)
}

func TestGetCustomerID_NoDB(t *testing.T) {
	svc := NewStripeService("sk_test_fake", nil, zap.NewNop())
	_, err := svc.GetCustomerID(t.Context(), "tenant-1")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "no database")
}

func TestGetSubscription_NoDB(t *testing.T) {
	svc := NewStripeService("sk_test_fake", nil, zap.NewNop())
	_, err := svc.GetSubscription(t.Context(), "tenant-1")
	assert.Error(t, err)
}

func TestCancelSubscription_NoDB(t *testing.T) {
	svc := NewStripeService("sk_test_fake", nil, zap.NewNop())
	err := svc.CancelSubscription(t.Context(), "tenant-1")
	assert.Error(t, err)
}

func TestCreateCheckoutSession_UnknownPlan(t *testing.T) {
	svc := NewStripeService("sk_test_fake", nil, zap.NewNop())
	_, err := svc.CreateCheckoutSession("cus_test", "nonexistent", "http://ok", "http://cancel")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "unknown plan")
}

func TestInvoiceDateFmt(t *testing.T) {
	got := InvoiceDateFmt(1704067200)
	// Should produce a formatted date string (timezone may vary).
	assert.Regexp(t, `^[A-Z][a-z]{2} \d{1,2}, \d{4}$`, got)
}

// Integration tests — only run with real Stripe test key.

func TestStripeService_CreateCustomer_Integration(t *testing.T) {
	if os.Getenv("STRIPE_TEST_KEY") == "" {
		t.Skip("STRIPE_TEST_KEY not set")
	}

	svc := NewStripeService(os.Getenv("STRIPE_TEST_KEY"), nil, zap.NewNop())
	customerID, err := svc.CreateCustomer(t.Context(), "test@stored.ge", "tenant-test-123")
	require.NoError(t, err)
	assert.NotEmpty(t, customerID)
	assert.Contains(t, customerID, "cus_")
}
