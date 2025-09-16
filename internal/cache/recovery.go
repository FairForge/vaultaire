// internal/cache/recovery.go
package cache

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// RecoveryStatus represents the status of a recovery operation
type RecoveryStatus string

const (
	RecoveryStatusSuccess RecoveryStatus = "success"
	RecoveryStatusPartial RecoveryStatus = "partial"
	RecoveryStatusFailed  RecoveryStatus = "failed"
)

// FailoverStatus represents failover state
type FailoverStatus string

const (
	FailoverStatusActive   FailoverStatus = "active"
	FailoverStatusInactive FailoverStatus = "inactive"
)

// RecoveryJournal tracks pending operations for crash recovery
type RecoveryJournal struct {
	Timestamp  time.Time          `json:"timestamp"`
	Operations []JournalOperation `json:"operations"`
}

// JournalOperation represents a pending cache operation
type JournalOperation struct {
	Type      string    `json:"type"`
	Key       string    `json:"key"`
	DataPath  string    `json:"data_path,omitempty"`
	Timestamp time.Time `json:"timestamp"`
}

// IntegrityReport contains results of integrity check
type IntegrityReport struct {
	TotalFiles     int      `json:"total_files"`
	CorruptedFiles int      `json:"corrupted_files"`
	CorruptedKeys  []string `json:"corrupted_keys"`
	MissingFiles   int      `json:"missing_files"`
	OrphanedFiles  int      `json:"orphaned_files"`
	SpaceReclaimed int64    `json:"space_reclaimed"`
}

// RecoveryReport contains recovery operation results
type RecoveryReport struct {
	Status         RecoveryStatus `json:"status"`
	RecoveredItems int            `json:"recovered_items"`
	FailedItems    int            `json:"failed_items"`
	Duration       time.Duration  `json:"duration"`
	Errors         []string       `json:"errors"`
}

// ReplicationConfig defines replication settings
type ReplicationConfig struct {
	Mode          ReplicationMode
	SecondaryPath string
	SyncInterval  time.Duration
	stopChan      chan bool
	ticker        *time.Ticker
}

// ReplicationMode defines how replication works
type ReplicationMode string

const (
	ReplicationModeAsync ReplicationMode = "async"
	ReplicationModeSync  ReplicationMode = "sync"
)

// Add to SSDCache struct (in ssd_cache.go):
// lastRecoveryReport *RecoveryReport
// replicationConfig *ReplicationConfig
// failoverStatus    FailoverStatus
// usingSecondary    bool
// recoveryMu        sync.RWMutex

// RecoverIndex rebuilds the index from disk files
func (c *SSDCache) RecoverIndex() (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	recovered := 0

	// Scan all shard directories
	for i := 0; i < c.shardCount; i++ {
		shardPath := filepath.Join(c.ssdPath, fmt.Sprintf("shard-%d", i))

		err := filepath.Walk(shardPath, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return nil // Skip errors
			}

			if info.IsDir() || !strings.HasSuffix(path, ".cache") {
				return nil
			}

			// Extract key from filename
			filename := filepath.Base(path)
			key := strings.TrimSuffix(filename, ".cache")

			// Check if already in index
			if _, exists := c.index[key]; exists {
				return nil
			}

			// Add to index
			c.index[key] = &SSDEntry{
				Key:        key,
				Size:       info.Size(),
				Path:       path,
				AccessTime: info.ModTime(),
			}

			recovered++
			return nil
		})

		if err != nil {
			return recovered, fmt.Errorf("failed to scan shard %d: %w", i, err)
		}
	}

	return recovered, nil
}

