package ha

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestNewBackupManager(t *testing.T) {
	gm, _ := NewGeoManager(DefaultGeoConfig())
	bm := NewBackupManager(gm)

	if bm == nil {
		t.Fatal("BackupManager is nil")
	}
	if bm.configs == nil {
		t.Error("configs map not initialized")
	}
	if bm.jobs == nil {
		t.Error("jobs map not initialized")
	}
}

func TestBackupManager_AddConfig(t *testing.T) {
	bm := NewBackupManager(nil)

	t.Run("valid config", func(t *testing.T) {
		config := &BackupConfig{
			Name:         "test-backup",
			Type:         BackupFull,
			SourceRegion: RegionNYC,
			TargetRegion: RegionLA,
		}
		err := bm.AddConfig(config)
		if err != nil {
			t.Fatalf("AddConfig failed: %v", err)
		}

		if config.RetentionDays != 30 {
			t.Errorf("Expected default retention 30, got %d", config.RetentionDays)
		}
		if config.MaxConcurrent != 4 {
			t.Errorf("Expected default concurrent 4, got %d", config.MaxConcurrent)
		}
	})

	t.Run("nil config", func(t *testing.T) {
		err := bm.AddConfig(nil)
		if err == nil {
			t.Error("Expected error for nil config")
		}
	})

	t.Run("empty name", func(t *testing.T) {
		err := bm.AddConfig(&BackupConfig{})
		if err == nil {
			t.Error("Expected error for empty name")
		}
	})
}

func TestBackupManager_GetConfig(t *testing.T) {
	bm := NewBackupManager(nil)
	if err := bm.AddConfig(&BackupConfig{Name: "test", Type: BackupFull}); err != nil {
		t.Fatalf("AddConfig failed: %v", err)
	}

	t.Run("existing config", func(t *testing.T) {
		config, ok := bm.GetConfig("test")
		if !ok {
			t.Fatal("Config not found")
		}
		if config.Name != "test" {
			t.Errorf("Expected 'test', got %s", config.Name)
		}
	})

	t.Run("non-existing config", func(t *testing.T) {
		_, ok := bm.GetConfig("nonexistent")
		if ok {
			t.Error("Should not find nonexistent config")
		}
	})
}

func TestBackupManager_ListConfigs(t *testing.T) {
	bm := NewBackupManager(nil)
	if err := bm.AddConfig(&BackupConfig{Name: "config1", Type: BackupFull}); err != nil {
		t.Fatalf("AddConfig failed: %v", err)
	}
	if err := bm.AddConfig(&BackupConfig{Name: "config2", Type: BackupIncremental}); err != nil {
		t.Fatalf("AddConfig failed: %v", err)
	}

	configs := bm.ListConfigs()
	if len(configs) != 2 {
		t.Errorf("Expected 2 configs, got %d", len(configs))
	}
}

func TestBackupManager_RemoveConfig(t *testing.T) {
	bm := NewBackupManager(nil)
	if err := bm.AddConfig(&BackupConfig{Name: "test", Type: BackupFull}); err != nil {
		t.Fatalf("AddConfig failed: %v", err)
	}

	t.Run("existing config", func(t *testing.T) {
		err := bm.RemoveConfig("test")
		if err != nil {
			t.Fatalf("RemoveConfig failed: %v", err)
		}
		_, ok := bm.GetConfig("test")
		if ok {
			t.Error("Config should be removed")
		}
	})

	t.Run("non-existing config", func(t *testing.T) {
		err := bm.RemoveConfig("nonexistent")
		if err == nil {
			t.Error("Expected error for nonexistent config")
		}
	})
}

func TestBackupManager_StartBackup(t *testing.T) {
	gm, _ := NewGeoManager(DefaultGeoConfig())
	gm.SetRegionHealth(RegionNYC, StateHealthy)
	gm.SetRegionHealth(RegionLA, StateHealthy)

	bm := NewBackupManager(gm)
	if err := bm.AddConfig(&BackupConfig{
		Name:         "test",
		Type:         BackupFull,
		SourceRegion: RegionNYC,
		TargetRegion: RegionLA,
	}); err != nil {
		t.Fatalf("AddConfig failed: %v", err)
	}

	ctx := context.Background()

	t.Run("successful start", func(t *testing.T) {
		job, err := bm.StartBackup(ctx, "test")
		if err != nil {
			t.Fatalf("StartBackup failed: %v", err)
		}
		if job.Status != BackupRunning {
			t.Errorf("Expected running, got %s", job.Status)
		}
		if job.SourceRegion != RegionNYC {
			t.Errorf("Expected NYC, got %s", job.SourceRegion)
		}
	})

	t.Run("config not found", func(t *testing.T) {
		_, err := bm.StartBackup(ctx, "nonexistent")
		if err == nil {
			t.Error("Expected error for nonexistent config")
		}
	})

	t.Run("unhealthy source region", func(t *testing.T) {
		gm.SetRegionHealth(RegionNYC, StateFailed)
		_, err := bm.StartBackup(ctx, "test")
		if err == nil {
			t.Error("Expected error for unhealthy source")
		}
	})
}

