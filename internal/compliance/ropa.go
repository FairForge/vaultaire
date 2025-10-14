package compliance

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"
)

// ROPAService handles GDPR Article 30 - Records of Processing Activities
type ROPAService struct {
	db     ROPADatabase
	logger *zap.Logger
}

// NewROPAService creates a new ROPA service
func NewROPAService(db ROPADatabase, logger *zap.Logger) *ROPAService {
	return &ROPAService{
		db:     db,
		logger: logger,
	}
}

// CreateActivity registers a new processing activity
func (s *ROPAService) CreateActivity(ctx context.Context, req *ProcessingActivityRequest) (*ProcessingActivity, error) {
	// Validation
	if req.Name == "" {
		return nil, fmt.Errorf("activity name required")
	}
	if req.Purpose == "" {
		return nil, fmt.Errorf("purpose required")
	}
	if req.LegalBasis == "" {
		return nil, fmt.Errorf("legal basis required")
	}

	// Validate legal basis
	if !s.isValidLegalBasis(req.LegalBasis) {
		return nil, fmt.Errorf("invalid legal basis: %s", req.LegalBasis)
	}

	// Validate special category basis if provided
	if req.SpecialCategoryBasis != "" && !s.isValidSpecialBasis(req.SpecialCategoryBasis) {
		return nil, fmt.Errorf("invalid special category basis: %s", req.SpecialCategoryBasis)
	}

	activity := &ProcessingActivity{
		ID:                   uuid.New(),
		Name:                 req.Name,
		Description:          req.Description,
		Purpose:              req.Purpose,
		LegalBasis:           req.LegalBasis,
		SpecialCategoryBasis: req.SpecialCategoryBasis,
		ControllerName:       req.ControllerName,
		ControllerContact:    req.ControllerContact,
		DPOName:              req.DPOName,
		DPOContact:           req.DPOContact,
		RetentionPeriod:      req.RetentionPeriod,
		SecurityMeasures:     req.SecurityMeasures,
		TransferDetails:      req.TransferDetails,
		Status:               ActivityStatusActive,
		CreatedAt:            time.Now(),
		UpdatedAt:            time.Now(),
	}

	if s.db != nil {
		if err := s.db.CreateActivity(ctx, activity); err != nil {
			return nil, fmt.Errorf("failed to create activity: %w", err)
		}

		// Add data categories
		if len(req.DataCategories) > 0 {
			categories := make([]DataCategory, 0, len(req.DataCategories))
			for _, cat := range req.DataCategories {
				categories = append(categories, DataCategory{
					ID:         uuid.New(),
					ActivityID: activity.ID,
					Category:   cat,
				})
			}
			if err := s.db.AddDataCategories(ctx, activity.ID, categories); err != nil {
				s.logger.Error("failed to add data categories", zap.Error(err))
			}
		}

		// Add data subject categories
		if len(req.DataSubjectCategories) > 0 {
			subjects := make([]DataSubjectCategory, 0, len(req.DataSubjectCategories))
			for _, subj := range req.DataSubjectCategories {
				subjects = append(subjects, DataSubjectCategory{
					ID:         uuid.New(),
					ActivityID: activity.ID,
					Category:   subj,
				})
			}
			if err := s.db.AddDataSubjects(ctx, activity.ID, subjects); err != nil {
				s.logger.Error("failed to add data subjects", zap.Error(err))
			}
		}

		// Add recipients
		if len(req.Recipients) > 0 {
			recipients := make([]Recipient, 0, len(req.Recipients))
			for _, rec := range req.Recipients {
				recipients = append(recipients, Recipient{
					ID:         uuid.New(),
					ActivityID: activity.ID,
					Name:       rec.Name,
					Type:       rec.Type,
					Purpose:    rec.Purpose,
					Country:    rec.Country,
					Safeguards: rec.Safeguards,
				})
			}
			if err := s.db.AddRecipients(ctx, activity.ID, recipients); err != nil {
				s.logger.Error("failed to add recipients", zap.Error(err))
			}
		}
	}

	s.logger.Info("processing activity created",
		zap.String("activity_id", activity.ID.String()),
		zap.String("name", activity.Name),
		zap.String("legal_basis", activity.LegalBasis))

	return activity, nil
}

