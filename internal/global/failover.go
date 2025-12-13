// internal/global/failover.go
package global

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// FailoverState represents the current failover state
type FailoverState string

const (
	FailoverStateNormal    FailoverState = "normal"
	FailoverStateDetecting FailoverState = "detecting"
	FailoverStateFailover  FailoverState = "failover"
	FailoverStateRecovery  FailoverState = "recovery"
)

// FailoverEvent represents a failover event
type FailoverEvent struct {
	ID             string
	Type           FailoverEventType
	SourceRegion   string
	TargetRegion   string
	Reason         string
	Timestamp      time.Time
	Duration       time.Duration
	Automatic      bool
	Acknowledged   bool
	AcknowledgedBy string
	AcknowledgedAt time.Time
}

// FailoverEventType represents types of failover events
type FailoverEventType string

const (
	EventFailoverStarted   FailoverEventType = "failover_started"
	EventFailoverCompleted FailoverEventType = "failover_completed"
	EventFailoverFailed    FailoverEventType = "failover_failed"
	EventRecoveryStarted   FailoverEventType = "recovery_started"
	EventRecoveryCompleted FailoverEventType = "recovery_completed"
	EventHealthDegraded    FailoverEventType = "health_degraded"
	EventHealthRestored    FailoverEventType = "health_restored"
)

// FailoverPolicy defines failover behavior for a region
type FailoverPolicy struct {
	ID                  string
	Name                string
	SourceRegion        string
	TargetRegions       []string
	Enabled             bool
	AutoFailover        bool
	AutoRecovery        bool
	HealthThreshold     float64
	FailoverDelay       time.Duration
	RecoveryDelay       time.Duration
	MinHealthyDuration  time.Duration
	MaxFailoverDuration time.Duration
	Priority            int
	CreatedAt           time.Time
	UpdatedAt           time.Time
}

// RegionHealth represents health metrics for a region
type RegionHealth struct {
	Region           string
	Healthy          bool
	HealthScore      float64
	AvailableNodes   int
	TotalNodes       int
	LastCheck        time.Time
	ConsecutiveFails int
	ConsecutiveOK    int
	Latency          time.Duration
	ErrorRate        float64
}

// FailoverManager manages regional failover
type FailoverManager struct {
	mu              sync.RWMutex
	policies        map[string]*FailoverPolicy
	regionHealth    map[string]*RegionHealth
	activeFailovers map[string]*ActiveFailover
	events          []*FailoverEvent
	edgeManager     *EdgeManager
	config          *FailoverConfig
	state           FailoverState
	stopCh          chan struct{}
}

// ActiveFailover represents an ongoing failover
type ActiveFailover struct {
	PolicyID     string
	SourceRegion string
	TargetRegion string
	StartedAt    time.Time
	State        FailoverState
	Progress     float64
	Error        string
}

// FailoverConfig configures the failover manager
type FailoverConfig struct {
	HealthCheckInterval       time.Duration
	FailoverTimeout           time.Duration
	RecoveryTimeout           time.Duration
	MaxEvents                 int
	EnableAutoFailover        bool
	EnableAutoRecovery        bool
	DefaultHealthThreshold    float64
	ConsecutiveFailsThreshold int
	ConsecutiveOKThreshold    int
}

// DefaultFailoverConfig returns default configuration
func DefaultFailoverConfig() *FailoverConfig {
	return &FailoverConfig{
		HealthCheckInterval:       30 * time.Second,
		FailoverTimeout:           5 * time.Minute,
		RecoveryTimeout:           10 * time.Minute,
		MaxEvents:                 1000,
		EnableAutoFailover:        true,
		EnableAutoRecovery:        true,
		DefaultHealthThreshold:    0.5,
		ConsecutiveFailsThreshold: 3,
		ConsecutiveOKThreshold:    5,
	}
}

// NewFailoverManager creates a new failover manager
func NewFailoverManager(edgeManager *EdgeManager, config *FailoverConfig) *FailoverManager {
	if config == nil {
		config = DefaultFailoverConfig()
	}

	return &FailoverManager{
		policies:        make(map[string]*FailoverPolicy),
		regionHealth:    make(map[string]*RegionHealth),
		activeFailovers: make(map[string]*ActiveFailover),
		events:          make([]*FailoverEvent, 0),
		edgeManager:     edgeManager,
		config:          config,
		state:           FailoverStateNormal,
		stopCh:          make(chan struct{}),
	}
}

