package compliance

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"
)

// GDPRService handles GDPR compliance operations
type GDPRService struct {
	db     *sql.DB
	logger *zap.Logger
}

// NewGDPRService creates a new GDPR compliance service
func NewGDPRService(db *sql.DB, logger *zap.Logger) *GDPRService {
	return &GDPRService{
		db:     db,
		logger: logger,
	}
}

// CreateSubjectAccessRequest creates a new SAR (Article 15)
func (s *GDPRService) CreateSubjectAccessRequest(ctx context.Context, userID uuid.UUID) (*SubjectAccessRequest, error) {
	if userID == uuid.Nil {
		return nil, fmt.Errorf("invalid user ID")
	}

	sar := &SubjectAccessRequest{
		ID:          uuid.New(),
		UserID:      userID,
		RequestDate: time.Now(),
		Status:      StatusPending,
		CreatedAt:   time.Now(),
		UpdatedAt:   time.Now(),
	}

	// If we have a database, persist it
	if s.db != nil {
		query := `
			INSERT INTO gdpr_subject_access_requests 
			(id, user_id, request_date, status, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6)
		`
		_, err := s.db.ExecContext(ctx, query,
			sar.ID, sar.UserID, sar.RequestDate, sar.Status, sar.CreatedAt, sar.UpdatedAt)
		if err != nil {
			return nil, fmt.Errorf("failed to create SAR: %w", err)
		}

		s.logger.Info("created subject access request",
			zap.String("sar_id", sar.ID.String()),
			zap.String("user_id", userID.String()))
	}

	return sar, nil
}

// ProcessSubjectAccessRequest processes a SAR and exports user data
func (s *GDPRService) ProcessSubjectAccessRequest(ctx context.Context, sarID uuid.UUID) error {
	if s.db == nil {
		return fmt.Errorf("database not configured")
	}

	// Update status to processing
	_, err := s.db.ExecContext(ctx,
		`UPDATE gdpr_subject_access_requests SET status = $1, updated_at = $2 WHERE id = $3`,
		StatusProcessing, time.Now(), sarID)
	if err != nil {
		return fmt.Errorf("failed to update SAR status: %w", err)
	}

	// Get the user ID for this SAR
	var userID uuid.UUID
	err = s.db.QueryRowContext(ctx,
		`SELECT user_id FROM gdpr_subject_access_requests WHERE id = $1`, sarID).Scan(&userID)
	if err != nil {
		return fmt.Errorf("failed to get SAR details: %w", err)
	}

	// Export user data
	export, err := s.exportUserData(ctx, userID)
	if err != nil {
		// Mark as failed
		_, _ = s.db.ExecContext(ctx,
			`UPDATE gdpr_subject_access_requests 
			 SET status = $1, error_message = $2, updated_at = $3 
			 WHERE id = $4`,
			StatusFailed, err.Error(), time.Now(), sarID)
		return fmt.Errorf("failed to export user data: %w", err)
	}

	// Save export to file
	exportPath := filepath.Join("/tmp", fmt.Sprintf("sar_%s.json", sarID.String()))
	data, err := json.MarshalIndent(export, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal export data: %w", err)
	}

	if err := os.WriteFile(exportPath, data, 0600); err != nil {
		return fmt.Errorf("failed to write export file: %w", err)
	}

	// Mark as completed
	now := time.Now()
	_, err = s.db.ExecContext(ctx,
		`UPDATE gdpr_subject_access_requests 
		 SET status = $1, completion_date = $2, data_export_path = $3, 
		     file_count = $4, total_size_bytes = $5, updated_at = $6 
		 WHERE id = $7`,
		StatusCompleted, now, exportPath, len(export.Files), export.calculateSize(), now, sarID)
	if err != nil {
		return fmt.Errorf("failed to mark SAR as completed: %w", err)
	}

	s.logger.Info("completed subject access request",
		zap.String("sar_id", sarID.String()),
		zap.String("user_id", userID.String()),
		zap.String("export_path", exportPath))

	return nil
}

// CreateDeletionRequest creates a right to erasure request (Article 17)
func (s *GDPRService) CreateDeletionRequest(ctx context.Context, userID uuid.UUID, userEmail, method string) (*DeletionRequest, error) {
	if userID == uuid.Nil {
		return nil, fmt.Errorf("invalid user ID")
	}

	// Validate deletion method
	validMethods := map[string]bool{
		DeletionMethodSoft:      true,
		DeletionMethodHard:      true,
		DeletionMethodAnonymize: true,
	}
	if !validMethods[method] {
		return nil, fmt.Errorf("invalid deletion method: %s", method)
	}

	req := &DeletionRequest{
		ID:             uuid.New(),
		UserID:         userID,
		UserEmail:      userEmail,
		RequestDate:    time.Now(),
		Status:         StatusPending,
		DeletionMethod: method,
		CreatedAt:      time.Now(),
		UpdatedAt:      time.Now(),
	}

	if s.db != nil {
		query := `
			INSERT INTO gdpr_deletion_requests 
			(id, user_id, user_email, request_date, status, deletion_method, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		`
		_, err := s.db.ExecContext(ctx, query,
			req.ID, req.UserID, req.UserEmail, req.RequestDate, req.Status, req.DeletionMethod,
			req.CreatedAt, req.UpdatedAt)
		if err != nil {
			return nil, fmt.Errorf("failed to create deletion request: %w", err)
		}

		s.logger.Info("created deletion request",
			zap.String("request_id", req.ID.String()),
			zap.String("user_id", userID.String()),
			zap.String("method", method))
	}

	return req, nil
}

