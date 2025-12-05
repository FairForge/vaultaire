// internal/devops/deployment.go
package devops

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Deployment strategies
const (
	StrategyRolling   = "rolling"
	StrategyRecreate  = "recreate"
	StrategyBlueGreen = "blue-green"
	StrategyCanary    = "canary"
)

// Deployment statuses
const (
	DeployStatusPending  = "pending"
	DeployStatusRunning  = "running"
	DeployStatusSuccess  = "success"
	DeployStatusFailed   = "failed"
	DeployStatusCanceled = "canceled"
	DeployStatusRollback = "rollback"
)

// HealthCheckConfig configures health checks
type HealthCheckConfig struct {
	Path     string        `json:"path"`
	Interval time.Duration `json:"interval"`
	Timeout  time.Duration `json:"timeout"`
	Retries  int           `json:"retries"`
}

// DeployHooks defines deployment hooks
type DeployHooks struct {
	PreDeploy  func() error
	PostDeploy func() error
}

// DeploymentConfig configures a deployment
type DeploymentConfig struct {
	Name           string             `json:"name"`
	Environment    string             `json:"environment"`
	Strategy       string             `json:"strategy"`
	Target         string             `json:"target"`
	Version        string             `json:"version"`
	Replicas       int                `json:"replicas"`
	MaxSurge       int                `json:"max_surge"`
	MaxUnavailable int                `json:"max_unavailable"`
	HealthCheck    *HealthCheckConfig `json:"health_check"`
	Hooks          *DeployHooks       `json:"-"`
}

// Validate checks configuration
func (c *DeploymentConfig) Validate() error {
	if c.Name == "" {
		return errors.New("deployment: name is required")
	}

	validStrategies := map[string]bool{
		StrategyRolling:   true,
		StrategyRecreate:  true,
		StrategyBlueGreen: true,
		StrategyCanary:    true,
	}

	if c.Strategy != "" && !validStrategies[c.Strategy] {
		return fmt.Errorf("deployment: invalid strategy: %s", c.Strategy)
	}

	return nil
}

// StatusEntry tracks status changes
type StatusEntry struct {
	Status    string    `json:"status"`
	Timestamp time.Time `json:"timestamp"`
	Message   string    `json:"message"`
}

// DeployMetrics contains deployment metrics
type DeployMetrics struct {
	Duration      time.Duration `json:"duration"`
	ReplicasTotal int           `json:"replicas_total"`
	ReplicasReady int           `json:"replicas_ready"`
}

// Deployment represents a deployment
type Deployment struct {
	id            string
	config        *DeploymentConfig
	status        string
	statusHistory []StatusEntry
	startedAt     time.Time
	endedAt       time.Time
	cancelFn      context.CancelFunc
	manager       *DeploymentManager
	mu            sync.Mutex
}

// ID returns the deployment ID
func (d *Deployment) ID() string {
	return d.id
}

// Status returns the current status
func (d *Deployment) Status() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.status
}

// Version returns the deployment version
func (d *Deployment) Version() string {
	return d.config.Version
}

// StatusHistory returns status history
func (d *Deployment) StatusHistory() []StatusEntry {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.statusHistory
}

// Metrics returns deployment metrics
func (d *Deployment) Metrics() *DeployMetrics {
	d.mu.Lock()
	defer d.mu.Unlock()

	duration := d.endedAt.Sub(d.startedAt)
	if d.endedAt.IsZero() {
		duration = time.Since(d.startedAt)
	}

	return &DeployMetrics{
		Duration:      duration,
		ReplicasTotal: d.config.Replicas,
		ReplicasReady: d.config.Replicas,
	}
}

// Cancel cancels the deployment
func (d *Deployment) Cancel() error {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.cancelFn != nil {
		d.cancelFn()
	}

	d.setStatusLocked(DeployStatusCanceled, "Deployment canceled")
	d.endedAt = time.Now()

	return nil
}

// Wait waits for the deployment to complete
func (d *Deployment) Wait(ctx context.Context) error {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			d.mu.Lock()
			status := d.status
			d.mu.Unlock()

			switch status {
			case DeployStatusSuccess, DeployStatusFailed, DeployStatusCanceled:
				return nil
			}
		}
	}
}

func (d *Deployment) setStatus(status, message string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.setStatusLocked(status, message)
}

func (d *Deployment) setStatusLocked(status, message string) {
	d.status = status
	d.statusHistory = append(d.statusHistory, StatusEntry{
		Status:    status,
		Timestamp: time.Now(),
		Message:   message,
	})
}

