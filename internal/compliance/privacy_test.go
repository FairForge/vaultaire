package compliance

import (
	"context"
	"testing"
	"time"
)

// MockPrivacyDatabase for testing
type MockPrivacyDatabase struct {
	controls map[string]*PrivacyControl
	policies map[string]*DataMinimizationPolicy
	bindings map[string]*PurposeBinding
}

func NewMockPrivacyDatabase() *MockPrivacyDatabase {
	return &MockPrivacyDatabase{
		controls: make(map[string]*PrivacyControl),
		policies: make(map[string]*DataMinimizationPolicy),
		bindings: make(map[string]*PurposeBinding),
	}
}

func (m *MockPrivacyDatabase) CreateControl(ctx context.Context, control *PrivacyControl) error {
	m.controls[control.ID] = control
	return nil
}

func (m *MockPrivacyDatabase) GetControl(ctx context.Context, id string) (*PrivacyControl, error) {
	return m.controls[id], nil
}

func (m *MockPrivacyDatabase) UpdateControl(ctx context.Context, control *PrivacyControl) error {
	m.controls[control.ID] = control
	return nil
}

func (m *MockPrivacyDatabase) ListControls(ctx context.Context) ([]*PrivacyControl, error) {
	var controls []*PrivacyControl
	for _, c := range m.controls {
		controls = append(controls, c)
	}
	return controls, nil
}

func (m *MockPrivacyDatabase) CreateMinimizationPolicy(ctx context.Context, policy *DataMinimizationPolicy) error {
	m.policies[policy.Purpose] = policy
	return nil
}

func (m *MockPrivacyDatabase) GetMinimizationPolicy(ctx context.Context, purpose string) (*DataMinimizationPolicy, error) {
	policy, ok := m.policies[purpose]
	if !ok {
		return &DataMinimizationPolicy{Active: false}, nil
	}
	return policy, nil
}

func (m *MockPrivacyDatabase) CreatePurposeBinding(ctx context.Context, binding *PurposeBinding) error {
	key := binding.DataID + ":" + binding.Purpose
	m.bindings[key] = binding
	return nil
}

func (m *MockPrivacyDatabase) CheckPurposeBinding(ctx context.Context, dataID, purpose string) (*PurposeBinding, error) {
	key := dataID + ":" + purpose
	return m.bindings[key], nil
}

func TestPrivacyService_EnableControl(t *testing.T) {
	ctx := context.Background()
	db := NewMockPrivacyDatabase()
	service := NewPrivacyService(db)

	config := map[string]interface{}{
		"level": "strict",
	}

	err := service.EnableControl(ctx, ControlDataMinimization, config)
	if err != nil {
		t.Fatalf("EnableControl failed: %v", err)
	}

	if len(service.controls) != 1 {
		t.Errorf("Expected 1 control, got %d", len(service.controls))
	}
}

func TestPrivacyService_MinimizeData(t *testing.T) {
	ctx := context.Background()
	db := NewMockPrivacyDatabase()
	service := NewPrivacyService(db)

	// Set up minimization policy
	policy := &DataMinimizationPolicy{
		Purpose:      "analytics",
		RequiredData: []string{"user_id", "timestamp"},
		OptionalData: []string{"country"},
		Active:       true,
	}
	_ = db.CreateMinimizationPolicy(ctx, policy)

	// Test data with extra fields
	data := map[string]interface{}{
		"user_id":   "123",
		"timestamp": "2024-01-01",
		"email":     "user@example.com", // Should be removed
		"country":   "US",
		"ssn":       "123-45-6789", // Should be removed
	}

	minimized, err := service.MinimizeData(ctx, "analytics", data)
	if err != nil {
		t.Fatalf("MinimizeData failed: %v", err)
	}

	// Check required fields present
	if _, ok := minimized["user_id"]; !ok {
		t.Error("Required field 'user_id' missing")
	}
	if _, ok := minimized["timestamp"]; !ok {
		t.Error("Required field 'timestamp' missing")
	}

	// Check sensitive fields removed
	if _, ok := minimized["email"]; ok {
		t.Error("Sensitive field 'email' should be removed")
	}
	if _, ok := minimized["ssn"]; ok {
		t.Error("Sensitive field 'ssn' should be removed")
	}

	// Check optional field present
	if _, ok := minimized["country"]; !ok {
		t.Error("Optional field 'country' should be included")
	}
}

func TestPrivacyService_CheckPurpose(t *testing.T) {
	ctx := context.Background()
	db := NewMockPrivacyDatabase()
	service := NewPrivacyService(db)

	// Create binding
	binding := &PurposeBinding{
		DataID:      "file123",
		Purpose:     "backup",
		LawfulBasis: "consent",
		BoundAt:     time.Now(),
	}
	_ = db.CreatePurposeBinding(ctx, binding)

	// Check valid purpose
	allowed, err := service.CheckPurpose(ctx, "file123", "backup")
	if err != nil {
		t.Fatalf("CheckPurpose failed: %v", err)
	}
	if !allowed {
		t.Error("Expected purpose to be allowed")
	}

	// Check invalid purpose
	allowed, err = service.CheckPurpose(ctx, "file123", "marketing")
	if err != nil {
		t.Fatalf("CheckPurpose failed: %v", err)
	}
	if allowed {
		t.Error("Expected purpose to be denied")
	}
}

func TestPrivacyService_Pseudonymize(t *testing.T) {
	ctx := context.Background()
	db := NewMockPrivacyDatabase()
	service := NewPrivacyService(db)

	data := map[string]interface{}{
		"email":   "user@example.com",
		"name":    "John Doe",
		"age":     30,
		"country": "US",
	}

	pseudonymized, mapping, err := service.Pseudonymize(ctx, data)
	if err != nil {
		t.Fatalf("Pseudonymize failed: %v", err)
	}

	// Check identifiers are pseudonymized
	if pseudonymized["email"] == "user@example.com" {
		t.Error("Email should be pseudonymized")
	}
	if pseudonymized["name"] == "John Doe" {
		t.Error("Name should be pseudonymized")
	}

	// Check non-identifiers are preserved
	if pseudonymized["age"] != 30 {
		t.Error("Age should be preserved")
	}
	if pseudonymized["country"] != "US" {
		t.Error("Country should be preserved")
	}

	// Check mapping exists
	if len(mapping) != 2 {
		t.Errorf("Expected 2 mappings, got %d", len(mapping))
	}
}
