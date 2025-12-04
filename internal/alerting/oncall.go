// internal/alerting/oncall.go
package alerting

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Rotation types
const (
	RotationTypeDaily  = "daily"
	RotationTypeWeekly = "weekly"
	RotationTypeCustom = "custom"
)

// RotationConfig configures an on-call rotation
type RotationConfig struct {
	Name          string        `json:"name"`
	Type          string        `json:"type"`
	Users         []string      `json:"users"`
	StartTime     time.Time     `json:"start_time"`
	ShiftDuration time.Duration `json:"shift_duration"`
	Layer         int           `json:"layer"`
}

// Validate checks configuration
func (c *RotationConfig) Validate() error {
	if c.Name == "" {
		return errors.New("oncall: name is required")
	}
	if len(c.Users) == 0 {
		return errors.New("oncall: at least one user is required")
	}
	validTypes := map[string]bool{
		RotationTypeDaily:  true,
		RotationTypeWeekly: true,
		RotationTypeCustom: true,
	}
	if !validTypes[c.Type] {
		return fmt.Errorf("oncall: invalid rotation type: %s", c.Type)
	}
	return nil
}

// OverrideConfig configures an override
type OverrideConfig struct {
	User    string    `json:"user"`
	StartAt time.Time `json:"start_at"`
	EndAt   time.Time `json:"end_at"`
	Reason  string    `json:"reason"`
}

// Override represents a schedule override
type Override struct {
	ID      string    `json:"id"`
	User    string    `json:"user"`
	StartAt time.Time `json:"start_at"`
	EndAt   time.Time `json:"end_at"`
	Reason  string    `json:"reason"`
}

// ShiftEntry represents a scheduled shift
type ShiftEntry struct {
	User    string    `json:"user"`
	StartAt time.Time `json:"start_at"`
	EndAt   time.Time `json:"end_at"`
}

// Rotation represents an on-call rotation
type Rotation struct {
	config    *RotationConfig
	overrides []*Override
	mu        sync.RWMutex
}

// Name returns the rotation name
func (r *Rotation) Name() string {
	return r.config.Name
}

// Layer returns the rotation layer
func (r *Rotation) Layer() int {
	return r.config.Layer
}

// shiftDuration returns the duration of a single shift
func (r *Rotation) shiftDuration() time.Duration {
	switch r.config.Type {
	case RotationTypeDaily:
		return 24 * time.Hour
	case RotationTypeWeekly:
		return 7 * 24 * time.Hour
	case RotationTypeCustom:
		if r.config.ShiftDuration > 0 {
			return r.config.ShiftDuration
		}
		return 24 * time.Hour
	default:
		return 24 * time.Hour
	}
}

// CurrentOnCall returns who is currently on call
func (r *Rotation) CurrentOnCall() string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	now := time.Now()

	// Check for active override
	for _, override := range r.overrides {
		if now.After(override.StartAt) && now.Before(override.EndAt) {
			return override.User
		}
	}

	// Calculate based on rotation
	return r.userAtTime(now)
}

func (r *Rotation) userAtTime(t time.Time) string {
	if len(r.config.Users) == 0 {
		return ""
	}

	elapsed := t.Sub(r.config.StartTime)
	if elapsed < 0 {
		return r.config.Users[0]
	}

	shiftDur := r.shiftDuration()
	shiftIndex := int(elapsed / shiftDur)
	userIndex := shiftIndex % len(r.config.Users)

	return r.config.Users[userIndex]
}

// AddOverride adds a schedule override
func (r *Rotation) AddOverride(config *OverrideConfig) (*Override, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	override := &Override{
		ID:      uuid.New().String(),
		User:    config.User,
		StartAt: config.StartAt,
		EndAt:   config.EndAt,
		Reason:  config.Reason,
	}

	r.overrides = append(r.overrides, override)
	return override, nil
}

// RemoveOverride removes an override
func (r *Rotation) RemoveOverride(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	for i, override := range r.overrides {
		if override.ID == id {
			r.overrides = append(r.overrides[:i], r.overrides[i+1:]...)
			return nil
		}
	}

	return fmt.Errorf("oncall: override %s not found", id)
}

// Schedule returns the schedule for a time range
func (r *Rotation) Schedule(start, end time.Time) []ShiftEntry {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var entries []ShiftEntry
	shiftDur := r.shiftDuration()

	// Align to shift boundary
	current := r.config.StartTime
	for current.Before(start) {
		current = current.Add(shiftDur)
	}
	if current.After(start) {
		current = current.Add(-shiftDur)
	}

	for current.Before(end) {
		shiftEnd := current.Add(shiftDur)
		user := r.userAtTime(current)

		entries = append(entries, ShiftEntry{
			User:    user,
			StartAt: current,
			EndAt:   shiftEnd,
		})

		current = shiftEnd
	}

	return entries
}

// NextHandoff returns the next handoff time
func (r *Rotation) NextHandoff() time.Time {
	now := time.Now()
	shiftDur := r.shiftDuration()

	elapsed := now.Sub(r.config.StartTime)
	if elapsed < 0 {
		return r.config.StartTime
	}

	shiftsCompleted := int(elapsed / shiftDur)
	nextHandoff := r.config.StartTime.Add(time.Duration(shiftsCompleted+1) * shiftDur)

	return nextHandoff
}

// Overrides returns all overrides
func (r *Rotation) Overrides() []*Override {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.overrides
}

// OnCallManager manages on-call rotations
type OnCallManager struct {
	rotations map[string]*Rotation
	mu        sync.RWMutex
}

// NewOnCallManager creates an on-call manager
func NewOnCallManager() *OnCallManager {
	return &OnCallManager{
		rotations: make(map[string]*Rotation),
	}
}

// AddRotation adds a rotation
func (m *OnCallManager) AddRotation(config *RotationConfig) (*Rotation, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.rotations[config.Name]; exists {
		return nil, fmt.Errorf("oncall: rotation %s already exists", config.Name)
	}

	rotation := &Rotation{
		config:    config,
		overrides: make([]*Override, 0),
	}

	m.rotations[config.Name] = rotation
	return rotation, nil
}

// GetRotation returns a rotation by name
func (m *OnCallManager) GetRotation(name string) *Rotation {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.rotations[name]
}

// RemoveRotation removes a rotation
func (m *OnCallManager) RemoveRotation(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.rotations[name]; !exists {
		return fmt.Errorf("oncall: rotation %s not found", name)
	}

	delete(m.rotations, name)
	return nil
}

// ListRotations returns all rotations
func (m *OnCallManager) ListRotations() []*Rotation {
	m.mu.RLock()
	defer m.mu.RUnlock()

	rotations := make([]*Rotation, 0, len(m.rotations))
	for _, r := range m.rotations {
		rotations = append(rotations, r)
	}
	return rotations
}

// WhoIsOnCall returns all currently on-call users
func (m *OnCallManager) WhoIsOnCall() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var oncall []string
	for _, rotation := range m.rotations {
		oncall = append(oncall, rotation.CurrentOnCall())
	}
	return oncall
}

// WhoIsOnCallByLayer returns the on-call user for a specific layer
func (m *OnCallManager) WhoIsOnCallByLayer(layer int) string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	for _, rotation := range m.rotations {
		if rotation.Layer() == layer {
			return rotation.CurrentOnCall()
		}
	}
	return ""
}
