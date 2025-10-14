package compliance

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"
)

// GDPRService handles GDPR compliance operations
type GDPRService struct {
	db     *sql.DB
	logger *zap.Logger
}

// NewGDPRService creates a new GDPR service
func NewGDPRService(db *sql.DB, logger *zap.Logger) *GDPRService {
	return &GDPRService{
		db:     db,
		logger: logger,
	}
}

// CreateSubjectAccessRequest creates a new SAR (Article 15)
func (s *GDPRService) CreateSubjectAccessRequest(ctx context.Context, userID uuid.UUID) (*SubjectAccessRequest, error) {
	sar := &SubjectAccessRequest{
		ID:          uuid.New(),
		UserID:      userID,
		RequestDate: time.Now(),
		Status:      "pending",
		CreatedAt:   time.Now(),
	}

	if s.db != nil {
		query := `
			INSERT INTO subject_access_requests (id, user_id, request_date, status, created_at)
			VALUES ($1, $2, $3, $4, $5)
		`
		_, err := s.db.ExecContext(ctx, query,
			sar.ID, sar.UserID, sar.RequestDate, sar.Status, sar.CreatedAt)
		if err != nil {
			return nil, fmt.Errorf("failed to create SAR: %w", err)
		}

		s.logger.Info("created subject access request",
			zap.String("sar_id", sar.ID.String()),
			zap.String("user_id", userID.String()))
	}

	return sar, nil
}

// GetUserData retrieves all data for a user (for SAR)
func (s *GDPRService) GetUserData(ctx context.Context, userID uuid.UUID) ([]byte, error) {
	data := map[string]interface{}{
		"user_id":    userID,
		"request_at": time.Now(),
		"data":       []string{}, // TODO: Collect actual user data
	}

	return json.Marshal(data)
}

// CompleteSubjectAccessRequest marks SAR as complete
func (s *GDPRService) CompleteSubjectAccessRequest(ctx context.Context, sarID uuid.UUID, dataPackage []byte) error {
	if s.db == nil {
		return fmt.Errorf("database not configured")
	}

	now := time.Now()
	query := `
		UPDATE subject_access_requests
		SET status = 'completed', data_package = $1, completed_at = $2
		WHERE id = $3
	`

	result, err := s.db.ExecContext(ctx, query, dataPackage, now, sarID)
	if err != nil {
		return fmt.Errorf("failed to complete SAR: %w", err)
	}

	rows, _ := result.RowsAffected()
	if rows == 0 {
		return fmt.Errorf("SAR not found: %s", sarID)
	}

	s.logger.Info("completed subject access request",
		zap.String("sar_id", sarID.String()))

	return nil
}

// CreateDeletionRequest creates a deletion request (Article 17)
func (s *GDPRService) CreateDeletionRequest(ctx context.Context, userID uuid.UUID, userEmail, reason string) (*DeletionRequest, error) {
	now := time.Now()
	req := &DeletionRequest{
		ID:             uuid.New(),
		UserID:         userID,
		RequestedBy:    userID, // User requested their own deletion
		UserEmail:      userEmail,
		RequestDate:    now,
		Reason:         reason,
		Scope:          DeletionScopeAll,
		DeletionMethod: "standard", // Backwards compatibility
		Status:         DeletionStatusPending,
		CreatedAt:      now,
		UpdatedAt:      now,
	}

	if s.db != nil {
		// Note: Using basic fields for now, full implementation in deletion.go
		s.logger.Info("created deletion request",
			zap.String("request_id", req.ID.String()),
			zap.String("user_id", userID.String()),
			zap.String("email", userEmail))
	}

	return req, nil
}

// CompleteDeletionRequest marks deletion as complete
func (s *GDPRService) CompleteDeletionRequest(ctx context.Context, requestID uuid.UUID) error {
	s.logger.Info("completed deletion request",
		zap.String("request_id", requestID.String()))
	return nil
}

// RecordProcessingActivity records a GDPR Article 30 activity
func (s *GDPRService) RecordProcessingActivity(ctx context.Context, activity *ProcessingActivity) error {
	activity.ID = uuid.New()
	activity.CreatedAt = time.Now()
	activity.UpdatedAt = time.Now()

	if s.db != nil {
		query := `
			INSERT INTO processing_activities
			(id, name, purpose, data_types, legal_basis, retention, description, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		`
		_, err := s.db.ExecContext(ctx, query,
			activity.ID, activity.Name, activity.Purpose,
			activity.DataCategories, activity.LegalBasis, activity.RetentionPeriod,
			activity.Description, activity.CreatedAt, activity.UpdatedAt)
		if err != nil {
			return fmt.Errorf("failed to record processing activity: %w", err)
		}

		s.logger.Info("recorded processing activity",
			zap.String("activity_id", activity.ID.String()),
			zap.String("name", activity.Name))
	}

	return nil
}

// GetDataInventory returns inventory of user data
func (s *GDPRService) GetDataInventory(ctx context.Context, userID uuid.UUID) ([]DataInventoryItem, error) {
	items := []DataInventoryItem{}

	if s.db != nil {
		query := `
			SELECT user_id, data_type, location, purpose, retention, created_at
			FROM data_inventory
			WHERE user_id = $1
			ORDER BY created_at DESC
		`

		rows, err := s.db.QueryContext(ctx, query, userID)
		if err != nil {
			return nil, fmt.Errorf("failed to get data inventory: %w", err)
		}
		defer func() { _ = rows.Close() }()

		for rows.Next() {
			var item DataInventoryItem
			err := rows.Scan(
				&item.UserID, &item.DataType, &item.Location,
				&item.Purpose, &item.Retention, &item.CreatedAt)
			if err != nil {
				continue
			}
			items = append(items, item)
		}
	}

	return items, nil
}

// ListProcessingActivities lists all processing activities
func (s *GDPRService) ListProcessingActivities(ctx context.Context) ([]*ProcessingActivity, error) {
	activities := []*ProcessingActivity{}

	if s.db == nil {
		return activities, nil
	}

	query := `
		SELECT id, name, purpose, data_types, legal_basis, retention, description, created_at, updated_at
		FROM processing_activities
		ORDER BY created_at DESC
	`

	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("failed to list processing activities: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var activity ProcessingActivity
		err := rows.Scan(
			&activity.ID, &activity.Name, &activity.Purpose,
			&activity.DataCategories, &activity.LegalBasis, &activity.RetentionPeriod,
			&activity.Description, &activity.CreatedAt, &activity.UpdatedAt)
		if err != nil {
			continue
		}
		activities = append(activities, &activity)
	}

	return activities, nil
}

// ProcessSubjectAccessRequest processes a SAR and returns user data
func (s *GDPRService) ProcessSubjectAccessRequest(ctx context.Context, sarID uuid.UUID) error {
	// Get user data
	var userID uuid.UUID
	if s.db != nil {
		query := `SELECT user_id FROM subject_access_requests WHERE id = $1`
		err := s.db.QueryRowContext(ctx, query, sarID).Scan(&userID)
		if err != nil {
			return fmt.Errorf("failed to get SAR: %w", err)
		}
	}

	// Collect user data
	dataPackage, err := s.GetUserData(ctx, userID)
	if err != nil {
		return fmt.Errorf("failed to get user data: %w", err)
	}

	// Complete the SAR
	return s.CompleteSubjectAccessRequest(ctx, sarID, dataPackage)
}
