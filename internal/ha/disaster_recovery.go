package ha

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// DRStatus represents disaster recovery status
type DRStatus string

const (
	DRStatusNormal     DRStatus = "normal"
	DRStatusAlert      DRStatus = "alert"
	DRStatusFailover   DRStatus = "failover"
	DRStatusRecovering DRStatus = "recovering"
)

// DREvent represents a disaster recovery event
type DREvent struct {
	Type      DREventType       `json:"type"`
	Region    Region            `json:"region"`
	Timestamp time.Time         `json:"timestamp"`
	Message   string            `json:"message"`
	Metadata  map[string]string `json:"metadata,omitempty"`
}

// DREventType categorizes DR events
type DREventType string

const (
	DREventRegionDown     DREventType = "region_down"
	DREventRegionUp       DREventType = "region_up"
	DREventFailoverStart  DREventType = "failover_start"
	DREventFailoverDone   DREventType = "failover_complete"
	DREventRecoveryStart  DREventType = "recovery_start"
	DREventRecoveryDone   DREventType = "recovery_complete"
	DREventBackupStart    DREventType = "backup_start"
	DREventBackupComplete DREventType = "backup_complete"
	DREventBackupFailed   DREventType = "backup_failed"
)

// DRConfig holds disaster recovery configuration
type DRConfig struct {
	FailoverThreshold    int           `json:"failover_threshold"`
	FailoverDelay        time.Duration `json:"failover_delay"`
	AutoFailover         bool          `json:"auto_failover"`
	AutoRecovery         bool          `json:"auto_recovery"`
	RecoveryDelay        time.Duration `json:"recovery_delay"`
	HealthCheckPeriod    time.Duration `json:"health_check_period"`
	BackupBeforeFailover bool          `json:"backup_before_failover"`
	VerifyAfterRecovery  bool          `json:"verify_after_recovery"`
}

// DefaultDRConfig returns sensible defaults
func DefaultDRConfig() *DRConfig {
	return &DRConfig{
		FailoverThreshold:    3,
		FailoverDelay:        30 * time.Second,
		AutoFailover:         true,
		AutoRecovery:         true,
		RecoveryDelay:        5 * time.Minute,
		HealthCheckPeriod:    10 * time.Second,
		BackupBeforeFailover: true,
		VerifyAfterRecovery:  true,
	}
}

// DROrchestrator coordinates disaster recovery operations
type DROrchestrator struct {
	config        *DRConfig
	geoManager    *GeoManager
	backupManager *BackupManager
	haOrch        *HAOrchestrator

	status        DRStatus
	activeRegion  Region
	failureCounts map[Region]int
	events        []DREvent
	mu            sync.RWMutex

	onEvent func(*DREvent)
	stopCh  chan struct{}
	wg      sync.WaitGroup
}

// NewDROrchestrator creates a new disaster recovery orchestrator
func NewDROrchestrator(
	config *DRConfig,
	geoManager *GeoManager,
	backupManager *BackupManager,
	haOrch *HAOrchestrator,
) (*DROrchestrator, error) {
	if config == nil {
		config = DefaultDRConfig()
	}
	if geoManager == nil {
		return nil, fmt.Errorf("geoManager required")
	}

	return &DROrchestrator{
		config:        config,
		geoManager:    geoManager,
		backupManager: backupManager,
		haOrch:        haOrch,
		status:        DRStatusNormal,
		activeRegion:  geoManager.config.PrimaryRegion,
		failureCounts: make(map[Region]int),
		events:        make([]DREvent, 0),
		stopCh:        make(chan struct{}),
	}, nil
}

// Start begins the DR monitoring loop
func (dr *DROrchestrator) Start(ctx context.Context) error {
	dr.wg.Add(1)
	go dr.monitorLoop(ctx)
	return nil
}

// Stop halts the DR monitoring
func (dr *DROrchestrator) Stop() {
	close(dr.stopCh)
	dr.wg.Wait()
}

