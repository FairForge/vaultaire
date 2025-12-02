package ha

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// BackupType defines the type of backup
type BackupType string

const (
	BackupFull        BackupType = "full"        // Complete backup
	BackupIncremental BackupType = "incremental" // Changes since last backup
	BackupSnapshot    BackupType = "snapshot"    // Point-in-time snapshot
)

// BackupStatus represents backup state
type BackupStatus string

const (
	BackupPending   BackupStatus = "pending"
	BackupRunning   BackupStatus = "running"
	BackupCompleted BackupStatus = "completed"
	BackupFailed    BackupStatus = "failed"
)

// BackupConfig defines backup configuration
type BackupConfig struct {
	Name            string            `json:"name"`
	Type            BackupType        `json:"type"`
	Schedule        string            `json:"schedule"` // Cron expression
	SourceRegion    Region            `json:"source_region"`
	TargetRegion    Region            `json:"target_region"`
	RetentionDays   int               `json:"retention_days"`
	Compression     bool              `json:"compression"`
	Encryption      bool              `json:"encryption"`
	MaxConcurrent   int               `json:"max_concurrent"`
	VerifyAfter     bool              `json:"verify_after"`
	NotifyOnFailure bool              `json:"notify_on_failure"`
	Tags            map[string]string `json:"tags"`
}

// BackupJob represents a backup execution
type BackupJob struct {
	ID            string            `json:"id"`
	ConfigName    string            `json:"config_name"`
	Type          BackupType        `json:"type"`
	Status        BackupStatus      `json:"status"`
	SourceRegion  Region            `json:"source_region"`
	TargetRegion  Region            `json:"target_region"`
	StartedAt     time.Time         `json:"started_at"`
	CompletedAt   *time.Time        `json:"completed_at,omitempty"`
	BytesTotal    int64             `json:"bytes_total"`
	BytesCopied   int64             `json:"bytes_copied"`
	ObjectsTotal  int64             `json:"objects_total"`
	ObjectsCopied int64             `json:"objects_copied"`
	Error         string            `json:"error,omitempty"`
	Metadata      map[string]string `json:"metadata"`
}

// BackupResult contains verification results
type BackupResult struct {
	JobID        string    `json:"job_id"`
	Verified     bool      `json:"verified"`
	VerifiedAt   time.Time `json:"verified_at"`
	ObjectsMatch int64     `json:"objects_match"`
	BytesMatch   int64     `json:"bytes_match"`
	Errors       []string  `json:"errors,omitempty"`
}

// BackupManager manages backup operations
type BackupManager struct {
	configs    map[string]*BackupConfig
	jobs       map[string]*BackupJob
	results    map[string]*BackupResult
	geoManager *GeoManager
	mu         sync.RWMutex

	// Callbacks
	onBackupStart    func(*BackupJob)
	onBackupComplete func(*BackupJob)
	onBackupFailed   func(*BackupJob, error)
}

// NewBackupManager creates a new backup manager
func NewBackupManager(geoManager *GeoManager) *BackupManager {
	return &BackupManager{
		configs:    make(map[string]*BackupConfig),
		jobs:       make(map[string]*BackupJob),
		results:    make(map[string]*BackupResult),
		geoManager: geoManager,
	}
}

// AddConfig adds a backup configuration
func (bm *BackupManager) AddConfig(config *BackupConfig) error {
	if config == nil {
		return fmt.Errorf("config required")
	}
	if config.Name == "" {
		return fmt.Errorf("config name required")
	}
	if config.RetentionDays <= 0 {
		config.RetentionDays = 30 // Default 30 days
	}
	if config.MaxConcurrent <= 0 {
		config.MaxConcurrent = 4 // Default 4 concurrent transfers
	}

	bm.mu.Lock()
	defer bm.mu.Unlock()

	bm.configs[config.Name] = config
	return nil
}

// GetConfig returns a backup configuration
func (bm *BackupManager) GetConfig(name string) (*BackupConfig, bool) {
	bm.mu.RLock()
	defer bm.mu.RUnlock()
	config, ok := bm.configs[name]
	return config, ok
}