// GetActivity retrieves a processing activity
func (s *ROPAService) GetActivity(ctx context.Context, activityID uuid.UUID) (*ProcessingActivity, error) {
	if s.db == nil {
		return nil, fmt.Errorf("database not configured")
	}

	activity, err := s.db.GetActivity(ctx, activityID)
	if err != nil {
		return nil, fmt.Errorf("failed to get activity: %w", err)
	}

	// Load related data
	categories, _ := s.db.GetDataCategories(ctx, activityID)
	activity.DataCategories = categories

	subjects, _ := s.db.GetDataSubjects(ctx, activityID)
	activity.DataSubjectCategories = subjects

	recipients, _ := s.db.GetRecipients(ctx, activityID)
	activity.Recipients = recipients

	return activity, nil
}

// UpdateActivity updates a processing activity
func (s *ROPAService) UpdateActivity(ctx context.Context, activityID uuid.UUID, updates map[string]interface{}) error {
	if s.db == nil {
		return fmt.Errorf("database not configured")
	}

	activity, err := s.db.GetActivity(ctx, activityID)
	if err != nil {
		return fmt.Errorf("failed to get activity: %w", err)
	}

	// Apply updates
	if name, ok := updates["name"].(string); ok {
		activity.Name = name
	}
	if desc, ok := updates["description"].(string); ok {
		activity.Description = desc
	}
	if purpose, ok := updates["purpose"].(string); ok {
		activity.Purpose = purpose
	}
	if retention, ok := updates["retention_period"].(string); ok {
		activity.RetentionPeriod = retention
	}
	if security, ok := updates["security_measures"].(string); ok {
		activity.SecurityMeasures = security
	}
	if status, ok := updates["status"].(string); ok {
		activity.Status = status
	}

	activity.UpdatedAt = time.Now()

	if err := s.db.UpdateActivity(ctx, activity); err != nil {
		return fmt.Errorf("failed to update activity: %w", err)
	}

	return nil
}

// DeleteActivity marks an activity as inactive
func (s *ROPAService) DeleteActivity(ctx context.Context, activityID uuid.UUID) error {
	if s.db == nil {
		return fmt.Errorf("database not configured")
	}

	// Don't actually delete - just mark as inactive
	return s.UpdateActivity(ctx, activityID, map[string]interface{}{
		"status": ActivityStatusInactive,
	})
}

// ListActivities retrieves all activities with optional filters
func (s *ROPAService) ListActivities(ctx context.Context, filters map[string]interface{}) ([]*ProcessingActivity, error) {
	if s.db == nil {
		return []*ProcessingActivity{}, nil
	}

	activities, err := s.db.ListActivities(ctx, filters)
	if err != nil {
		return nil, fmt.Errorf("failed to list activities: %w", err)
	}

	return activities, nil
}

// ReviewActivity marks an activity as reviewed
func (s *ROPAService) ReviewActivity(ctx context.Context, activityID uuid.UUID, reviewedBy uuid.UUID, notes string) error {
	if s.db == nil {
		return fmt.Errorf("database not configured")
	}

	review := &ActivityReview{
		ID:         uuid.New(),
		ActivityID: activityID,
		ReviewedBy: reviewedBy,
		Notes:      notes,
		ReviewedAt: time.Now(),
	}

	if err := s.db.CreateReview(ctx, review); err != nil {
		return fmt.Errorf("failed to create review: %w", err)
	}

	// Update activity
	now := time.Now()
	activity, err := s.db.GetActivity(ctx, activityID)
	if err != nil {
		return fmt.Errorf("failed to get activity: %w", err)
	}

	activity.LastReviewedAt = &now
	activity.ReviewedBy = &reviewedBy
	activity.UpdatedAt = now

	if err := s.db.UpdateActivity(ctx, activity); err != nil {
		return fmt.Errorf("failed to update activity: %w", err)
	}

	s.logger.Info("activity reviewed",
		zap.String("activity_id", activityID.String()),
		zap.String("reviewed_by", reviewedBy.String()))

	return nil
}

