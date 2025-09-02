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

	// Define the missing variables
	customerID := "cus_test123"
	successURL := "https://stored.ge/success"
	cancelURL := "https://stored.ge/cancel"

	sessionURL, err := service.CreateCheckoutSession(customerID, successURL, cancelURL)
	if err != nil {
		t.Fatalf("Failed to create checkout session: %v", err)
	}

	if sessionURL == "" {
		t.Error("Expected checkout URL")
	}
}