// ListConfigs returns all backup configurations
func (bm *BackupManager) ListConfigs() []*BackupConfig {
	bm.mu.RLock()
	defer bm.mu.RUnlock()

	configs := make([]*BackupConfig, 0, len(bm.configs))
	for _, config := range bm.configs {
		configs = append(configs, config)
	}
	return configs
}

// RemoveConfig removes a backup configuration
func (bm *BackupManager) RemoveConfig(name string) error {
	bm.mu.Lock()
	defer bm.mu.Unlock()

	if _, ok := bm.configs[name]; !ok {
		return fmt.Errorf("config not found: %s", name)
	}
	delete(bm.configs, name)
	return nil
}

// StartBackup initiates a backup job
func (bm *BackupManager) StartBackup(ctx context.Context, configName string) (*BackupJob, error) {
	bm.mu.Lock()
	defer bm.mu.Unlock()

	config, ok := bm.configs[configName]
	if !ok {
		return nil, fmt.Errorf("config not found: %s", configName)
	}

	// Check if source region is healthy
	if bm.geoManager != nil {
		health := bm.geoManager.GetRegionHealth(config.SourceRegion)
		if health == StateFailed {
			return nil, fmt.Errorf("source region %s is not healthy", config.SourceRegion)
		}
	}

	job := &BackupJob{
		ID:           fmt.Sprintf("backup-%d", time.Now().UnixNano()),
		ConfigName:   configName,
		Type:         config.Type,
		Status:       BackupRunning,
		SourceRegion: config.SourceRegion,
		TargetRegion: config.TargetRegion,
		StartedAt:    time.Now(),
		Metadata:     make(map[string]string),
	}

	bm.jobs[job.ID] = job

	if bm.onBackupStart != nil {
		bm.onBackupStart(job)
	}

	return job, nil
}

// CompleteBackup marks a backup job as completed
func (bm *BackupManager) CompleteBackup(jobID string, bytesTotal, bytesCopied, objectsTotal, objectsCopied int64) error {
	bm.mu.Lock()
	defer bm.mu.Unlock()

	job, ok := bm.jobs[jobID]
	if !ok {
		return fmt.Errorf("job not found: %s", jobID)
	}

	now := time.Now()
	job.Status = BackupCompleted
	job.CompletedAt = &now
	job.BytesTotal = bytesTotal
	job.BytesCopied = bytesCopied
	job.ObjectsTotal = objectsTotal
	job.ObjectsCopied = objectsCopied

	if bm.onBackupComplete != nil {
		bm.onBackupComplete(job)
	}

	return nil
}

// FailBackup marks a backup job as failed
func (bm *BackupManager) FailBackup(jobID string, err error) error {
	bm.mu.Lock()
	defer bm.mu.Unlock()

	job, ok := bm.jobs[jobID]
	if !ok {
		return fmt.Errorf("job not found: %s", jobID)
	}

	now := time.Now()
	job.Status = BackupFailed
	job.CompletedAt = &now
	job.Error = err.Error()

	if bm.onBackupFailed != nil {
		bm.onBackupFailed(job, err)
	}

	return nil
}

// GetJob returns a backup job
func (bm *BackupManager) GetJob(jobID string) (*BackupJob, bool) {
	bm.mu.RLock()
	defer bm.mu.RUnlock()
	job, ok := bm.jobs[jobID]
	return job, ok
}

// ListJobs returns all backup jobs
func (bm *BackupManager) ListJobs() []*BackupJob {
	bm.mu.RLock()
	defer bm.mu.RUnlock()

	jobs := make([]*BackupJob, 0, len(bm.jobs))
	for _, job := range bm.jobs {
		jobs = append(jobs, job)
	}
	return jobs
}

// ListJobsByStatus returns jobs filtered by status
func (bm *BackupManager) ListJobsByStatus(status BackupStatus) []*BackupJob {
	bm.mu.RLock()
	defer bm.mu.RUnlock()

	var jobs []*BackupJob
	for _, job := range bm.jobs {
		if job.Status == status {
			jobs = append(jobs, job)
		}
	}
	return jobs
}

