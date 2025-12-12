// internal/global/replication.go
package global

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// taskIDCounter ensures unique task IDs
var taskIDCounter uint64

// ReplicationMode defines how data is replicated
type ReplicationMode string

const (
	ReplicationSync   ReplicationMode = "sync"   // Wait for all replicas
	ReplicationAsync  ReplicationMode = "async"  // Return immediately, replicate in background
	ReplicationQuorum ReplicationMode = "quorum" // Wait for majority
)

// ReplicationState represents the state of a replica
type ReplicationState string

const (
	ReplicaStateActive     ReplicationState = "active"
	ReplicaStateLagging    ReplicationState = "lagging"
	ReplicaStateCatchingUp ReplicationState = "catching_up"
	ReplicaStateFailed     ReplicationState = "failed"
	ReplicaStatePaused     ReplicationState = "paused"
)

// ReplicationPolicy defines replication rules
type ReplicationPolicy struct {
	ID              string
	Name            string
	Enabled         bool
	Mode            ReplicationMode
	SourceRegion    string
	TargetRegions   []string
	MinReplicas     int
	MaxReplicas     int
	ReplicaSelector ReplicaSelector
	RetryPolicy     RetryPolicy
	Filters         []ReplicationFilter
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// ReplicaSelector defines how target replicas are selected
type ReplicaSelector struct {
	Strategy    SelectionStrategy
	Preferences []string // Preferred regions/locations
	Exclusions  []string // Excluded regions/locations
	MaxLatency  time.Duration
}

// SelectionStrategy for replica selection
type SelectionStrategy string

const (
	StrategyNearest    SelectionStrategy = "nearest"
	StrategyRoundRobin SelectionStrategy = "round_robin"
	StrategyLeastLoad  SelectionStrategy = "least_load"
	StrategyRandom     SelectionStrategy = "random"
	StrategyManual     SelectionStrategy = "manual"
)

// RetryPolicy for failed replications
type RetryPolicy struct {
	MaxRetries     int
	InitialBackoff time.Duration
	MaxBackoff     time.Duration
	BackoffFactor  float64
}

// DefaultRetryPolicy returns a sensible default retry policy
func DefaultRetryPolicy() RetryPolicy {
	return RetryPolicy{
		MaxRetries:     5,
		InitialBackoff: time.Second,
		MaxBackoff:     5 * time.Minute,
		BackoffFactor:  2.0,
	}
}

// ReplicationFilter defines what objects to replicate
type ReplicationFilter struct {
	Type    FilterType
	Pattern string
	Include bool
}

// FilterType for replication filters
type FilterType string

const (
	FilterPrefix  FilterType = "prefix"
	FilterSuffix  FilterType = "suffix"
	FilterRegex   FilterType = "regex"
	FilterSizeMin FilterType = "size_min"
	FilterSizeMax FilterType = "size_max"
	FilterTag     FilterType = "tag"
)

// ReplicationTask represents a single replication operation
type ReplicationTask struct {
	ID           string
	PolicyID     string
	SourceRegion string
	TargetRegion string
	Container    string
	Key          string
	Size         int64
	State        ReplicationTaskState
	Retries      int
	Error        string
	CreatedAt    time.Time
	StartedAt    time.Time
	CompletedAt  time.Time
}

// ReplicationTaskState represents task state
type ReplicationTaskState string

const (
	TaskStatePending   ReplicationTaskState = "pending"
	TaskStateRunning   ReplicationTaskState = "running"
	TaskStateCompleted ReplicationTaskState = "completed"
	TaskStateFailed    ReplicationTaskState = "failed"
	TaskStateCancelled ReplicationTaskState = "cancelled"
)

// ReplicationManager manages cross-region replication
type ReplicationManager struct {
	mu           sync.RWMutex
	policies     map[string]*ReplicationPolicy
	tasks        map[string]*ReplicationTask
	regionStatus map[string]*RegionReplicationStatus
	edgeManager  *EdgeManager
	config       *ReplicationConfig
	metrics      *ReplicationMetrics
	taskQueue    chan *ReplicationTask
	stopCh       chan struct{}
	wg           sync.WaitGroup
}

// ReplicationConfig configures the replication manager
type ReplicationConfig struct {
	WorkerCount        int
	QueueSize          int
	DefaultMode        ReplicationMode
	DefaultMinReplicas int
	EnableMetrics      bool
	TaskTimeout        time.Duration
}

// DefaultReplicationConfig returns default configuration
func DefaultReplicationConfig() *ReplicationConfig {
	return &ReplicationConfig{
		WorkerCount:        4,
		QueueSize:          1000,
		DefaultMode:        ReplicationAsync,
		DefaultMinReplicas: 2,
		EnableMetrics:      true,
		TaskTimeout:        30 * time.Minute,
	}
}

// RegionReplicationStatus tracks replication status per region
type RegionReplicationStatus struct {
	Region          string
	State           ReplicationState
	LastSync        time.Time
	LagBytes        int64
	LagObjects      int64
	PendingTasks    int64
	CompletedTasks  int64
	FailedTasks     int64
	BytesReplicated int64
}

// ReplicationMetrics tracks replication performance
type ReplicationMetrics struct {
	mu               sync.RWMutex
	TotalTasks       int64
	CompletedTasks   int64
	FailedTasks      int64
	BytesReplicated  int64
	AverageLatency   time.Duration
	TasksByRegion    map[string]int64
	FailuresByRegion map[string]int64
}

// NewReplicationManager creates a new replication manager
func NewReplicationManager(edgeManager *EdgeManager, config *ReplicationConfig) *ReplicationManager {
	if config == nil {
		config = DefaultReplicationConfig()
	}

	return &ReplicationManager{
		policies:     make(map[string]*ReplicationPolicy),
		tasks:        make(map[string]*ReplicationTask),
		regionStatus: make(map[string]*RegionReplicationStatus),
		edgeManager:  edgeManager,
		config:       config,
		metrics: &ReplicationMetrics{
			TasksByRegion:    make(map[string]int64),
			FailuresByRegion: make(map[string]int64),
		},
		taskQueue: make(chan *ReplicationTask, config.QueueSize),
		stopCh:    make(chan struct{}),
	}
}

// Start begins the replication workers
func (rm *ReplicationManager) Start() {
	for i := 0; i < rm.config.WorkerCount; i++ {
		rm.wg.Add(1)
		go rm.worker(i)
	}
}

// Stop stops the replication manager
func (rm *ReplicationManager) Stop() {
	close(rm.stopCh)
	rm.wg.Wait()
}

func (rm *ReplicationManager) worker(id int) {
	defer rm.wg.Done()

	for {
		select {
		case task := <-rm.taskQueue:
			rm.processTask(task)
		case <-rm.stopCh:
			return
		}
	}
}

func (rm *ReplicationManager) processTask(task *ReplicationTask) {
	rm.mu.Lock()
	task.State = TaskStateRunning
	task.StartedAt = time.Now()
	rm.mu.Unlock()

	// Simulate replication work
	// In real implementation, this would copy data between regions
	err := rm.executeReplication(task)

	rm.mu.Lock()
	defer rm.mu.Unlock()

	if err != nil {
		task.State = TaskStateFailed
		task.Error = err.Error()
		rm.metrics.mu.Lock()
		rm.metrics.FailedTasks++
		rm.metrics.FailuresByRegion[task.TargetRegion]++
		rm.metrics.mu.Unlock()

		// Update region status
		if status, ok := rm.regionStatus[task.TargetRegion]; ok {
			status.FailedTasks++
		}
	} else {
		task.State = TaskStateCompleted
		task.CompletedAt = time.Now()
		rm.metrics.mu.Lock()
		rm.metrics.CompletedTasks++
		rm.metrics.BytesReplicated += task.Size
		rm.metrics.TasksByRegion[task.TargetRegion]++
		rm.metrics.mu.Unlock()

		// Update region status
		if status, ok := rm.regionStatus[task.TargetRegion]; ok {
			status.CompletedTasks++
			status.BytesReplicated += task.Size
			status.LastSync = time.Now()
		}
	}
}

func (rm *ReplicationManager) executeReplication(task *ReplicationTask) error {
	// Placeholder for actual replication logic
	// In real implementation:
	// 1. Read object from source region
	// 2. Write object to target region
	// 3. Verify integrity (checksum)
	return nil
}

// AddPolicy adds a replication policy
func (rm *ReplicationManager) AddPolicy(policy *ReplicationPolicy) error {
	if policy.ID == "" {
		return fmt.Errorf("policy ID required")
	}

	rm.mu.Lock()
	defer rm.mu.Unlock()

	now := time.Now()
	if policy.CreatedAt.IsZero() {
		policy.CreatedAt = now
	}
	policy.UpdatedAt = now

	rm.policies[policy.ID] = policy

	// Initialize region status for targets
	for _, region := range policy.TargetRegions {
		if _, ok := rm.regionStatus[region]; !ok {
			rm.regionStatus[region] = &RegionReplicationStatus{
				Region: region,
				State:  ReplicaStateActive,
			}
		}
	}

	return nil
}

// GetPolicy returns a policy by ID
func (rm *ReplicationManager) GetPolicy(id string) (*ReplicationPolicy, bool) {
	rm.mu.RLock()
	defer rm.mu.RUnlock()
	policy, ok := rm.policies[id]
	return policy, ok
}

// GetPolicies returns all policies
func (rm *ReplicationManager) GetPolicies() []*ReplicationPolicy {
	rm.mu.RLock()
	defer rm.mu.RUnlock()

	policies := make([]*ReplicationPolicy, 0, len(rm.policies))
	for _, p := range rm.policies {
		policies = append(policies, p)
	}
	return policies
}

// RemovePolicy removes a policy
func (rm *ReplicationManager) RemovePolicy(id string) bool {
	rm.mu.Lock()
	defer rm.mu.Unlock()

	if _, ok := rm.policies[id]; !ok {
		return false
	}
	delete(rm.policies, id)
	return true
}

// EnablePolicy enables a policy
func (rm *ReplicationManager) EnablePolicy(id string) error {
	rm.mu.Lock()
	defer rm.mu.Unlock()

	policy, ok := rm.policies[id]
	if !ok {
		return fmt.Errorf("policy not found: %s", id)
	}
	policy.Enabled = true
	policy.UpdatedAt = time.Now()
	return nil
}

// DisablePolicy disables a policy
func (rm *ReplicationManager) DisablePolicy(id string) error {
	rm.mu.Lock()
	defer rm.mu.Unlock()

	policy, ok := rm.policies[id]
	if !ok {
		return fmt.Errorf("policy not found: %s", id)
	}
	policy.Enabled = false
	policy.UpdatedAt = time.Now()
	return nil
}

// SubmitTask submits a replication task
func (rm *ReplicationManager) SubmitTask(task *ReplicationTask) error {
	if task.ID == "" {
		task.ID = fmt.Sprintf("task-%d-%d", time.Now().UnixNano(), atomic.AddUint64(&taskIDCounter, 1))
	}
	task.State = TaskStatePending
	task.CreatedAt = time.Now()

	rm.mu.Lock()
	rm.tasks[task.ID] = task
	rm.metrics.mu.Lock()
	rm.metrics.TotalTasks++
	rm.metrics.mu.Unlock()

	// Update region pending count
	if status, ok := rm.regionStatus[task.TargetRegion]; ok {
		status.PendingTasks++
	}
	rm.mu.Unlock()

	select {
	case rm.taskQueue <- task:
		return nil
	default:
		return fmt.Errorf("task queue full")
	}
}

// GetTask returns a task by ID
func (rm *ReplicationManager) GetTask(id string) (*ReplicationTask, bool) {
	rm.mu.RLock()
	defer rm.mu.RUnlock()
	task, ok := rm.tasks[id]
	return task, ok
}

// GetPendingTasks returns pending tasks for a region
func (rm *ReplicationManager) GetPendingTasks(region string) []*ReplicationTask {
	rm.mu.RLock()
	defer rm.mu.RUnlock()

	var pending []*ReplicationTask
	for _, task := range rm.tasks {
		if task.TargetRegion == region && task.State == TaskStatePending {
			pending = append(pending, task)
		}
	}
	return pending
}

// GetRegionStatus returns status for a region
func (rm *ReplicationManager) GetRegionStatus(region string) (*RegionReplicationStatus, bool) {
	rm.mu.RLock()
	defer rm.mu.RUnlock()
	status, ok := rm.regionStatus[region]
	return status, ok
}

// GetAllRegionStatus returns status for all regions
func (rm *ReplicationManager) GetAllRegionStatus() map[string]*RegionReplicationStatus {
	rm.mu.RLock()
	defer rm.mu.RUnlock()

	result := make(map[string]*RegionReplicationStatus)
	for k, v := range rm.regionStatus {
		result[k] = v
	}
	return result
}

// SetRegionState sets the state of a region
func (rm *ReplicationManager) SetRegionState(region string, state ReplicationState) {
	rm.mu.Lock()
	defer rm.mu.Unlock()

	if status, ok := rm.regionStatus[region]; ok {
		status.State = state
	}
}

// GetMetrics returns replication metrics
func (rm *ReplicationManager) GetMetrics() *ReplicationMetrics {
	rm.metrics.mu.RLock()
	defer rm.metrics.mu.RUnlock()

	// Return a copy
	return &ReplicationMetrics{
		TotalTasks:       rm.metrics.TotalTasks,
		CompletedTasks:   rm.metrics.CompletedTasks,
		FailedTasks:      rm.metrics.FailedTasks,
		BytesReplicated:  rm.metrics.BytesReplicated,
		AverageLatency:   rm.metrics.AverageLatency,
		TasksByRegion:    copyMapInt64(rm.metrics.TasksByRegion),
		FailuresByRegion: copyMapInt64(rm.metrics.FailuresByRegion),
	}
}

func copyMapInt64(m map[string]int64) map[string]int64 {
	result := make(map[string]int64)
	for k, v := range m {
		result[k] = v
	}
	return result
}

// ReplicateObject creates replication tasks for an object based on policies
func (rm *ReplicationManager) ReplicateObject(ctx context.Context, sourceRegion, container, key string, size int64) ([]*ReplicationTask, error) {
	rm.mu.RLock()
	policies := make([]*ReplicationPolicy, 0)
	for _, p := range rm.policies {
		if p.Enabled && p.SourceRegion == sourceRegion {
			if rm.matchesFilters(p.Filters, container, key, size) {
				policies = append(policies, p)
			}
		}
	}
	rm.mu.RUnlock()

	var tasks []*ReplicationTask
	for _, policy := range policies {
		for _, targetRegion := range policy.TargetRegions {
			task := &ReplicationTask{
				PolicyID:     policy.ID,
				SourceRegion: sourceRegion,
				TargetRegion: targetRegion,
				Container:    container,
				Key:          key,
				Size:         size,
			}
			if err := rm.SubmitTask(task); err != nil {
				return tasks, fmt.Errorf("failed to submit task for %s: %w", targetRegion, err)
			}
			tasks = append(tasks, task)
		}
	}

	return tasks, nil
}

func (rm *ReplicationManager) matchesFilters(filters []ReplicationFilter, container, key string, size int64) bool {
	if len(filters) == 0 {
		return true
	}

	for _, filter := range filters {
		match := false
		fullPath := container + "/" + key

		switch filter.Type {
		case FilterPrefix:
			match = len(fullPath) >= len(filter.Pattern) && fullPath[:len(filter.Pattern)] == filter.Pattern
		case FilterSuffix:
			match = len(fullPath) >= len(filter.Pattern) && fullPath[len(fullPath)-len(filter.Pattern):] == filter.Pattern
		case FilterSizeMin:
			// Pattern should be parsed as int64, simplified here
			match = size >= 0
		case FilterSizeMax:
			match = size <= 1<<40 // 1TB max
		default:
			match = true
		}

		if filter.Include && !match {
			return false
		}
		if !filter.Include && match {
			return false
		}
	}

	return true
}

// WaitForReplication waits for sync replication to complete
func (rm *ReplicationManager) WaitForReplication(ctx context.Context, taskIDs []string) error {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			allComplete := true
			for _, id := range taskIDs {
				task, ok := rm.GetTask(id)
				if !ok {
					continue
				}
				if task.State == TaskStatePending || task.State == TaskStateRunning {
					allComplete = false
					break
				}
				if task.State == TaskStateFailed {
					return fmt.Errorf("replication task %s failed: %s", id, task.Error)
				}
			}
			if allComplete {
				return nil
			}
		}
	}
}

