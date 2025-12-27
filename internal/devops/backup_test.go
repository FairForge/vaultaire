// internal/devops/backup_test.go
package devops

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewBackupManager(t *testing.T) {
	t.Run("creates with defaults", func(t *testing.T) {
		manager := NewBackupManager(nil)
		assert.NotNil(t, manager)
		assert.False(t, manager.config.Enabled)
	})

	t.Run("creates with custom config", func(t *testing.T) {
		config := &BackupConfig{Enabled: true, RetentionDays: 30}
		manager := NewBackupManager(config)
		assert.True(t, manager.config.Enabled)
		assert.Equal(t, 30, manager.config.RetentionDays)
	})
}

func TestBackupManager_CreateBackup(t *testing.T) {
	manager := NewBackupManager(&BackupConfig{
		Enabled:       true,
		RetentionDays: 7,
		Encryption:    true,
		Compression:   true,
	})

	t.Run("creates backup", func(t *testing.T) {
		backup, err := manager.CreateBackup(BackupTypeFull, BackupTargetDatabase)
		require.NoError(t, err)
		assert.NotEmpty(t, backup.ID)
		assert.Equal(t, BackupTypeFull, backup.Type)
		assert.Equal(t, BackupTargetDatabase, backup.Target)
		assert.Equal(t, BackupStatusPending, backup.Status)
		assert.True(t, backup.Encrypted)
		assert.True(t, backup.Compressed)
	})

	t.Run("sets expiration", func(t *testing.T) {
		backup, _ := manager.CreateBackup(BackupTypeIncremental, BackupTargetConfig)
		assert.NotNil(t, backup.ExpiresAt)
		assert.True(t, backup.ExpiresAt.After(time.Now()))
	})
}

func TestBackupManager_GetBackup(t *testing.T) {
	manager := NewBackupManager(nil)
	backup, _ := manager.CreateBackup(BackupTypeFull, BackupTargetDatabase)

	t.Run("returns existing backup", func(t *testing.T) {
		found := manager.GetBackup(backup.ID)
		assert.NotNil(t, found)
		assert.Equal(t, backup.ID, found.ID)
	})

	t.Run("returns nil for unknown", func(t *testing.T) {
		found := manager.GetBackup("unknown")
		assert.Nil(t, found)
	})
}

func TestBackupManager_ListBackups(t *testing.T) {
	manager := NewBackupManager(nil)
	_, _ = manager.CreateBackup(BackupTypeFull, BackupTargetDatabase)
	_, _ = manager.CreateBackup(BackupTypeIncremental, BackupTargetConfig)

	backups := manager.ListBackups()
	assert.Len(t, backups, 2)
}

func TestBackupManager_ListBackupsByTarget(t *testing.T) {
	manager := NewBackupManager(nil)
	_, _ = manager.CreateBackup(BackupTypeFull, BackupTargetDatabase)
	_, _ = manager.CreateBackup(BackupTypeFull, BackupTargetDatabase)
	_, _ = manager.CreateBackup(BackupTypeFull, BackupTargetConfig)

	dbBackups := manager.ListBackupsByTarget(BackupTargetDatabase)
	assert.Len(t, dbBackups, 2)

	configBackups := manager.ListBackupsByTarget(BackupTargetConfig)
	assert.Len(t, configBackups, 1)
}

func TestBackupManager_ListBackupsByStatus(t *testing.T) {
	manager := NewBackupManager(nil)
	b1, _ := manager.CreateBackup(BackupTypeFull, BackupTargetDatabase)
	b2, _ := manager.CreateBackup(BackupTypeFull, BackupTargetConfig)
	_, _ = manager.CreateBackup(BackupTypeFull, BackupTargetLogs)

	_ = manager.UpdateBackupStatus(b1.ID, BackupStatusCompleted)
	_ = manager.UpdateBackupStatus(b2.ID, BackupStatusCompleted)

	completed := manager.ListBackupsByStatus(BackupStatusCompleted)
	assert.Len(t, completed, 2)

	pending := manager.ListBackupsByStatus(BackupStatusPending)
	assert.Len(t, pending, 1)
}

func TestBackupManager_UpdateBackupStatus(t *testing.T) {
	manager := NewBackupManager(nil)
	backup, _ := manager.CreateBackup(BackupTypeFull, BackupTargetDatabase)

	t.Run("updates status", func(t *testing.T) {
		err := manager.UpdateBackupStatus(backup.ID, BackupStatusRunning)
		assert.NoError(t, err)
		assert.Equal(t, BackupStatusRunning, manager.GetBackup(backup.ID).Status)
	})

	t.Run("sets completed time on completion", func(t *testing.T) {
		err := manager.UpdateBackupStatus(backup.ID, BackupStatusCompleted)
		assert.NoError(t, err)
		assert.NotNil(t, manager.GetBackup(backup.ID).CompletedAt)
	})

	t.Run("errors for unknown backup", func(t *testing.T) {
		err := manager.UpdateBackupStatus("unknown", BackupStatusCompleted)
		assert.Error(t, err)
	})
}