func TestBackupManager_CompleteBackup(t *testing.T) {
	bm := NewBackupManager(nil)
	if err := bm.AddConfig(&BackupConfig{Name: "test", Type: BackupFull}); err != nil {
		t.Fatalf("AddConfig failed: %v", err)
	}

	ctx := context.Background()
	job, err := bm.StartBackup(ctx, "test")
	if err != nil {
		t.Fatalf("StartBackup failed: %v", err)
	}

	err = bm.CompleteBackup(job.ID, 1000, 1000, 10, 10)
	if err != nil {
		t.Fatalf("CompleteBackup failed: %v", err)
	}

	updatedJob, _ := bm.GetJob(job.ID)
	if updatedJob.Status != BackupCompleted {
		t.Errorf("Expected completed, got %s", updatedJob.Status)
	}
	if updatedJob.BytesCopied != 1000 {
		t.Errorf("Expected 1000 bytes, got %d", updatedJob.BytesCopied)
	}
}

func TestBackupManager_FailBackup(t *testing.T) {
	bm := NewBackupManager(nil)
	if err := bm.AddConfig(&BackupConfig{Name: "test", Type: BackupFull}); err != nil {
		t.Fatalf("AddConfig failed: %v", err)
	}

	ctx := context.Background()
	job, err := bm.StartBackup(ctx, "test")
	if err != nil {
		t.Fatalf("StartBackup failed: %v", err)
	}

	err = bm.FailBackup(job.ID, errors.New("test error"))
	if err != nil {
		t.Fatalf("FailBackup failed: %v", err)
	}

	updatedJob, _ := bm.GetJob(job.ID)
	if updatedJob.Status != BackupFailed {
		t.Errorf("Expected failed, got %s", updatedJob.Status)
	}
	if updatedJob.Error != "test error" {
		t.Errorf("Expected 'test error', got %s", updatedJob.Error)
	}
}

func TestBackupManager_ListJobsByStatus(t *testing.T) {
	bm := NewBackupManager(nil)
	if err := bm.AddConfig(&BackupConfig{Name: "test", Type: BackupFull}); err != nil {
		t.Fatalf("AddConfig failed: %v", err)
	}

	ctx := context.Background()

	// Create first job
	job1, err := bm.StartBackup(ctx, "test")
	if err != nil {
		t.Fatalf("StartBackup job1 failed: %v", err)
	}

	// Small delay to ensure different timestamps
	time.Sleep(time.Millisecond)

	// Create second job
	job2, err := bm.StartBackup(ctx, "test")
	if err != nil {
		t.Fatalf("StartBackup job2 failed: %v", err)
	}

	// Verify both jobs exist
	if job1.ID == job2.ID {
		t.Fatal("Jobs have same ID - timestamp collision")
	}

	// Complete first job
	if err := bm.CompleteBackup(job1.ID, 100, 100, 1, 1); err != nil {
		t.Fatalf("CompleteBackup failed: %v", err)
	}

	// Check statuses
	running := bm.ListJobsByStatus(BackupRunning)
	if len(running) != 1 {
		t.Errorf("Expected 1 running job, got %d", len(running))
	}

	completed := bm.ListJobsByStatus(BackupCompleted)
	if len(completed) != 1 {
		t.Errorf("Expected 1 completed job, got %d", len(completed))
	}

	_ = job2
}

func TestBackupManager_UpdateProgress(t *testing.T) {
	bm := NewBackupManager(nil)
	if err := bm.AddConfig(&BackupConfig{Name: "test", Type: BackupFull}); err != nil {
		t.Fatalf("AddConfig failed: %v", err)
	}

	ctx := context.Background()
	job, err := bm.StartBackup(ctx, "test")
	if err != nil {
		t.Fatalf("StartBackup failed: %v", err)
	}

	err = bm.UpdateProgress(job.ID, 500, 5)
	if err != nil {
		t.Fatalf("UpdateProgress failed: %v", err)
	}

	updatedJob, _ := bm.GetJob(job.ID)
	if updatedJob.BytesCopied != 500 {
		t.Errorf("Expected 500 bytes, got %d", updatedJob.BytesCopied)
	}
	if updatedJob.ObjectsCopied != 5 {
		t.Errorf("Expected 5 objects, got %d", updatedJob.ObjectsCopied)
	}
}

