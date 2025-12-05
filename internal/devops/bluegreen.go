// internal/devops/bluegreen.go
package devops

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Slots
const (
	SlotBlue  = "blue"
	SlotGreen = "green"
)

// Blue-green statuses
const (
	BlueGreenStatusPending   = "pending"
	BlueGreenStatusDeploying = "deploying"
	BlueGreenStatusReady     = "ready"
	BlueGreenStatusSwitching = "switching"
	BlueGreenStatusFailed    = "failed"
)

// BlueGreenHealthCheck configures health checking
type BlueGreenHealthCheck struct {
	Endpoint string        `json:"endpoint"`
	Interval time.Duration `json:"interval"`
	Timeout  time.Duration `json:"timeout"`
}

// BlueGreenConfig configures a blue-green deployment
type BlueGreenConfig struct {
	Name        string                `json:"name"`
	Environment string                `json:"environment"`
	Target      string                `json:"target"`
	Version     string                `json:"version"`
	Replicas    int                   `json:"replicas"`
	AutoSwitch  bool                  `json:"auto_switch"`
	HealthCheck *BlueGreenHealthCheck `json:"health_check"`
}

// Validate checks configuration
func (c *BlueGreenConfig) Validate() error {
	if c.Name == "" {
		return errors.New("bluegreen: name is required")
	}
	return nil
}

// BlueGreenState tracks the state of a blue-green deployment
type BlueGreenState struct {
	ActiveSlot   string `json:"active_slot"`
	BlueVersion  string `json:"blue_version"`
	GreenVersion string `json:"green_version"`
	BlueReady    bool   `json:"blue_ready"`
	GreenReady   bool   `json:"green_ready"`
}

// BlueGreenDeploy represents a blue-green deployment
type BlueGreenDeploy struct {
	id         string
	config     *BlueGreenConfig
	status     string
	activeSlot string
	targetSlot string
	startedAt  time.Time
	endedAt    time.Time
	manager    *BlueGreenManager
	mu         sync.Mutex
}

// ID returns the deployment ID
func (d *BlueGreenDeploy) ID() string {
	return d.id
}

// Status returns the current status
func (d *BlueGreenDeploy) Status() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.status
}

// ActiveSlot returns the active slot
func (d *BlueGreenDeploy) ActiveSlot() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.activeSlot
}

// InactiveSlot returns the inactive slot
func (d *BlueGreenDeploy) InactiveSlot() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.targetSlot
}

// Wait waits for deployment to complete
func (d *BlueGreenDeploy) Wait(ctx context.Context) error {
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

			if status == BlueGreenStatusReady || status == BlueGreenStatusFailed {
				return nil
			}
		}
	}
}

func (d *BlueGreenDeploy) execute(ctx context.Context) {
	d.mu.Lock()
	d.status = BlueGreenStatusDeploying
	d.startedAt = time.Now()
	d.mu.Unlock()

	target := d.manager.getTarget(d.config.Target)
	if target != nil {
		if err := target.DeployToSlot(ctx, d.config, d.targetSlot); err != nil {
			d.mu.Lock()
			d.status = BlueGreenStatusFailed
			d.mu.Unlock()
			return
		}

		// Health check if configured
		if d.config.HealthCheck != nil {
			healthy, _ := target.CheckHealth(ctx, d.config.Name, d.config.Environment, d.targetSlot)
			if !healthy {
				d.mu.Lock()
				d.status = BlueGreenStatusFailed
				d.mu.Unlock()
				return
			}
		}
	}

	// Update state
	d.manager.updateState(d.config.Name, d.config.Environment, d.targetSlot, d.config.Version)

	// Auto-switch if enabled and not first deployment
	if d.config.AutoSwitch && d.activeSlot != "" {
		_ = d.manager.switchTraffic(ctx, d.config.Name, d.config.Environment, d.targetSlot)
	}

	d.mu.Lock()
	d.status = BlueGreenStatusReady
	d.endedAt = time.Now()
	d.mu.Unlock()
}

// BlueGreenTarget is the interface for blue-green targets
type BlueGreenTarget interface {
	DeployToSlot(ctx context.Context, config *BlueGreenConfig, slot string) error
	SwitchTraffic(ctx context.Context, name, env, slot string) error
	CheckHealth(ctx context.Context, name, env, slot string) (bool, error)
	CleanupSlot(ctx context.Context, name, env, slot string) error
}

// BlueGreenManagerConfig configures the manager
type BlueGreenManagerConfig struct {
	HealthCheckRetries int
}