// UpdateProgress updates job progress
func (bm *BackupManager) UpdateProgress(jobID string, bytesCopied, objectsCopied int64) error {
	bm.mu.Lock()
	defer bm.mu.Unlock()

	job, ok := bm.jobs[jobID]
	if !ok {
		return fmt.Errorf("job not found: %s", jobID)
	}

	job.BytesCopied = bytesCopied
	job.ObjectsCopied = objectsCopied
	return nil
}

// VerifyBackup verifies a completed backup
func (bm *BackupManager) VerifyBackup(ctx context.Context, jobID string) (*BackupResult, error) {
	bm.mu.Lock()
	defer bm.mu.Unlock()

	job, ok := bm.jobs[jobID]
	if !ok {
		return nil, fmt.Errorf("job not found: %s", jobID)
	}

	if job.Status != BackupCompleted {
		return nil, fmt.Errorf("job not completed: %s", job.Status)
	}

	// Simulated verification (real implementation would compare source/target)
	result := &BackupResult{
		JobID:        jobID,
		Verified:     true,
		VerifiedAt:   time.Now(),
		ObjectsMatch: job.ObjectsCopied,
		BytesMatch:   job.BytesCopied,
	}

	bm.results[jobID] = result
	return result, nil
}

// GetVerificationResult returns verification result for a job
func (bm *BackupManager) GetVerificationResult(jobID string) (*BackupResult, bool) {
	bm.mu.RLock()
	defer bm.mu.RUnlock()
	result, ok := bm.results[jobID]
	return result, ok
}

// SetCallbacks sets backup event callbacks
func (bm *BackupManager) SetCallbacks(
	onStart func(*BackupJob),
	onComplete func(*BackupJob),
	onFailed func(*BackupJob, error),
) {
	bm.mu.Lock()
	defer bm.mu.Unlock()
	bm.onBackupStart = onStart
	bm.onBackupComplete = onComplete
	bm.onBackupFailed = onFailed
}

// GetStats returns backup statistics
func (bm *BackupManager) GetStats() *BackupStats {
	bm.mu.RLock()
	defer bm.mu.RUnlock()

	stats := &BackupStats{
		TotalConfigs: len(bm.configs),
	}

	for _, job := range bm.jobs {
		switch job.Status {
		case BackupCompleted:
			stats.CompletedJobs++
			stats.TotalBytesCopied += job.BytesCopied
			stats.TotalObjectsCopied += job.ObjectsCopied
		case BackupFailed:
			stats.FailedJobs++
		case BackupRunning:
			stats.RunningJobs++
		case BackupPending:
			stats.PendingJobs++
		}
	}

	return stats
}

// BackupStats contains aggregate backup statistics
type BackupStats struct {
	TotalConfigs       int   `json:"total_configs"`
	PendingJobs        int   `json:"pending_jobs"`
	RunningJobs        int   `json:"running_jobs"`
	CompletedJobs      int   `json:"completed_jobs"`
	FailedJobs         int   `json:"failed_jobs"`
	TotalBytesCopied   int64 `json:"total_bytes_copied"`
	TotalObjectsCopied int64 `json:"total_objects_copied"`
}

// DefaultBackupConfigs returns sensible defaults for NYC/LA setup
func DefaultBackupConfigs() []*BackupConfig {
	return []*BackupConfig{
		{
			Name:            "daily-full",
			Type:            BackupFull,
			Schedule:        "0 2 * * *", // 2 AM daily
			SourceRegion:    RegionNYC,
			TargetRegion:    RegionLA,
			RetentionDays:   30,
			Compression:     true,
			Encryption:      true,
			MaxConcurrent:   8,
			VerifyAfter:     true,
			NotifyOnFailure: true,
		},
		{
			Name:            "hourly-incremental",
			Type:            BackupIncremental,
			Schedule:        "0 * * * *", // Every hour
			SourceRegion:    RegionNYC,
			TargetRegion:    RegionLA,
			RetentionDays:   7,
			Compression:     true,
			Encryption:      true,
			MaxConcurrent:   4,
			VerifyAfter:     false,
			NotifyOnFailure: true,
		},
	}
}
