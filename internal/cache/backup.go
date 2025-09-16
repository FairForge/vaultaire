// internal/cache/backup.go
package cache

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// BackupType defines the type of backup
type BackupType string

const (
	BackupTypeFull        BackupType = "full"
	BackupTypeIncremental BackupType = "incremental"
)

// BackupInfo contains metadata about a backup
type BackupInfo struct {
	ID           string                 `json:"id"`
	Type         BackupType             `json:"type"`
	Timestamp    time.Time              `json:"timestamp"`
	ItemCount    int                    `json:"item_count"`
	Size         int64                  `json:"size"`
	Encrypted    bool                   `json:"encrypted"`
	BaseBackupID string                 `json:"base_backup_id,omitempty"`
	Checksum     string                 `json:"checksum"`
	CacheStats   map[string]interface{} `json:"cache_stats"`
}

// BackupSchedule defines automatic backup configuration
type BackupSchedule struct {
	Enabled    bool
	Interval   time.Duration
	BackupDir  string
	Type       BackupType
	MaxBackups int
	OnSuccess  func(*BackupInfo)
	OnError    func(error)

	ticker   *time.Ticker
	stopChan chan bool
}

// BackupManifest contains the backup metadata and item list
type BackupManifest struct {
	Info    *BackupInfo  `json:"info"`
	Items   []BackupItem `json:"items"`
	Version int          `json:"version"`
}

// BackupItem represents a single item in the backup
type BackupItem struct {
	Key       string    `json:"key"`
	Size      int64     `json:"size"`
	Checksum  string    `json:"checksum"`
	Timestamp time.Time `json:"timestamp"`
	Offset    int64     `json:"offset"`
}

// Add to SSDCache struct (in ssd_cache.go)
// Add these fields to the SSDCache struct:
// backupScheduler *BackupSchedule
// backupMu        sync.RWMutex

// CreateBackup creates a full backup of the cache
func (c *SSDCache) CreateBackup(backupDir string, backupType BackupType) (*BackupInfo, error) {
	c.backupMu.Lock()
	defer c.backupMu.Unlock()

	// Generate backup ID
	backupID := fmt.Sprintf("backup-%d", time.Now().Unix())
	backupPath := filepath.Join(backupDir, backupID)

	// Create backup directory
	if err := os.MkdirAll(backupPath, 0755); err != nil {
		return nil, fmt.Errorf("failed to create backup dir: %w", err)
	}

	info := &BackupInfo{
		ID:         backupID,
		Type:       backupType,
		Timestamp:  time.Now(),
		CacheStats: c.Stats(),
	}

	manifest := &BackupManifest{
		Info:    info,
		Items:   []BackupItem{},
		Version: 1,
	}

	// Create data file
	dataFile := filepath.Join(backupPath, "data.bak")
	df, err := os.Create(dataFile)
	if err != nil {
		return nil, fmt.Errorf("failed to create data file: %w", err)
	}
	defer func() { _ = df.Close() }()

	offset := int64(0)

	// Backup memory items
	c.memMu.RLock()
	for key, elem := range c.memItems {
		item := elem.Value.(*ssdMemItem)

		// Write data to backup file
		n, err := df.Write(item.data)
		if err != nil {
			c.memMu.RUnlock()
			return nil, fmt.Errorf("failed to write data: %w", err)
		}

		// Calculate checksum
		hash := sha256.Sum256(item.data)
		checksum := fmt.Sprintf("%x", hash)

		manifest.Items = append(manifest.Items, BackupItem{
			Key:       key,
			Size:      int64(n),
			Checksum:  checksum,
			Timestamp: time.Now(),
			Offset:    offset,
		})

		offset += int64(n)
		info.ItemCount++
		info.Size += int64(n)
	}
	c.memMu.RUnlock()

	// Backup SSD items
	c.mu.RLock()
	for key, entry := range c.index {
		// Read from SSD
		data, err := os.ReadFile(entry.Path)
		if err != nil {
			continue // Skip failed reads
		}

		// Decrypt if needed
		decrypted, err := c.decryptData(data)
		if err != nil {
			continue
		}

		// Decompress if needed
		decompressed, err := c.decompressData(decrypted)
		if err != nil {
			continue
		}

		// Write to backup
		n, err := df.Write(decompressed)
		if err != nil {
			c.mu.RUnlock()
			return nil, fmt.Errorf("failed to write data: %w", err)
		}

		hash := sha256.Sum256(decompressed)
		checksum := fmt.Sprintf("%x", hash)

		manifest.Items = append(manifest.Items, BackupItem{
			Key:       key,
			Size:      int64(n),
			Checksum:  checksum,
			Timestamp: entry.AccessTime,
			Offset:    offset,
		})

		offset += int64(n)
		info.ItemCount++
		info.Size += int64(n)
	}
	c.mu.RUnlock()

	// Calculate overall checksum
	dataChecksum, err := c.calculateFileChecksum(dataFile)
	if err != nil {
		return nil, fmt.Errorf("failed to calculate checksum: %w", err)
	}
	info.Checksum = dataChecksum

	// Write manifest
	manifestFile := filepath.Join(backupPath, "manifest.json")
	manifestData, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("failed to marshal manifest: %w", err)
	}

	if err := os.WriteFile(manifestFile, manifestData, 0644); err != nil {
		return nil, fmt.Errorf("failed to write manifest: %w", err)
	}

	return info, nil
}