// AddPolicy adds a failover policy
func (fm *FailoverManager) AddPolicy(policy *FailoverPolicy) error {
	if policy.ID == "" {
		return fmt.Errorf("policy ID required")
	}
	if policy.SourceRegion == "" {
		return fmt.Errorf("source region required")
	}
	if len(policy.TargetRegions) == 0 {
		return fmt.Errorf("at least one target region required")
	}

	fm.mu.Lock()
	defer fm.mu.Unlock()

	now := time.Now()
	if policy.CreatedAt.IsZero() {
		policy.CreatedAt = now
	}
	policy.UpdatedAt = now

	if policy.HealthThreshold == 0 {
		policy.HealthThreshold = fm.config.DefaultHealthThreshold
	}

	fm.policies[policy.ID] = policy
	return nil
}

// GetPolicy returns a policy by ID
func (fm *FailoverManager) GetPolicy(id string) (*FailoverPolicy, bool) {
	fm.mu.RLock()
	defer fm.mu.RUnlock()
	policy, ok := fm.policies[id]
	return policy, ok
}

// GetPolicies returns all policies
func (fm *FailoverManager) GetPolicies() []*FailoverPolicy {
	fm.mu.RLock()
	defer fm.mu.RUnlock()

	policies := make([]*FailoverPolicy, 0, len(fm.policies))
	for _, p := range fm.policies {
		policies = append(policies, p)
	}
	return policies
}

// RemovePolicy removes a policy
func (fm *FailoverManager) RemovePolicy(id string) bool {
	fm.mu.Lock()
	defer fm.mu.Unlock()

	if _, ok := fm.policies[id]; !ok {
		return false
	}
	delete(fm.policies, id)
	return true
}

// EnablePolicy enables a policy
func (fm *FailoverManager) EnablePolicy(id string) error {
	fm.mu.Lock()
	defer fm.mu.Unlock()

	policy, ok := fm.policies[id]
	if !ok {
		return fmt.Errorf("policy not found: %s", id)
	}
	policy.Enabled = true
	policy.UpdatedAt = time.Now()
	return nil
}

// DisablePolicy disables a policy
func (fm *FailoverManager) DisablePolicy(id string) error {
	fm.mu.Lock()
	defer fm.mu.Unlock()

	policy, ok := fm.policies[id]
	if !ok {
		return fmt.Errorf("policy not found: %s", id)
	}
	policy.Enabled = false
	policy.UpdatedAt = time.Now()
	return nil
}

// UpdateRegionHealth updates health for a region
func (fm *FailoverManager) UpdateRegionHealth(health *RegionHealth) {
	fm.mu.Lock()
	defer fm.mu.Unlock()

	health.LastCheck = time.Now()

	existing, ok := fm.regionHealth[health.Region]
	if ok {
		if health.Healthy {
			health.ConsecutiveOK = existing.ConsecutiveOK + 1
			health.ConsecutiveFails = 0
		} else {
			health.ConsecutiveFails = existing.ConsecutiveFails + 1
			health.ConsecutiveOK = 0
		}
	} else {
		if health.Healthy {
			health.ConsecutiveOK = 1
		} else {
			health.ConsecutiveFails = 1
		}
	}

	fm.regionHealth[health.Region] = health

	// Check for automatic failover
	if fm.config.EnableAutoFailover && health.ConsecutiveFails >= fm.config.ConsecutiveFailsThreshold {
		fm.checkAutoFailover(health.Region)
	}

	// Check for automatic recovery
	if fm.config.EnableAutoRecovery && health.ConsecutiveOK >= fm.config.ConsecutiveOKThreshold {
		fm.checkAutoRecovery(health.Region)
	}
}

// GetRegionHealth returns health for a region
func (fm *FailoverManager) GetRegionHealth(region string) (*RegionHealth, bool) {
	fm.mu.RLock()
	defer fm.mu.RUnlock()
	health, ok := fm.regionHealth[region]
	return health, ok
}

// GetAllRegionHealth returns health for all regions
func (fm *FailoverManager) GetAllRegionHealth() map[string]*RegionHealth {
	fm.mu.RLock()
	defer fm.mu.RUnlock()

	result := make(map[string]*RegionHealth)
	for k, v := range fm.regionHealth {
		result[k] = v
	}
	return result
}

func (fm *FailoverManager) checkAutoFailover(region string) {
	for _, policy := range fm.policies {
		if !policy.Enabled || !policy.AutoFailover {
			continue
		}
		if policy.SourceRegion != region {
			continue
		}

		// Check if already failing over
		if _, ok := fm.activeFailovers[policy.ID]; ok {
			continue
		}

		// Find best target
		targetRegion := fm.selectTargetRegion(policy)
		if targetRegion == "" {
			continue
		}

		// Initiate failover (don't hold lock during failover)
		go func(policyID, region, target string) {
			_ = fm.initiateFailover(policyID, region, target, true)
		}(policy.ID, region, targetRegion)
	}
}