func TestBackupManager_CompleteBackup(t *testing.T) {
	manager := NewBackupManager(nil)
	backup, _ := manager.CreateBackup(BackupTypeFull, BackupTargetDatabase)

	err := manager.CompleteBackup(backup.ID, 1024*1024, "abc123", "/backups/db.tar.gz")
	require.NoError(t, err)

	updated := manager.GetBackup(backup.ID)
	assert.Equal(t, BackupStatusCompleted, updated.Status)
	assert.Equal(t, int64(1024*1024), updated.Size)
	assert.Equal(t, "abc123", updated.Checksum)
	assert.Equal(t, "/backups/db.tar.gz", updated.Location)
}

func TestBackupManager_SetBackupError(t *testing.T) {
	manager := NewBackupManager(nil)
	backup, _ := manager.CreateBackup(BackupTypeFull, BackupTargetDatabase)

	err := manager.SetBackupError(backup.ID, "disk full")
	require.NoError(t, err)

	updated := manager.GetBackup(backup.ID)
	assert.Equal(t, BackupStatusFailed, updated.Status)
	assert.Equal(t, "disk full", updated.ErrorMessage)
}

func TestBackupManager_VerifyChecksum(t *testing.T) {
	manager := NewBackupManager(nil)
	backup, _ := manager.CreateBackup(BackupTypeFull, BackupTargetDatabase)
	_ = manager.CompleteBackup(backup.ID, 1024, "correct-checksum", "/backup")

	t.Run("verifies correct checksum", func(t *testing.T) {
		result, err := manager.VerifyChecksum(backup.ID, "correct-checksum")
		require.NoError(t, err)
		assert.True(t, result.Verified)
		assert.True(t, result.ChecksumMatch)
		assert.Equal(t, BackupStatusVerified, manager.GetBackup(backup.ID).Status)
	})

	t.Run("detects incorrect checksum", func(t *testing.T) {
		backup2, _ := manager.CreateBackup(BackupTypeFull, BackupTargetConfig)
		_ = manager.CompleteBackup(backup2.ID, 1024, "expected", "/backup2")

		result, err := manager.VerifyChecksum(backup2.ID, "wrong")
		require.NoError(t, err)
		assert.False(t, result.Verified)
		assert.False(t, result.ChecksumMatch)
		assert.Equal(t, BackupStatusCorrupted, manager.GetBackup(backup2.ID).Status)
	})

	t.Run("errors for unknown backup", func(t *testing.T) {
		_, err := manager.VerifyChecksum("unknown", "checksum")
		assert.Error(t, err)
	})
}

func TestBackupManager_VerifyBackup(t *testing.T) {
	manager := NewBackupManager(nil)

	// Create and complete a backup
	data := []byte("backup data content")
	checksum, _ := CalculateChecksum(bytes.NewReader(data))

	backup, _ := manager.CreateBackup(BackupTypeFull, BackupTargetDatabase)
	_ = manager.CompleteBackup(backup.ID, int64(len(data)), checksum, "/backup")

	t.Run("verifies valid backup", func(t *testing.T) {
		result, err := manager.VerifyBackup(backup.ID, bytes.NewReader(data))
		require.NoError(t, err)
		assert.True(t, result.Verified)
		assert.True(t, result.ChecksumMatch)
		assert.True(t, result.Readable)
	})

	t.Run("detects corrupted backup", func(t *testing.T) {
		backup2, _ := manager.CreateBackup(BackupTypeFull, BackupTargetConfig)
		_ = manager.CompleteBackup(backup2.ID, 100, "original-checksum", "/backup2")

		result, err := manager.VerifyBackup(backup2.ID, strings.NewReader("different data"))
		require.NoError(t, err)
		assert.False(t, result.Verified)
		assert.False(t, result.ChecksumMatch)
	})
}

func TestBackupManager_DeleteBackup(t *testing.T) {
	manager := NewBackupManager(nil)
	backup, _ := manager.CreateBackup(BackupTypeFull, BackupTargetDatabase)

	t.Run("deletes backup", func(t *testing.T) {
		err := manager.DeleteBackup(backup.ID)
		assert.NoError(t, err)
		assert.Nil(t, manager.GetBackup(backup.ID))
	})

	t.Run("errors for unknown", func(t *testing.T) {
		err := manager.DeleteBackup("unknown")
		assert.Error(t, err)
	})
}

func TestBackupManager_CleanupExpired(t *testing.T) {
	manager := NewBackupManager(&BackupConfig{RetentionDays: 0}) // Immediate expiration

	backup, _ := manager.CreateBackup(BackupTypeFull, BackupTargetDatabase)
	// Force expiration
	expired := time.Now().Add(-1 * time.Hour)
	manager.backups[backup.ID].ExpiresAt = &expired

	deleted, err := manager.CleanupExpired()
	require.NoError(t, err)
	assert.Len(t, deleted, 1)
	assert.Contains(t, deleted, backup.ID)
}

