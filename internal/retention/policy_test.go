package retention

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestPolicyService_CreatePolicy(t *testing.T) {
	t.Run("creates policy with backend targeting", func(t *testing.T) {
		service := NewPolicyService(nil, zap.NewNop())
		ctx := context.Background()

		policy := &RetentionPolicy{
			Name:                 "Lyve Compliance Storage",
			Description:          "7-year retention with object lock",
			DataCategory:         CategoryAuditLogs,
			RetentionPeriod:      7 * 365 * 24 * time.Hour,
			Action:               ActionArchive,
			BackendID:            "lyve-us-east-1",
			ContainerName:        "compliance-data",
			UseBackendObjectLock: true, // Use Lyve's native object lock
			Enabled:              true,
		}

		created, err := service.CreatePolicy(ctx, policy)

		require.NoError(t, err)
		assert.NotEqual(t, uuid.Nil, created.ID)
		assert.Equal(t, "lyve-us-east-1", created.BackendID)
		assert.True(t, created.UseBackendObjectLock)
	})

	t.Run("creates global policy for all backends", func(t *testing.T) {
		service := NewPolicyService(nil, zap.NewNop())
		ctx := context.Background()

		policy := &RetentionPolicy{
			Name:            "Global Temp File Cleanup",
			DataCategory:    CategoryTempFiles,
			RetentionPeriod: 7 * 24 * time.Hour,
			Action:          ActionDelete,
			// BackendID empty = applies to all backends
			Enabled: true,
		}

		created, err := service.CreatePolicy(ctx, policy)

		require.NoError(t, err)
		assert.Empty(t, created.BackendID) // Global policy
	})

	t.Run("validates data category", func(t *testing.T) {
		service := NewPolicyService(nil, zap.NewNop())
		ctx := context.Background()

		policy := &RetentionPolicy{
			Name:            "Invalid Category",
			DataCategory:    "invalid_category",
			RetentionPeriod: 30 * 24 * time.Hour,
		}

		_, err := service.CreatePolicy(ctx, policy)

		assert.Error(t, err)
		assert.Contains(t, err.Error(), "invalid data category")
	})
}

func TestPolicyService_GetPoliciesForBackend(t *testing.T) {
	t.Run("filters policies by backend", func(t *testing.T) {
		service := NewPolicyService(nil, zap.NewNop())
		ctx := context.Background()

		policies, err := service.GetPoliciesForBackend(ctx, "lyve-us-east-1", "")

		require.NoError(t, err)
		assert.NotNil(t, policies)
	})
}
