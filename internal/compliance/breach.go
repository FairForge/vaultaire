package compliance

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"
)

// BreachService handles GDPR Article 33 & 34 - Data Breach Notification
type BreachService struct {
	db     BreachDatabase
	logger *zap.Logger
}

// NewBreachService creates a new breach notification service
func NewBreachService(db BreachDatabase, logger *zap.Logger) *BreachService {
	return &BreachService{
		db:     db,
		logger: logger,
	}
}

// DetectBreach records a new data breach
func (s *BreachService) DetectBreach(ctx context.Context, req *BreachRequest) (*BreachRecord, error) {
	if req.BreachType == "" {
		return nil, fmt.Errorf("breach_type required")
	}
	if req.Description == "" {
		return nil, fmt.Errorf("description required")
	}

	detectedAt := time.Now()
	if req.DetectedAt != nil {
		detectedAt = *req.DetectedAt
	}

	// Calculate 72-hour deadline (GDPR Article 33.1)
	deadline := detectedAt.Add(72 * time.Hour)

	breach := &BreachRecord{
		ID:                  uuid.New(),
		BreachType:          req.BreachType,
		Status:              BreachStatusDetected,
		DetectedAt:          detectedAt,
		AffectedUserCount:   req.AffectedUserCount,
		AffectedRecordCount: req.AffectedRecordCount,
		DataCategories:      req.DataCategories,
		Description:         req.Description,
		RootCause:           req.RootCause,
		DeadlineAt:          deadline,
		CreatedAt:           time.Now(),
		UpdatedAt:           time.Now(),
	}

	// Assess severity immediately
	assessment := s.AssessSeverity(breach)
	breach.Severity = assessment.Severity

	if s.db != nil {
		if err := s.db.CreateBreach(ctx, breach); err != nil {
			return nil, fmt.Errorf("failed to create breach: %w", err)
		}

		// Add affected users if provided
		if len(req.AffectedUserIDs) > 0 {
			if err := s.db.AddAffectedUsers(ctx, breach.ID, req.AffectedUserIDs); err != nil {
				s.logger.Error("failed to add affected users", zap.Error(err))
			}
		}
	}

	s.logger.Warn("data breach detected",
		zap.String("breach_id", breach.ID.String()),
		zap.String("type", breach.BreachType),
		zap.String("severity", breach.Severity),
		zap.Int("affected_users", breach.AffectedUserCount))

	return breach, nil
}

// AssessSeverity determines the severity of a breach
func (s *BreachService) AssessSeverity(breach *BreachRecord) *BreachAssessment {
	assessment := &BreachAssessment{
		BreachID:          breach.ID,
		AffectedUserCount: breach.AffectedUserCount,
		AssessedAt:        time.Now(),
	}

	// Calculate risk level (0-100)
	riskLevel := 0

	// Factor 1: Number of affected users
	if breach.AffectedUserCount > 10000 {
		riskLevel += 40
	} else if breach.AffectedUserCount > 1000 {
		riskLevel += 30
	} else if breach.AffectedUserCount > 100 {
		riskLevel += 20
	} else if breach.AffectedUserCount > 10 {
		riskLevel += 10
	}

	// Factor 2: Data sensitivity
	sensitiveCategories := []string{"financial", "health", "biometric", "password", "ssn"}
	for _, category := range breach.DataCategories {
		for _, sensitive := range sensitiveCategories {
			if category == sensitive {
				riskLevel += 15
				break
			}
		}
	}

	// Factor 3: Breach type severity
	switch breach.BreachType {
	case BreachTypeRansomware, BreachTypeUnauthorizedAccess:
		riskLevel += 20
	case BreachTypeDataLeakage, BreachTypeInsiderThreat:
		riskLevel += 15
	case BreachTypePhishing, BreachTypeThirdParty:
		riskLevel += 10
	}

	// Cap at 100
	if riskLevel > 100 {
		riskLevel = 100
	}

	assessment.RiskLevel = riskLevel

	// Determine severity and notification requirements
	if riskLevel >= 80 {
		assessment.Severity = BreachSeverityCritical
		assessment.RequiresAuthority = true
		assessment.RequiresSubjects = true
	} else if riskLevel >= 60 {
		assessment.Severity = BreachSeverityHigh
		assessment.RequiresAuthority = true
		assessment.RequiresSubjects = true
	} else if riskLevel >= 40 {
		assessment.Severity = BreachSeverityMedium
		assessment.RequiresAuthority = true
		assessment.RequiresSubjects = false
	} else {
		assessment.Severity = BreachSeverityLow
		assessment.RequiresAuthority = false
		assessment.RequiresSubjects = false
	}

	return assessment
}