func (fm *FailoverManager) checkAutoRecovery(region string) {
	for _, policy := range fm.policies {
		if !policy.Enabled || !policy.AutoRecovery {
			continue
		}

		active, ok := fm.activeFailovers[policy.ID]
		if !ok || active.SourceRegion != region {
			continue
		}

		// Initiate recovery
		go func(policyID string) {
			_ = fm.initiateRecovery(policyID, true)
		}(policy.ID)
	}
}

func (fm *FailoverManager) selectTargetRegion(policy *FailoverPolicy) string {
	for _, target := range policy.TargetRegions {
		health, ok := fm.regionHealth[target]
		if !ok || health.Healthy {
			return target
		}
	}
	return ""
}

// InitiateFailover manually initiates a failover
func (fm *FailoverManager) InitiateFailover(ctx context.Context, policyID string) error {
	fm.mu.RLock()
	policy, ok := fm.policies[policyID]
	fm.mu.RUnlock()

	if !ok {
		return fmt.Errorf("policy not found: %s", policyID)
	}

	targetRegion := fm.selectTargetRegion(policy)
	if targetRegion == "" {
		return fmt.Errorf("no healthy target region available")
	}

	return fm.initiateFailover(policyID, policy.SourceRegion, targetRegion, false)
}

func (fm *FailoverManager) initiateFailover(policyID, sourceRegion, targetRegion string, automatic bool) error {
	fm.mu.Lock()

	// Create active failover record
	active := &ActiveFailover{
		PolicyID:     policyID,
		SourceRegion: sourceRegion,
		TargetRegion: targetRegion,
		StartedAt:    time.Now(),
		State:        FailoverStateFailover,
	}
	fm.activeFailovers[policyID] = active
	fm.state = FailoverStateFailover

	// Record event
	fm.addEvent(&FailoverEvent{
		Type:         EventFailoverStarted,
		SourceRegion: sourceRegion,
		TargetRegion: targetRegion,
		Reason:       "Health threshold breached",
		Automatic:    automatic,
	})

	fm.mu.Unlock()

	// Perform failover (simplified - real implementation would do actual traffic switching)
	// In production, this would update DNS, load balancer configs, etc.

	fm.mu.Lock()
	active.State = FailoverStateFailover
	active.Progress = 1.0

	fm.addEvent(&FailoverEvent{
		Type:         EventFailoverCompleted,
		SourceRegion: sourceRegion,
		TargetRegion: targetRegion,
		Duration:     time.Since(active.StartedAt),
		Automatic:    automatic,
	})
	fm.mu.Unlock()

	return nil
}

// InitiateRecovery manually initiates recovery
func (fm *FailoverManager) InitiateRecovery(ctx context.Context, policyID string) error {
	return fm.initiateRecovery(policyID, false)
}

func (fm *FailoverManager) initiateRecovery(policyID string, automatic bool) error {
	fm.mu.Lock()

	active, ok := fm.activeFailovers[policyID]
	if !ok {
		fm.mu.Unlock()
		return fmt.Errorf("no active failover for policy: %s", policyID)
	}

	active.State = FailoverStateRecovery
	fm.state = FailoverStateRecovery

	fm.addEvent(&FailoverEvent{
		Type:         EventRecoveryStarted,
		SourceRegion: active.SourceRegion,
		TargetRegion: active.TargetRegion,
		Automatic:    automatic,
	})

	fm.mu.Unlock()

	// Perform recovery

	fm.mu.Lock()
	fm.addEvent(&FailoverEvent{
		Type:         EventRecoveryCompleted,
		SourceRegion: active.SourceRegion,
		TargetRegion: active.TargetRegion,
		Duration:     time.Since(active.StartedAt),
		Automatic:    automatic,
	})

	delete(fm.activeFailovers, policyID)

	if len(fm.activeFailovers) == 0 {
		fm.state = FailoverStateNormal
	}
	fm.mu.Unlock()

	return nil
}

func (fm *FailoverManager) addEvent(event *FailoverEvent) {
	if event.ID == "" {
		event.ID = fmt.Sprintf("event-%d", time.Now().UnixNano())
	}
	if event.Timestamp.IsZero() {
		event.Timestamp = time.Now()
	}

	fm.events = append(fm.events, event)

	// Trim old events
	if len(fm.events) > fm.config.MaxEvents {
		fm.events = fm.events[len(fm.events)-fm.config.MaxEvents:]
	}
}

// GetEvents returns recent events
func (fm *FailoverManager) GetEvents(limit int) []*FailoverEvent {
	fm.mu.RLock()
	defer fm.mu.RUnlock()

	if limit <= 0 || limit > len(fm.events) {
		limit = len(fm.events)
	}

	result := make([]*FailoverEvent, limit)
	copy(result, fm.events[len(fm.events)-limit:])
	return result
}

