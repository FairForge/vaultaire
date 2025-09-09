// internal/usage/auto_upgrade.go
package usage

import (
	"context"
	"fmt"
	"time"
)

type UsagePattern struct {
	AverageUsage int64
	PeakUsage    int64
	LimitHits    int
	Period       time.Duration
}

type UpgradeSuggestion struct {
	TenantID        string    `json:"tenant_id"`
	CurrentTier     string    `json:"current_tier"`
	RecommendedTier string    `json:"recommended_tier"`
	Reason          string    `json:"reason"`
	Benefits        []string  `json:"benefits"`
	EstimatedSaving float64   `json:"estimated_saving,omitempty"`
	CreatedAt       time.Time `json:"created_at"`
}

type AutoUpgradeManager struct {
	quotaManager   *QuotaManager
	templates      *QuotaTemplates
	hitThreshold   int           // Number of limit hits before suggesting upgrade
	analysisWindow time.Duration // Time window to analyze usage
}

func NewAutoUpgradeManager(qm *QuotaManager) *AutoUpgradeManager {
	return &AutoUpgradeManager{
		quotaManager:   qm,
		templates:      NewQuotaTemplates(),
		hitThreshold:   5,                  // Suggest upgrade after 5 limit hits
		analysisWindow: 7 * 24 * time.Hour, // Analyze last 7 days
	}
}

func (aum *AutoUpgradeManager) InitializeSchema(ctx context.Context) error {
	schema := `
    CREATE TABLE IF NOT EXISTS upgrade_triggers (
        tenant_id TEXT PRIMARY KEY REFERENCES tenant_quotas(tenant_id),
        limit_hits INT DEFAULT 0,
        last_hit_at TIMESTAMP,
        last_suggestion_at TIMESTAMP,
        suggestion_count INT DEFAULT 0,
        created_at TIMESTAMP DEFAULT NOW(),
        updated_at TIMESTAMP DEFAULT NOW()
    );

    CREATE TABLE IF NOT EXISTS upgrade_suggestions (
        id SERIAL PRIMARY KEY,
        tenant_id TEXT NOT NULL REFERENCES tenant_quotas(tenant_id),
        current_tier VARCHAR(50),
        recommended_tier VARCHAR(50),
        reason TEXT,
        status VARCHAR(20) DEFAULT 'pending',
        created_at TIMESTAMP DEFAULT NOW()
    );

    CREATE INDEX IF NOT EXISTS idx_upgrade_triggers_hits
        ON upgrade_triggers(limit_hits)
        WHERE limit_hits > 0;
    `

	_, err := aum.quotaManager.db.ExecContext(ctx, schema)
	return err
}

func (aum *AutoUpgradeManager) RecordLimitHit(ctx context.Context, tenantID, limitType string) error {
	query := `
        INSERT INTO upgrade_triggers (tenant_id, limit_hits, last_hit_at)
        VALUES ($1, 1, NOW())
        ON CONFLICT (tenant_id)
        DO UPDATE SET
            limit_hits = upgrade_triggers.limit_hits + 1,
            last_hit_at = NOW(),
            updated_at = NOW()`

	_, err := aum.quotaManager.db.ExecContext(ctx, query, tenantID)
	if err != nil {
		return fmt.Errorf("recording limit hit: %w", err)
	}

	// Also record in usage events for analysis
	eventQuery := `
        INSERT INTO quota_usage_events (tenant_id, operation, bytes_delta, object_key, timestamp)
        VALUES ($1, 'LIMIT_HIT', 0, $2, NOW())`

	_, err = aum.quotaManager.db.ExecContext(ctx, eventQuery, tenantID, limitType)
	return err
}

func (aum *AutoUpgradeManager) CheckUpgradeNeeded(ctx context.Context, tenantID string) (*UpgradeSuggestion, error) {
	// Get current limit hits
	var limitHits int
	var lastHit *time.Time

	query := `
        SELECT limit_hits, last_hit_at
        FROM upgrade_triggers
        WHERE tenant_id = $1`

	err := aum.quotaManager.db.QueryRowContext(ctx, query, tenantID).Scan(&limitHits, &lastHit)
	if err != nil {
		return nil, nil // No triggers recorded yet
	}

	// Check if threshold met
	if limitHits < aum.hitThreshold {
		return nil, nil
	}

	// Get current tier
	var currentTier string
	tierQuery := `SELECT tier FROM tenant_quotas WHERE tenant_id = $1`
	err = aum.quotaManager.db.QueryRowContext(ctx, tierQuery, tenantID).Scan(&currentTier)
	if err != nil {
		return nil, fmt.Errorf("getting current tier: %w", err)
	}

	// Generate usage pattern
	pattern := UsagePattern{
		LimitHits: limitHits,
	}

	// Generate suggestion
	suggestion := aum.GenerateSuggestion(currentTier, pattern)
	suggestion.TenantID = tenantID
	suggestion.CurrentTier = currentTier
	suggestion.Reason = "Consistently hitting storage limits"
	suggestion.CreatedAt = time.Now()

	// Store suggestion
	_, err = aum.quotaManager.db.ExecContext(ctx, `
        INSERT INTO upgrade_suggestions (tenant_id, current_tier, recommended_tier, reason)
        VALUES ($1, $2, $3, $4)`,
		tenantID, currentTier, suggestion.RecommendedTier, suggestion.Reason)

	return suggestion, err
}

func (aum *AutoUpgradeManager) GenerateSuggestion(currentTier string, pattern UsagePattern) *UpgradeSuggestion {
	suggestion := &UpgradeSuggestion{
		CurrentTier: currentTier,
	}

	// Determine next tier
	switch currentTier {
	case "starter":
		suggestion.RecommendedTier = "professional"
		suggestion.Benefits = []string{
			"100x more storage capacity",
			"Priority support",
			"Object versioning",
			"Cross-region replication",
		}
	case "professional":
		suggestion.RecommendedTier = "enterprise"
		suggestion.Benefits = []string{
			"100x more storage capacity",
			"24/7 phone support",
			"Custom integrations",
			"Dedicated account manager",
		}
	case "enterprise":
		suggestion.RecommendedTier = "custom"
		suggestion.Benefits = []string{
			"Unlimited storage",
			"Custom features",
			"White-label options",
			"SLA guarantees",
		}
	default:
		return nil
	}

	return suggestion
}

func (aum *AutoUpgradeManager) ResetLimitHits(ctx context.Context, tenantID string) error {
	query := `
        UPDATE upgrade_triggers
        SET limit_hits = 0,
            updated_at = NOW()
        WHERE tenant_id = $1`

	_, err := aum.quotaManager.db.ExecContext(ctx, query, tenantID)
	return err
}

func (aum *AutoUpgradeManager) GetUpgradeSuggestions(ctx context.Context, tenantID string) ([]*UpgradeSuggestion, error) {
	query := `
        SELECT current_tier, recommended_tier, reason, created_at
        FROM upgrade_suggestions
        WHERE tenant_id = $1
        ORDER BY created_at DESC
        LIMIT 10`

	rows, err := aum.quotaManager.db.QueryContext(ctx, query, tenantID)
	if err != nil {
		return nil, fmt.Errorf("getting suggestions: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var suggestions []*UpgradeSuggestion
	for rows.Next() {
		var s UpgradeSuggestion
		s.TenantID = tenantID

		err := rows.Scan(&s.CurrentTier, &s.RecommendedTier, &s.Reason, &s.CreatedAt)
		if err != nil {
			continue
		}

		suggestions = append(suggestions, &s)
	}

	return suggestions, nil
}
