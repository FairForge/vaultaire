// internal/cache/recovery_test.go
package cache

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSSDCache_Recovery(t *testing.T) {
	t.Run("recovers_from_corrupted_index", func(t *testing.T) {
		cacheDir := t.TempDir()
		cache, err := NewSSDCache(100, 10*1024*1024, cacheDir) // Small memory to force SSD
		require.NoError(t, err)

		// Add test data
		_ = cache.Put("key1", []byte("data1"))
		_ = cache.Put("key2", []byte("data2"))

		// Force to SSD
		for i := 0; i < 5; i++ {
			_ = cache.Put(fmt.Sprintf("filler-%d", i), make([]byte, 30))
		}

		// Verify items are on SSD
		cache.mu.RLock()
		ssdCount := len(cache.index)
		cache.mu.RUnlock()
		require.GreaterOrEqual(t, ssdCount, 2, "Should have items on SSD")

		// Corrupt the index
		cache.mu.Lock()
		cache.index = make(map[string]*SSDEntry) // Clear index
		cache.mu.Unlock()

		// Run recovery
		recovered, err := cache.RecoverIndex()
		require.NoError(t, err)
		assert.GreaterOrEqual(t, recovered, 2, "Should recover at least 2 entries")

		// Verify data is accessible
		data, ok := cache.Get("key1")
		assert.True(t, ok)
		assert.Equal(t, []byte("data1"), data)
	})

	t.Run("repairs_orphaned_files", func(t *testing.T) {
		cacheDir := t.TempDir()
		cache, err := NewSSDCache(1024, 10*1024*1024, cacheDir)
		require.NoError(t, err)

		// Create orphaned file (file without index entry)
		orphanPath := filepath.Join(cacheDir, "shard-0", "orphan.cache")
		_ = os.WriteFile(orphanPath, []byte("orphaned data"), 0644)

		// Run cleanup
		report, err := cache.CleanOrphanedFiles()
		require.NoError(t, err)

		assert.Equal(t, 1, report.OrphanedFiles)
		assert.Greater(t, report.SpaceReclaimed, int64(0))
		assert.FileExists(t, filepath.Join(cacheDir, "orphaned", "orphan.cache"))
	})

	t.Run("detects_and_fixes_corruption", func(t *testing.T) {
		t.Skip("TODO: Fix corruption detection for in-memory items")

		cacheDir := t.TempDir()
		cache, err := NewSSDCache(100, 10*1024*1024, cacheDir)
		require.NoError(t, err)

		// Add data
		_ = cache.Put("good", []byte("good data"))
		_ = cache.Put("bad", []byte("will be corrupted"))

		// Force to SSD
		for i := 0; i < 5; i++ {
			_ = cache.Put(fmt.Sprintf("filler-%d", i), make([]byte, 30))
		}

		// Corrupt a file on disk
		cache.mu.RLock()
		if entry, ok := cache.index["bad"]; ok {
			_ = os.WriteFile(entry.Path, []byte("XXX"), 0644) // Corrupt
		}
		cache.mu.RUnlock()

		// Run integrity check
		report, err := cache.CheckIntegrity()
		require.NoError(t, err)

		assert.Equal(t, 1, report.CorruptedFiles)
		assert.Contains(t, report.CorruptedKeys, "bad")

		// Fix corruption
		fixed, err := cache.RepairCorruption(report)
		require.NoError(t, err)
		assert.Equal(t, 1, fixed)

		// Good data should still work
		data, ok := cache.Get("good")
		assert.True(t, ok)
		assert.Equal(t, []byte("good data"), data)
	})

	t.Run("recovers_from_crash", func(t *testing.T) {
		cacheDir := t.TempDir()
		journalPath := filepath.Join(cacheDir, "recovery.journal")

		// Simulate crash with pending operations in journal
		journal := &RecoveryJournal{
			Timestamp: time.Now(),
			Operations: []JournalOperation{
				{Type: "PUT", Key: "pending1", DataPath: "/tmp/data1"},
				{Type: "DELETE", Key: "toDelete"},
			},
		}

		journalData, _ := json.Marshal(journal)
		_ = os.WriteFile(journalPath, journalData, 0644)

		// Create cache (should auto-recover)
		cache, err := NewSSDCache(1024, 10*1024*1024, cacheDir)
		require.NoError(t, err)

		// Check journal was processed
		assert.NoFileExists(t, journalPath, "Journal should be cleared after recovery")

		// Verify recovery report
		report := cache.GetLastRecoveryReport()
		assert.NotNil(t, report)
		assert.Equal(t, RecoveryStatusSuccess, report.Status)
	})
}

