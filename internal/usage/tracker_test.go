package usage

import (
	"context"
	"testing"
)

func TestUsageTracker_RecordUpload(t *testing.T) {
	tracker := NewUsageTracker(nil)
	
	// Record an upload
	err := tracker.RecordUpload(context.Background(), "user-1", "bucket-1", "file.txt", 1024)
	if err != nil {
		t.Fatalf("Failed to record upload: %v", err)
	}
	
	// Get usage
	usage, err := tracker.GetUsage(context.Background(), "user-1")
	if err != nil {
		t.Fatalf("Failed to get usage: %v", err)
	}
	
	if usage.StorageBytes != 1024 {
		t.Errorf("Expected storage 1024, got %d", usage.StorageBytes)
	}
	
	if usage.ObjectCount != 1 {
		t.Errorf("Expected 1 object, got %d", usage.ObjectCount)
	}
}

func TestUsageTracker_RecordDelete(t *testing.T) {
	tracker := NewUsageTracker(nil)
	
	// Record an upload first
	_ = tracker.RecordUpload(context.Background(), "user-1", "bucket-1", "file.txt", 2048)
	
	// Record a delete
	err := tracker.RecordDelete(context.Background(), "user-1", "bucket-1", "file.txt", 2048)
	if err != nil {
		t.Fatalf("Failed to record delete: %v", err)
	}
	
	// Get usage - should be zero
	usage, err := tracker.GetUsage(context.Background(), "user-1")
	if err != nil {
		t.Fatalf("Failed to get usage: %v", err)
	}
	
	if usage.StorageBytes != 0 {
		t.Errorf("Expected storage 0, got %d", usage.StorageBytes)
	}
	
	if usage.ObjectCount != 0 {
		t.Errorf("Expected 0 objects, got %d", usage.ObjectCount)
	}
}

func TestUsageTracker_RecordBandwidth(t *testing.T) {
	tracker := NewUsageTracker(nil)
	
	// Record download bandwidth
	err := tracker.RecordBandwidth(context.Background(), "user-1", BandwidthTypeDownload, 1024*1024)
	if err != nil {
		t.Fatalf("Failed to record bandwidth: %v", err)
	}
	
	// Record upload bandwidth
	err = tracker.RecordBandwidth(context.Background(), "user-1", BandwidthTypeUpload, 512*1024)
	if err != nil {
		t.Fatalf("Failed to record bandwidth: %v", err)
	}
	
	// Get usage
	usage, err := tracker.GetUsage(context.Background(), "user-1")
	if err != nil {
		t.Fatalf("Failed to get usage: %v", err)
	}
	
	if usage.BandwidthDownload != 1024*1024 {
		t.Errorf("Expected download bandwidth %d, got %d", 1024*1024, usage.BandwidthDownload)
	}
	
	if usage.BandwidthUpload != 512*1024 {
		t.Errorf("Expected upload bandwidth %d, got %d", 512*1024, usage.BandwidthUpload)
	}
}

func TestQuotaEnforcer_CheckQuota(t *testing.T) {
	enforcer := NewQuotaEnforcer()
	
	// Set quota for user
	enforcer.SetQuota("user-1", &Quota{
		MaxStorageBytes: 1024 * 1024, // 1MB
		MaxObjects:      10,
		MaxBandwidthMonth: 10 * 1024 * 1024, // 10MB
	})
	
	// Check quota - should pass
	allowed, err := enforcer.CheckQuota(context.Background(), "user-1", &Usage{
		StorageBytes: 512 * 1024, // 512KB
		ObjectCount:  5,
	})
	if err != nil {
		t.Fatalf("Failed to check quota: %v", err)
	}
	if !allowed {
		t.Error("Expected quota check to pass")
	}
	
	// Check quota - should fail (over storage)
	allowed, err = enforcer.CheckQuota(context.Background(), "user-1", &Usage{
		StorageBytes: 2 * 1024 * 1024, // 2MB
		ObjectCount:  5,
	})
	// When quota is exceeded, we expect an error
	if err == nil {
		t.Error("Expected error when quota exceeded")
	}
	if allowed {
		t.Error("Expected quota check to fail")
	}
}

func TestQuotaEnforcer_GetDefaultQuota(t *testing.T) {
	enforcer := NewQuotaEnforcer()
	
	quota := enforcer.GetDefaultQuota()
	
	// Free tier defaults
	expectedStorage := int64(5 * 1024 * 1024 * 1024) // 5GB
	if quota.MaxStorageBytes != expectedStorage {
		t.Errorf("Expected default storage %d, got %d", expectedStorage, quota.MaxStorageBytes)
	}
	
	if quota.MaxObjects != 10000 {
		t.Errorf("Expected default 10000 objects, got %d", quota.MaxObjects)
	}
}
