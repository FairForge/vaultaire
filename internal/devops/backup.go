// internal/devops/backup.go
package devops

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"
)

// backupCounter for unique IDs
var backupCounter int64
var backupCounterMu sync.Mutex

// BackupType represents the type of backup
type BackupType string

const (
	BackupTypeFull         BackupType = "full"
	BackupTypeIncremental  BackupType = "incremental"
	BackupTypeDifferential BackupType = "differential"
	BackupTypeSnapshot     BackupType = "snapshot"
)

// BackupStatus represents backup state
type BackupStatus string

const (
	BackupStatusPending   BackupStatus = "pending"
	BackupStatusRunning   BackupStatus = "running"
	BackupStatusCompleted BackupStatus = "completed"
	BackupStatusFailed    BackupStatus = "failed"
	BackupStatusVerifying BackupStatus = "verifying"
	BackupStatusVerified  BackupStatus = "verified"
	BackupStatusCorrupted BackupStatus = "corrupted"
)

// BackupTarget represents what to back up
type BackupTarget string

const (
	BackupTargetDatabase BackupTarget = "database"
	BackupTargetConfig   BackupTarget = "config"
	BackupTargetStorage  BackupTarget = "storage"
	BackupTargetLogs     BackupTarget = "logs"
	BackupTargetFull     BackupTarget = "full"
)

// Backup represents a backup record
type Backup struct {
	ID            string            `json:"id"`
	Type          BackupType        `json:"type"`
	Target        BackupTarget      `json:"target"`
	Status        BackupStatus      `json:"status"`
	StartedAt     time.Time         `json:"started_at"`
	CompletedAt   *time.Time        `json:"completed_at,omitempty"`
	Size          int64             `json:"size"`
	Checksum      string            `json:"checksum"`
	Location      string            `json:"location"`
	Encrypted     bool              `json:"encrypted"`
	Compressed    bool              `json:"compressed"`
	RetentionDays int               `json:"retention_days"`
	ExpiresAt     *time.Time        `json:"expires_at,omitempty"`
	VerifiedAt    *time.Time        `json:"verified_at,omitempty"`
	ErrorMessage  string            `json:"error_message,omitempty"`
	Metadata      map[string]string `json:"metadata,omitempty"`
}

// IsExpired checks if backup has expired
func (b *Backup) IsExpired() bool {
	if b.ExpiresAt == nil {
		return false
	}
	return time.Now().After(*b.ExpiresAt)
}

// Age returns the age of the backup
func (b *Backup) Age() time.Duration {
	return time.Since(b.StartedAt)
}

// BackupConfig configures backup behavior
type BackupConfig struct {
	Enabled           bool          `json:"enabled"`
	Schedule          string        `json:"schedule"` // Cron expression
	RetentionDays     int           `json:"retention_days"`
	FullBackupDay     int           `json:"full_backup_day"` // Day of week (0=Sunday)
	Encryption        bool          `json:"encryption"`
	Compression       bool          `json:"compression"`
	VerifyAfterBackup bool          `json:"verify_after_backup"`
	MaxConcurrent     int           `json:"max_concurrent"`
	Timeout           time.Duration `json:"timeout"`
	Destinations      []string      `json:"destinations"`
}

// DefaultBackupConfigs returns environment-specific configurations
var DefaultBackupConfigs = map[string]*BackupConfig{
	EnvTypeDevelopment: {
		Enabled:           false,
		RetentionDays:     7,
		Encryption:        false,
		Compression:       true,
		VerifyAfterBackup: false,
		MaxConcurrent:     1,
		Timeout:           30 * time.Minute,
	},
	EnvTypeStaging: {
		Enabled:           true,
		Schedule:          "0 2 * * *", // 2 AM daily
		RetentionDays:     14,
		FullBackupDay:     0, // Sunday
		Encryption:        true,
		Compression:       true,
		VerifyAfterBackup: true,
		MaxConcurrent:     2,
		Timeout:           2 * time.Hour,
	},
	EnvTypeProduction: {
		Enabled:           true,
		Schedule:          "0 */6 * * *", // Every 6 hours
		RetentionDays:     30,
		FullBackupDay:     0, // Sunday
		Encryption:        true,
		Compression:       true,
		VerifyAfterBackup: true,
		MaxConcurrent:     3,
		Timeout:           4 * time.Hour,
		Destinations:      []string{"primary", "offsite"},
	},
}

// VerificationResult holds backup verification results
type VerificationResult struct {
	BackupID      string        `json:"backup_id"`
	Verified      bool          `json:"verified"`
	VerifiedAt    time.Time     `json:"verified_at"`
	ChecksumMatch bool          `json:"checksum_match"`
	Readable      bool          `json:"readable"`
	Restorable    bool          `json:"restorable"`
	ErrorMessage  string        `json:"error_message,omitempty"`
	Duration      time.Duration `json:"duration"`
}