// CreateIncrementalBackup creates an incremental backup since the last full backup
func (c *SSDCache) CreateIncrementalBackup(backupDir string, baseBackupID string) (*BackupInfo, error) {
	// Load base backup manifest
	baseManifestPath := filepath.Join(backupDir, baseBackupID, "manifest.json")
	baseManifestData, err := os.ReadFile(baseManifestPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read base manifest: %w", err)
	}

	var baseManifest BackupManifest
	if err := json.Unmarshal(baseManifestData, &baseManifest); err != nil {
		return nil, fmt.Errorf("failed to parse base manifest: %w", err)
	}

	// Track what was in base backup
	baseItems := make(map[string]string) // key -> checksum
	for _, item := range baseManifest.Items {
		baseItems[item.Key] = item.Checksum
	}

	// Create incremental backup with only changed items
	info := &BackupInfo{
		ID:           fmt.Sprintf("backup-incr-%d", time.Now().Unix()),
		Type:         BackupTypeIncremental,
		BaseBackupID: baseBackupID,
		Timestamp:    time.Now(),
	}

	backupPath := filepath.Join(backupDir, info.ID)
	if err := os.MkdirAll(backupPath, 0755); err != nil {
		return nil, fmt.Errorf("failed to create backup dir: %w", err)
	}

	manifest := &BackupManifest{
		Info:    info,
		Items:   []BackupItem{},
		Version: 1,
	}

	dataFile := filepath.Join(backupPath, "data.bak")
	df, err := os.Create(dataFile)
	if err != nil {
		return nil, fmt.Errorf("failed to create data file: %w", err)
	}
	defer func() { _ = df.Close() }()

	offset := int64(0)

	// Check memory items for changes
	c.memMu.RLock()
	for key, elem := range c.memItems {
		item := elem.Value.(*ssdMemItem)
		hash := sha256.Sum256(item.data)
		checksum := fmt.Sprintf("%x", hash)

		// Skip if unchanged
		if baseChecksum, exists := baseItems[key]; exists && baseChecksum == checksum {
			continue
		}

		// Write changed item
		n, err := df.Write(item.data)
		if err != nil {
			c.memMu.RUnlock()
			return nil, err
		}

		manifest.Items = append(manifest.Items, BackupItem{
			Key:       key,
			Size:      int64(n),
			Checksum:  checksum,
			Timestamp: time.Now(),
			Offset:    offset,
		})

		offset += int64(n)
		info.ItemCount++
		info.Size += int64(n)
	}
	c.memMu.RUnlock()

	// Write manifest
	manifestFile := filepath.Join(backupPath, "manifest.json")
	manifestData, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return nil, err
	}

	if err := os.WriteFile(manifestFile, manifestData, 0644); err != nil {
		return nil, err
	}

	return info, nil
}

// RestoreFromBackup restores cache from a backup
func (c *SSDCache) RestoreFromBackup(backupDir string, backupID string) error {
	manifestPath := filepath.Join(backupDir, backupID, "manifest.json")
	manifestData, err := os.ReadFile(manifestPath)
	if err != nil {
		return fmt.Errorf("failed to read manifest: %w", err)
	}

	var manifest BackupManifest
	if err := json.Unmarshal(manifestData, &manifest); err != nil {
		return fmt.Errorf("failed to parse manifest: %w", err)
	}

	dataFile := filepath.Join(backupDir, backupID, "data.bak")
	df, err := os.Open(dataFile)
	if err != nil {
		return fmt.Errorf("failed to open data file: %w", err)
	}
	defer func() { _ = df.Close() }()

	// Restore each item
	for _, item := range manifest.Items {
		// Read data from backup
		data := make([]byte, item.Size)
		if _, err := df.ReadAt(data, item.Offset); err != nil {
			return fmt.Errorf("failed to read item %s: %w", item.Key, err)
		}

		// Verify checksum
		hash := sha256.Sum256(data)
		checksum := fmt.Sprintf("%x", hash)
		if checksum != item.Checksum {
			return fmt.Errorf("checksum mismatch for %s", item.Key)
		}

		// Restore to cache
		if err := c.Put(item.Key, data); err != nil {
			return fmt.Errorf("failed to restore %s: %w", item.Key, err)
		}
	}

	return nil
}