// GetActiveFailovers returns all active failovers
func (fm *FailoverManager) GetActiveFailovers() map[string]*ActiveFailover {
	fm.mu.RLock()
	defer fm.mu.RUnlock()

	result := make(map[string]*ActiveFailover)
	for k, v := range fm.activeFailovers {
		result[k] = v
	}
	return result
}

// GetState returns current failover state
func (fm *FailoverManager) GetState() FailoverState {
	fm.mu.RLock()
	defer fm.mu.RUnlock()
	return fm.state
}

// AcknowledgeEvent acknowledges an event
func (fm *FailoverManager) AcknowledgeEvent(eventID, acknowledgedBy string) error {
	fm.mu.Lock()
	defer fm.mu.Unlock()

	for _, event := range fm.events {
		if event.ID == eventID {
			event.Acknowledged = true
			event.AcknowledgedBy = acknowledgedBy
			event.AcknowledgedAt = time.Now()
			return nil
		}
	}
	return fmt.Errorf("event not found: %s", eventID)
}

// IsRegionHealthy checks if a region is healthy
func (fm *FailoverManager) IsRegionHealthy(region string) bool {
	fm.mu.RLock()
	defer fm.mu.RUnlock()

	health, ok := fm.regionHealth[region]
	if !ok {
		return true // Assume healthy if no data
	}
	return health.Healthy
}

// GetCurrentTarget returns the current target region for a source
func (fm *FailoverManager) GetCurrentTarget(sourceRegion string) string {
	fm.mu.RLock()
	defer fm.mu.RUnlock()

	for _, active := range fm.activeFailovers {
		if active.SourceRegion == sourceRegion {
			return active.TargetRegion
		}
	}
	return sourceRegion // No failover, return source
}

// FailoverReport generates a failover status report
type FailoverReport struct {
	GeneratedAt      time.Time
	State            FailoverState
	TotalPolicies    int
	EnabledPolicies  int
	ActiveFailovers  int
	TotalRegions     int
	HealthyRegions   int
	UnhealthyRegions int
	RecentEvents     []*FailoverEvent
	RegionDetails    map[string]*RegionHealth
}

// GenerateReport generates a failover status report
func (fm *FailoverManager) GenerateReport() *FailoverReport {
	fm.mu.RLock()
	defer fm.mu.RUnlock()

	report := &FailoverReport{
		GeneratedAt:     time.Now(),
		State:           fm.state,
		TotalPolicies:   len(fm.policies),
		ActiveFailovers: len(fm.activeFailovers),
		TotalRegions:    len(fm.regionHealth),
		RegionDetails:   make(map[string]*RegionHealth),
	}

	for _, p := range fm.policies {
		if p.Enabled {
			report.EnabledPolicies++
		}
	}

	for region, health := range fm.regionHealth {
		report.RegionDetails[region] = health
		if health.Healthy {
			report.HealthyRegions++
		} else {
			report.UnhealthyRegions++
		}
	}

	// Get last 10 events
	eventCount := 10
	if len(fm.events) < eventCount {
		eventCount = len(fm.events)
	}
	report.RecentEvents = make([]*FailoverEvent, eventCount)
	copy(report.RecentEvents, fm.events[len(fm.events)-eventCount:])

	return report
}

// CommonFailoverPolicies returns common failover configurations
func CommonFailoverPolicies() []*FailoverPolicy {
	return []*FailoverPolicy{
		{
			ID:              "us-failover",
			Name:            "US Region Failover",
			SourceRegion:    "us-east",
			TargetRegions:   []string{"us-west"},
			Enabled:         true,
			AutoFailover:    true,
			AutoRecovery:    true,
			HealthThreshold: 0.5,
			FailoverDelay:   30 * time.Second,
			RecoveryDelay:   5 * time.Minute,
		},
		{
			ID:              "eu-failover",
			Name:            "EU Region Failover",
			SourceRegion:    "eu-west",
			TargetRegions:   []string{"eu-central"},
			Enabled:         true,
			AutoFailover:    true,
			AutoRecovery:    true,
			HealthThreshold: 0.5,
			FailoverDelay:   30 * time.Second,
			RecoveryDelay:   5 * time.Minute,
		},
		{
			ID:              "ap-failover",
			Name:            "Asia Pacific Failover",
			SourceRegion:    "ap-southeast",
			TargetRegions:   []string{"ap-northeast"},
			Enabled:         true,
			AutoFailover:    true,
			AutoRecovery:    true,
			HealthThreshold: 0.5,
			FailoverDelay:   30 * time.Second,
			RecoveryDelay:   5 * time.Minute,
		},
	}
}
