package ha

import (
	"sync"
	"time"
)

// BackendState represents the health state of a backend
type BackendState int

const (
	StateHealthy BackendState = iota
	StateDegraded
	StateFailed
	StateRecovering
	StateUnknown
)

func (s BackendState) String() string {
	switch s {
	case StateHealthy:
		return "healthy"
	case StateDegraded:
		return "degraded"
	case StateFailed:
		return "failed"
	case StateRecovering:
		return "recovering"
	default:
		return "unknown"
	}
}

// SystemStatus represents overall system health
type SystemStatus int

const (
	SystemHealthy SystemStatus = iota
	SystemDegraded
	SystemCritical
)

func (s SystemStatus) String() string {
	switch s {
	case SystemHealthy:
		return "healthy"
	case SystemDegraded:
		return "degraded"
	case SystemCritical:
		return "critical"
	default:
		return "unknown"
	}
}

// EventType represents HA event types
type EventType int

const (
	EventBackendRegistered EventType = iota
	EventBackendHealthy
	EventBackendDegraded
	EventBackendFailed
	EventBackendRecovered
	EventBackendRecovering // New: emitted when entering recovery state
	EventFailoverStarted
	EventFailoverCompleted
	EventCircuitOpened
	EventCircuitClosed
)

// HAEvent represents an HA system event
type HAEvent struct {
	Type      EventType
	Backend   string
	Timestamp time.Time
	Message   string
	Details   map[string]interface{}
}

// BackendConfig configures a backend for HA
type BackendConfig struct {
	Primary           bool
	HealthCheck       func() error
	CheckInterval     time.Duration
	FailureThreshold  int // Failures before marking failed
	RecoveryThreshold int // Successes before marking healthy
	CircuitBreaker    bool
}

// FailoverRule defines failover behavior
type FailoverRule struct {
	SecondaryBackend string
	AutoFailover     bool
	FailoverDelay    time.Duration
}

// BackendStatus tracks runtime status
type BackendStatus struct {
	State            BackendState
	ConsecutiveFails int
	ConsecutiveOK    int
	LastCheck        time.Time
	LastError        error
	Latency          time.Duration
	CircuitOpen      bool
	Config           BackendConfig
}

// HAOrchestrator coordinates high availability
type HAOrchestrator struct {
	mu            sync.RWMutex
	backends      map[string]*BackendStatus
	failoverRules map[string]FailoverRule
	activeBackend map[string]string // Maps logical name to active backend
	subscribers   []func(HAEvent)
	eventChan     chan HAEvent
	stopChan      chan struct{}
}

// NewHAOrchestrator creates a new HA orchestrator
func NewHAOrchestrator() *HAOrchestrator {
	o := &HAOrchestrator{
		backends:      make(map[string]*BackendStatus),
		failoverRules: make(map[string]FailoverRule),
		activeBackend: make(map[string]string),
		subscribers:   make([]func(HAEvent), 0),
		eventChan:     make(chan HAEvent, 100),
		stopChan:      make(chan struct{}),
	}

	// Start event dispatcher
	go o.eventDispatcher()

	return o
}

// RegisterBackend registers a backend for HA monitoring
func (o *HAOrchestrator) RegisterBackend(id string, config BackendConfig) {
	o.mu.Lock()
	defer o.mu.Unlock()

	// Set defaults
	if config.FailureThreshold == 0 {
		config.FailureThreshold = 3
	}
	if config.RecoveryThreshold == 0 {
		config.RecoveryThreshold = 2
	}
	if config.CheckInterval == 0 {
		config.CheckInterval = 30 * time.Second
	}

	o.backends[id] = &BackendStatus{
		State:     StateHealthy,
		LastCheck: time.Now(),
		Config:    config,
	}

	// Set as active if primary
	if config.Primary {
		o.activeBackend[id] = id
	}

	o.emitEvent(HAEvent{
		Type:      EventBackendRegistered,
		Backend:   id,
		Timestamp: time.Now(),
		Message:   "Backend registered",
	})
}

// ReportHealthCheck reports a health check result
func (o *HAOrchestrator) ReportHealthCheck(id string, healthy bool, latency time.Duration, err error) {
	o.mu.Lock()
	defer o.mu.Unlock()

	status, exists := o.backends[id]
	if !exists {
		return
	}

	status.LastCheck = time.Now()
	status.Latency = latency
	status.LastError = err

	previousState := status.State

	if healthy {
		status.ConsecutiveFails = 0
		status.ConsecutiveOK++

		switch status.State {
		case StateFailed, StateDegraded:
			// Transition to recovering
			status.State = StateRecovering
			o.emitEvent(HAEvent{
				Type:      EventBackendRecovering,
				Backend:   id,
				Timestamp: time.Now(),
				Message:   "Backend entering recovery",
			})
			// Check if immediately recovered
			if status.ConsecutiveOK >= status.Config.RecoveryThreshold {
				status.State = StateHealthy
				status.CircuitOpen = false
				o.emitEvent(HAEvent{
					Type:      EventBackendRecovered,
					Backend:   id,
					Timestamp: time.Now(),
					Message:   "Backend recovered",
				})
			}
		case StateRecovering:
			if status.ConsecutiveOK >= status.Config.RecoveryThreshold {
				status.State = StateHealthy
				status.CircuitOpen = false
				o.emitEvent(HAEvent{
					Type:      EventBackendRecovered,
					Backend:   id,
					Timestamp: time.Now(),
					Message:   "Backend recovered",
				})
			}
		}
	} else {
		status.ConsecutiveOK = 0
		status.ConsecutiveFails++

		threshold := status.Config.FailureThreshold

		if status.ConsecutiveFails >= threshold {
			status.State = StateFailed
			if status.Config.CircuitBreaker {
				status.CircuitOpen = true
				o.emitEvent(HAEvent{
					Type:      EventCircuitOpened,
					Backend:   id,
					Timestamp: time.Now(),
					Message:   "Circuit breaker opened",
				})
			}
		} else if status.ConsecutiveFails >= threshold-1 && status.State == StateHealthy {
			status.State = StateDegraded
			o.emitEvent(HAEvent{
				Type:      EventBackendDegraded,
				Backend:   id,
				Timestamp: time.Now(),
				Message:   "Backend degraded",
			})
		}

		// Emit failed event on state transition
		if previousState != StateFailed && status.State == StateFailed {
			o.emitEvent(HAEvent{
				Type:      EventBackendFailed,
				Backend:   id,
				Timestamp: time.Now(),
				Message:   "Backend failed",
			})

			// Trigger automatic failover if configured
			if rule, hasRule := o.failoverRules[id]; hasRule && rule.AutoFailover {
				go o.performFailover(id, rule)
			}
		}
	}
}

