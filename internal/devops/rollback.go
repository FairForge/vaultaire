// internal/devops/rollback.go
package devops

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// Rollback strategies
const (
	RollbackStrategyImmediate = "immediate"
	RollbackStrategyGradual   = "gradual"
	RollbackStrategyBlueGreen = "blue-green"
)

// Rollback statuses
const (
	RollbackStatusPending = "pending"
	RollbackStatusRunning = "running"
	RollbackStatusSuccess = "success"
	RollbackStatusFailed  = "failed"
)

// ReleaseRecord tracks a deployed release
type ReleaseRecord struct {
	Name        string            `json:"name"`
	Environment string            `json:"environment"`
	Version     string            `json:"version"`
	Artifact    string            `json:"artifact"`
	Executor    string            `json:"executor"`
	Timestamp   time.Time         `json:"timestamp"`
	Metadata    map[string]string `json:"metadata"`
}

// RollbackConfig configures a rollback
type RollbackConfig struct {
	Name          string        `json:"name"`
	Environment   string        `json:"environment"`
	TargetVersion string        `json:"target_version"`
	Strategy      string        `json:"strategy"`
	Steps         []int         `json:"steps"`
	StepDelay     time.Duration `json:"step_delay"`
}

// Validate checks configuration
func (c *RollbackConfig) Validate() error {
	if c.Name == "" {
		return errors.New("rollback: name is required")
	}
	return nil
}

// RollbackResult contains rollback results
type RollbackResult struct {
	FromVersion   string        `json:"from_version"`
	TargetVersion string        `json:"target_version"`
	Status        string        `json:"status"`
	Duration      time.Duration `json:"duration"`
	Error         string        `json:"error,omitempty"`
	Timestamp     time.Time     `json:"timestamp"`
}

// AutoRollbackConfig configures auto-rollback
type AutoRollbackConfig struct {
	Enabled    bool          `json:"enabled"`
	ErrorRate  float64       `json:"error_rate"`
	LatencyP99 time.Duration `json:"latency_p99"`
	Window     time.Duration `json:"window"`
}

// RollbackExecutor executes rollbacks
type RollbackExecutor interface {
	Deploy(ctx context.Context, artifact string) error
	SetWeight(ctx context.Context, name, env string, weight int) error
	Validate(ctx context.Context, artifact string) (bool, error)
	GetCurrentVersion(ctx context.Context, name, env string) (string, error)
}

// RollbackManagerConfig configures the manager
type RollbackManagerConfig struct {
	MaxHistory int
}

// RollbackManager manages rollbacks
type RollbackManager struct {
	config          *RollbackManagerConfig
	releases        map[string][]*ReleaseRecord // key: "name:env"
	rollbackHistory map[string][]*RollbackResult
	autoConfigs     map[string]*AutoRollbackConfig
	executors       map[string]RollbackExecutor
	mu              sync.RWMutex
}

// NewRollbackManager creates a rollback manager
func NewRollbackManager(config *RollbackManagerConfig) *RollbackManager {
	if config == nil {
		config = &RollbackManagerConfig{MaxHistory: 100}
	}

	return &RollbackManager{
		config:          config,
		releases:        make(map[string][]*ReleaseRecord),
		rollbackHistory: make(map[string][]*RollbackResult),
		autoConfigs:     make(map[string]*AutoRollbackConfig),
		executors:       make(map[string]RollbackExecutor),
	}
}

// RegisterExecutor registers a rollback executor
func (m *RollbackManager) RegisterExecutor(name string, executor RollbackExecutor) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.executors[name] = executor
}

func (m *RollbackManager) getExecutor(name string) RollbackExecutor {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.executors[name]
}

func (m *RollbackManager) key(name, env string) string {
	return fmt.Sprintf("%s:%s", name, env)
}

// RecordRelease records a deployed release
func (m *RollbackManager) RecordRelease(record *ReleaseRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	key := m.key(record.Name, record.Environment)
	m.releases[key] = append(m.releases[key], record)

	// Trim history if needed
	if len(m.releases[key]) > m.config.MaxHistory {
		m.releases[key] = m.releases[key][1:]
	}

	return nil
}

// GetReleases returns release history
func (m *RollbackManager) GetReleases(name, env string) []*ReleaseRecord {
	m.mu.RLock()
	defer m.mu.RUnlock()

	key := m.key(name, env)
	return m.releases[key]
}