// BackupManager manages backups
type BackupManager struct {
	config  *BackupConfig
	backups map[string]*Backup
	mu      sync.RWMutex
}

// NewBackupManager creates a backup manager
func NewBackupManager(config *BackupConfig) *BackupManager {
	if config == nil {
		config = DefaultBackupConfigs[EnvTypeDevelopment]
	}
	return &BackupManager{
		config:  config,
		backups: make(map[string]*Backup),
	}
}

// GetConfig returns the configuration
func (m *BackupManager) GetConfig() *BackupConfig {
	return m.config
}

// IsEnabled returns whether backups are enabled
func (m *BackupManager) IsEnabled() bool {
	return m.config.Enabled
}

// CreateBackup creates a new backup record
func (m *BackupManager) CreateBackup(backupType BackupType, target BackupTarget) (*Backup, error) {
	backupCounterMu.Lock()
	backupCounter++
	counter := backupCounter
	backupCounterMu.Unlock()

	id := fmt.Sprintf("backup-%d-%d", time.Now().UnixNano(), counter)

	backup := &Backup{
		ID:            id,
		Type:          backupType,
		Target:        target,
		Status:        BackupStatusPending,
		StartedAt:     time.Now(),
		Encrypted:     m.config.Encryption,
		Compressed:    m.config.Compression,
		RetentionDays: m.config.RetentionDays,
		Metadata:      make(map[string]string),
	}

	// Calculate expiration
	expires := backup.StartedAt.AddDate(0, 0, m.config.RetentionDays)
	backup.ExpiresAt = &expires

	m.mu.Lock()
	m.backups[id] = backup
	m.mu.Unlock()

	return backup, nil
}

// GetBackup returns a backup by ID
func (m *BackupManager) GetBackup(id string) *Backup {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.backups[id]
}

// ListBackups returns all backups
func (m *BackupManager) ListBackups() []*Backup {
	m.mu.RLock()
	defer m.mu.RUnlock()

	backups := make([]*Backup, 0, len(m.backups))
	for _, b := range m.backups {
		backups = append(backups, b)
	}
	return backups
}

// ListBackupsByTarget returns backups for a specific target
func (m *BackupManager) ListBackupsByTarget(target BackupTarget) []*Backup {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var backups []*Backup
	for _, b := range m.backups {
		if b.Target == target {
			backups = append(backups, b)
		}
	}
	return backups
}

// ListBackupsByStatus returns backups with a specific status
func (m *BackupManager) ListBackupsByStatus(status BackupStatus) []*Backup {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var backups []*Backup
	for _, b := range m.backups {
		if b.Status == status {
			backups = append(backups, b)
		}
	}
	return backups
}

// UpdateBackupStatus updates a backup's status
func (m *BackupManager) UpdateBackupStatus(id string, status BackupStatus) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	backup, exists := m.backups[id]
	if !exists {
		return fmt.Errorf("backup: %s not found", id)
	}

	backup.Status = status

	if status == BackupStatusCompleted || status == BackupStatusFailed {
		now := time.Now()
		backup.CompletedAt = &now
	}

	return nil
}

// SetBackupError sets an error message on a backup
func (m *BackupManager) SetBackupError(id, errorMsg string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	backup, exists := m.backups[id]
	if !exists {
		return fmt.Errorf("backup: %s not found", id)
	}

	backup.Status = BackupStatusFailed
	backup.ErrorMessage = errorMsg
	now := time.Now()
	backup.CompletedAt = &now

	return nil
}

// CompleteBackup marks a backup as complete
func (m *BackupManager) CompleteBackup(id string, size int64, checksum, location string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	backup, exists := m.backups[id]
	if !exists {
		return fmt.Errorf("backup: %s not found", id)
	}

	now := time.Now()
	backup.Status = BackupStatusCompleted
	backup.CompletedAt = &now
	backup.Size = size
	backup.Checksum = checksum
	backup.Location = location

	return nil
}

// VerifyBackup verifies a backup's integrity
func (m *BackupManager) VerifyBackup(id string, data io.Reader) (*VerificationResult, error) {
	m.mu.Lock()
	backup, exists := m.backups[id]
	if !exists {
		m.mu.Unlock()
		return nil, fmt.Errorf("backup: %s not found", id)
	}
	backup.Status = BackupStatusVerifying
	expectedChecksum := backup.Checksum
	m.mu.Unlock()

	startTime := time.Now()
	result := &VerificationResult{
		BackupID:   id,
		VerifiedAt: startTime,
	}

	// Calculate checksum
	hash := sha256.New()
	bytesRead, err := io.Copy(hash, data)
	if err != nil {
		result.ErrorMessage = fmt.Sprintf("failed to read backup: %v", err)
		m.updateBackupVerification(id, BackupStatusCorrupted, &startTime)
		result.Duration = time.Since(startTime)
		return result, nil
	}

	result.Readable = bytesRead > 0
	actualChecksum := hex.EncodeToString(hash.Sum(nil))
	result.ChecksumMatch = actualChecksum == expectedChecksum

	if result.ChecksumMatch && result.Readable {
		result.Verified = true
		result.Restorable = true
		m.updateBackupVerification(id, BackupStatusVerified, &startTime)
	} else {
		result.ErrorMessage = "checksum mismatch"
		m.updateBackupVerification(id, BackupStatusCorrupted, &startTime)
	}

	result.Duration = time.Since(startTime)
	return result, nil
}