// ReplicationReport generates a replication status report
type ReplicationReport struct {
	GeneratedAt     time.Time
	TotalPolicies   int
	ActivePolicies  int
	TotalRegions    int
	HealthyRegions  int
	LaggingRegions  int
	FailedRegions   int
	TotalTasks      int64
	PendingTasks    int64
	CompletedTasks  int64
	FailedTasks     int64
	BytesReplicated int64
	RegionDetails   map[string]*RegionReplicationStatus
}

// GenerateReport generates a replication status report
func (rm *ReplicationManager) GenerateReport() *ReplicationReport {
	rm.mu.RLock()
	defer rm.mu.RUnlock()

	report := &ReplicationReport{
		GeneratedAt:   time.Now(),
		TotalPolicies: len(rm.policies),
		RegionDetails: make(map[string]*RegionReplicationStatus),
	}

	for _, p := range rm.policies {
		if p.Enabled {
			report.ActivePolicies++
		}
	}

	for region, status := range rm.regionStatus {
		report.TotalRegions++
		report.RegionDetails[region] = status

		switch status.State {
		case ReplicaStateActive:
			report.HealthyRegions++
		case ReplicaStateLagging, ReplicaStateCatchingUp:
			report.LaggingRegions++
		case ReplicaStateFailed:
			report.FailedRegions++
		}

		report.PendingTasks += status.PendingTasks
		report.CompletedTasks += status.CompletedTasks
		report.FailedTasks += status.FailedTasks
		report.BytesReplicated += status.BytesReplicated
	}

	rm.metrics.mu.RLock()
	report.TotalTasks = rm.metrics.TotalTasks
	rm.metrics.mu.RUnlock()

	return report
}

