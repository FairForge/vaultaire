package compliance

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"
	"go.uber.org/zap"
)

// DeletionService handles GDPR Article 17 Right to Erasure
type DeletionService struct {
	db          *sql.DB
	holdService interface{} // HoldService for checking legal holds
	logger      *zap.Logger
}

// NewDeletionService creates a new deletion service
func NewDeletionService(db *sql.DB, holdService interface{}, logger *zap.Logger) *DeletionService {
	return &DeletionService{
		db:          db,
		holdService: holdService,
		logger:      logger,
	}
}

// CreateRequest creates a new deletion request
func (s *DeletionService) CreateRequest(ctx context.Context, request *DeletionRequest) (*DeletionRequest, error) {
	// Validate
	if request.UserID == uuid.Nil {
		return nil, fmt.Errorf("user ID required")
	}
	if request.RequestedBy == uuid.Nil {
		return nil, fmt.Errorf("requested by required")
	}
	if request.Reason == "" {
		return nil, fmt.Errorf("reason required")
	}

	// Set defaults
	request.ID = uuid.New()
	request.Status = DeletionStatusPending
	request.CreatedAt = time.Now()
	if request.Scope == "" {
		request.Scope = DeletionScopeAll
	}

	// Check legal holds
	canDelete, err := s.CanDeleteUser(ctx, request.UserID)
	if err != nil {
		return nil, fmt.Errorf("failed to check legal holds: %w", err)
	}
	if !canDelete {
		return nil, fmt.Errorf("cannot delete: user has active legal hold")
	}

	// Persist if database available
	if s.db != nil {
		query := `
			INSERT INTO deletion_requests
			(id, user_id, requested_by, reason, scope, container_filter,
			 include_backups, preserve_audit, scheduled_for, status, created_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		`
		_, err := s.db.ExecContext(ctx, query,
			request.ID, request.UserID, request.RequestedBy, request.Reason,
			request.Scope, pq.Array(request.ContainerFilter),
			request.IncludeBackups, request.PreserveAudit, request.ScheduledFor,
			request.Status, request.CreatedAt)
		if err != nil {
			return nil, fmt.Errorf("failed to create deletion request: %w", err)
		}

		s.logger.Info("created deletion request",
			zap.String("request_id", request.ID.String()),
			zap.String("user_id", request.UserID.String()),
			zap.String("scope", request.Scope))
	}

	return request, nil
}

// CanDeleteUser checks if user data can be deleted (no legal holds)
func (s *DeletionService) CanDeleteUser(ctx context.Context, userID uuid.UUID) (bool, error) {
	// Without database, allow deletion
	if s.db == nil {
		return true, nil
	}

	// Check for active legal holds
	query := `
		SELECT COUNT(*)
		FROM legal_holds
		WHERE user_id = $1 AND status = 'active'
		AND (expires_at IS NULL OR expires_at > NOW())
	`

	var count int
	err := s.db.QueryRowContext(ctx, query, userID).Scan(&count)
	if err != nil {
		return false, fmt.Errorf("failed to check legal holds: %w", err)
	}

	return count == 0, nil
}

// GenerateCertificate creates a deletion certificate with cryptographic proof
func (s *DeletionService) GenerateCertificate(ctx context.Context, request *DeletionRequest, proofs []DeletionProof) (*DeletionCertificate, error) {
	// Generate hash of all proofs
	hashData, err := json.Marshal(proofs)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal proofs: %w", err)
	}

	hash := sha256.Sum256(hashData)
	hashString := hex.EncodeToString(hash[:])

	cert := &DeletionCertificate{
		RequestID:       request.ID,
		UserID:          request.UserID,
		CompletedAt:     time.Now(),
		ItemsDeleted:    request.ItemsDeleted,
		BytesDeleted:    request.BytesDeleted,
		Proofs:          proofs,
		CertificateHash: hashString,
	}

	return cert, nil
}