// NotifyAuthority sends notification to data protection authority (72-hour requirement)
func (s *BreachService) NotifyAuthority(ctx context.Context, breachID uuid.UUID) error {
	if s.db == nil {
		return fmt.Errorf("database not configured")
	}

	breach, err := s.db.GetBreach(ctx, breachID)
	if err != nil {
		return fmt.Errorf("failed to get breach: %w", err)
	}

	if breach.NotifiedAuthority {
		return fmt.Errorf("authority already notified")
	}

	// Check if within 72-hour deadline
	now := time.Now()
	withinDeadline := now.Before(breach.DeadlineAt)

	// Create notification record
	notification := &BreachNotification{
		ID:               uuid.New(),
		BreachID:         breachID,
		NotificationType: NotificationTypeAuthority,
		Recipient:        "data-protection-authority", // Would be actual DPA contact
		SentAt:           now,
		Method:           NotificationMethodEmail,
		Status:           "sent",
		Content:          s.generateAuthorityNotification(breach),
		Metadata: map[string]interface{}{
			"within_deadline": withinDeadline,
			"hours_elapsed":   now.Sub(breach.DetectedAt).Hours(),
		},
		CreatedAt: now,
	}

	if err := s.db.CreateNotification(ctx, notification); err != nil {
		return fmt.Errorf("failed to create notification: %w", err)
	}

	// Update breach record
	reportedAt := now
	breach.NotifiedAuthority = true
	breach.AuthorityNotifiedAt = &reportedAt
	breach.ReportedAt = &reportedAt
	breach.Status = BreachStatusReported
	breach.UpdatedAt = now

	if err := s.db.UpdateBreach(ctx, breach); err != nil {
		return fmt.Errorf("failed to update breach: %w", err)
	}

	s.logger.Info("authority notified",
		zap.String("breach_id", breachID.String()),
		zap.Bool("within_deadline", withinDeadline))

	return nil
}

// NotifySubjects sends notifications to affected individuals
func (s *BreachService) NotifySubjects(ctx context.Context, breachID uuid.UUID) error {
	if s.db == nil {
		return fmt.Errorf("database not configured")
	}

	breach, err := s.db.GetBreach(ctx, breachID)
	if err != nil {
		return fmt.Errorf("failed to get breach: %w", err)
	}

	if breach.NotifiedSubjects {
		return fmt.Errorf("subjects already notified")
	}

	// Check if subject notification is required
	assessment := s.AssessSeverity(breach)
	if !assessment.RequiresSubjects {
		return fmt.Errorf("subject notification not required for this severity level")
	}

	affectedUsers, err := s.db.GetAffectedUsers(ctx, breachID)
	if err != nil {
		return fmt.Errorf("failed to get affected users: %w", err)
	}

	now := time.Now()
	notifiedCount := 0

	// Send notification to each affected user
	for _, affected := range affectedUsers {
		if affected.Notified {
			continue
		}

		// Create notification record
		notification := &BreachNotification{
			ID:               uuid.New(),
			BreachID:         breachID,
			NotificationType: NotificationTypeSubject,
			Recipient:        affected.UserID.String(),
			SentAt:           now,
			Method:           NotificationMethodEmail,
			Status:           "sent",
			Content:          s.generateSubjectNotification(breach),
			CreatedAt:        now,
		}

		if err := s.db.CreateNotification(ctx, notification); err != nil {
			s.logger.Error("failed to create subject notification",
				zap.String("user_id", affected.UserID.String()),
				zap.Error(err))
			continue
		}

		// Update affected user record
		affected.Notified = true
		affected.NotifiedAt = &now
		affected.Method = NotificationMethodEmail

		if err := s.db.UpdateAffectedUser(ctx, affected); err != nil {
			s.logger.Error("failed to update affected user",
				zap.String("user_id", affected.UserID.String()),
				zap.Error(err))
		}

		notifiedCount++
	}

	// Update breach record
	notifiedAt := now
	breach.NotifiedSubjects = true
	breach.SubjectsNotifiedAt = &notifiedAt
	breach.UpdatedAt = now

	if err := s.db.UpdateBreach(ctx, breach); err != nil {
		return fmt.Errorf("failed to update breach: %w", err)
	}

	s.logger.Info("subjects notified",
		zap.String("breach_id", breachID.String()),
		zap.Int("notified", notifiedCount),
		zap.Int("total", len(affectedUsers)))

	return nil
}

