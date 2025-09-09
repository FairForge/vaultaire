// internal/usage/billing_integration_test.go
package usage

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBillingIntegration_CalculateUsageCharges(t *testing.T) {
	db := setupTestDB(t)
	defer func() { _ = db.Close() }()

	qm := NewQuotaManager(db)
	require.NoError(t, qm.InitializeSchema(context.Background()))

	bi := NewBillingIntegration(qm)
	require.NoError(t, bi.InitializeSchema(context.Background()))
	ctx := context.Background()

	// Create tenant with professional tier
	tenantID := "tenant-billing-1"
	require.NoError(t, qm.CreateTenant(ctx, tenantID, "professional", 107374182400)) // 100GB limit

	// Reserve 100GB (CheckAndReserve sets total usage to this amount)
	_, err := qm.CheckAndReserve(ctx, tenantID, 107374182400) // 100GB
	require.NoError(t, err)

	// Calculate charges
	charges, err := bi.CalculateUsageCharges(ctx, tenantID, time.Now().AddDate(0, -1, 0), time.Now())
	require.NoError(t, err)

	assert.Equal(t, tenantID, charges.TenantID)
	assert.Equal(t, "professional", charges.Tier)
	assert.Equal(t, 3.99, charges.BaseRate) // $3.99/TB

	// 100GB = 0.09765625 TB * $3.99/TB = $0.3896484375
	assert.InDelta(t, 0.3896, charges.TotalCharge, 0.01)
}

func TestBillingIntegration_HandleOverageBilling(t *testing.T) {
	db := setupTestDB(t)
	defer func() { _ = db.Close() }()

	qm := NewQuotaManager(db)
	require.NoError(t, qm.InitializeSchema(context.Background()))

	bi := NewBillingIntegration(qm)
	require.NoError(t, bi.InitializeSchema(context.Background()))
	ctx := context.Background()

	// Create tenant with 10GB limit
	tenantID := "tenant-overage-1"
	require.NoError(t, qm.CreateTenant(ctx, tenantID, "starter", 10737418240)) // 10GB

	// Enable overage billing
	require.NoError(t, bi.SetBillingPolicy(ctx, tenantID, BillingPolicyOverage))

	// Use 15GB (5GB overage)
	_, err := qm.CheckAndReserve(ctx, tenantID, 10737418240) // Use full 10GB
	require.NoError(t, err)

	// Try to use 5GB more (overage)
	overageBytes := int64(5368709120)
	charge, err := bi.ProcessOverageUsage(ctx, tenantID, overageBytes)
	require.NoError(t, err)

	assert.Equal(t, tenantID, charge.TenantID)
	assert.Equal(t, "overage", charge.ChargeType)
	assert.Equal(t, overageBytes, charge.OverageBytes)
	assert.InDelta(t, 0.05, charge.Amount, 0.01) // 5GB * $0.01/GB overage rate
}

func TestBillingIntegration_GenerateInvoice(t *testing.T) {
	db := setupTestDB(t)
	defer func() { _ = db.Close() }()

	qm := NewQuotaManager(db)
	require.NoError(t, qm.InitializeSchema(context.Background()))

	bi := NewBillingIntegration(qm)
	require.NoError(t, bi.InitializeSchema(context.Background()))
	ctx := context.Background()

	// Use unique tenant ID to avoid conflicts
	tenantID := "tenant-invoice-" + time.Now().Format("20060102150405")
	require.NoError(t, qm.CreateTenant(ctx, tenantID, "professional", 107374182400))

	// Use current time for charges so they fall within the date range
	now := time.Now()
	start := now.AddDate(0, 0, -1) // Yesterday

	// Record some charges
	_, err := bi.RecordCharge(ctx, tenantID, "base", 3.99, "Monthly base charge")
	require.NoError(t, err)
	_, err = bi.RecordCharge(ctx, tenantID, "overage", 0.50, "Overage charge")
	require.NoError(t, err)

	// Generate invoice for period including today
	invoice, err := bi.GenerateInvoice(ctx, tenantID, start, now.Add(time.Hour))
	require.NoError(t, err)

	assert.Equal(t, tenantID, invoice.TenantID)
	assert.Len(t, invoice.LineItems, 2)
	assert.Equal(t, 4.49, invoice.TotalAmount)
	assert.Equal(t, InvoiceStatusPending, invoice.Status)
}

func TestBillingIntegration_UsageCredits(t *testing.T) {
	db := setupTestDB(t)
	defer func() { _ = db.Close() }()

	qm := NewQuotaManager(db)
	require.NoError(t, qm.InitializeSchema(context.Background()))

	bi := NewBillingIntegration(qm)
	require.NoError(t, bi.InitializeSchema(context.Background()))
	ctx := context.Background()

	tenantID := "tenant-credit-1"
	require.NoError(t, qm.CreateTenant(ctx, tenantID, "professional", 107374182400))

	// Use some storage to generate charges
	_, err := qm.CheckAndReserve(ctx, tenantID, 10737418240) // 10GB
	require.NoError(t, err)

	// Apply credit BEFORE calculating charges
	credit, err := bi.ApplyCredit(ctx, tenantID, 10.00, "Promotional credit")
	require.NoError(t, err)
	assert.Equal(t, 10.00, credit.Amount)

	// Calculate charges with credit
	charges, err := bi.CalculateUsageCharges(ctx, tenantID, time.Now().AddDate(0, -1, 0), time.Now())
	require.NoError(t, err)

	// 10GB = 0.00976562 TB * $3.99 = ~$0.039
	// With $10 credit, total charge should be 0
	assert.True(t, charges.CreditsApplied > 0)
	assert.Equal(t, 0.0, charges.TotalCharge)
}
