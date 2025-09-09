// internal/usage/auto_upgrade_test.go
package usage

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAutoUpgrade_DetectUpgradeNeeded(t *testing.T) {
	db := setupTestDB(t)
	defer func() { _ = db.Close() }()

	qm := NewQuotaManager(db)
	require.NoError(t, qm.InitializeSchema(context.Background()))

	au := NewAutoUpgradeManager(qm)
	require.NoError(t, au.InitializeSchema(context.Background()))
	ctx := context.Background()

	// Create tenant with small quota
	require.NoError(t, qm.CreateTenant(ctx, "tenant-1", "starter", 1073741824)) // 1GB

	// Simulate hitting limits multiple times
	for i := 0; i < 5; i++ {
		// Use 95% of quota
		_, _ = qm.CheckAndReserve(ctx, "tenant-1", 1020054733) // 973MB
		// Record limit hit event
		require.NoError(t, au.RecordLimitHit(ctx, "tenant-1", "storage"))
		// Release quota for next iteration
		_ = qm.ReleaseQuota(ctx, "tenant-1", 1020054733)
	}

	// Check if upgrade is needed
	suggestion, err := au.CheckUpgradeNeeded(ctx, "tenant-1")
	require.NoError(t, err)
	assert.NotNil(t, suggestion)
	assert.Equal(t, "professional", suggestion.RecommendedTier)
	assert.Equal(t, "Consistently hitting storage limits", suggestion.Reason)
}

func TestAutoUpgrade_GenerateUpgradeSuggestion(t *testing.T) {
	db := setupTestDB(t)
	defer func() { _ = db.Close() }()

	qm := NewQuotaManager(db)
	require.NoError(t, qm.InitializeSchema(context.Background()))

	au := NewAutoUpgradeManager(qm)
	require.NoError(t, au.InitializeSchema(context.Background()))
	ctx := context.Background()

	tests := []struct {
		name            string
		currentTier     string
		usagePattern    UsagePattern
		expectedTier    string
		expectedBenefit string
	}{
		{
			name:        "starter to professional",
			currentTier: "starter",
			usagePattern: UsagePattern{
				AverageUsage: 900000000,  // 900MB average
				PeakUsage:    1073741824, // 1GB peak
				LimitHits:    10,
			},
			expectedTier:    "professional",
			expectedBenefit: "100x more storage capacity",
		},
		{
			name:        "professional to enterprise",
			currentTier: "professional",
			usagePattern: UsagePattern{
				AverageUsage: 90000000000,  // 90GB average
				PeakUsage:    107374182400, // 100GB peak
				LimitHits:    15,
			},
			expectedTier:    "enterprise",
			expectedBenefit: "100x more storage capacity",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tenantID := "tenant-" + tt.name

			// Create tenant
			templates := NewQuotaTemplates()
			template, _ := templates.GetTemplate(tt.currentTier)
			require.NoError(t, qm.CreateTenant(ctx, tenantID, tt.currentTier, template.StorageLimit))

			// Generate suggestion based on pattern
			suggestion := au.GenerateSuggestion(tt.currentTier, tt.usagePattern)

			assert.Equal(t, tt.expectedTier, suggestion.RecommendedTier)
			assert.Contains(t, suggestion.Benefits, tt.expectedBenefit)
		})
	}
}

func TestAutoUpgrade_AutoUpgradeThresholds(t *testing.T) {
	db := setupTestDB(t)
	defer func() { _ = db.Close() }()

	qm := NewQuotaManager(db)
	require.NoError(t, qm.InitializeSchema(context.Background()))

	au := NewAutoUpgradeManager(qm)
	require.NoError(t, au.InitializeSchema(context.Background()))
	ctx := context.Background()

	// Create tenant
	require.NoError(t, qm.CreateTenant(ctx, "tenant-3", "starter", 1073741824))

	// Test different thresholds
	tests := []struct {
		limitHits     int
		shouldTrigger bool
	}{
		{limitHits: 2, shouldTrigger: false}, // Too few
		{limitHits: 5, shouldTrigger: true},  // Default threshold
		{limitHits: 10, shouldTrigger: true}, // Well above threshold
	}

	for _, tt := range tests {
		// Reset and record hits
		_ = au.ResetLimitHits(ctx, "tenant-3")

		for i := 0; i < tt.limitHits; i++ {
			_ = au.RecordLimitHit(ctx, "tenant-3", "storage")
		}

		suggestion, _ := au.CheckUpgradeNeeded(ctx, "tenant-3")
		if tt.shouldTrigger {
			assert.NotNil(t, suggestion)
		} else {
			assert.Nil(t, suggestion)
		}
	}
}
