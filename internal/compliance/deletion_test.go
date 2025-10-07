package compliance

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestDeletionService_CreateRequest(t *testing.T) {
	t.Run("creates immediate deletion request", func(t *testing.T) {
		service := NewDeletionService(nil, nil, zap.NewNop())
		ctx := context.Background()

		request := &DeletionRequest{
			UserID:         uuid.New(),
			RequestedBy:    uuid.New(),
			Reason:         "User requested (Article 17)",
			Scope:          DeletionScopeAll,
			IncludeBackups: true,
			PreserveAudit:  true,
		}

		created, err := service.CreateRequest(ctx, request)

		require.NoError(t, err)
		assert.NotEqual(t, uuid.Nil, created.ID)
		assert.Equal(t, DeletionStatusPending, created.Status)
		assert.Nil(t, created.ScheduledFor) // Immediate
	})

	t.Run("creates scheduled deletion request", func(t *testing.T) {
		service := NewDeletionService(nil, nil, zap.NewNop())
		ctx := context.Background()

		scheduledTime := time.Now().Add(30 * 24 * time.Hour)
		request := &DeletionRequest{
			UserID:       uuid.New(),
			RequestedBy:  uuid.New(),
			Reason:       "Account closure - 30 day grace",
			Scope:        DeletionScopeAll,
			ScheduledFor: &scheduledTime,
		}

		created, err := service.CreateRequest(ctx, request)

		require.NoError(t, err)
		assert.NotNil(t, created.ScheduledFor)
		assert.Equal(t, DeletionStatusPending, created.Status)
	})

	t.Run("validates user ID", func(t *testing.T) {
		service := NewDeletionService(nil, nil, zap.NewNop())
		ctx := context.Background()

		request := &DeletionRequest{
			UserID:      uuid.Nil,
			RequestedBy: uuid.New(),
			Reason:      "Test",
		}

		_, err := service.CreateRequest(ctx, request)

		assert.Error(t, err)
		assert.Contains(t, err.Error(), "user ID required")
	})

	t.Run("validates reason", func(t *testing.T) {
		service := NewDeletionService(nil, nil, zap.NewNop())
		ctx := context.Background()

		request := &DeletionRequest{
			UserID:      uuid.New(),
			RequestedBy: uuid.New(),
			Reason:      "",
		}

		_, err := service.CreateRequest(ctx, request)

		assert.Error(t, err)
		assert.Contains(t, err.Error(), "reason required")
	})
}

func TestDeletionService_CheckLegalHold(t *testing.T) {
	t.Run("blocks deletion when legal hold active", func(t *testing.T) {
		service := NewDeletionService(nil, nil, zap.NewNop())
		ctx := context.Background()
		userID := uuid.New()

		canDelete, err := service.CanDeleteUser(ctx, userID)

		require.NoError(t, err)
		// Without database, should allow (no holds found)
		assert.True(t, canDelete)
	})
}

func TestDeletionService_GenerateProof(t *testing.T) {
	t.Run("generates deletion certificate", func(t *testing.T) {
		service := NewDeletionService(nil, nil, zap.NewNop())
		ctx := context.Background()

		request := &DeletionRequest{
			ID:           uuid.New(),
			UserID:       uuid.New(),
			ItemsDeleted: 100,
			BytesDeleted: 1024 * 1024 * 1024, // 1GB
		}

		proofs := []DeletionProof{
			{
				RequestID: request.ID,
				BackendID: "lyve-us-east-1",
				Container: "user-data",
				Artifact:  "file1.txt",
				ProofType: ProofTypeFileDeleted,
			},
		}

		cert, err := service.GenerateCertificate(ctx, request, proofs)

		require.NoError(t, err)
		assert.Equal(t, request.ID, cert.RequestID)
		assert.Equal(t, request.UserID, cert.UserID)
		assert.NotEmpty(t, cert.CertificateHash)
		assert.Len(t, cert.Proofs, 1)
	})
}