// GetDataInventory returns categories of data stored for a user
func (s *GDPRService) GetDataInventory(ctx context.Context, userID uuid.UUID) (map[string]interface{}, error) {
	inventory := map[string]interface{}{
		"profile": map[string]string{
			"description": "User profile information",
			"fields":      "email, name, preferences, created_at",
		},
		"files": map[string]string{
			"description": "Stored files and metadata",
			"fields":      "file_path, size, created_at, last_modified",
		},
		"audit_logs": map[string]string{
			"description": "System access and activity logs",
			"fields":      "timestamp, event_type, action, ip_address",
		},
		"billing_history": map[string]string{
			"description": "Payment and subscription history",
			"fields":      "date, amount, description, invoice_id",
		},
	}

	return inventory, nil
}

// ListProcessingActivities returns all registered data processing activities (Article 30)
func (s *GDPRService) ListProcessingActivities(ctx context.Context) ([]ProcessingActivity, error) {
	// Return standard processing activities
	activities := []ProcessingActivity{
		{
			ID:           uuid.New(),
			ActivityName: "User Account Management",
			Purpose:      "Provide cloud storage service",
			LegalBasis:   LegalBasisContract,
			DataCategories: []string{
				"email", "name", "password_hash", "preferences",
			},
			RetentionPeriod:      365 * 24 * time.Hour, // 1 year after account closure
			ThirdPartyProcessors: []string{"Stripe (payments)", "PostgreSQL (hosting)"},
			CreatedAt:            time.Now(),
			UpdatedAt:            time.Now(),
		},
		{
			ID:           uuid.New(),
			ActivityName: "File Storage and Retrieval",
			Purpose:      "Store and retrieve user files",
			LegalBasis:   LegalBasisContract,
			DataCategories: []string{
				"file_metadata", "file_contents", "access_timestamps",
			},
			RetentionPeriod:      30 * 24 * time.Hour, // 30 days after account closure
			ThirdPartyProcessors: []string{"AWS S3", "OneDrive", "Lyve Cloud"},
			CreatedAt:            time.Now(),
			UpdatedAt:            time.Now(),
		},
		{
			ID:           uuid.New(),
			ActivityName: "Audit Logging",
			Purpose:      "Security and compliance monitoring",
			LegalBasis:   LegalBasisLegitimateInterest,
			DataCategories: []string{
				"ip_address", "user_agent", "action_type", "timestamp",
			},
			RetentionPeriod:      7 * 365 * 24 * time.Hour, // 7 years for compliance
			ThirdPartyProcessors: []string{},
			CreatedAt:            time.Now(),
			UpdatedAt:            time.Now(),
		},
	}

	// If we have a database, load from there instead
	if s.db != nil {
		rows, err := s.db.QueryContext(ctx, `
			SELECT id, activity_name, purpose, legal_basis, data_categories, 
			       retention_period, third_party_processors, created_at, updated_at
			FROM gdpr_processing_activities
		`)
		if err != nil {
			// Return default activities if table doesn't exist yet
			return activities, nil
		}
		defer func() { _ = rows.Close() }()

		dbActivities := []ProcessingActivity{}
		for rows.Next() {
			var a ProcessingActivity
			var retentionNanos int64
			err := rows.Scan(&a.ID, &a.ActivityName, &a.Purpose, &a.LegalBasis,
				&a.DataCategories, &retentionNanos, &a.ThirdPartyProcessors,
				&a.CreatedAt, &a.UpdatedAt)
			if err != nil {
				continue
			}
			a.RetentionPeriod = time.Duration(retentionNanos)
			dbActivities = append(dbActivities, a)
		}

		if len(dbActivities) > 0 {
			return dbActivities, nil
		}
	}

	return activities, nil
}

// exportUserData gathers all user data for export
func (s *GDPRService) exportUserData(ctx context.Context, userID uuid.UUID) (*UserDataExport, error) {
	export := &UserDataExport{
		UserID:         userID,
		Email:          "", // Will be filled from database
		Profile:        make(map[string]interface{}),
		Files:          []FileMetadata{},
		AuditLogs:      []AuditLogEntry{},
		BillingHistory: []BillingRecord{},
		ExportDate:     time.Now(),
	}

	if s.db == nil {
		return export, nil
	}

	// Get user profile
	var email, username string
	var createdAt time.Time
	err := s.db.QueryRowContext(ctx,
		`SELECT email, username, created_at FROM users WHERE id = $1`, userID).
		Scan(&email, &username, &createdAt)
	if err == nil {
		export.Email = email
		export.Profile = map[string]interface{}{
			"username":   username,
			"created_at": createdAt,
		}
	}

	// Get audit logs (last 90 days)
	rows, err := s.db.QueryContext(ctx, `
		SELECT timestamp, event_type, action, resource, ip, user_agent
		FROM audit_logs
		WHERE user_id = $1 AND timestamp > NOW() - INTERVAL '90 days'
		ORDER BY timestamp DESC
		LIMIT 1000
	`, userID)
	if err == nil {
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var log AuditLogEntry
			var ip, userAgent sql.NullString
			if err := rows.Scan(&log.Timestamp, &log.EventType, &log.Action,
				&log.Resource, &ip, &userAgent); err == nil {
				log.IPAddress = ip.String
				log.UserAgent = userAgent.String
				export.AuditLogs = append(export.AuditLogs, log)
			}
		}
	}

	return export, nil
}

// calculateSize calculates total size of exported data
func (e *UserDataExport) calculateSize() int64 {
	var total int64
	for _, f := range e.Files {
		total += f.Size
	}
	return total
}
