package retention

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"
)

// HoldService manages legal holds
type HoldService struct {
	db     *sql.DB
	logger *zap.Logger
}

// NewHoldService creates a new hold service
func NewHoldService(db *sql.DB, logger *zap.Logger) *HoldService {
	return &HoldService{
		db:     db,
		logger: logger,
	}
}

// CreateHold creates a new legal hold
func (s *HoldService) CreateHold(ctx context.Context, hold *LegalHold) (*LegalHold, error) {
	// Validate
	if hold.UserID == uuid.Nil {
		return nil, fmt.Errorf("user ID required")
	}
	if hold.Reason == "" {
		return nil, fmt.Errorf("reason required")
	}
	if hold.CreatedBy == uuid.Nil {
		return nil, fmt.Errorf("created by required")
	}

	// Set defaults
	hold.ID = uuid.New()
	hold.Status = HoldStatusActive
	hold.CreatedAt = time.Now()

	// Persist if database available
	if s.db != nil {
		query := `
			INSERT INTO legal_holds
			(id, user_id, reason, case_number, created_by, expires_at, status, created_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		`
		_, err := s.db.ExecContext(ctx, query,
			hold.ID, hold.UserID, hold.Reason, sqlNullString(hold.CaseNumber),
			hold.CreatedBy, hold.ExpiresAt, hold.Status, hold.CreatedAt)
		if err != nil {
			return nil, fmt.Errorf("failed to create hold: %w", err)
		}

		s.logger.Info("created legal hold",
			zap.String("hold_id", hold.ID.String()),
			zap.String("user_id", hold.UserID.String()),
			zap.String("reason", hold.Reason))
	}

	return hold, nil
}

// IsUserOnHold checks if a user has any active legal holds
func (s *HoldService) IsUserOnHold(ctx context.Context, userID uuid.UUID) (bool, error) {
	if s.db == nil {
		return false, nil
	}

	query := `
		SELECT COUNT(*)
		FROM legal_holds
		WHERE user_id = $1 AND status = $2
		AND (expires_at IS NULL OR expires_at > NOW())
	`

	var count int
	err := s.db.QueryRowContext(ctx, query, userID, HoldStatusActive).Scan(&count)
	if err != nil {
		return false, fmt.Errorf("failed to check hold status: %w", err)
	}

	return count > 0, nil
}

// ReleaseHold releases a legal hold
func (s *HoldService) ReleaseHold(ctx context.Context, holdID uuid.UUID) error {
	if s.db == nil {
		return fmt.Errorf("database not configured")
	}

	query := `
		UPDATE legal_holds
		SET status = $1, released_at = $2
		WHERE id = $3 AND status = $4
	`

	now := time.Now()
	result, err := s.db.ExecContext(ctx, query,
		HoldStatusReleased, now, holdID, HoldStatusActive)
	if err != nil {
		return fmt.Errorf("failed to release hold: %w", err)
	}

	rows, _ := result.RowsAffected()
	if rows == 0 {
		return fmt.Errorf("hold not found or already released: %s", holdID)
	}

	s.logger.Info("released legal hold",
		zap.String("hold_id", holdID.String()))

	return nil
}

// ListActiveHolds lists all active legal holds
func (s *HoldService) ListActiveHolds(ctx context.Context) ([]*LegalHold, error) {
	holds := []*LegalHold{}

	if s.db == nil {
		return holds, nil
	}

	query := `
		SELECT id, user_id, reason, case_number, created_by,
		       expires_at, released_at, status, created_at
		FROM legal_holds
		WHERE status = $1
		ORDER BY created_at DESC
	`

	rows, err := s.db.QueryContext(ctx, query, HoldStatusActive)
	if err != nil {
		return nil, fmt.Errorf("failed to list holds: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var hold LegalHold
		var caseNumber sql.NullString
		var expiresAt, releasedAt sql.NullTime

		err := rows.Scan(
			&hold.ID, &hold.UserID, &hold.Reason, &caseNumber, &hold.CreatedBy,
			&expiresAt, &releasedAt, &hold.Status, &hold.CreatedAt)
		if err != nil {
			continue
		}

		hold.CaseNumber = caseNumber.String
		if expiresAt.Valid {
			hold.ExpiresAt = &expiresAt.Time
		}
		if releasedAt.Valid {
			hold.ReleasedAt = &releasedAt.Time
		}

		holds = append(holds, &hold)
	}

	return holds, nil
}

// ExpireHolds marks expired holds as expired
func (s *HoldService) ExpireHolds(ctx context.Context) (int, error) {
	if s.db == nil {
		return 0, fmt.Errorf("database not configured")
	}

	query := `
		UPDATE legal_holds
		SET status = $1
		WHERE status = $2
		AND expires_at IS NOT NULL
		AND expires_at <= NOW()
	`

	result, err := s.db.ExecContext(ctx, query, HoldStatusExpired, HoldStatusActive)
	if err != nil {
		return 0, fmt.Errorf("failed to expire holds: %w", err)
	}

	rows, _ := result.RowsAffected()
	if rows > 0 {
		s.logger.Info("expired legal holds", zap.Int64("count", rows))
	}

	return int(rows), nil
}