func TestBackupManager_GetLatestBackup(t *testing.T) {
	manager := NewBackupManager(nil)

	// Create backups with slight time differences
	b1, _ := manager.CreateBackup(BackupTypeFull, BackupTargetDatabase)
	_ = manager.CompleteBackup(b1.ID, 100, "c1", "/b1")

	time.Sleep(10 * time.Millisecond)

	b2, _ := manager.CreateBackup(BackupTypeFull, BackupTargetDatabase)
	_ = manager.CompleteBackup(b2.ID, 200, "c2", "/b2")

	latest := manager.GetLatestBackup(BackupTargetDatabase)
	require.NotNil(t, latest)
	assert.Equal(t, b2.ID, latest.ID)
}

func TestBackupManager_GetLatestVerifiedBackup(t *testing.T) {
	manager := NewBackupManager(nil)

	b1, _ := manager.CreateBackup(BackupTypeFull, BackupTargetDatabase)
	_ = manager.CompleteBackup(b1.ID, 100, "c1", "/b1")
	_, _ = manager.VerifyChecksum(b1.ID, "c1")

	b2, _ := manager.CreateBackup(BackupTypeFull, BackupTargetDatabase)
	_ = manager.CompleteBackup(b2.ID, 200, "c2", "/b2")
	// b2 not verified

	latest := manager.GetLatestVerifiedBackup(BackupTargetDatabase)
	require.NotNil(t, latest)
	assert.Equal(t, b1.ID, latest.ID)
}

func TestBackupManager_NeedsBackup(t *testing.T) {
	manager := NewBackupManager(nil)

	t.Run("needs backup when none exist", func(t *testing.T) {
		assert.True(t, manager.NeedsBackup(BackupTargetDatabase, 1*time.Hour))
	})

	t.Run("does not need backup when recent", func(t *testing.T) {
		backup, _ := manager.CreateBackup(BackupTypeFull, BackupTargetDatabase)
		_ = manager.CompleteBackup(backup.ID, 100, "c", "/b")

		assert.False(t, manager.NeedsBackup(BackupTargetDatabase, 1*time.Hour))
	})
}

func TestBackupManager_GetStats(t *testing.T) {
	manager := NewBackupManager(&BackupConfig{Enabled: true, RetentionDays: 30})

	b1, _ := manager.CreateBackup(BackupTypeFull, BackupTargetDatabase)
	_ = manager.CompleteBackup(b1.ID, 1000, "c", "/b")

	_, _ = manager.CreateBackup(BackupTypeIncremental, BackupTargetConfig)

	stats := manager.GetStats()
	assert.Equal(t, 2, stats["total_backups"])
	assert.Equal(t, int64(1000), stats["total_size"])
	assert.True(t, stats["enabled"].(bool))
}

func TestBackup_IsExpired(t *testing.T) {
	t.Run("not expired when no expiration set", func(t *testing.T) {
		backup := &Backup{}
		assert.False(t, backup.IsExpired())
	})

	t.Run("not expired when future", func(t *testing.T) {
		future := time.Now().Add(1 * time.Hour)
		backup := &Backup{ExpiresAt: &future}
		assert.False(t, backup.IsExpired())
	})

	t.Run("expired when past", func(t *testing.T) {
		past := time.Now().Add(-1 * time.Hour)
		backup := &Backup{ExpiresAt: &past}
		assert.True(t, backup.IsExpired())
	})
}

func TestCalculateChecksum(t *testing.T) {
	data := []byte("test data for checksum")
	checksum, err := CalculateChecksum(bytes.NewReader(data))
	require.NoError(t, err)
	assert.NotEmpty(t, checksum)
	assert.Len(t, checksum, 64) // SHA256 hex length
}

func TestValidateBackupID(t *testing.T) {
	assert.Error(t, ValidateBackupID(""))
	assert.Error(t, ValidateBackupID("short"))
	assert.NoError(t, ValidateBackupID("backup-1234567890"))
}

func TestBackupTypeConstants(t *testing.T) {
	assert.Equal(t, BackupType("full"), BackupTypeFull)
	assert.Equal(t, BackupType("incremental"), BackupTypeIncremental)
	assert.Equal(t, BackupType("differential"), BackupTypeDifferential)
	assert.Equal(t, BackupType("snapshot"), BackupTypeSnapshot)
}

func TestBackupStatusConstants(t *testing.T) {
	assert.Equal(t, BackupStatus("pending"), BackupStatusPending)
	assert.Equal(t, BackupStatus("completed"), BackupStatusCompleted)
	assert.Equal(t, BackupStatus("verified"), BackupStatusVerified)
	assert.Equal(t, BackupStatus("corrupted"), BackupStatusCorrupted)
}

func TestDefaultBackupConfigs(t *testing.T) {
	t.Run("production has encryption", func(t *testing.T) {
		config := DefaultBackupConfigs[EnvTypeProduction]
		assert.True(t, config.Encryption)
		assert.True(t, config.VerifyAfterBackup)
		assert.Equal(t, 30, config.RetentionDays)
	})

	t.Run("development is minimal", func(t *testing.T) {
		config := DefaultBackupConfigs[EnvTypeDevelopment]
		assert.False(t, config.Enabled)
		assert.Equal(t, 7, config.RetentionDays)
	})
}
