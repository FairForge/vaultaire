package compliance

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"
)

// ConsentService handles GDPR Article 7 & 8 - Consent Management
type ConsentService struct {
	db     ConsentDatabase
	logger *zap.Logger
}

// NewConsentService creates a new consent management service
func NewConsentService(db ConsentDatabase, logger *zap.Logger) *ConsentService {
	return &ConsentService{
		db:     db,
		logger: logger,
	}
}

// GrantConsent grants consent for a specific purpose
func (s *ConsentService) GrantConsent(ctx context.Context, req *ConsentRequest) (*ConsentRecord, error) {
	if req.UserID == uuid.Nil {
		return nil, fmt.Errorf("user_id required")
	}
	if req.Purpose == "" {
		return nil, fmt.Errorf("purpose required")
	}

	// Validate purpose exists
	if s.db != nil {
		if _, err := s.db.GetConsentPurpose(ctx, req.Purpose); err != nil {
			return nil, fmt.Errorf("invalid purpose: %s", req.Purpose)
		}
	}

	now := time.Now()
	var consent *ConsentRecord

	// Check if consent already exists
	if s.db != nil {
		existing, err := s.db.GetConsent(ctx, req.UserID, req.Purpose)
		if err == nil && existing != nil {
			// Make a copy to avoid race conditions
			consentCopy := *existing
			consent = &consentCopy

			// Update consent
			consent.Granted = req.Granted
			consent.Method = req.Method
			consent.IPAddress = req.IPAddress
			consent.UserAgent = req.UserAgent
			consent.TermsVersion = req.TermsVersion
			consent.Metadata = req.Metadata
			consent.UpdatedAt = now

			if req.Granted {
				consent.GrantedAt = &now
				consent.WithdrawnAt = nil
			} else {
				consent.WithdrawnAt = &now
			}

			if err := s.db.UpdateConsent(ctx, consent); err != nil {
				return nil, fmt.Errorf("failed to update consent: %w", err)
			}
		} else {
			// Create new consent
			consent = &ConsentRecord{
				ID:           uuid.New(),
				UserID:       req.UserID,
				Purpose:      req.Purpose,
				Granted:      req.Granted,
				Method:       req.Method,
				IPAddress:    req.IPAddress,
				UserAgent:    req.UserAgent,
				TermsVersion: req.TermsVersion,
				Metadata:     req.Metadata,
				CreatedAt:    now,
				UpdatedAt:    now,
			}

			if req.Granted {
				consent.GrantedAt = &now
			}

			if err := s.db.CreateConsent(ctx, consent); err != nil {
				return nil, fmt.Errorf("failed to create consent: %w", err)
			}
		}

		// Audit log
		action := ConsentActionGrant
		if !req.Granted {
			action = ConsentActionWithdraw
		}
		if err := s.auditConsent(ctx, req.UserID, req.Purpose, action, req); err != nil {
			s.logger.Error("failed to audit consent", zap.Error(err))
		}
	} else {
		// No database - create in-memory consent
		consent = &ConsentRecord{
			ID:           uuid.New(),
			UserID:       req.UserID,
			Purpose:      req.Purpose,
			Granted:      req.Granted,
			Method:       req.Method,
			IPAddress:    req.IPAddress,
			UserAgent:    req.UserAgent,
			TermsVersion: req.TermsVersion,
			Metadata:     req.Metadata,
			CreatedAt:    now,
			UpdatedAt:    now,
		}

		if req.Granted {
			consent.GrantedAt = &now
		}
	}

	s.logger.Info("consent updated",
		zap.String("user_id", req.UserID.String()),
		zap.String("purpose", req.Purpose),
		zap.Bool("granted", req.Granted))

	return consent, nil
}

