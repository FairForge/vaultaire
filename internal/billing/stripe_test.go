package billing

import (
	"os"
	"testing"
)

func TestStripeService_CreateCustomer(t *testing.T) {
	// Skip if no real API key
	if os.Getenv("STRIPE_TEST_KEY") == "" {
		t.Skip("STRIPE_TEST_KEY not set")
	}

	service := &StripeService{
		apiKey: os.Getenv("STRIPE_TEST_KEY"),
	}

	customerID, err := service.CreateCustomer("user@example.com", "tenant-123")
	if err != nil {
		t.Fatalf("Failed to create customer: %v", err)
	}

	if customerID == "" {
		t.Error("Expected customer ID, got empty string")
	}
}

func TestStripeService_CreateCheckoutSession(t *testing.T) {
	if os.Getenv("STRIPE_TEST_KEY") == "" {
		t.Skip("STRIPE_TEST_KEY not set")
	}

	service := &StripeService{
		apiKey: os.Getenv("STRIPE_TEST_KEY"),
	}

	session, err := service.CreateCheckoutSession("cus_123", "price_1TB", "https://stored.ge/success")
	if err != nil {
		t.Fatalf("Failed to create session: %v", err)
	}

	if session.URL == "" {
		t.Error("Expected checkout URL")
	}
}
