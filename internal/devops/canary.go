// internal/devops/canary.go
package devops

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Canary statuses
const (
	CanaryStatusPending    = "pending"
	CanaryStatusRunning    = "running"
	CanaryStatusPaused     = "paused"
	CanaryStatusComplete   = "complete"
	CanaryStatusRolledBack = "rolled_back"
	CanaryStatusAborted    = "aborted"
)

// CanaryStep defines a rollout step
type CanaryStep struct {
	Weight         int           `json:"weight"`
	Duration       time.Duration `json:"duration"`
	ManualApproval bool          `json:"manual_approval"`
}

// CanaryAnalysis defines analysis thresholds
type CanaryAnalysis struct {
	MaxErrorRate  float64       `json:"max_error_rate"`
	MaxLatencyP99 time.Duration `json:"max_latency_p99"`
}

// CanaryConfig configures a canary deployment
type CanaryConfig struct {
	Name        string          `json:"name"`
	Environment string          `json:"environment"`
	Target      string          `json:"target"`
	Version     string          `json:"version"`
	Steps       []CanaryStep    `json:"steps"`
	Analysis    *CanaryAnalysis `json:"analysis"`
}

// Validate checks configuration
func (c *CanaryConfig) Validate() error {
	if c.Name == "" {
		return errors.New("canary: name is required")
	}
	if len(c.Steps) == 0 {
		return errors.New("canary: at least one step is required")
	}
	return nil
}

// CanaryMetrics contains canary metrics
type CanaryMetrics struct {
	ErrorRate    float64       `json:"error_rate"`
	LatencyP99   time.Duration `json:"latency_p99"`
	RequestCount int64         `json:"request_count"`
}

// StepHistoryEntry tracks step progression
type StepHistoryEntry struct {
	Step      int       `json:"step"`
	Weight    int       `json:"weight"`
	Timestamp time.Time `json:"timestamp"`
}

// CanaryDeploy represents a canary deployment
type CanaryDeploy struct {
	id            string
	config        *CanaryConfig
	status        string
	currentStep   int
	currentWeight int
	stepHistory   []StepHistoryEntry
	metrics       *CanaryMetrics
	promoteCh     chan struct{}
	abortCh       chan struct{}
	manager       *CanaryManager
	mu            sync.Mutex
}

// ID returns the deployment ID
func (d *CanaryDeploy) ID() string {
	return d.id
}

// Status returns the current status
func (d *CanaryDeploy) Status() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.status
}

// CurrentWeight returns the current traffic weight
func (d *CanaryDeploy) CurrentWeight() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.currentWeight
}

// StepHistory returns step progression history
func (d *CanaryDeploy) StepHistory() []StepHistoryEntry {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.stepHistory
}

// Metrics returns collected metrics
func (d *CanaryDeploy) Metrics() *CanaryMetrics {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.metrics
}

// Promote advances to the next step
func (d *CanaryDeploy) Promote() error {
	d.mu.Lock()
	if d.status != CanaryStatusPaused {
		d.mu.Unlock()
		return errors.New("canary: not paused")
	}
	d.mu.Unlock()

	select {
	case d.promoteCh <- struct{}{}:
		return nil
	default:
		return errors.New("canary: promote already pending")
	}
}

// Abort aborts the canary
func (d *CanaryDeploy) Abort() error {
	d.mu.Lock()
	d.status = CanaryStatusAborted
	d.mu.Unlock()

	select {
	case d.abortCh <- struct{}{}:
	default:
	}

	return nil
}

// Wait waits for the canary to complete
func (d *CanaryDeploy) Wait(ctx context.Context) error {
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
			case CanaryStatusComplete, CanaryStatusRolledBack, CanaryStatusAborted:
				return nil
			}
		}
	}
}

