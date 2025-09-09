// internal/usage/templates.go
package usage

import (
	"context"
	"fmt"
)

type QuotaTemplate struct {
	Name           string   `json:"name"`
	DisplayName    string   `json:"display_name"`
	StorageLimit   int64    `json:"storage_limit"`
	BandwidthLimit int64    `json:"bandwidth_limit"`
	MaxObjects     int64    `json:"max_objects"`
	MaxBuckets     int64    `json:"max_buckets"`
	PricePerTB     float64  `json:"price_per_tb"`
	Features       []string `json:"features"`
}

type QuotaTemplates struct {
	templates map[string]QuotaTemplate
}

func NewQuotaTemplates() *QuotaTemplates {
	return &QuotaTemplates{
		templates: map[string]QuotaTemplate{
			"starter": {
				Name:           "starter",
				DisplayName:    "Starter",
				StorageLimit:   1073741824,  // 1GB
				BandwidthLimit: 10737418240, // 10GB
				MaxObjects:     10000,
				MaxBuckets:     3,
				PricePerTB:     0, // Free tier
				Features: []string{
					"Basic S3 API",
					"99.9% uptime SLA",
					"Email support",
				},
			},
			"professional": {
				Name:           "professional",
				DisplayName:    "Professional",
				StorageLimit:   107374182400,  // 100GB
				BandwidthLimit: 1073741824000, // 1TB
				MaxObjects:     1000000,
				MaxBuckets:     50,
				PricePerTB:     3.99,
				Features: []string{
					"Full S3 API",
					"99.95% uptime SLA",
					"Priority support",
					"Object versioning",
					"Cross-region replication",
				},
			},
			"enterprise": {
				Name:           "enterprise",
				DisplayName:    "Enterprise",
				StorageLimit:   10737418240000,  // 10TB
				BandwidthLimit: 107374182400000, // 100TB
				MaxObjects:     -1,              // unlimited
				MaxBuckets:     -1,              // unlimited
				PricePerTB:     2.99,            // Volume discount
				Features: []string{
					"Full S3 API",
					"99.99% uptime SLA",
					"24/7 phone support",
					"Object versioning",
					"Cross-region replication",
					"Custom integrations",
					"Dedicated account manager",
				},
			},
			"custom": {
				Name:           "custom",
				DisplayName:    "Custom",
				StorageLimit:   -1, // negotiated
				BandwidthLimit: -1, // negotiated
				MaxObjects:     -1, // negotiated
				MaxBuckets:     -1, // negotiated
				PricePerTB:     -1, // negotiated
				Features: []string{
					"Everything in Enterprise",
					"Custom features",
					"White-label options",
					"SLA guarantees",
				},
			},
		},
	}
}

func (qt *QuotaTemplates) GetTemplate(tier string) (QuotaTemplate, error) {
	template, exists := qt.templates[tier]
	if !exists {
		return QuotaTemplate{}, fmt.Errorf("unknown tier: %s", tier)
	}
	return template, nil
}

func (qt *QuotaTemplates) ListTemplates() []QuotaTemplate {
	result := make([]QuotaTemplate, 0, len(qt.templates))
	for _, template := range qt.templates {
		result = append(result, template)
	}
	return result
}

func (qt *QuotaTemplates) ApplyTemplate(ctx context.Context, qm *QuotaManager, tenantID, tier string) error {
	template, err := qt.GetTemplate(tier)
	if err != nil {
		return err
	}

	return qm.CreateTenant(ctx, tenantID, tier, template.StorageLimit)
}

func (qt *QuotaTemplates) UpgradeTier(ctx context.Context, qm *QuotaManager, tenantID, newTier string) error {
	template, err := qt.GetTemplate(newTier)
	if err != nil {
		return err
	}

	// Update the tenant's quota to the new tier
	return qm.UpdateQuota(ctx, tenantID, template.StorageLimit)
}