// Rollback performs a rollback to the previous version
func (m *RollbackManager) Rollback(ctx context.Context, config *RollbackConfig) (*RollbackResult, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}

	m.mu.RLock()
	key := m.key(config.Name, config.Environment)
	releases := m.releases[key]
	m.mu.RUnlock()

	if len(releases) < 2 {
		return nil, errors.New("rollback: no previous version available")
	}

	// Get previous version
	previousRelease := releases[len(releases)-2]
	config.TargetVersion = previousRelease.Version

	return m.executeRollback(ctx, config, previousRelease)
}

// RollbackToVersion rolls back to a specific version
func (m *RollbackManager) RollbackToVersion(ctx context.Context, config *RollbackConfig) (*RollbackResult, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}

	m.mu.RLock()
	key := m.key(config.Name, config.Environment)
	releases := m.releases[key]
	m.mu.RUnlock()

	// Find target release
	var targetRelease *ReleaseRecord
	for _, r := range releases {
		if r.Version == config.TargetVersion {
			targetRelease = r
			break
		}
	}

	if targetRelease == nil {
		return nil, fmt.Errorf("rollback: version %s not found", config.TargetVersion)
	}

	return m.executeRollback(ctx, config, targetRelease)
}

func (m *RollbackManager) executeRollback(ctx context.Context, config *RollbackConfig, target *ReleaseRecord) (*RollbackResult, error) {
	startTime := time.Now()

	result := &RollbackResult{
		TargetVersion: target.Version,
		Status:        RollbackStatusRunning,
		Timestamp:     startTime,
	}

	executor := m.getExecutor(target.Executor)

	switch config.Strategy {
	case RollbackStrategyGradual:
		if err := m.executeGradualRollback(ctx, config, target, executor); err != nil {
			result.Status = RollbackStatusFailed
			result.Error = err.Error()
			return result, err
		}
	default: // Immediate
		if executor != nil {
			if err := executor.Deploy(ctx, target.Artifact); err != nil {
				result.Status = RollbackStatusFailed
				result.Error = err.Error()
				return result, err
			}
		}
	}

	result.Status = RollbackStatusSuccess
	result.Duration = time.Since(startTime)

	// Record rollback
	m.mu.Lock()
	key := m.key(config.Name, config.Environment)
	m.rollbackHistory[key] = append(m.rollbackHistory[key], result)
	m.mu.Unlock()

	return result, nil
}

func (m *RollbackManager) executeGradualRollback(ctx context.Context, config *RollbackConfig, target *ReleaseRecord, executor RollbackExecutor) error {
	if executor == nil {
		return nil
	}

	steps := config.Steps
	if len(steps) == 0 {
		steps = []int{25, 50, 75, 100}
	}

	for _, weight := range steps {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		if err := executor.SetWeight(ctx, config.Name, config.Environment, weight); err != nil {
			return err
		}

		if config.StepDelay > 0 && weight < 100 {
			time.Sleep(config.StepDelay)
		}
	}

	return nil
}

// ConfigureAutoRollback configures automatic rollback
func (m *RollbackManager) ConfigureAutoRollback(name, env string, config *AutoRollbackConfig) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	key := m.key(name, env)
	m.autoConfigs[key] = config

	return nil
}

// GetRollbackHistory returns rollback history
func (m *RollbackManager) GetRollbackHistory(name, env string) []*RollbackResult {
	m.mu.RLock()
	defer m.mu.RUnlock()

	key := m.key(name, env)
	return m.rollbackHistory[key]
}

// ValidateRollback validates a rollback target
func (m *RollbackManager) ValidateRollback(ctx context.Context, name, env, version string) (bool, error) {
	m.mu.RLock()
	key := m.key(name, env)
	releases := m.releases[key]
	m.mu.RUnlock()

	var targetRelease *ReleaseRecord
	for _, r := range releases {
		if r.Version == version {
			targetRelease = r
			break
		}
	}

	if targetRelease == nil {
		return false, fmt.Errorf("rollback: version %s not found", version)
	}

	executor := m.getExecutor(targetRelease.Executor)
	if executor == nil {
		return true, nil
	}

	return executor.Validate(ctx, targetRelease.Artifact)
}
