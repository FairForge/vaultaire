// internal/usage/overage_test.go
package usage

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOverageHandler_CheckOverage(t *testing.T) {
	db := setupTestDB(t)
	defer func() { _ = db.Close() }()

	qm := NewQuotaManager(db)
	require.NoError(t, qm.InitializeSchema(context.Background()))

	handler := NewOverageHandler(qm)
	ctx := context.Background()

	// Create tenant with 1GB limit
	require.NoError(t, qm.CreateTenant(ctx, "tenant-1", "starter", 1073741824))

	// Use 900MB (under limit, under 90%)
	allowed, err := qm.CheckAndReserve(ctx, "tenant-1", 943718400)
	require.NoError(t, err)
	assert.True(t, allowed)

	// Check overage status - should be OK at 87.89%
	status, err := handler.CheckOverage(ctx, "tenant-1")
	require.NoError(t, err)
	assert.Equal(t, OverageStatusOK, status.Status)
	assert.InDelta(t, 87.89, status.UsagePercent, 0.1)

	// Try to add 200MB more (would exceed limit)
	allowed, err = qm.CheckAndReserve(ctx, "tenant-1", 209715200)
	require.NoError(t, err)
	assert.False(t, allowed) // Not allowed, stays at 900MB

	// Check overage status - still at 87.89%, should still be OK
	status, err = handler.CheckOverage(ctx, "tenant-1")
	require.NoError(t, err)
	assert.Equal(t, OverageStatusOK, status.Status) // Changed from WARNING

	// Now use up to 95% to trigger WARNING
	allowed, err = qm.CheckAndReserve(ctx, "tenant-1", 80530637) // ~77MB more to reach 95%
	require.NoError(t, err)
	assert.True(t, allowed)

	// Check overage status - should be WARNING at 95%
	status, err = handler.CheckOverage(ctx, "tenant-1")
	require.NoError(t, err)
	assert.Equal(t, OverageStatusWarning, status.Status)
	assert.InDelta(t, 95.0, status.UsagePercent, 0.5)
}

func TestOverageHandler_HandleOverage(t *testing.T) {
	db := setupTestDB(t)
	defer func() { _ = db.Close() }()

	qm := NewQuotaManager(db)
	require.NoError(t, qm.InitializeSchema(context.Background()))

	handler := NewOverageHandler(qm)
	ctx := context.Background()

	tests := []struct {
		name    string
		policy  OveragePolicy
		usage   int64
		limit   int64
		wantErr bool
		allowed bool
	}{
		{
			name:    "hard limit blocks",
			policy:  OveragePolicyHardLimit,
			usage:   1073741824, // 1GB
			limit:   1073741824, // 1GB
			wantErr: false,
			allowed: false,
		},
		{
			name:    "soft limit allows with grace",
			policy:  OveragePolicySoftLimit,
			usage:   1073741824,
			limit:   1073741824,
			wantErr: false,
			allowed: true, // Allows up to 10% overage
		},
		{
			name:    "billing allows with charge",
			policy:  OveragePolicyBilling,
			usage:   1073741824,
			limit:   1073741824,
			wantErr: false,
			allowed: true, // Always allows, bills for overage
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create tenant with policy
			tenantID := "tenant-" + tt.name
			require.NoError(t, qm.CreateTenant(ctx, tenantID, "starter", tt.limit))

			// Set overage policy
			handler.SetPolicy(tenantID, tt.policy)

			// Use up to limit
			allowed, err := qm.CheckAndReserve(ctx, tenantID, tt.usage)
			require.NoError(t, err)
			assert.True(t, allowed)

			// Try to exceed
			action, err := handler.HandleOverage(ctx, tenantID, 104857600) // 100MB more

			if tt.wantErr {
				assert.Error(t, err)
				return
			}

			require.NoError(t, err)
			assert.Equal(t, tt.allowed, action.Allowed)

			if tt.policy == OveragePolicyBilling {
				assert.Greater(t, action.OverageCharge, 0.0)
			}
		})
	}
}