func TestBackupManager_VerifyBackup(t *testing.T) {
	bm := NewBackupManager(nil)
	if err := bm.AddConfig(&BackupConfig{Name: "test", Type: BackupFull}); err != nil {
		t.Fatalf("AddConfig failed: %v", err)
	}

	ctx := context.Background()
	job, err := bm.StartBackup(ctx, "test")
	if err != nil {
		t.Fatalf("StartBackup failed: %v", err)
	}

	t.Run("verify incomplete job", func(t *testing.T) {
		_, err := bm.VerifyBackup(ctx, job.ID)
		if err == nil {
			t.Error("Expected error for incomplete job")
		}
	})

	t.Run("verify completed job", func(t *testing.T) {
		if err := bm.CompleteBackup(job.ID, 1000, 1000, 10, 10); err != nil {
			t.Fatalf("CompleteBackup failed: %v", err)
		}

		result, err := bm.VerifyBackup(ctx, job.ID)
		if err != nil {
			t.Fatalf("VerifyBackup failed: %v", err)
		}
		if !result.Verified {
			t.Error("Expected verified=true")
		}
		if result.ObjectsMatch != 10 {
			t.Errorf("Expected 10 objects match, got %d", result.ObjectsMatch)
		}
	})
}

func TestBackupManager_Callbacks(t *testing.T) {
	bm := NewBackupManager(nil)
	if err := bm.AddConfig(&BackupConfig{Name: "test", Type: BackupFull}); err != nil {
		t.Fatalf("AddConfig failed: %v", err)
	}

	var startCalled, completeCalled, failedCalled bool

	bm.SetCallbacks(
		func(j *BackupJob) { startCalled = true },
		func(j *BackupJob) { completeCalled = true },
		func(j *BackupJob, err error) { failedCalled = true },
	)

	ctx := context.Background()
	job, err := bm.StartBackup(ctx, "test")
	if err != nil {
		t.Fatalf("StartBackup failed: %v", err)
	}

	if !startCalled {
		t.Error("Start callback not called")
	}

	if err := bm.CompleteBackup(job.ID, 100, 100, 1, 1); err != nil {
		t.Fatalf("CompleteBackup failed: %v", err)
	}
	if !completeCalled {
		t.Error("Complete callback not called")
	}

	time.Sleep(time.Millisecond) // Ensure different ID
	job2, err := bm.StartBackup(ctx, "test")
	if err != nil {
		t.Fatalf("StartBackup job2 failed: %v", err)
	}
	if err := bm.FailBackup(job2.ID, errors.New("test")); err != nil {
		t.Fatalf("FailBackup failed: %v", err)
	}
	if !failedCalled {
		t.Error("Failed callback not called")
	}
}

func TestBackupManager_GetStats(t *testing.T) {
	bm := NewBackupManager(nil)
	if err := bm.AddConfig(&BackupConfig{Name: "test1", Type: BackupFull}); err != nil {
		t.Fatalf("AddConfig failed: %v", err)
	}
	if err := bm.AddConfig(&BackupConfig{Name: "test2", Type: BackupIncremental}); err != nil {
		t.Fatalf("AddConfig failed: %v", err)
	}

	ctx := context.Background()
	job1, err := bm.StartBackup(ctx, "test1")
	if err != nil {
		t.Fatalf("StartBackup job1 failed: %v", err)
	}

	time.Sleep(time.Millisecond) // Ensure different ID
	job2, err := bm.StartBackup(ctx, "test1")
	if err != nil {
		t.Fatalf("StartBackup job2 failed: %v", err)
	}

	if err := bm.CompleteBackup(job1.ID, 1000, 1000, 10, 10); err != nil {
		t.Fatalf("CompleteBackup failed: %v", err)
	}
	if err := bm.FailBackup(job2.ID, errors.New("test")); err != nil {
		t.Fatalf("FailBackup failed: %v", err)
	}

	stats := bm.GetStats()

	if stats.TotalConfigs != 2 {
		t.Errorf("Expected 2 configs, got %d", stats.TotalConfigs)
	}
	if stats.CompletedJobs != 1 {
		t.Errorf("Expected 1 completed, got %d", stats.CompletedJobs)
	}
	if stats.FailedJobs != 1 {
		t.Errorf("Expected 1 failed, got %d", stats.FailedJobs)
	}
	if stats.TotalBytesCopied != 1000 {
		t.Errorf("Expected 1000 bytes, got %d", stats.TotalBytesCopied)
	}
}

func TestDefaultBackupConfigs(t *testing.T) {
	configs := DefaultBackupConfigs()

	if len(configs) != 2 {
		t.Errorf("Expected 2 default configs, got %d", len(configs))
	}

	var dailyFull *BackupConfig
	for _, c := range configs {
		if c.Name == "daily-full" {
			dailyFull = c
			break
		}
	}

	if dailyFull == nil {
		t.Fatal("daily-full config not found")
	}
	if dailyFull.Type != BackupFull {
		t.Errorf("Expected full backup, got %s", dailyFull.Type)
	}
	if dailyFull.SourceRegion != RegionNYC {
		t.Errorf("Expected NYC source, got %s", dailyFull.SourceRegion)
	}
	if dailyFull.TargetRegion != RegionLA {
		t.Errorf("Expected LA target, got %s", dailyFull.TargetRegion)
	}
	if !dailyFull.Compression {
		t.Error("Expected compression enabled")
	}
	if !dailyFull.Encryption {
		t.Error("Expected encryption enabled")
	}
}