func (d *CanaryDeploy) execute(ctx context.Context) {
	d.mu.Lock()
	d.status = CanaryStatusRunning
	d.mu.Unlock()

	target := d.manager.getTarget(d.config.Target)

	for i, step := range d.config.Steps {
		select {
		case <-d.abortCh:
			return
		default:
		}

		// Set traffic weight
		d.mu.Lock()
		d.currentStep = i
		d.currentWeight = step.Weight
		d.stepHistory = append(d.stepHistory, StepHistoryEntry{
			Step:      i,
			Weight:    step.Weight,
			Timestamp: time.Now(),
		})
		d.mu.Unlock()

		if target != nil {
			_ = target.SetWeight(ctx, d.config.Name, d.config.Environment, step.Weight)
		}

		// Check if manual approval required
		if step.ManualApproval {
			d.mu.Lock()
			d.status = CanaryStatusPaused
			d.mu.Unlock()

			select {
			case <-d.promoteCh:
				d.mu.Lock()
				d.status = CanaryStatusRunning
				d.mu.Unlock()
			case <-d.abortCh:
				return
			case <-ctx.Done():
				return
			}
			continue
		}

		// Wait for step duration
		if step.Duration > 0 {
			timer := time.NewTimer(step.Duration)
			select {
			case <-timer.C:
			case <-d.abortCh:
				timer.Stop()
				return
			case <-ctx.Done():
				timer.Stop()
				return
			}
		}

		// Collect and analyze metrics
		if target != nil && d.config.Analysis != nil {
			metrics, err := target.GetMetrics(ctx, d.config.Name, d.config.Environment)
			if err == nil {
				d.mu.Lock()
				d.metrics = metrics
				d.mu.Unlock()

				// Check thresholds
				if d.config.Analysis.MaxErrorRate > 0 && metrics.ErrorRate > d.config.Analysis.MaxErrorRate {
					d.mu.Lock()
					d.status = CanaryStatusRolledBack
					d.mu.Unlock()
					_ = target.Rollback(ctx, d.config.Name, d.config.Environment)
					return
				}
			}
		}
	}

	d.mu.Lock()
	d.status = CanaryStatusComplete
	d.mu.Unlock()

	if target != nil {
		_ = target.Promote(ctx, d.config.Name, d.config.Environment)
	}
}

// CanaryTarget is the interface for canary targets
type CanaryTarget interface {
	SetWeight(ctx context.Context, name, env string, weight int) error
	GetMetrics(ctx context.Context, name, env string) (*CanaryMetrics, error)
	Rollback(ctx context.Context, name, env string) error
	Promote(ctx context.Context, name, env string) error
}

// CanaryManagerConfig configures the manager
type CanaryManagerConfig struct {
	MetricsInterval time.Duration
}

// CanaryManager manages canary deployments
type CanaryManager struct {
	config   *CanaryManagerConfig
	canaries map[string]*CanaryDeploy
	targets  map[string]CanaryTarget
	mu       sync.RWMutex
}

// NewCanaryManager creates a canary manager
func NewCanaryManager(config *CanaryManagerConfig) *CanaryManager {
	if config == nil {
		config = &CanaryManagerConfig{MetricsInterval: 30 * time.Second}
	}

	return &CanaryManager{
		config:   config,
		canaries: make(map[string]*CanaryDeploy),
		targets:  make(map[string]CanaryTarget),
	}
}

// RegisterTarget registers a canary target
func (m *CanaryManager) RegisterTarget(name string, target CanaryTarget) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.targets[name] = target
}

func (m *CanaryManager) getTarget(name string) CanaryTarget {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.targets[name]
}

// Deploy creates a canary deployment
func (m *CanaryManager) Deploy(ctx context.Context, config *CanaryConfig) (*CanaryDeploy, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}

	deploy := &CanaryDeploy{
		id:          uuid.New().String(),
		config:      config,
		status:      CanaryStatusPending,
		stepHistory: make([]StepHistoryEntry, 0),
		promoteCh:   make(chan struct{}, 1),
		abortCh:     make(chan struct{}, 1),
		manager:     m,
	}

	m.mu.Lock()
	m.canaries[deploy.id] = deploy
	m.mu.Unlock()

	go deploy.execute(ctx)

	return deploy, nil
}

// Get returns a canary by ID
func (m *CanaryManager) Get(id string) *CanaryDeploy {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.canaries[id]
}

// List returns all canaries
func (m *CanaryManager) List() []*CanaryDeploy {
	m.mu.RLock()
	defer m.mu.RUnlock()

	canaries := make([]*CanaryDeploy, 0, len(m.canaries))
	for _, c := range m.canaries {
		canaries = append(canaries, c)
	}
	return canaries
}
