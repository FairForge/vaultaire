// internal/usage/billing_integration.go
package usage

import (
	"context"
	"fmt"
	"time"
)

type BillingPolicy string

const (
	BillingPolicyStandard BillingPolicy = "standard"
	BillingPolicyOverage  BillingPolicy = "overage"
	BillingPolicyPrepaid  BillingPolicy = "prepaid"
)

type InvoiceStatus string

const (
	InvoiceStatusPending InvoiceStatus = "pending"
	InvoiceStatusPaid    InvoiceStatus = "paid"
	InvoiceStatusOverdue InvoiceStatus = "overdue"
)

type UsageCharge struct {
	TenantID       string    `json:"tenant_id"`
	Tier           string    `json:"tier"`
	Period         time.Time `json:"period"`
	StorageGB      float64   `json:"storage_gb"`
	BandwidthGB    float64   `json:"bandwidth_gb"`
	BaseRate       float64   `json:"base_rate"`
	BaseCharge     float64   `json:"base_charge"`
	OverageCharge  float64   `json:"overage_charge"`
	CreditsApplied float64   `json:"credits_applied"`
	TotalCharge    float64   `json:"total_charge"`
}

type OverageCharge struct {
	TenantID     string    `json:"tenant_id"`
	ChargeType   string    `json:"charge_type"`
	OverageBytes int64     `json:"overage_bytes"`
	Amount       float64   `json:"amount"`
	CreatedAt    time.Time `json:"created_at"`
}

type Invoice struct {
	ID          string        `json:"id"`
	TenantID    string        `json:"tenant_id"`
	Period      time.Time     `json:"period"`
	LineItems   []LineItem    `json:"line_items"`
	TotalAmount float64       `json:"total_amount"`
	Status      InvoiceStatus `json:"status"`
	DueDate     time.Time     `json:"due_date"`
	CreatedAt   time.Time     `json:"created_at"`
}

type LineItem struct {
	Description string  `json:"description"`
	Quantity    float64 `json:"quantity"`
	Rate        float64 `json:"rate"`
	Amount      float64 `json:"amount"`
}