// VerifyChecksum verifies just the checksum without full data
func (m *BackupManager) VerifyChecksum(id, checksum string) (*VerificationResult, error) {
	m.mu.Lock()
	backup, exists := m.backups[id]
	if !exists {
		m.mu.Unlock()
		return nil, fmt.Errorf("backup: %s not found", id)
	}
	expectedChecksum := backup.Checksum
	m.mu.Unlock()

	now := time.Now()
	result := &VerificationResult{
		BackupID:      id,
		VerifiedAt:    now,
		ChecksumMatch: checksum == expectedChecksum,
		Readable:      true,
	}

	if result.ChecksumMatch {
		result.Verified = true
		result.Restorable = true
		m.updateBackupVerification(id, BackupStatusVerified, &now)
	} else {
		result.ErrorMessage = "checksum mismatch"
		m.updateBackupVerification(id, BackupStatusCorrupted, &now)
	}

	return result, nil
}

func (m *BackupManager) updateBackupVerification(id string, status BackupStatus, verifiedAt *time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if backup, exists := m.backups[id]; exists {
		backup.Status = status
		backup.VerifiedAt = verifiedAt
	}
}

// DeleteBackup deletes a backup
func (m *BackupManager) DeleteBackup(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.backups[id]; !exists {
		return fmt.Errorf("backup: %s not found", id)
	}

	delete(m.backups, id)
	return nil
}

// CleanupExpired removes expired backups
func (m *BackupManager) CleanupExpired() ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	var deleted []string
	for id, backup := range m.backups {
		if backup.IsExpired() {
			delete(m.backups, id)
			deleted = append(deleted, id)
		}
	}

	return deleted, nil
}

// GetLatestBackup returns the most recent backup for a target
func (m *BackupManager) GetLatestBackup(target BackupTarget) *Backup {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var latest *Backup
	for _, b := range m.backups {
		if b.Target == target && b.Status == BackupStatusCompleted {
			if latest == nil || b.StartedAt.After(latest.StartedAt) {
				latest = b
			}
		}
	}
	return latest
}

// GetLatestVerifiedBackup returns the most recent verified backup
func (m *BackupManager) GetLatestVerifiedBackup(target BackupTarget) *Backup {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var latest *Backup
	for _, b := range m.backups {
		if b.Target == target && b.Status == BackupStatusVerified {
			if latest == nil || b.StartedAt.After(latest.StartedAt) {
				latest = b
			}
		}
	}
	return latest
}

// GetStats returns backup statistics
func (m *BackupManager) GetStats() map[string]interface{} {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var totalSize int64
	statusCounts := make(map[BackupStatus]int)
	targetCounts := make(map[BackupTarget]int)

	for _, b := range m.backups {
		totalSize += b.Size
		statusCounts[b.Status]++
		targetCounts[b.Target]++
	}

	return map[string]interface{}{
		"total_backups":  len(m.backups),
		"total_size":     totalSize,
		"by_status":      statusCounts,
		"by_target":      targetCounts,
		"enabled":        m.config.Enabled,
		"retention_days": m.config.RetentionDays,
	}
}

// NeedsBackup checks if a target needs a new backup
func (m *BackupManager) NeedsBackup(target BackupTarget, maxAge time.Duration) bool {
	latest := m.GetLatestBackup(target)
	if latest == nil {
		return true
	}
	return latest.Age() > maxAge
}

// CalculateChecksum calculates SHA256 checksum of data
func CalculateChecksum(data io.Reader) (string, error) {
	hash := sha256.New()
	if _, err := io.Copy(hash, data); err != nil {
		return "", fmt.Errorf("backup: failed to calculate checksum: %w", err)
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

// ValidateBackupID validates a backup ID format
func ValidateBackupID(id string) error {
	if id == "" {
		return errors.New("backup: ID is required")
	}
	if len(id) < 10 {
		return errors.New("backup: ID is too short")
	}
	return nil
}

// GetBackupConfigForEnvironment returns config for an environment
func GetBackupConfigForEnvironment(envType string) *BackupConfig {
	if config, ok := DefaultBackupConfigs[envType]; ok {
		return config
	}
	return DefaultBackupConfigs[EnvTypeDevelopment]
}