// CommonReplicationPolicies returns common replication configurations
func CommonReplicationPolicies() []*ReplicationPolicy {
	return []*ReplicationPolicy{
		{
			ID:            "us-multi-region",
			Name:          "US Multi-Region Replication",
			Enabled:       true,
			Mode:          ReplicationAsync,
			SourceRegion:  "us-east",
			TargetRegions: []string{"us-west"},
			MinReplicas:   1,
			RetryPolicy:   DefaultRetryPolicy(),
		},
		{
			ID:            "eu-multi-region",
			Name:          "EU Multi-Region Replication",
			Enabled:       true,
			Mode:          ReplicationAsync,
			SourceRegion:  "eu-west",
			TargetRegions: []string{"eu-central"},
			MinReplicas:   1,
			RetryPolicy:   DefaultRetryPolicy(),
		},
		{
			ID:            "cross-atlantic",
			Name:          "Cross-Atlantic Replication",
			Enabled:       false,
			Mode:          ReplicationAsync,
			SourceRegion:  "us-east",
			TargetRegions: []string{"eu-west"},
			MinReplicas:   1,
			RetryPolicy:   DefaultRetryPolicy(),
		},
		{
			ID:            "asia-pacific",
			Name:          "Asia Pacific Replication",
			Enabled:       true,
			Mode:          ReplicationAsync,
			SourceRegion:  "ap-southeast",
			TargetRegions: []string{"ap-northeast"},
			MinReplicas:   1,
			RetryPolicy:   DefaultRetryPolicy(),
		},
	}
}
