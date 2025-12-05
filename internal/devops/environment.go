// internal/devops/environment.go
package devops

import (
	"errors"
	"fmt"
	"sync"
	"time"
)

// Environment types
const (
	EnvTypeDevelopment = "development"
	EnvTypeStaging     = "staging"
	EnvTypeProduction  = "production"
	EnvTypeTesting     = "testing"
)

// Tiers
const (
	TierPrimary   = "primary"
	TierSecondary = "secondary"
	TierDR        = "disaster-recovery"
)

// EnvironmentConfig configures an environment
type EnvironmentConfig struct {
	Name        string `json:"name"`
	Type        string `json:"type"`
	Tier        string `json:"tier"`
	Description string `json:"description"`
}

// Validate checks configuration
func (c *EnvironmentConfig) Validate() error {
	if c.Name == "" {
		return errors.New("environment: name is required")
	}

	validTypes := map[string]bool{
		EnvTypeDevelopment: true,
		EnvTypeStaging:     true,
		EnvTypeProduction:  true,
		EnvTypeTesting:     true,
	}

	if c.Type != "" && !validTypes[c.Type] {
		return fmt.Errorf("environment: invalid type: %s", c.Type)
	}

	return nil
}

// MaintenanceWindow defines a maintenance window
type MaintenanceWindow struct {
	Day       time.Weekday  `json:"day"`
	StartHour int           `json:"start_hour"`
	Duration  time.Duration `json:"duration"`
}

// ResourceLimits defines resource limits
type ResourceLimits struct {
	MaxCPU     string `json:"max_cpu"`
	MaxMemory  string `json:"max_memory"`
	MaxStorage string `json:"max_storage"`
}

// LockInfo contains lock information
type LockInfo struct {
	Reason   string    `json:"reason"`
	LockedBy string    `json:"locked_by"`
	LockedAt time.Time `json:"locked_at"`
}

// Environment represents a deployment environment
type Environment struct {
	config            *EnvironmentConfig
	variables         map[string]string
	secrets           map[string]string
	locked            bool
	lockInfo          *LockInfo
	maintenanceWindow *MaintenanceWindow
	resourceLimits    *ResourceLimits
	mu                sync.RWMutex
}

// Name returns the environment name
func (e *Environment) Name() string {
	return e.config.Name
}

// Type returns the environment type
func (e *Environment) Type() string {
	return e.config.Type
}

// SetVariable sets a variable
func (e *Environment) SetVariable(key, value string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.variables[key] = value
	return nil
}

// GetVariable gets a variable
func (e *Environment) GetVariable(key string) (string, error) {
	e.mu.RLock()
	defer e.mu.RUnlock()

	value, exists := e.variables[key]
	if !exists {
		return "", fmt.Errorf("environment: variable %s not found", key)
	}
	return value, nil
}

// Variables returns all variables
func (e *Environment) Variables() map[string]string {
	e.mu.RLock()
	defer e.mu.RUnlock()

	vars := make(map[string]string)
	for k, v := range e.variables {
		vars[k] = v
	}
	return vars
}

// SetSecret sets a secret
func (e *Environment) SetSecret(key, value string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.secrets[key] = value
	return nil
}

// GetSecret gets a secret
func (e *Environment) GetSecret(key string) (string, error) {
	e.mu.RLock()
	defer e.mu.RUnlock()

	value, exists := e.secrets[key]
	if !exists {
		return "", fmt.Errorf("environment: secret %s not found", key)
	}
	return value, nil
}

// Lock locks the environment
func (e *Environment) Lock(reason, lockedBy string) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	e.locked = true
	e.lockInfo = &LockInfo{
		Reason:   reason,
		LockedBy: lockedBy,
		LockedAt: time.Now(),
	}
	return nil
}

// Unlock unlocks the environment
func (e *Environment) Unlock() error {
	e.mu.Lock()
	defer e.mu.Unlock()

	e.locked = false
	e.lockInfo = nil
	return nil
}

// IsLocked returns whether the environment is locked
func (e *Environment) IsLocked() bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.locked
}

