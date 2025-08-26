package billing

import (
	"context"
	"fmt"
	"time"
)

// Customer represents a billing customer
type Customer struct {
	UserID    string
	Email     string
	StripeID  string
	CreatedAt time.Time
}

// UsageRecord represents usage for billing
type UsageRecord struct {
	StorageGB   float64
	BandwidthGB float64
	Timestamp   time.Time
}

// Charges represents calculated charges
type Charges struct {
	StorageCents   int
	BandwidthCents int
	TotalCents     int
}

// Invoice represents a billing invoice
type Invoice struct {
	ID          string
	CustomerID  string
	AmountCents int
	Status      string
	CreatedAt   time.Time
}

// Payment represents a payment transaction
type Payment struct {
	ID          string
	CustomerID  string
	AmountCents int
	Status      string
	ProcessedAt time.Time
}

// BillingService handles billing operations
type BillingService struct {
	apiKey    string
	customers map[string]*Customer // In-memory for MVP
	invoices  map[string]*Invoice
	payments  map[string]*Payment
}

// NewBillingService creates a new billing service
func NewBillingService(apiKey string) *BillingService {
	return &BillingService{
		apiKey:    apiKey,
		customers: make(map[string]*Customer),
		invoices:  make(map[string]*Invoice),
		payments:  make(map[string]*Payment),
	}
}

// CreateCustomer creates a billing customer
func (b *BillingService) CreateCustomer(ctx context.Context, userID, email string) (*Customer, error) {
	// In production, this would call Stripe API
	// For MVP, simulate with in-memory storage
	
	customer := &Customer{
		UserID:    userID,
		Email:     email,
		StripeID:  fmt.Sprintf("cus_%d", time.Now().Unix()),
		CreatedAt: time.Now(),
	}
	
	b.customers[customer.StripeID] = customer
	
	return customer, nil
}

// RecordUsage records usage for billing
func (b *BillingService) RecordUsage(ctx context.Context, customerID string, usage *UsageRecord) error {
	// In production, this would create Stripe Usage Records
	// For MVP, we just validate the customer exists
	
	if _, exists := b.customers[customerID]; !exists {
		return fmt.Errorf("customer not found: %s", customerID)
	}
	
	// TODO: Store usage records for monthly billing
	
	return nil
}

// CalculateCharges calculates charges based on usage
func (b *BillingService) CalculateCharges(usage *UsageRecord) *Charges {
	charges := &Charges{}
	
	// Free tier: 5GB storage, 50GB bandwidth
	const (
		freeStorageGB   = 5.0
		freeBandwidthGB = 50.0
		// $3.99/TB = $0.00399/GB = ~$0.01/2.5GB
		storageRatePerGB   = 1 // 1 cent per GB over free tier
		bandwidthRatePerGB = 1 // 1 cent per GB over free tier
	)
	
	// Calculate storage charges
	if usage.StorageGB > freeStorageGB {
		overageGB := usage.StorageGB - freeStorageGB
		charges.StorageCents = int(overageGB * storageRatePerGB)
	}
	
	// Calculate bandwidth charges
	if usage.BandwidthGB > freeBandwidthGB {
		overageGB := usage.BandwidthGB - freeBandwidthGB
		charges.BandwidthCents = int(overageGB * bandwidthRatePerGB)
	}
	
	charges.TotalCents = charges.StorageCents + charges.BandwidthCents
	
	return charges
}

// CreateInvoice creates an invoice for charges
func (b *BillingService) CreateInvoice(ctx context.Context, customerID string, charges *Charges) (*Invoice, error) {
	// In production, this would create a Stripe Invoice
	
	invoice := &Invoice{
		ID:          fmt.Sprintf("inv_%d", time.Now().Unix()),
		CustomerID:  customerID,
		AmountCents: charges.TotalCents,
		Status:      "pending",
		CreatedAt:   time.Now(),
	}
	
	b.invoices[invoice.ID] = invoice
	
	return invoice, nil
}

// ProcessPayment processes a payment
func (b *BillingService) ProcessPayment(ctx context.Context, customerID, paymentMethodID string, amountCents int) (*Payment, error) {
	// In production, this would call Stripe Payment Intent API
	
	payment := &Payment{
		ID:          fmt.Sprintf("pay_%d", time.Now().Unix()),
		CustomerID:  customerID,
		AmountCents: amountCents,
		Status:      "succeeded", // Simulate success
		ProcessedAt: time.Now(),
	}
	
	b.payments[payment.ID] = payment
	
	return payment, nil
}

// GetCustomer retrieves a customer
func (b *BillingService) GetCustomer(ctx context.Context, customerID string) (*Customer, error) {
	customer, exists := b.customers[customerID]
	if !exists {
		return nil, fmt.Errorf("customer not found")
	}
	
	return customer, nil
}