// performFailover executes automatic failover
func (o *HAOrchestrator) performFailover(id string, rule FailoverRule) {
	// Wait for configured delay
	time.Sleep(rule.FailoverDelay)

	o.mu.Lock()
	defer o.mu.Unlock()

	// Verify still failed
	status, exists := o.backends[id]
	if !exists || status.State != StateFailed {
		return
	}

	// Check secondary is healthy
	secondary, hasSecondary := o.backends[rule.SecondaryBackend]
	if !hasSecondary || secondary.State == StateFailed {
		return
	}

	o.emitEvent(HAEvent{
		Type:      EventFailoverStarted,
		Backend:   id,
		Timestamp: time.Now(),
		Message:   "Failover started to " + rule.SecondaryBackend,
	})

	// Switch active backend
	o.activeBackend[id] = rule.SecondaryBackend

	o.emitEvent(HAEvent{
		Type:      EventFailoverCompleted,
		Backend:   id,
		Timestamp: time.Now(),
		Message:   "Failover completed to " + rule.SecondaryBackend,
	})
}

// ConfigureFailover sets up failover rules
func (o *HAOrchestrator) ConfigureFailover(id string, rule FailoverRule) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.failoverRules[id] = rule
}

// GetBackendStatus returns current backend status
func (o *HAOrchestrator) GetBackendStatus(id string) *BackendStatus {
	o.mu.RLock()
	defer o.mu.RUnlock()

	if status, exists := o.backends[id]; exists {
		// Return copy to avoid races
		copy := *status
		return &copy
	}
	return &BackendStatus{State: StateUnknown}
}

// GetActiveBackend returns the currently active backend for a logical name
func (o *HAOrchestrator) GetActiveBackend(id string) string {
	o.mu.RLock()
	defer o.mu.RUnlock()

	if active, exists := o.activeBackend[id]; exists {
		return active
	}
	return id
}

// IsCircuitOpen checks if circuit breaker is open
func (o *HAOrchestrator) IsCircuitOpen(id string) bool {
	o.mu.RLock()
	defer o.mu.RUnlock()

	if status, exists := o.backends[id]; exists {
		return status.CircuitOpen
	}
	return false
}

// GetHealthyBackends returns list of healthy backend IDs
func (o *HAOrchestrator) GetHealthyBackends() []string {
	o.mu.RLock()
	defer o.mu.RUnlock()

	healthy := make([]string, 0)
	for id, status := range o.backends {
		if status.State == StateHealthy || status.State == StateRecovering {
			healthy = append(healthy, id)
		}
	}
	return healthy
}

// GetSystemStatus returns overall system status
func (o *HAOrchestrator) GetSystemStatus() SystemStatus {
	o.mu.RLock()
	defer o.mu.RUnlock()

	if len(o.backends) == 0 {
		return SystemHealthy
	}

	healthyCount := 0
	primaryFailed := false

	for _, status := range o.backends {
		if status.State == StateHealthy {
			healthyCount++
		}
		if status.Config.Primary && status.State == StateFailed {
			primaryFailed = true
		}
	}

	switch {
	case healthyCount == len(o.backends):
		return SystemHealthy
	case healthyCount == 0:
		return SystemCritical
	case primaryFailed:
		return SystemDegraded
	default:
		return SystemDegraded
	}
}

// Subscribe registers an event listener
func (o *HAOrchestrator) Subscribe(handler func(HAEvent)) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.subscribers = append(o.subscribers, handler)
}

// emitEvent sends an event to subscribers
func (o *HAOrchestrator) emitEvent(event HAEvent) {
	select {
	case o.eventChan <- event:
	default:
		// Channel full, drop event (non-blocking)
	}
}

// eventDispatcher dispatches events to subscribers
func (o *HAOrchestrator) eventDispatcher() {
	for {
		select {
		case event := <-o.eventChan:
			o.mu.RLock()
			for _, handler := range o.subscribers {
				go handler(event)
			}
			o.mu.RUnlock()
		case <-o.stopChan:
			return
		}
	}
}

// Stop shuts down the orchestrator
func (o *HAOrchestrator) Stop() {
	close(o.stopChan)
}