func (d *Deployment) execute(ctx context.Context) {
	d.setStatus(DeployStatusRunning, "Deployment started")
	d.startedAt = time.Now()

	// Run pre-deploy hook
	if d.config.Hooks != nil && d.config.Hooks.PreDeploy != nil {
		if err := d.config.Hooks.PreDeploy(); err != nil {
			d.setStatus(DeployStatusFailed, fmt.Sprintf("Pre-deploy hook failed: %v", err))
			d.endedAt = time.Now()
			return
		}
	}

	// Execute deployment via target
	target := d.manager.getTarget(d.config.Target)
	if target != nil {
		if err := target.Deploy(ctx, d.config); err != nil {
			if ctx.Err() != nil {
				return // Already canceled
			}
			d.setStatus(DeployStatusFailed, fmt.Sprintf("Deployment failed: %v", err))
			d.endedAt = time.Now()
			return
		}
	}

	// Run post-deploy hook
	if d.config.Hooks != nil && d.config.Hooks.PostDeploy != nil {
		if err := d.config.Hooks.PostDeploy(); err != nil {
			d.setStatus(DeployStatusFailed, fmt.Sprintf("Post-deploy hook failed: %v", err))
			d.endedAt = time.Now()
			return
		}
	}

	d.setStatus(DeployStatusSuccess, "Deployment completed successfully")
	d.endedAt = time.Now()
}

// DeployTarget is the interface for deployment targets
type DeployTarget interface {
	Deploy(ctx context.Context, config *DeploymentConfig) error
	Rollback(ctx context.Context, config *DeploymentConfig) error
	Status(name, env string) (string, error)
	Scale(name, env string, replicas int) error
}

// DeploymentManagerConfig configures the manager
type DeploymentManagerConfig struct {
	MaxConcurrent int
}

// DeploymentManager manages deployments
type DeploymentManager struct {
	config      *DeploymentManagerConfig
	deployments map[string]*Deployment
	history     map[string][]*Deployment // key: "name:env"
	targets     map[string]DeployTarget
	mu          sync.RWMutex
}

// NewDeploymentManager creates a deployment manager
func NewDeploymentManager(config *DeploymentManagerConfig) *DeploymentManager {
	if config == nil {
		config = &DeploymentManagerConfig{MaxConcurrent: 10}
	}

	return &DeploymentManager{
		config:      config,
		deployments: make(map[string]*Deployment),
		history:     make(map[string][]*Deployment),
		targets:     make(map[string]DeployTarget),
	}
}

// RegisterTarget registers a deployment target
func (m *DeploymentManager) RegisterTarget(name string, target DeployTarget) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.targets[name] = target
}

func (m *DeploymentManager) getTarget(name string) DeployTarget {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.targets[name]
}

// Deploy creates and starts a deployment
func (m *DeploymentManager) Deploy(ctx context.Context, config *DeploymentConfig) (*Deployment, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}

	deployCtx, cancelFn := context.WithCancel(ctx)

	deployment := &Deployment{
		id:            uuid.New().String(),
		config:        config,
		status:        DeployStatusPending,
		statusHistory: make([]StatusEntry, 0),
		cancelFn:      cancelFn,
		manager:       m,
	}

	deployment.statusHistory = append(deployment.statusHistory, StatusEntry{
		Status:    DeployStatusPending,
		Timestamp: time.Now(),
		Message:   "Deployment created",
	})

	m.mu.Lock()
	m.deployments[deployment.id] = deployment

	// Track history
	historyKey := fmt.Sprintf("%s:%s", config.Name, config.Environment)
	m.history[historyKey] = append(m.history[historyKey], deployment)
	m.mu.Unlock()

	go deployment.execute(deployCtx)

	return deployment, nil
}

// Rollback rolls back to the previous deployment
func (m *DeploymentManager) Rollback(ctx context.Context, name, env string) (*Deployment, error) {
	m.mu.RLock()
	historyKey := fmt.Sprintf("%s:%s", name, env)
	history := m.history[historyKey]
	m.mu.RUnlock()

	if len(history) < 2 {
		return nil, errors.New("deployment: no previous version to rollback to")
	}

	// Get the second-to-last deployment
	previousDeploy := history[len(history)-2]

	// Create a new deployment with the previous version
	config := &DeploymentConfig{
		Name:        name,
		Environment: env,
		Strategy:    previousDeploy.config.Strategy,
		Target:      previousDeploy.config.Target,
		Version:     previousDeploy.config.Version,
		Replicas:    previousDeploy.config.Replicas,
	}

	return m.Deploy(ctx, config)
}

// List returns all deployments
func (m *DeploymentManager) List() []*Deployment {
	m.mu.RLock()
	defer m.mu.RUnlock()

	deployments := make([]*Deployment, 0, len(m.deployments))
	for _, d := range m.deployments {
		deployments = append(deployments, d)
	}
	return deployments
}

// GetHistory returns deployment history for an app/env
func (m *DeploymentManager) GetHistory(name, env string) []*Deployment {
	m.mu.RLock()
	defer m.mu.RUnlock()

	historyKey := fmt.Sprintf("%s:%s", name, env)
	return m.history[historyKey]
}