// CreateEncryptedBackup creates an encrypted backup
func (c *SSDCache) CreateEncryptedBackup(backupDir string, backupType BackupType, encryptionKey []byte) (*BackupInfo, error) {
	// First create regular backup
	info, err := c.CreateBackup(backupDir, backupType)
	if err != nil {
		return nil, err
	}

	// Encrypt the data file
	dataFile := filepath.Join(backupDir, info.ID, "data.bak")
	data, err := os.ReadFile(dataFile)
	if err != nil {
		return nil, err
	}

	// Use AES-256-GCM for backup encryption
	block, err := aes.NewCipher(encryptionKey[:32])
	if err != nil {
		return nil, err
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}

	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}

	encrypted := gcm.Seal(nonce, nonce, data, nil)

	if err := os.WriteFile(dataFile, encrypted, 0644); err != nil {
		return nil, err
	}

	info.Encrypted = true
	return info, nil
}

// ValidateBackup checks backup integrity
func (c *SSDCache) ValidateBackup(backupDir string, backupID string) (bool, error) {
	manifestPath := filepath.Join(backupDir, backupID, "manifest.json")
	manifestData, err := os.ReadFile(manifestPath)
	if err != nil {
		return false, fmt.Errorf("failed to read manifest: %w", err)
	}

	var manifest BackupManifest
	if err := json.Unmarshal(manifestData, &manifest); err != nil {
		return false, fmt.Errorf("failed to parse manifest: %w", err)
	}

	// Verify data file checksum
	dataFile := filepath.Join(backupDir, backupID, "data.bak")
	actualChecksum, err := c.calculateFileChecksum(dataFile)
	if err != nil {
		return false, fmt.Errorf("failed to calculate checksum: %w", err)
	}

	if actualChecksum != manifest.Info.Checksum {
		return false, fmt.Errorf("checksum mismatch")
	}

	return true, nil
}

// calculateFileChecksum computes SHA256 of a file
func (c *SSDCache) calculateFileChecksum(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}

	return fmt.Sprintf("%x", h.Sum(nil)), nil
}

// StartBackupScheduler starts automatic backups
func (c *SSDCache) StartBackupScheduler(schedule *BackupSchedule) error {
	c.backupMu.Lock()
	defer c.backupMu.Unlock()

	if c.backupScheduler != nil && c.backupScheduler.ticker != nil {
		return fmt.Errorf("scheduler already running")
	}

	c.backupScheduler = schedule
	schedule.ticker = time.NewTicker(schedule.Interval)
	schedule.stopChan = make(chan bool)

	go func() {
		for {
			select {
			case <-schedule.ticker.C:
				info, err := c.CreateBackup(schedule.BackupDir, schedule.Type)
				if err != nil {
					if schedule.OnError != nil {
						schedule.OnError(err)
					}
				} else {
					if schedule.OnSuccess != nil {
						schedule.OnSuccess(info)
					}
					// Clean old backups
					c.cleanOldBackups(schedule.BackupDir, schedule.MaxBackups)
				}
			case <-schedule.stopChan:
				return
			}
		}
	}()

	return nil
}

// StopBackupScheduler stops automatic backups
func (c *SSDCache) StopBackupScheduler() {
	c.backupMu.Lock()
	defer c.backupMu.Unlock()

	if c.backupScheduler != nil && c.backupScheduler.ticker != nil {
		c.backupScheduler.ticker.Stop()
		c.backupScheduler.stopChan <- true
		c.backupScheduler = nil
	}
}

// cleanOldBackups removes old backups keeping only maxBackups
func (c *SSDCache) cleanOldBackups(backupDir string, maxBackups int) {
	// Implementation would list backups, sort by timestamp, and delete old ones
	// Simplified for brevity
}