func TestSSDCache_DisasterRecovery(t *testing.T) {
	t.Run("recovers_from_total_failure", func(t *testing.T) {
		backupDir := t.TempDir()
		originalDir := t.TempDir()

		// Create original cache
		original, err := NewSSDCache(1024, 10*1024*1024, originalDir)
		require.NoError(t, err)

		// Add critical data
		testData := map[string][]byte{
			"critical1": []byte("critical data 1"),
			"critical2": []byte("critical data 2"),
		}

		for k, v := range testData {
			_ = original.Put(k, v)
		}

		// Create backup before disaster
		backup, err := original.CreateBackup(backupDir, BackupTypeFull)
		require.NoError(t, err)

		// Simulate total failure - delete cache directory
		_ = os.RemoveAll(originalDir)

		// Perform disaster recovery
		newDir := t.TempDir()
		recovered, err := RecoverFromDisaster(newDir, backupDir, backup.ID)
		require.NoError(t, err)

		// Verify all data recovered
		for k, expectedData := range testData {
			actualData, ok := recovered.Get(k)
			assert.True(t, ok)
			assert.Equal(t, expectedData, actualData)
		}
	})

	t.Run("automatic_failover", func(t *testing.T) {
		primaryDir := t.TempDir()
		secondaryDir := t.TempDir()

		// Create primary with replication
		config := &ReplicationConfig{
			Mode:          ReplicationModeAsync,
			SecondaryPath: secondaryDir,
			SyncInterval:  100 * time.Millisecond,
		}

		primary, err := NewSSDCacheWithReplication(1024, 10*1024*1024, primaryDir, config)
		require.NoError(t, err)

		// Add data
		_ = primary.Put("replicated", []byte("replicated data"))

		// Wait for replication
		time.Sleep(200 * time.Millisecond)

		// Simulate primary failure
		primary.SimulateFailure()

		// Automatic failover should activate secondary
		data, ok := primary.Get("replicated")
		assert.True(t, ok, "Should failover to secondary")
		assert.Equal(t, []byte("replicated data"), data)

		// Check failover status
		status := primary.GetFailoverStatus()
		assert.Equal(t, FailoverStatusActive, status)
		assert.True(t, primary.IsUsingSecondary())
	})
}

func TestSSDCache_RecoveryTools(t *testing.T) {
	t.Run("exports_cache_data", func(t *testing.T) {
		cache, err := NewSSDCache(1024, 10*1024*1024, t.TempDir())
		require.NoError(t, err)

		// Add test data
		_ = cache.Put("export1", []byte("data to export"))

		// Export to JSON
		exportPath := filepath.Join(t.TempDir(), "export.json")
		err = cache.ExportToJSON(exportPath)
		require.NoError(t, err)

		// Verify export
		data, err := os.ReadFile(exportPath)
		require.NoError(t, err)
		assert.Contains(t, string(data), "export1")
		assert.Contains(t, string(data), "data to export")
	})

	t.Run("imports_cache_data", func(t *testing.T) {
		cache, err := NewSSDCache(1024, 10*1024*1024, t.TempDir())
		require.NoError(t, err)

		// Create import file
		importData := map[string]string{
			"imported1": "imported data 1",
			"imported2": "imported data 2",
		}

		importPath := filepath.Join(t.TempDir(), "import.json")
		jsonData, _ := json.Marshal(importData)
		_ = os.WriteFile(importPath, jsonData, 0644)

		// Import
		count, err := cache.ImportFromJSON(importPath)
		require.NoError(t, err)
		assert.Equal(t, 2, count)

		// Verify imported data
		data, ok := cache.Get("imported1")
		assert.True(t, ok)
		assert.Equal(t, []byte("imported data 1"), data)
	})
}