// ValidateCompliance checks if an activity is compliant
func (s *ROPAService) ValidateCompliance(ctx context.Context, activityID uuid.UUID) (*ComplianceCheck, error) {
	activity, err := s.GetActivity(ctx, activityID)
	if err != nil {
		return nil, fmt.Errorf("failed to get activity: %w", err)
	}

	check := &ComplianceCheck{
		ActivityID:   activity.ID,
		ActivityName: activity.Name,
		IsCompliant:  true,
		Issues:       []string{},
		Warnings:     []string{},
		CheckedAt:    time.Now(),
	}

	// Check required fields
	if activity.Name == "" {
		check.Issues = append(check.Issues, "Activity name is required")
		check.IsCompliant = false
	}
	if activity.Purpose == "" {
		check.Issues = append(check.Issues, "Purpose is required")
		check.IsCompliant = false
	}
	if activity.LegalBasis == "" {
		check.Issues = append(check.Issues, "Legal basis is required")
		check.IsCompliant = false
	}
	if activity.RetentionPeriod == "" {
		check.Issues = append(check.Issues, "Retention period is required")
		check.IsCompliant = false
	}
	if activity.SecurityMeasures == "" {
		check.Warnings = append(check.Warnings, "Security measures should be documented")
	}

	// Check data categories
	if len(activity.DataCategories) == 0 {
		check.Warnings = append(check.Warnings, "No data categories specified")
	}

	// Check data subjects
	if len(activity.DataSubjectCategories) == 0 {
		check.Warnings = append(check.Warnings, "No data subject categories specified")
	}

	// Check review status (annual review required)
	if activity.LastReviewedAt == nil {
		check.Warnings = append(check.Warnings, "Activity has never been reviewed")
	} else {
		daysSinceReview := time.Since(*activity.LastReviewedAt).Hours() / 24
		if daysSinceReview > 365 {
			check.Warnings = append(check.Warnings, fmt.Sprintf("Activity hasn't been reviewed in %.0f days", daysSinceReview))
		}
	}

	return check, nil
}

// GenerateROPAReport generates a complete ROPA report
func (s *ROPAService) GenerateROPAReport(ctx context.Context, organizationName string) (*ROPAReport, error) {
	activities, err := s.ListActivities(ctx, map[string]interface{}{
		"status": ActivityStatusActive,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list activities: %w", err)
	}

	// Load full details for each activity
	for i, activity := range activities {
		fullActivity, err := s.GetActivity(ctx, activity.ID)
		if err != nil {
			s.logger.Error("failed to load activity details", zap.Error(err))
			continue
		}
		activities[i] = fullActivity
	}

	// Find last review date
	var lastReview time.Time
	for _, activity := range activities {
		if activity.LastReviewedAt != nil && activity.LastReviewedAt.After(lastReview) {
			lastReview = *activity.LastReviewedAt
		}
	}

	// Next review is 1 year from last review
	nextReview := lastReview.AddDate(1, 0, 0)
	if lastReview.IsZero() {
		nextReview = time.Now().AddDate(1, 0, 0)
	}

	report := &ROPAReport{
		GeneratedAt:      time.Now(),
		OrganizationName: organizationName,
		Activities:       activities,
		TotalActivities:  len(activities),
		LastReviewDate:   lastReview,
		NextReviewDue:    nextReview,
	}

	return report, nil
}

// GetROPAStats retrieves ROPA statistics
func (s *ROPAService) GetROPAStats(ctx context.Context) (*ROPAStats, error) {
	if s.db == nil {
		return &ROPAStats{
			ActivitiesByLegalBasis: make(map[string]int),
			ActivitiesByStatus:     make(map[string]int),
		}, nil
	}

	stats, err := s.db.GetROPAStats(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get stats: %w", err)
	}

	return stats, nil
}

// isValidLegalBasis checks if a legal basis is valid
func (s *ROPAService) isValidLegalBasis(basis string) bool {
	validBases := []string{
		LegalBasisConsent,
		LegalBasisContract,
		LegalBasisLegalObligation,
		LegalBasisVitalInterests,
		LegalBasisPublicTask,
		LegalBasisLegitimateInterests,
	}
	for _, valid := range validBases {
		if basis == valid {
			return true
		}
	}
	return false
}

// isValidSpecialBasis checks if a special category basis is valid
func (s *ROPAService) isValidSpecialBasis(basis string) bool {
	validBases := []string{
		SpecialBasisExplicitConsent,
		SpecialBasisEmployment,
		SpecialBasisVitalInterests,
		SpecialBasisLegitimateActivities,
		SpecialBasisPublicData,
		SpecialBasisLegalClaims,
		SpecialBasisSubstantialPublicInterest,
		SpecialBasisHealthCare,
		SpecialBasisPublicHealth,
		SpecialBasisArchiving,
	}
	for _, valid := range validBases {
		if basis == valid {
			return true
		}
	}
	return false
}
