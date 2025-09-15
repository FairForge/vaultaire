// internal/cache/backup_test.go
package cache

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSSDCache_Backup(t *testing.T) {
	t.Run("creates_full_backup", func(t *testing.T) {
		cacheDir := t.TempDir()
		backupDir := t.TempDir()

		cache, err := NewSSDCache(1024, 10*1024*1024, cacheDir)
		require.NoError(t, err)

		// Add test data
		testData := map[string][]byte{
			"file1.txt": []byte("content 1"),
			"file2.txt": []byte("content 2"),
			"file3.txt": []byte("content 3"),
		}

		for key, data := range testData {
			err = cache.Put(key, data)
			require.NoError(t, err)
		}

		// Create backup
		backup, err := cache.CreateBackup(backupDir, BackupTypeFull)
		require.NoError(t, err)

		assert.NotEmpty(t, backup.ID)
		assert.Equal(t, BackupTypeFull, backup.Type)
		assert.Equal(t, 3, backup.ItemCount)
		assert.Greater(t, backup.Size, int64(0))
		assert.WithinDuration(t, time.Now(), backup.Timestamp, 2*time.Second)

		// Verify backup files exist
		manifestPath := filepath.Join(backupDir, backup.ID, "manifest.json")
		assert.FileExists(t, manifestPath)
	})

	t.Run("creates_incremental_backup", func(t *testing.T) {
		cacheDir := t.TempDir()
		backupDir := t.TempDir()

		cache, err := NewSSDCache(1024, 10*1024*1024, cacheDir)
		require.NoError(t, err)

		// Initial data
		_ = cache.Put("initial.txt", []byte("initial data"))

		// Create full backup
		fullBackup, err := cache.CreateBackup(backupDir, BackupTypeFull)
		require.NoError(t, err)

		// Add more data
		_ = cache.Put("new.txt", []byte("new data"))

		// Create incremental backup
		incrBackup, err := cache.CreateIncrementalBackup(backupDir, fullBackup.ID)
		require.NoError(t, err)

		assert.Equal(t, BackupTypeIncremental, incrBackup.Type)
		assert.Equal(t, fullBackup.ID, incrBackup.BaseBackupID)
		assert.Equal(t, 1, incrBackup.ItemCount) // Only new item
	})

	t.Run("restores_from_backup", func(t *testing.T) {
		cacheDir := t.TempDir()
		backupDir := t.TempDir()
		restoreDir := t.TempDir()

		// Create original cache
		cache, err := NewSSDCache(1024, 10*1024*1024, cacheDir)
		require.NoError(t, err)

		testData := map[string][]byte{
			"doc1.txt": []byte("document 1"),
			"doc2.txt": []byte("document 2"),
		}

		for key, data := range testData {
			_ = cache.Put(key, data)
		}

		// Create backup
		backup, err := cache.CreateBackup(backupDir, BackupTypeFull)
		require.NoError(t, err)

		// Create new cache and restore
		newCache, err := NewSSDCache(1024, 10*1024*1024, restoreDir)
		require.NoError(t, err)

		err = newCache.RestoreFromBackup(backupDir, backup.ID)
		require.NoError(t, err)

		// Verify data restored
		for key, expectedData := range testData {
			actualData, ok := newCache.Get(key)
			assert.True(t, ok)
			assert.Equal(t, expectedData, actualData)
		}
	})

	t.Run("backup_with_encryption", func(t *testing.T) {
		cacheDir := t.TempDir()
		backupDir := t.TempDir()

		cache, err := NewSSDCache(1024, 10*1024*1024, cacheDir)
		require.NoError(t, err)

		// Enable encryption - FIXED: Make it exactly 32 bytes
		encKey := []byte("backup-encryption-key-32-bytes!!")[:32] // Added one more '!'
		err = cache.EnableEncryption(encKey)
		require.NoError(t, err)

		_ = cache.Put("secret.txt", []byte("sensitive data"))

		// Create encrypted backup - FIXED: Make this 32 bytes too
		backupKey := []byte("backup-key-for-secure-storage!!!")[:32] // Take first 32 bytes
		backup, err := cache.CreateEncryptedBackup(backupDir, BackupTypeFull, backupKey)
		require.NoError(t, err)

		assert.True(t, backup.Encrypted)

		// Verify backup files are encrypted
		dataFile := filepath.Join(backupDir, backup.ID, "data.bak")
		rawData, err := os.ReadFile(dataFile)
		require.NoError(t, err)
		assert.NotContains(t, string(rawData), "sensitive data")
	})
}

func TestSSDCache_BackupScheduler(t *testing.T) {
	t.Run("schedules_automatic_backups", func(t *testing.T) {
		cache, err := NewSSDCache(1024, 10*1024*1024, t.TempDir())
		require.NoError(t, err)

		backupDir := t.TempDir()

		// Use a done channel to signal completion
		backupDone := make(chan bool, 1)

		// Configure backup schedule with shorter interval
		schedule := &BackupSchedule{
			Enabled:    true,
			Interval:   50 * time.Millisecond, // Even shorter for testing
			BackupDir:  backupDir,
			Type:       BackupTypeFull,
			MaxBackups: 3,
			OnSuccess: func(b *BackupInfo) {
				backupDone <- true // Signal completion
			},
			OnError: func(err error) { /* alert */ },
		}

		err = cache.StartBackupScheduler(schedule)
		require.NoError(t, err)
		defer cache.StopBackupScheduler() // Always stop

		// Add data to trigger backup
		_ = cache.Put("test.txt", []byte("test data"))

		// Wait for backup with timeout
		select {
		case <-backupDone:
			// Success
		case <-time.After(500 * time.Millisecond):
			t.Fatal("Backup did not complete in time")
		}

		// Check backup was created
		entries, err := os.ReadDir(backupDir)
		require.NoError(t, err)
		assert.GreaterOrEqual(t, len(entries), 1)
	})
}

func TestSSDCache_BackupValidation(t *testing.T) {
	t.Run("validates_backup_integrity", func(t *testing.T) {
		cacheDir := t.TempDir()
		backupDir := t.TempDir()

		cache, err := NewSSDCache(1024, 10*1024*1024, cacheDir)
		require.NoError(t, err)

		_ = cache.Put("file.txt", []byte("test content"))

		// Create backup
		backup, err := cache.CreateBackup(backupDir, BackupTypeFull)
		require.NoError(t, err)

		// Validate backup
		valid, err := cache.ValidateBackup(backupDir, backup.ID)
		require.NoError(t, err)
		assert.True(t, valid)

		// Corrupt backup
		dataFile := filepath.Join(backupDir, backup.ID, "data.bak")
		_ = os.WriteFile(dataFile, []byte("corrupted"), 0644)

		// Validation should fail
		valid, err = cache.ValidateBackup(backupDir, backup.ID)
		assert.Error(t, err)
		assert.False(t, valid)
	})
}
