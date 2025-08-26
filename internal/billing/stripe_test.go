package billing

import (
	"context"
	"testing"
	"time"
)

func TestBillingService_CreateCustomer(t *testing.T) {
	service := NewBillingService("test_key")
	
	customer, err := service.CreateCustomer(context.Background(), "user-1", "test@example.com")
	if err != nil {
		t.Fatalf("Failed to create customer: %v", err)
	}
	
	if customer.UserID != "user-1" {
		t.Errorf("Expected user ID user-1, got %s", customer.UserID)
	}
	
	if customer.Email != "test@example.com" {
		t.Errorf("Expected email test@example.com, got %s", customer.Email)
	}
	
	if customer.StripeID == "" {
		t.Error("Expected Stripe ID to be set")
	}
}

func TestBillingService_RecordUsage(t *testing.T) {
	service := NewBillingService("test_key")
	
	// Create customer first
	customer, _ := service.CreateCustomer(context.Background(), "user-1", "test@example.com")
	
	// Record usage
	err := service.RecordUsage(context.Background(), customer.StripeID, &UsageRecord{
		StorageGB:     2.5,
		BandwidthGB:   10.0,
		Timestamp:     time.Now(),
	})
	
	if err != nil {
		t.Fatalf("Failed to record usage: %v", err)
	}
}

func TestBillingService_CalculateCharges(t *testing.T) {
	service := NewBillingService("test_key")
	
	// Test free tier (no charges)
	charges := service.CalculateCharges(&UsageRecord{
		StorageGB:   2.0, // Under 5GB free
		BandwidthGB: 10.0, // Under 50GB free
	})
	
	if charges.TotalCents != 0 {
		t.Errorf("Expected 0 charges for free tier, got %d", charges.TotalCents)
	}
	
	// Test overage charges
	charges = service.CalculateCharges(&UsageRecord{
		StorageGB:   10.0, // 5GB over = $0.05
		BandwidthGB: 100.0, // 50GB over = $0.50
	})
	
	expectedCents := 55 // $0.55
	if charges.TotalCents != expectedCents {
		t.Errorf("Expected %d cents, got %d", expectedCents, charges.TotalCents)
	}
}

func TestBillingService_CreateInvoice(t *testing.T) {
	service := NewBillingService("test_key")
	
	customer, _ := service.CreateCustomer(context.Background(), "user-1", "test@example.com")
	
	invoice, err := service.CreateInvoice(context.Background(), customer.StripeID, &Charges{
		StorageCents:   10,
		BandwidthCents: 45,
		TotalCents:     55,
	})
	
	if err != nil {
		t.Fatalf("Failed to create invoice: %v", err)
	}
	
	if invoice.AmountCents != 55 {
		t.Errorf("Expected invoice amount 55 cents, got %d", invoice.AmountCents)
	}
	
	if invoice.Status != "pending" {
		t.Errorf("Expected pending status, got %s", invoice.Status)
	}
}

func TestBillingService_ProcessPayment(t *testing.T) {
	service := NewBillingService("test_key")
	
	payment, err := service.ProcessPayment(context.Background(), "cus_test", "pm_test", 399)
	if err != nil {
		t.Fatalf("Failed to process payment: %v", err)
	}
	
	if payment.AmountCents != 399 {
		t.Errorf("Expected payment amount 399 cents, got %d", payment.AmountCents)
	}
	
	if payment.Status != "succeeded" {
		t.Errorf("Expected succeeded status, got %s", payment.Status)
	}
}