func (dr *DROrchestrator) monitorLoop(ctx context.Context) {
	defer dr.wg.Done()

	ticker := time.NewTicker(dr.config.HealthCheckPeriod)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-dr.stopCh:
			return
		case <-ticker.C:
			dr.checkHealth(ctx)
		}
	}
}

func (dr *DROrchestrator) checkHealth(ctx context.Context) {
	dr.mu.Lock()
	defer dr.mu.Unlock()

	activeHealth := dr.geoManager.GetRegionHealth(dr.activeRegion)

	switch activeHealth {
	case StateHealthy:
		dr.failureCounts[dr.activeRegion] = 0
		if dr.status == DRStatusRecovering {
			dr.completeRecovery(ctx)
		}

	case StateDegraded:
		dr.failureCounts[dr.activeRegion]++
		if dr.status == DRStatusNormal {
			dr.status = DRStatusAlert
			dr.emitEvent(DREvent{
				Type:      DREventRegionDown,
				Region:    dr.activeRegion,
				Timestamp: time.Now(),
				Message:   fmt.Sprintf("Region %s degraded (%d failures)", dr.activeRegion, dr.failureCounts[dr.activeRegion]),
			})
		}

	case StateFailed:
		dr.failureCounts[dr.activeRegion]++
		if dr.failureCounts[dr.activeRegion] >= dr.config.FailoverThreshold && dr.config.AutoFailover {
			dr.initiateFailover(ctx)
		}
	}
}

func (dr *DROrchestrator) initiateFailover(ctx context.Context) {
	if dr.status == DRStatusFailover {
		return
	}

	dr.status = DRStatusFailover
	failedRegion := dr.activeRegion

	dr.emitEvent(DREvent{
		Type:      DREventFailoverStart,
		Region:    failedRegion,
		Timestamp: time.Now(),
		Message:   fmt.Sprintf("Initiating failover from %s", failedRegion),
	})

	if dr.config.BackupBeforeFailover && dr.backupManager != nil {
		dr.emitEvent(DREvent{
			Type:      DREventBackupStart,
			Region:    failedRegion,
			Timestamp: time.Now(),
			Message:   "Emergency backup before failover",
		})
	}

	newRegion := dr.geoManager.FailoverRegion(failedRegion)
	dr.activeRegion = newRegion

	dr.emitEvent(DREvent{
		Type:      DREventFailoverDone,
		Region:    newRegion,
		Timestamp: time.Now(),
		Message:   fmt.Sprintf("Failover complete: now active on %s", newRegion),
		Metadata: map[string]string{
			"from_region": string(failedRegion),
			"to_region":   string(newRegion),
		},
	})
}

// InitiateRecovery begins recovery to original region
func (dr *DROrchestrator) InitiateRecovery(ctx context.Context, targetRegion Region) error {
	dr.mu.Lock()
	defer dr.mu.Unlock()

	if dr.status != DRStatusFailover && dr.status != DRStatusAlert {
		return fmt.Errorf("not in failover state, current status: %s", dr.status)
	}

	health := dr.geoManager.GetRegionHealth(targetRegion)
	if health != StateHealthy {
		return fmt.Errorf("target region %s not healthy: %s", targetRegion, health)
	}

	dr.status = DRStatusRecovering
	dr.emitEvent(DREvent{
		Type:      DREventRecoveryStart,
		Region:    targetRegion,
		Timestamp: time.Now(),
		Message:   fmt.Sprintf("Starting recovery to %s", targetRegion),
	})

	return nil
}

func (dr *DROrchestrator) completeRecovery(ctx context.Context) {
	dr.geoManager.RecoverRegion(dr.activeRegion)
	dr.status = DRStatusNormal
	dr.failureCounts[dr.activeRegion] = 0

	dr.emitEvent(DREvent{
		Type:      DREventRecoveryDone,
		Region:    dr.activeRegion,
		Timestamp: time.Now(),
		Message:   fmt.Sprintf("Recovery complete: %s fully operational", dr.activeRegion),
	})
}