// BlueGreenManager manages blue-green deployments
type BlueGreenManager struct {
	config  *BlueGreenManagerConfig
	states  map[string]*BlueGreenState // key: "name:env"
	targets map[string]BlueGreenTarget
	mu      sync.RWMutex
}

// NewBlueGreenManager creates a blue-green manager
func NewBlueGreenManager(config *BlueGreenManagerConfig) *BlueGreenManager {
	if config == nil {
		config = &BlueGreenManagerConfig{HealthCheckRetries: 3}
	}

	return &BlueGreenManager{
		config:  config,
		states:  make(map[string]*BlueGreenState),
		targets: make(map[string]BlueGreenTarget),
	}
}

// RegisterTarget registers a deployment target
func (m *BlueGreenManager) RegisterTarget(name string, target BlueGreenTarget) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.targets[name] = target
}

func (m *BlueGreenManager) getTarget(name string) BlueGreenTarget {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.targets[name]
}

func (m *BlueGreenManager) stateKey(name, env string) string {
	return fmt.Sprintf("%s:%s", name, env)
}

// Deploy creates a blue-green deployment
func (m *BlueGreenManager) Deploy(ctx context.Context, config *BlueGreenConfig) (*BlueGreenDeploy, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}

	m.mu.Lock()
	key := m.stateKey(config.Name, config.Environment)
	state := m.states[key]

	var activeSlot, targetSlot string

	if state == nil {
		// First deployment goes to blue
		activeSlot = ""
		targetSlot = SlotBlue
		m.states[key] = &BlueGreenState{
			ActiveSlot: SlotBlue,
		}
	} else {
		// Deploy to inactive slot
		activeSlot = state.ActiveSlot
		if activeSlot == SlotBlue {
			targetSlot = SlotGreen
		} else {
			targetSlot = SlotBlue
		}
	}
	m.mu.Unlock()

	deploy := &BlueGreenDeploy{
		id:         uuid.New().String(),
		config:     config,
		status:     BlueGreenStatusPending,
		activeSlot: activeSlot,
		targetSlot: targetSlot,
		manager:    m,
	}

	go deploy.execute(ctx)

	return deploy, nil
}

func (m *BlueGreenManager) updateState(name, env, slot, version string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	key := m.stateKey(name, env)
	state := m.states[key]
	if state == nil {
		state = &BlueGreenState{}
		m.states[key] = state
	}

	if slot == SlotBlue {
		state.BlueVersion = version
		state.BlueReady = true
	} else {
		state.GreenVersion = version
		state.GreenReady = true
	}
}

func (m *BlueGreenManager) switchTraffic(ctx context.Context, name, env, slot string) error {
	target := m.getTarget("mock") // Would need to track target per deployment
	if target != nil {
		if err := target.SwitchTraffic(ctx, name, env, slot); err != nil {
			return err
		}
	}

	m.mu.Lock()
	key := m.stateKey(name, env)
	if state := m.states[key]; state != nil {
		state.ActiveSlot = slot
	}
	m.mu.Unlock()

	return nil
}

// Switch switches traffic to the inactive slot
func (m *BlueGreenManager) Switch(ctx context.Context, name, env string) error {
	m.mu.RLock()
	key := m.stateKey(name, env)
	state := m.states[key]
	m.mu.RUnlock()

	if state == nil {
		return errors.New("bluegreen: no deployment found")
	}

	var newActive string
	if state.ActiveSlot == SlotBlue {
		newActive = SlotGreen
	} else {
		newActive = SlotBlue
	}

	return m.switchTraffic(ctx, name, env, newActive)
}

// Rollback switches back to the previous slot
func (m *BlueGreenManager) Rollback(ctx context.Context, name, env string) error {
	return m.Switch(ctx, name, env)
}

// Cleanup removes the inactive slot
func (m *BlueGreenManager) Cleanup(ctx context.Context, name, env string) error {
	m.mu.RLock()
	key := m.stateKey(name, env)
	state := m.states[key]
	m.mu.RUnlock()

	if state == nil {
		return errors.New("bluegreen: no deployment found")
	}

	var inactiveSlot string
	if state.ActiveSlot == SlotBlue {
		inactiveSlot = SlotGreen
	} else {
		inactiveSlot = SlotBlue
	}

	target := m.getTarget("mock")
	if target != nil {
		return target.CleanupSlot(ctx, name, env, inactiveSlot)
	}

	return nil
}

// GetState returns the state for a deployment
func (m *BlueGreenManager) GetState(name, env string) *BlueGreenState {
	m.mu.RLock()
	defer m.mu.RUnlock()

	key := m.stateKey(name, env)
	return m.states[key]
}
