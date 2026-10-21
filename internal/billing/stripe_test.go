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
}

func TestGetCustomerID_NoDB(t *testing.T) {
	svc := NewStripeService("sk_test_fake", nil, zap.NewNop())
	_, err := svc.GetCustomerID(t.Context(), "tenant-1")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "no database")
}

func TestCancel_NoDB(t *testing.T) {
	svc := NewStripeService("sk_test_fake", nil, zap.NewNop())
	err := svc.Cancel(t.Context(), "tenant-1")
	assert.Error(t, err)
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
	customerID, err := svc.createCustomer(t.Context(), "test@stored.ge", "tenant-test-123")
	require.NoError(t, err)
	assert.NotEmpty(t, customerID)
	assert.Contains(t, customerID, "cus_")
}