// ForceFailover manually triggers failover
func (dr *DROrchestrator) ForceFailover(ctx context.Context, targetRegion Region) error {
	dr.mu.Lock()
	defer dr.mu.Unlock()

	health := dr.geoManager.GetRegionHealth(targetRegion)
	if health == StateFailed {
		return fmt.Errorf("target region %s is failed", targetRegion)
	}

	oldRegion := dr.activeRegion
	dr.geoManager.SetRegionHealth(oldRegion, StateFailed)
	dr.activeRegion = targetRegion
	dr.status = DRStatusFailover // Set status to failover

	dr.emitEvent(DREvent{
		Type:      DREventFailoverDone,
		Region:    targetRegion,
		Timestamp: time.Now(),
		Message:   fmt.Sprintf("Manual failover: %s → %s", oldRegion, targetRegion),
		Metadata: map[string]string{
			"type":        "manual",
			"from_region": string(oldRegion),
			"to_region":   string(targetRegion),
		},
	})

	return nil
}

// GetStatus returns current DR status
func (dr *DROrchestrator) GetStatus() *DRStatusReport {
	dr.mu.RLock()
	defer dr.mu.RUnlock()

	regionStatuses := dr.geoManager.GetStatus()

	return &DRStatusReport{
		Status:        dr.status,
		ActiveRegion:  dr.activeRegion,
		Regions:       regionStatuses,
		FailureCounts: dr.copyFailureCounts(),
		LastEvent:     dr.getLastEvent(),
	}
}

// DRStatusReport contains full DR status
type DRStatusReport struct {
	Status        DRStatus                 `json:"status"`
	ActiveRegion  Region                   `json:"active_region"`
	Regions       map[Region]*RegionStatus `json:"regions"`
	FailureCounts map[Region]int           `json:"failure_counts"`
	LastEvent     *DREvent                 `json:"last_event,omitempty"`
}

// GetEvents returns recent DR events
func (dr *DROrchestrator) GetEvents(limit int) []DREvent {
	dr.mu.RLock()
	defer dr.mu.RUnlock()

	if limit <= 0 || limit > len(dr.events) {
		limit = len(dr.events)
	}

	start := len(dr.events) - limit
	if start < 0 {
		start = 0
	}

	result := make([]DREvent, limit)
	copy(result, dr.events[start:])
	return result
}

// SetEventCallback sets the event notification callback
func (dr *DROrchestrator) SetEventCallback(cb func(*DREvent)) {
	dr.mu.Lock()
	defer dr.mu.Unlock()
	dr.onEvent = cb
}

func (dr *DROrchestrator) emitEvent(event DREvent) {
	dr.events = append(dr.events, event)

	if len(dr.events) > 1000 {
		dr.events = dr.events[len(dr.events)-1000:]
	}

	if dr.onEvent != nil {
		dr.onEvent(&event)
	}
}

func (dr *DROrchestrator) copyFailureCounts() map[Region]int {
	counts := make(map[Region]int, len(dr.failureCounts))
	for k, v := range dr.failureCounts {
		counts[k] = v
	}
	return counts
}

func (dr *DROrchestrator) getLastEvent() *DREvent {
	if len(dr.events) == 0 {
		return nil
	}
	event := dr.events[len(dr.events)-1]
	return &event
}

// GetActiveRegion returns the currently active region
func (dr *DROrchestrator) GetActiveRegion() Region {
	dr.mu.RLock()
	defer dr.mu.RUnlock()
	return dr.activeRegion
}

// IsHealthy returns true if DR status is normal
func (dr *DROrchestrator) IsHealthy() bool {
	dr.mu.RLock()
	defer dr.mu.RUnlock()
	return dr.status == DRStatusNormal
}

// SimulateRegionFailure is for testing
func (dr *DROrchestrator) SimulateRegionFailure(region Region) {
	dr.mu.Lock()
	defer dr.mu.Unlock()

	dr.geoManager.SetRegionHealth(region, StateFailed)
	dr.failureCounts[region] = dr.config.FailoverThreshold
}