// CleanOrphanedFiles removes files without index entries
func (c *SSDCache) CleanOrphanedFiles() (*IntegrityReport, error) {
	report := &IntegrityReport{}

	// Create orphaned directory
	orphanedDir := filepath.Join(c.ssdPath, "orphaned")
	if err := os.MkdirAll(orphanedDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create orphaned dir: %w", err)
	}

	c.mu.RLock()
	// Build map of valid paths
	validPaths := make(map[string]bool)
	for _, entry := range c.index {
		validPaths[entry.Path] = true
	}
	c.mu.RUnlock()

	// Scan for orphaned files
	for i := 0; i < c.shardCount; i++ {
		shardPath := filepath.Join(c.ssdPath, fmt.Sprintf("shard-%d", i))

		err := filepath.Walk(shardPath, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() || !strings.HasSuffix(path, ".cache") {
				return nil
			}

			if !validPaths[path] {
				// Move to orphaned directory
				newPath := filepath.Join(orphanedDir, filepath.Base(path))
				if err := os.Rename(path, newPath); err == nil {
					report.OrphanedFiles++
					report.SpaceReclaimed += info.Size()
				}
			}

			return nil
		})

		if err != nil {
			return report, err
		}
	}

	return report, nil
}

// CheckIntegrity verifies cache data integrity
func (c *SSDCache) CheckIntegrity() (*IntegrityReport, error) {
	report := &IntegrityReport{}

	c.mu.RLock()
	defer c.mu.RUnlock()

	for key, entry := range c.index {
		report.TotalFiles++

		// Check file exists
		if _, err := os.Stat(entry.Path); os.IsNotExist(err) {
			report.MissingFiles++
			report.CorruptedKeys = append(report.CorruptedKeys, key)
			continue
		}

		// Try to read and decrypt/decompress
		data, err := os.ReadFile(entry.Path)
		if err != nil {
			report.CorruptedFiles++
			report.CorruptedKeys = append(report.CorruptedKeys, key)
			continue
		}

		// Try decryption
		decrypted, err := c.decryptData(data)
		if err != nil {
			report.CorruptedFiles++
			report.CorruptedKeys = append(report.CorruptedKeys, key)
			continue
		}

		// Try decompression
		_, err = c.decompressData(decrypted)
		if err != nil {
			report.CorruptedFiles++
			report.CorruptedKeys = append(report.CorruptedKeys, key)
		}
	}

	return report, nil
}

// RepairCorruption attempts to fix corrupted entries
func (c *SSDCache) RepairCorruption(report *IntegrityReport) (int, error) {
	fixed := 0

	c.mu.Lock()
	defer c.mu.Unlock()

	for _, key := range report.CorruptedKeys {
		// Remove from index
		if entry, exists := c.index[key]; exists {
			delete(c.index, key)
			c.currentSize -= entry.Size

			// Remove file
			_ = os.Remove(entry.Path)
			fixed++
		}

		// Also remove from memory if present
		c.memMu.Lock()
		if elem, ok := c.memItems[key]; ok {
			item := elem.Value.(*ssdMemItem)
			c.memLRU.Remove(elem)
			delete(c.memItems, key)
			c.memCurrBytes -= item.size
		}
		c.memMu.Unlock()
	}

	return fixed, nil
}

// GetLastRecoveryReport returns the last recovery report
func (c *SSDCache) GetLastRecoveryReport() *RecoveryReport {
	c.recoveryMu.RLock()
	defer c.recoveryMu.RUnlock()
	return c.lastRecoveryReport
}

// SimulateFailure simulates a primary cache failure for testing
func (c *SSDCache) SimulateFailure() {
	c.recoveryMu.Lock()
	defer c.recoveryMu.Unlock()

	if c.replicationConfig != nil {
		c.failoverStatus = FailoverStatusActive
		c.usingSecondary = true
	}
}

// GetFailoverStatus returns current failover status
func (c *SSDCache) GetFailoverStatus() FailoverStatus {
	c.recoveryMu.RLock()
	defer c.recoveryMu.RUnlock()
	return c.failoverStatus
}