// WithdrawConsent withdraws consent for a specific purpose
func (s *ConsentService) WithdrawConsent(ctx context.Context, userID uuid.UUID, purpose string, req *ConsentRequest) error {
	if userID == uuid.Nil {
		return fmt.Errorf("user_id required")
	}
	if purpose == "" {
		return fmt.Errorf("purpose required")
	}

	if s.db == nil {
		return fmt.Errorf("database not configured")
	}

	existing, err := s.db.GetConsent(ctx, userID, purpose)
	if err != nil {
		return fmt.Errorf("consent not found: %w", err)
	}

	// Make a copy to avoid race conditions
	consent := *existing
	now := time.Now()
	consent.Granted = false
	consent.WithdrawnAt = &now
	consent.Method = req.Method
	consent.IPAddress = req.IPAddress
	consent.UserAgent = req.UserAgent
	consent.UpdatedAt = now

	if err := s.db.UpdateConsent(ctx, &consent); err != nil {
		return fmt.Errorf("failed to withdraw consent: %w", err)
	}

	// Audit log
	if err := s.auditConsent(ctx, userID, purpose, ConsentActionWithdraw, req); err != nil {
		s.logger.Error("failed to audit consent withdrawal", zap.Error(err))
	}

	s.logger.Info("consent withdrawn",
		zap.String("user_id", userID.String()),
		zap.String("purpose", purpose))

	return nil
}

// GetConsentStatus retrieves all consents for a user
func (s *ConsentService) GetConsentStatus(ctx context.Context, userID uuid.UUID) (*ConsentStatus, error) {
	if userID == uuid.Nil {
		return nil, fmt.Errorf("user_id required")
	}

	if s.db == nil {
		return &ConsentStatus{
			UserID:   userID,
			Consents: make(map[string]*ConsentRecord),
		}, nil
	}

	consents, err := s.db.ListUserConsents(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("failed to get consents: %w", err)
	}

	status := &ConsentStatus{
		UserID:    userID,
		Consents:  make(map[string]*ConsentRecord),
		UpdatedAt: time.Now(),
	}

	for _, consent := range consents {
		status.Consents[consent.Purpose] = consent
		if consent.UpdatedAt.After(status.UpdatedAt) {
			status.UpdatedAt = consent.UpdatedAt
		}
	}

	return status, nil
}

// CheckConsent checks if user has granted consent for a purpose
func (s *ConsentService) CheckConsent(ctx context.Context, userID uuid.UUID, purpose string) (bool, error) {
	if s.db == nil {
		return false, nil
	}

	consent, err := s.db.GetConsent(ctx, userID, purpose)
	if err != nil {
		if err == ErrNotFound {
			return false, nil
		}
		return false, err
	}

	return consent.Granted && consent.WithdrawnAt == nil, nil
}

// GetConsentHistory retrieves consent audit history for a user
func (s *ConsentService) GetConsentHistory(ctx context.Context, userID uuid.UUID) ([]*ConsentAuditEntry, error) {
	if userID == uuid.Nil {
		return nil, fmt.Errorf("user_id required")
	}

	if s.db == nil {
		return []*ConsentAuditEntry{}, nil
	}

	history, err := s.db.GetConsentHistory(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("failed to get consent history: %w", err)
	}

	return history, nil
}

// ListPurposes lists all available consent purposes
func (s *ConsentService) ListPurposes(ctx context.Context) ([]*ConsentPurpose, error) {
	if s.db == nil {
		return []*ConsentPurpose{}, nil
	}

	purposes, err := s.db.ListConsentPurposes(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to list purposes: %w", err)
	}

	return purposes, nil
}

// VerifyAge checks if user meets minimum age requirement
func (s *ConsentService) VerifyAge(birthDate time.Time, minimumAge int) bool {
	age := time.Now().Year() - birthDate.Year()

	// Adjust if birthday hasn't occurred yet this year
	if time.Now().YearDay() < birthDate.YearDay() {
		age--
	}

	return age >= minimumAge
}

// RequiresParentalConsent checks if user requires parental consent (under 16)
func (s *ConsentService) RequiresParentalConsent(birthDate time.Time) bool {
	return !s.VerifyAge(birthDate, 16)
}

// auditConsent creates an audit log entry for consent actions
func (s *ConsentService) auditConsent(ctx context.Context, userID uuid.UUID, purpose, action string, req *ConsentRequest) error {
	if s.db == nil {
		return nil
	}

	entry := &ConsentAuditEntry{
		ID:        uuid.New(),
		UserID:    userID,
		Purpose:   purpose,
		Action:    action,
		Granted:   req.Granted,
		Method:    req.Method,
		IPAddress: req.IPAddress,
		UserAgent: req.UserAgent,
		Metadata:  req.Metadata,
		CreatedAt: time.Now(),
	}

	return s.db.CreateConsentAudit(ctx, entry)
}
