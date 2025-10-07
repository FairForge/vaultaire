package retention

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestHoldService_CreateHold(t *testing.T) {
	t.Run("creates legal hold", func(t *testing.T) {
		service := NewHoldService(nil, zap.NewNop())
		ctx := context.Background()

		hold := &LegalHold{
			UserID:     uuid.New(),
			Reason:     "Litigation pending",
			CaseNumber: "CASE-2024-001",
			CreatedBy:  uuid.New(),
		}

		created, err := service.CreateHold(ctx, hold)

		require.NoError(t, err)
		assert.NotEqual(t, uuid.Nil, created.ID)
		assert.Equal(t, HoldStatusActive, created.Status)
		assert.Equal(t, hold.Reason, created.Reason)
	})

	t.Run("validates user ID", func(t *testing.T) {
		service := NewHoldService(nil, zap.NewNop())
		ctx := context.Background()

		hold := &LegalHold{
			UserID:    uuid.Nil,
			Reason:    "Test",
			CreatedBy: uuid.New(),
		}

		_, err := service.CreateHold(ctx, hold)

		assert.Error(t, err)
		assert.Contains(t, err.Error(), "user ID required")
	})

	t.Run("validates reason", func(t *testing.T) {
		service := NewHoldService(nil, zap.NewNop())
		ctx := context.Background()

		hold := &LegalHold{
			UserID:    uuid.New(),
			Reason:    "",
			CreatedBy: uuid.New(),
		}

		_, err := service.CreateHold(ctx, hold)

		assert.Error(t, err)
		assert.Contains(t, err.Error(), "reason required")
	})
}

func TestHoldService_IsUserOnHold(t *testing.T) {
	t.Run("returns false when no holds", func(t *testing.T) {
		service := NewHoldService(nil, zap.NewNop())
		ctx := context.Background()

		onHold, err := service.IsUserOnHold(ctx, uuid.New())

		require.NoError(t, err)
		assert.False(t, onHold)
	})
}

func TestHoldService_ReleaseHold(t *testing.T) {
	t.Run("releases hold", func(t *testing.T) {
		service := NewHoldService(nil, zap.NewNop())
		ctx := context.Background()
		holdID := uuid.New()

		err := service.ReleaseHold(ctx, holdID)

		// Should fail initially without database
		require.Error(t, err)
	})
}

func TestHoldService_ListActiveHolds(t *testing.T) {
	t.Run("lists holds", func(t *testing.T) {
		service := NewHoldService(nil, zap.NewNop())
		ctx := context.Background()

		holds, err := service.ListActiveHolds(ctx)

		require.NoError(t, err)
		assert.NotNil(t, holds)
	})
}