// SetMaintenanceWindow sets the maintenance window
func (e *Environment) SetMaintenanceWindow(window *MaintenanceWindow) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.maintenanceWindow = window
	return nil
}

// InMaintenanceWindow checks if currently in maintenance window
func (e *Environment) InMaintenanceWindow() bool {
	e.mu.RLock()
	defer e.mu.RUnlock()

	if e.maintenanceWindow == nil {
		return false
	}

	now := time.Now()
	if now.Weekday() != e.maintenanceWindow.Day {
		return false
	}

	startOfDay := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	windowStart := startOfDay.Add(time.Duration(e.maintenanceWindow.StartHour) * time.Hour)
	windowEnd := windowStart.Add(e.maintenanceWindow.Duration)

	return now.After(windowStart) && now.Before(windowEnd)
}

// SetResourceLimits sets resource limits
func (e *Environment) SetResourceLimits(limits *ResourceLimits) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.resourceLimits = limits
	return nil
}

// ResourceLimits returns resource limits
func (e *Environment) ResourceLimits() *ResourceLimits {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.resourceLimits
}

// EnvironmentManagerConfig configures the manager
type EnvironmentManagerConfig struct {
	MaxEnvironments int
}

// EnvironmentManager manages environments
type EnvironmentManager struct {
	config         *EnvironmentManagerConfig
	environments   map[string]*Environment
	promotionPaths map[string]string // source -> target
	mu             sync.RWMutex
}

// NewEnvironmentManager creates an environment manager
func NewEnvironmentManager(config *EnvironmentManagerConfig) *EnvironmentManager {
	if config == nil {
		config = &EnvironmentManagerConfig{MaxEnvironments: 100}
	}

	return &EnvironmentManager{
		config:         config,
		environments:   make(map[string]*Environment),
		promotionPaths: make(map[string]string),
	}
}

// Create creates an environment
func (m *EnvironmentManager) Create(config *EnvironmentConfig) (*Environment, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.environments[config.Name]; exists {
		return nil, fmt.Errorf("environment: %s already exists", config.Name)
	}

	env := &Environment{
		config:    config,
		variables: make(map[string]string),
		secrets:   make(map[string]string),
	}

	m.environments[config.Name] = env
	return env, nil
}

// Get returns an environment by name
func (m *EnvironmentManager) Get(name string) *Environment {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.environments[name]
}

// List returns all environments
func (m *EnvironmentManager) List() []*Environment {
	m.mu.RLock()
	defer m.mu.RUnlock()

	envs := make([]*Environment, 0, len(m.environments))
	for _, e := range m.environments {
		envs = append(envs, e)
	}
	return envs
}

// Delete deletes an environment
func (m *EnvironmentManager) Delete(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.environments[name]; !exists {
		return fmt.Errorf("environment: %s not found", name)
	}

	delete(m.environments, name)
	delete(m.promotionPaths, name)

	return nil
}

// SetPromotionPath sets the promotion path
func (m *EnvironmentManager) SetPromotionPath(source, target string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.promotionPaths[source] = target
	return nil
}

// GetNextEnvironment returns the next environment in promotion path
func (m *EnvironmentManager) GetNextEnvironment(name string) string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.promotionPaths[name]
}

// Clone clones an environment
func (m *EnvironmentManager) Clone(sourceName, targetName string) (*Environment, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	source, exists := m.environments[sourceName]
	if !exists {
		return nil, fmt.Errorf("environment: %s not found", sourceName)
	}

	if _, exists := m.environments[targetName]; exists {
		return nil, fmt.Errorf("environment: %s already exists", targetName)
	}

	source.mu.RLock()
	defer source.mu.RUnlock()

	// Clone variables
	vars := make(map[string]string)
	for k, v := range source.variables {
		vars[k] = v
	}

	// Clone secrets
	secrets := make(map[string]string)
	for k, v := range source.secrets {
		secrets[k] = v
	}

	clone := &Environment{
		config: &EnvironmentConfig{
			Name:        targetName,
			Type:        source.config.Type,
			Tier:        source.config.Tier,
			Description: source.config.Description,
		},
		variables: vars,
		secrets:   secrets,
	}

	m.environments[targetName] = clone
	return clone, nil
}