// GetBreachStatus retrieves breach details and deadline status
func (s *BreachService) GetBreachStatus(ctx context.Context, breachID uuid.UUID) (*BreachRecord, error) {
	if s.db == nil {
		return nil, fmt.Errorf("database not configured")
	}

	breach, err := s.db.GetBreach(ctx, breachID)
	if err != nil {
		return nil, fmt.Errorf("failed to get breach: %w", err)
	}

	return breach, nil
}

// UpdateBreach updates a breach record
func (s *BreachService) UpdateBreach(ctx context.Context, breachID uuid.UUID, updates map[string]interface{}) error {
	if s.db == nil {
		return fmt.Errorf("database not configured")
	}

	breach, err := s.db.GetBreach(ctx, breachID)
	if err != nil {
		return fmt.Errorf("failed to get breach: %w", err)
	}

	// Apply updates
	if status, ok := updates["status"].(string); ok {
		breach.Status = status
	}
	if consequences, ok := updates["consequences"].(string); ok {
		breach.Consequences = consequences
	}
	if mitigation, ok := updates["mitigation"].(string); ok {
		breach.Mitigation = mitigation
	}

	breach.UpdatedAt = time.Now()

	if err := s.db.UpdateBreach(ctx, breach); err != nil {
		return fmt.Errorf("failed to update breach: %w", err)
	}

	return nil
}

// ListBreaches retrieves breaches with optional filters
func (s *BreachService) ListBreaches(ctx context.Context, filters map[string]interface{}) ([]*BreachRecord, error) {
	if s.db == nil {
		return []*BreachRecord{}, nil
	}

	breaches, err := s.db.ListBreaches(ctx, filters)
	if err != nil {
		return nil, fmt.Errorf("failed to list breaches: %w", err)
	}

	return breaches, nil
}

// GetBreachStats retrieves breach statistics
func (s *BreachService) GetBreachStats(ctx context.Context) (*BreachStats, error) {
	if s.db == nil {
		return &BreachStats{
			BreachesByType:     make(map[string]int),
			BreachesBySeverity: make(map[string]int),
			BreachesByStatus:   make(map[string]int),
		}, nil
	}

	stats, err := s.db.GetBreachStats(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get stats: %w", err)
	}

	return stats, nil
}

// CheckDeadline checks if breach is within 72-hour reporting deadline
func (s *BreachService) CheckDeadline(breach *BreachRecord) (bool, time.Duration) {
	now := time.Now()
	withinDeadline := now.Before(breach.DeadlineAt)
	remaining := breach.DeadlineAt.Sub(now)

	if remaining < 0 {
		remaining = 0
	}

	return withinDeadline, remaining
}

// generateAuthorityNotification creates formal notification for DPA
func (s *BreachService) generateAuthorityNotification(breach *BreachRecord) string {
	return fmt.Sprintf(`DATA BREACH NOTIFICATION - GDPR Article 33

Breach ID: %s
Detected: %s
Type: %s
Severity: %s

Affected Data Subjects: %d
Affected Records: %d
Data Categories: %v

Description: %s

Root Cause: %s

Consequences: %s

Mitigation Measures: %s

Contact: compliance@fairforge.io
`,
		breach.ID.String(),
		breach.DetectedAt.Format(time.RFC3339),
		breach.BreachType,
		breach.Severity,
		breach.AffectedUserCount,
		breach.AffectedRecordCount,
		breach.DataCategories,
		breach.Description,
		breach.RootCause,
		breach.Consequences,
		breach.Mitigation)
}

// generateSubjectNotification creates plain-language notification for users
func (s *BreachService) generateSubjectNotification(breach *BreachRecord) string {
	return fmt.Sprintf(`IMPORTANT SECURITY NOTICE

We are writing to inform you of a data security incident that may affect your personal information.

What Happened:
%s

What Information Was Affected:
Your account may have been affected. The incident involved: %v

What We Are Doing:
%s

What You Can Do:
- Monitor your account for any suspicious activity
- Change your password immediately
- Enable two-factor authentication if not already enabled
- Contact us if you notice anything unusual

For more information or questions, please contact:
Email: security@fairforge.io
Reference: %s

We take the security of your data seriously and apologize for any inconvenience.
`,
		breach.Description,
		breach.DataCategories,
		breach.Mitigation,
		breach.ID.String())
}