// IsUsingSecondary returns true if using secondary cache
func (c *SSDCache) IsUsingSecondary() bool {
	c.recoveryMu.RLock()
	defer c.recoveryMu.RUnlock()
	return c.usingSecondary
}

// ExportToJSON exports cache contents to JSON
func (c *SSDCache) ExportToJSON(path string) error {
	export := make(map[string]string)

	// Export memory items
	c.memMu.RLock()
	for key, elem := range c.memItems {
		item := elem.Value.(*ssdMemItem)
		export[key] = string(item.data)
	}
	c.memMu.RUnlock()

	// Export SSD items
	c.mu.RLock()
	for key, entry := range c.index {
		if _, exists := export[key]; exists {
			continue // Already exported from memory
		}

		data, err := os.ReadFile(entry.Path)
		if err != nil {
			continue
		}

		decrypted, _ := c.decryptData(data)
		decompressed, _ := c.decompressData(decrypted)
		export[key] = string(decompressed)
	}
	c.mu.RUnlock()

	// Write JSON
	jsonData, err := json.MarshalIndent(export, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(path, jsonData, 0644)
}

// ImportFromJSON imports cache contents from JSON
func (c *SSDCache) ImportFromJSON(path string) (int, error) {
	jsonData, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}

	var data map[string]string
	if err := json.Unmarshal(jsonData, &data); err != nil {
		return 0, err
	}

	count := 0
	for key, value := range data {
		if err := c.Put(key, []byte(value)); err == nil {
			count++
		}
	}

	return count, nil
}

// RecoverFromDisaster creates a new cache from backup
func RecoverFromDisaster(cacheDir, backupDir, backupID string) (*SSDCache, error) {
	// Create new cache
	cache, err := NewSSDCache(1024*1024, 100*1024*1024, cacheDir)
	if err != nil {
		return nil, err
	}

	// Restore from backup
	if err := cache.RestoreFromBackup(backupDir, backupID); err != nil {
		return nil, err
	}

	return cache, nil
}

// NewSSDCacheWithReplication creates a cache with replication
func NewSSDCacheWithReplication(memSize, ssdSize int64, primaryPath string, config *ReplicationConfig) (*SSDCache, error) {
	cache, err := NewSSDCache(memSize, ssdSize, primaryPath)
	if err != nil {
		return nil, err
	}

	cache.replicationConfig = config
	cache.failoverStatus = FailoverStatusInactive

	// Start replication
	if config.Mode == ReplicationModeAsync {
		config.ticker = time.NewTicker(config.SyncInterval)
		config.stopChan = make(chan bool)

		go func() {
			for {
				select {
				case <-config.ticker.C:
					// Replicate to secondary (simplified)
					// In production, this would sync data to secondary
				case <-config.stopChan:
					return
				}
			}
		}()
	}

	return cache, nil
}

// processJournal processes recovery journal on startup
func (c *SSDCache) processJournal() {
	journalPath := filepath.Join(c.ssdPath, "recovery.journal")

	data, err := os.ReadFile(journalPath)
	if err != nil {
		return // No journal
	}

	var journal RecoveryJournal
	if err := json.Unmarshal(data, &journal); err != nil {
		return
	}

	report := &RecoveryReport{
		Status: RecoveryStatusSuccess,
	}

	// Process operations
	for _, op := range journal.Operations {
		switch op.Type {
		case "PUT":
			// Recover pending PUT operations
			if op.DataPath != "" {
				if data, err := os.ReadFile(op.DataPath); err == nil {
					if err := c.Put(op.Key, data); err == nil {
						report.RecoveredItems++
					} else {
						report.FailedItems++
					}
				}
			}
		case "DELETE":
			// Complete pending DELETE operations
			_ = c.Delete(op.Key)
		}
	}

	c.recoveryMu.Lock()
	c.lastRecoveryReport = report
	c.recoveryMu.Unlock()

	// Clear journal after processing
	_ = os.Remove(journalPath)
}