type Credit struct {
	ID          string     `json:"id"`
	TenantID    string     `json:"tenant_id"`
	Amount      float64    `json:"amount"`
	Balance     float64    `json:"balance"`
	Description string     `json:"description"`
	ExpiresAt   *time.Time `json:"expires_at,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
}

type BillingIntegration struct {
	quotaManager *QuotaManager
	templates    *QuotaTemplates
	overageRate  float64 // $ per GB over limit
}

func NewBillingIntegration(qm *QuotaManager) *BillingIntegration {
	return &BillingIntegration{
		quotaManager: qm,
		templates:    NewQuotaTemplates(),
		overageRate:  0.01, // $0.01 per GB overage
	}
}

func (bi *BillingIntegration) InitializeSchema(ctx context.Context) error {
	schema := `
    CREATE TABLE IF NOT EXISTS billing_policies (
        tenant_id TEXT PRIMARY KEY REFERENCES tenant_quotas(tenant_id),
        policy VARCHAR(20) NOT NULL DEFAULT 'standard',
        overage_enabled BOOLEAN DEFAULT false,
        prepaid_balance DECIMAL(10,2) DEFAULT 0,
        created_at TIMESTAMP DEFAULT NOW(),
        updated_at TIMESTAMP DEFAULT NOW()
    );

    CREATE TABLE IF NOT EXISTS billing_charges (
        id SERIAL PRIMARY KEY,
        tenant_id TEXT NOT NULL REFERENCES tenant_quotas(tenant_id),
        charge_type VARCHAR(50) NOT NULL,
        amount DECIMAL(10,2) NOT NULL,
        description TEXT,
        period_start DATE,
        period_end DATE,
        created_at TIMESTAMP DEFAULT NOW()
    );

    CREATE TABLE IF NOT EXISTS billing_credits (
        id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
        tenant_id TEXT NOT NULL REFERENCES tenant_quotas(tenant_id),
        amount DECIMAL(10,2) NOT NULL,
        balance DECIMAL(10,2) NOT NULL,
        description TEXT,
        expires_at TIMESTAMP,
        created_at TIMESTAMP DEFAULT NOW()
    );

    CREATE TABLE IF NOT EXISTS invoices (
        id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
        tenant_id TEXT NOT NULL REFERENCES tenant_quotas(tenant_id),
        period DATE NOT NULL,
        total_amount DECIMAL(10,2) NOT NULL,
        status VARCHAR(20) NOT NULL DEFAULT 'pending',
        due_date DATE NOT NULL,
        created_at TIMESTAMP DEFAULT NOW()
    );

    CREATE INDEX IF NOT EXISTS idx_charges_tenant_period
        ON billing_charges(tenant_id, period_start, period_end);
    CREATE INDEX IF NOT EXISTS idx_credits_tenant_balance
        ON billing_credits(tenant_id, balance) WHERE balance > 0;
    `

	_, err := bi.quotaManager.db.ExecContext(ctx, schema)
	return err
}

func (bi *BillingIntegration) CalculateUsageCharges(ctx context.Context, tenantID string, start, end time.Time) (*UsageCharge, error) {
	// Get usage data
	used, limit, err := bi.quotaManager.GetUsage(ctx, tenantID)
	if err != nil {
		return nil, fmt.Errorf("getting usage: %w", err)
	}

	// Get tier info
	var tier string
	err = bi.quotaManager.db.QueryRowContext(ctx,
		"SELECT tier FROM tenant_quotas WHERE tenant_id = $1", tenantID).Scan(&tier)
	if err != nil {
		return nil, fmt.Errorf("getting tier: %w", err)
	}

	template, err := bi.templates.GetTemplate(tier)
	if err != nil {
		return nil, err
	}

	// Calculate charges
	storageGB := float64(used) / 1073741824
	storageTB := storageGB / 1024

	charge := &UsageCharge{
		TenantID:   tenantID,
		Tier:       tier,
		Period:     end,
		StorageGB:  storageGB,
		BaseRate:   template.PricePerTB,
		BaseCharge: storageTB * template.PricePerTB,
	}

	// Calculate overage if applicable
	if used > limit {
		overageBytes := used - limit
		overageGB := float64(overageBytes) / 1073741824
		charge.OverageCharge = overageGB * bi.overageRate
	}

	// Apply credits
	credits, err := bi.GetAvailableCredits(ctx, tenantID)
	if err == nil && credits > 0 {
		if credits >= charge.BaseCharge+charge.OverageCharge {
			charge.CreditsApplied = charge.BaseCharge + charge.OverageCharge
			charge.TotalCharge = 0
		} else {
			charge.CreditsApplied = credits
			charge.TotalCharge = charge.BaseCharge + charge.OverageCharge - credits
		}
	} else {
		charge.TotalCharge = charge.BaseCharge + charge.OverageCharge
	}

	return charge, nil
}

func (bi *BillingIntegration) SetBillingPolicy(ctx context.Context, tenantID string, policy BillingPolicy) error {
	query := `
        INSERT INTO billing_policies (tenant_id, policy, overage_enabled)
        VALUES ($1, $2, $3)
        ON CONFLICT (tenant_id)
        DO UPDATE SET policy = $2, overage_enabled = $3, updated_at = NOW()`

	overageEnabled := policy == BillingPolicyOverage
	_, err := bi.quotaManager.db.ExecContext(ctx, query, tenantID, policy, overageEnabled)
	return err
}

func (bi *BillingIntegration) ProcessOverageUsage(ctx context.Context, tenantID string, overageBytes int64) (*OverageCharge, error) {
	// Check if overage billing is enabled
	var overageEnabled bool
	err := bi.quotaManager.db.QueryRowContext(ctx,
		"SELECT overage_enabled FROM billing_policies WHERE tenant_id = $1", tenantID).Scan(&overageEnabled)

	if err != nil || !overageEnabled {
		return nil, fmt.Errorf("overage billing not enabled for tenant %s", tenantID)
	}

	// Calculate overage charge
	overageGB := float64(overageBytes) / 1073741824
	amount := overageGB * bi.overageRate

	charge := &OverageCharge{
		TenantID:     tenantID,
		ChargeType:   "overage",
		OverageBytes: overageBytes,
		Amount:       amount,
		CreatedAt:    time.Now(),
	}

	// Record the charge
	_, err = bi.RecordCharge(ctx, tenantID, "overage", amount, fmt.Sprintf("Overage: %.2f GB", overageGB))
	if err != nil {
		return nil, fmt.Errorf("recording overage charge: %w", err)
	}

	return charge, nil
}

func (bi *BillingIntegration) RecordCharge(ctx context.Context, tenantID, chargeType string, amount float64, description string) (int64, error) {
	query := `
        INSERT INTO billing_charges (tenant_id, charge_type, amount, description)
        VALUES ($1, $2, $3, $4)
        RETURNING id`

	var id int64
	err := bi.quotaManager.db.QueryRowContext(ctx, query,
		tenantID, chargeType, amount, description).Scan(&id)

	return id, err
}

func (bi *BillingIntegration) GenerateInvoice(ctx context.Context, tenantID string, start, end time.Time) (*Invoice, error) {
	// Get all charges for the period
	query := `
        SELECT charge_type, amount, description
        FROM billing_charges
        WHERE tenant_id = $1
        AND created_at >= $2
        AND created_at <= $3`

	rows, err := bi.quotaManager.db.QueryContext(ctx, query, tenantID, start, end)
	if err != nil {
		return nil, fmt.Errorf("querying charges: %w", err)
	}
	defer func() { _ = rows.Close() }()

	invoice := &Invoice{
		TenantID:  tenantID,
		Period:    end,
		Status:    InvoiceStatusPending,
		DueDate:   end.AddDate(0, 0, 30), // 30 days to pay
		CreatedAt: time.Now(),
	}

	var total float64
	for rows.Next() {
		var chargeType, description string
		var amount float64

		if err := rows.Scan(&chargeType, &amount, &description); err != nil {
			continue
		}

		invoice.LineItems = append(invoice.LineItems, LineItem{
			Description: description,
			Quantity:    1,
			Rate:        amount,
			Amount:      amount,
		})

		total += amount
	}

	invoice.TotalAmount = total

	// Store invoice
	query = `
        INSERT INTO invoices (tenant_id, period, total_amount, status, due_date)
        VALUES ($1, $2, $3, $4, $5)
        RETURNING id`

	err = bi.quotaManager.db.QueryRowContext(ctx, query,
		tenantID, end, total, invoice.Status, invoice.DueDate).Scan(&invoice.ID)

	return invoice, err
}

func (bi *BillingIntegration) ApplyCredit(ctx context.Context, tenantID string, amount float64, description string) (*Credit, error) {
	credit := &Credit{
		TenantID:    tenantID,
		Amount:      amount,
		Balance:     amount,
		Description: description,
		CreatedAt:   time.Now(),
	}

	query := `
        INSERT INTO billing_credits (tenant_id, amount, balance, description)
        VALUES ($1, $2, $3, $4)
        RETURNING id`

	err := bi.quotaManager.db.QueryRowContext(ctx, query,
		tenantID, amount, amount, description).Scan(&credit.ID)

	return credit, err
}

func (bi *BillingIntegration) GetAvailableCredits(ctx context.Context, tenantID string) (float64, error) {
	var total float64
	query := `
        SELECT COALESCE(SUM(balance), 0)
        FROM billing_credits
        WHERE tenant_id = $1 AND balance > 0
        AND (expires_at IS NULL OR expires_at > NOW())`

	err := bi.quotaManager.db.QueryRowContext(ctx, query, tenantID).Scan(&total)
	return total, err
}
