package compliance

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestGDPRService_CreateSubjectAccessRequest(t *testing.T) {
	t.Run("creates SAR successfully", func(t *testing.T) {
		// Arrange
		service := NewGDPRService(nil, zap.NewNop())
		ctx := context.Background()
		userID := uuid.New()

		// Act
		sar, err := service.CreateSubjectAccessRequest(ctx, userID)

		// Assert
		require.NoError(t, err)
		assert.NotEqual(t, uuid.Nil, sar.ID)
		assert.Equal(t, userID, sar.UserID)
		assert.Equal(t, StatusPending, sar.Status)
		assert.False(t, sar.RequestDate.IsZero())
	})

	t.Run("returns error for invalid user ID", func(t *testing.T) {
		service := NewGDPRService(nil, zap.NewNop())
		ctx := context.Background()

		_, err := service.CreateSubjectAccessRequest(ctx, uuid.Nil)

		assert.Error(t, err)
		assert.Contains(t, err.Error(), "invalid user ID")
	})
}

func TestGDPRService_ProcessSubjectAccessRequest(t *testing.T) {
	t.Run("exports user data", func(t *testing.T) {
		service := NewGDPRService(nil, zap.NewNop())
		ctx := context.Background()
		sarID := uuid.New()

		err := service.ProcessSubjectAccessRequest(ctx, sarID)

		// Should fail initially (not implemented)
		require.Error(t, err)
	})
}

func TestGDPRService_CreateDeletionRequest(t *testing.T) {
	t.Run("creates deletion request", func(t *testing.T) {
		service := NewGDPRService(nil, zap.NewNop())
		ctx := context.Background()
		userID := uuid.New()
		userEmail := "test@example.com"

		req, err := service.CreateDeletionRequest(ctx, userID, userEmail, DeletionMethodHard)

		require.NoError(t, err)
		assert.NotEqual(t, uuid.Nil, req.ID)
		assert.Equal(t, userID, req.UserID)
		assert.Equal(t, userEmail, req.UserEmail)
		assert.Equal(t, DeletionMethodHard, req.DeletionMethod)
		assert.Equal(t, StatusPending, req.Status)
	})

	t.Run("validates deletion method", func(t *testing.T) {
		service := NewGDPRService(nil, zap.NewNop())
		ctx := context.Background()

		_, err := service.CreateDeletionRequest(ctx, uuid.New(), "test@example.com", "invalid_method")

		assert.Error(t, err)
		assert.Contains(t, err.Error(), "invalid deletion method")
	})
}

func TestGDPRService_GetDataInventory(t *testing.T) {
	t.Run("returns data categories", func(t *testing.T) {
		service := NewGDPRService(nil, zap.NewNop())
		ctx := context.Background()
		userID := uuid.New()

		inventory, err := service.GetDataInventory(ctx, userID)

		require.NoError(t, err)
		assert.NotNil(t, inventory)
		// Should have standard categories
		assert.Contains(t, inventory, "profile")
		assert.Contains(t, inventory, "files")
		assert.Contains(t, inventory, "audit_logs")
	})
}

func TestGDPRService_ListProcessingActivities(t *testing.T) {
	t.Run("lists all processing activities", func(t *testing.T) {
		service := NewGDPRService(nil, zap.NewNop())
		ctx := context.Background()

		activities, err := service.ListProcessingActivities(ctx)

		require.NoError(t, err)
		assert.NotNil(t, activities)
		// Should have at least core processing activities
		assert.GreaterOrEqual(t, len(activities), 1)
	})
}
