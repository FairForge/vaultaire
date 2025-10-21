package compliance

import (
	"context"
	"fmt"
	"time"
)

// PrivacyControl represents a privacy control mechanism
type PrivacyControl struct {
	ID        string                 `json:"id"`
	Type      PrivacyControlType     `json:"type"`
	Purpose   string                 `json:"purpose"`
	DataTypes []string               `json:"data_types"`
	Enabled   bool                   `json:"enabled"`
	Config    map[string]interface{} `json:"config"`
	CreatedAt time.Time              `json:"created_at"`
	UpdatedAt time.Time              `json:"updated_at"`
}

// PrivacyControlType defines types of privacy controls
type PrivacyControlType string

const (
	ControlDataMinimization  PrivacyControlType = "data_minimization"
	ControlPurposeLimitation PrivacyControlType = "purpose_limitation"
	ControlAccessControl     PrivacyControlType = "access_control"
	ControlPseudonymization  PrivacyControlType = "pseudonymization"
	ControlEncryption        PrivacyControlType = "encryption"
)

// DataMinimizationPolicy defines what data to collect
type DataMinimizationPolicy struct {
	ID            string   `json:"id"`
	Purpose       string   `json:"purpose"`
	RequiredData  []string `json:"required_data"`
	OptionalData  []string `json:"optional_data"`
	RetentionDays int      `json:"retention_days"`
	Active        bool     `json:"active"`
}

// PurposeBinding binds data to specific purposes
type PurposeBinding struct {
	DataID      string     `json:"data_id"`
	Purpose     string     `json:"purpose"`
	LawfulBasis string     `json:"lawful_basis"`
	BoundAt     time.Time  `json:"bound_at"`
	ExpiresAt   *time.Time `json:"expires_at,omitempty"`
}

// PrivacyService handles privacy controls
type PrivacyService struct {
	db       PrivacyDatabase
	controls map[string]*PrivacyControl
}

// PrivacyDatabase interface for privacy operations
type PrivacyDatabase interface {
	CreateControl(ctx context.Context, control *PrivacyControl) error
	GetControl(ctx context.Context, id string) (*PrivacyControl, error)
	UpdateControl(ctx context.Context, control *PrivacyControl) error
	ListControls(ctx context.Context) ([]*PrivacyControl, error)

	CreateMinimizationPolicy(ctx context.Context, policy *DataMinimizationPolicy) error
	GetMinimizationPolicy(ctx context.Context, purpose string) (*DataMinimizationPolicy, error)

	CreatePurposeBinding(ctx context.Context, binding *PurposeBinding) error
	CheckPurposeBinding(ctx context.Context, dataID, purpose string) (*PurposeBinding, error)
}

// NewPrivacyService creates a new privacy service
func NewPrivacyService(db PrivacyDatabase) *PrivacyService {
	return &PrivacyService{
		db:       db,
		controls: make(map[string]*PrivacyControl),
	}
}

// EnableControl enables a privacy control
func (s *PrivacyService) EnableControl(ctx context.Context, controlType PrivacyControlType, config map[string]interface{}) error {
	control := &PrivacyControl{
		ID:        fmt.Sprintf("control_%s_%d", controlType, time.Now().Unix()),
		Type:      controlType,
		Enabled:   true,
		Config:    config,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}

	if err := s.db.CreateControl(ctx, control); err != nil {
		return fmt.Errorf("create control: %w", err)
	}

	s.controls[control.ID] = control
	return nil
}

// MinimizeData applies data minimization rules
func (s *PrivacyService) MinimizeData(ctx context.Context, purpose string, data map[string]interface{}) (map[string]interface{}, error) {
	policy, err := s.db.GetMinimizationPolicy(ctx, purpose)
	if err != nil {
		return nil, fmt.Errorf("get minimization policy: %w", err)
	}

	if !policy.Active {
		return data, nil
	}

	minimized := make(map[string]interface{})
	for _, field := range policy.RequiredData {
		if val, ok := data[field]; ok {
			minimized[field] = val
		}
	}

	// Include optional data if present
	for _, field := range policy.OptionalData {
		if val, ok := data[field]; ok {
			minimized[field] = val
		}
	}

	return minimized, nil
}

// CheckPurpose verifies data can be used for purpose
func (s *PrivacyService) CheckPurpose(ctx context.Context, dataID, purpose string) (bool, error) {
	binding, err := s.db.CheckPurposeBinding(ctx, dataID, purpose)
	if err != nil {
		return false, err
	}

	if binding == nil {
		return false, nil
	}

	// Check if binding has expired
	if binding.ExpiresAt != nil && binding.ExpiresAt.Before(time.Now()) {
		return false, nil
	}

	return true, nil
}

// Pseudonymize replaces identifying data with pseudonyms
func (s *PrivacyService) Pseudonymize(ctx context.Context, data map[string]interface{}) (map[string]interface{}, map[string]string, error) {
	pseudonymized := make(map[string]interface{})
	mapping := make(map[string]string)

	for key, value := range data {
		if s.isIdentifier(key) {
			pseudonym := s.generatePseudonym(key, fmt.Sprintf("%v", value))
			pseudonymized[key] = pseudonym
			mapping[pseudonym] = fmt.Sprintf("%v", value)
		} else {
			pseudonymized[key] = value
		}
	}

	return pseudonymized, mapping, nil
}

// isIdentifier checks if field is personally identifiable
func (s *PrivacyService) isIdentifier(field string) bool {
	identifiers := []string{"email", "name", "phone", "ssn", "user_id", "ip_address"}
	for _, id := range identifiers {
		if field == id {
			return true
		}
	}
	return false
}

// generatePseudonym creates a pseudonym for data
func (s *PrivacyService) generatePseudonym(field, value string) string {
	// Simple pseudonymization - in production use proper hashing
	return fmt.Sprintf("PSEUDO_%s_%d", field, time.Now().UnixNano())
}
