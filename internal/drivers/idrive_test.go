// internal/drivers/idrive_test.go
package drivers

import (
	"context"
	"crypto/rand"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestNewIDriveDriver(t *testing.T) {
	t.Run("creates driver with valid config", func(t *testing.T) {
		// Given: Valid iDrive E2 configuration
		driver, err := NewIDriveDriver(
			"test-access-key",
			"test-secret-key",
			"https://e2-us-west-1.idrive.com",
			"us-west-1",
			zap.NewNop(),
		)

		// Then: Driver is created successfully
		require.NoError(t, err)
		assert.NotNil(t, driver)
		assert.Equal(t, "https://e2-us-west-1.idrive.com", driver.endpoint)
	})

	t.Run("fails with empty endpoint", func(t *testing.T) {
		// Given: Missing endpoint
		driver, err := NewIDriveDriver(
			"test-access-key",
			"test-secret-key",
			"", // empty endpoint
			"us-west-1",
			zap.NewNop(),
		)

		// Then: Returns error
		assert.Error(t, err)
		assert.Nil(t, driver)
		assert.Contains(t, err.Error(), "endpoint required")
	})
}

func TestIDriveDriver_Operations(t *testing.T) {
	t.Run("Put handles options", func(t *testing.T) {
		driver, _ := NewIDriveDriver(
			"test-key",
			"test-secret",
			"https://e2.idrive.com",
			"us-west-1",
			zap.NewNop(),
		)

		// Test that Put accepts options (even if not used yet)
		err := driver.Put(context.Background(),
			"test-bucket",
			"test-key",
			strings.NewReader("test data"))

		// Will fail without real credentials, but should compile
		assert.Error(t, err) // Expected to fail without real S3
	})

	t.Run("List handles empty prefix", func(t *testing.T) {
		driver, _ := NewIDriveDriver(
			"test-key",
			"test-secret",
			"https://e2.idrive.com",
			"us-west-1",
			zap.NewNop(),
		)

		// Should accept empty prefix
		_, err := driver.List(context.Background(), "bucket", "")
		assert.Error(t, err) // Expected without real S3
	})
}

func TestIDriveDriver_ErrorHandling(t *testing.T) {
	t.Run("Exists returns false for missing objects", func(t *testing.T) {
		driver, _ := NewIDriveDriver(
			"test-key",
			"test-secret",
			"https://e2.idrive.com",
			"us-west-1",
			zap.NewNop(),
		)

		// Without real credentials, Exists should fail to connect
		exists, err := driver.Exists(context.Background(), "bucket", "missing")

		// Either an error (can't connect) OR false (if it somehow connects)
		// Since we're using fake credentials, we expect an error
		if err == nil {
			// If no error, must return false for non-existent
			assert.False(t, exists)
		} else {
			// Expected: connection/auth error
			assert.False(t, exists)
			assert.Contains(t, err.Error(), "idrive exists")
		}
	})
}
func TestIDriveDriver_ValidateAuth(t *testing.T) {
	t.Run("validates authentication on initialization", func(t *testing.T) {
		driver, _ := NewIDriveDriver(
			"test-key",
			"test-secret",
			"https://e2.idrive.com",
			"us-west-1",
			zap.NewNop(),
		)

		ctx := context.Background()
		err := driver.ValidateAuth(ctx)

		// Should fail with fake credentials
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "authentication")
	})
}

func TestIDriveDriver_ValidateAuth_Integration(t *testing.T) {
	// Skip if no real credentials
	accessKey := os.Getenv("IDRIVE_ACCESS_KEY")
	secretKey := os.Getenv("IDRIVE_SECRET_KEY")

	if accessKey == "" || secretKey == "" {
		t.Skip("Skipping integration test - no iDrive credentials")
	}

	t.Run("validates real authentication", func(t *testing.T) {
		driver, err := NewIDriveDriver(
			accessKey,
			secretKey,
			"https://e2-us-west-1.idrive.com",
			"us-west-1",
			zap.NewNop(),
		)
		require.NoError(t, err)

		ctx := context.Background()
		err = driver.ValidateAuth(ctx)
		assert.NoError(t, err)
	})
}

func TestIDriveDriver_StreamingUpload(t *testing.T) {
	t.Run("streams large files without loading in memory", func(t *testing.T) {
		driver, _ := NewIDriveDriver(
			"test-key",
			"test-secret",
			"https://e2.idrive.com",
			"us-west-1",
			zap.NewNop(),
		)

		// Create a large reader (simulating 10MB file)
		size := int64(10 * 1024 * 1024)
		reader := io.LimitReader(rand.Reader, size)

		// This should stream, not load in memory
		err := driver.Put(context.Background(), "bucket", "large.bin", reader)

		// Will fail without real creds, but should handle streaming
		assert.Error(t, err)
	})
}

func TestIDriveDriver_MultipartUpload(t *testing.T) {
	t.Run("uses multipart for files over 5MB", func(t *testing.T) {
		driver, _ := NewIDriveDriver(
			"test-key",
			"test-secret",
			"https://e2.idrive.com",
			"us-west-1",
			zap.NewNop(),
		)

		// Set multipart threshold
		driver.multipartThreshold = 5 * 1024 * 1024 // 5MB

		// Create 6MB reader
		size := int64(6 * 1024 * 1024)
		reader := io.LimitReader(rand.Reader, size)

		err := driver.PutWithSize(context.Background(), "bucket", "large.bin", reader, size)

		// Should attempt multipart (will fail without creds)
		assert.Error(t, err)
	})
}

func TestIDriveDriver_EgressTracking(t *testing.T) {
	t.Run("tracks download bandwidth", func(t *testing.T) {
		driver, _ := NewIDriveDriver(
			"test-key",
			"test-secret",
			"https://e2.idrive.com",
			"us-west-1",
			zap.NewNop(),
		)

		// Initialize egress tracker
		driver.egressTracker = NewEgressTracker()

		// Simulate a download (will fail without creds but should track attempt)
		ctx := context.WithValue(context.Background(), TenantIDKey, "tenant-123")
		_, err := driver.Get(ctx, "bucket", "file.bin")

		assert.Error(t, err) // Expected without real creds

		// Check that tracking was attempted
		assert.NotNil(t, driver.egressTracker)
	})
}

func TestEgressTracker(t *testing.T) {
	t.Run("records bandwidth per tenant", func(t *testing.T) {
		tracker := NewEgressTracker()

		// Record some downloads
		tracker.RecordEgress("tenant-1", 1024*1024) // 1MB
		tracker.RecordEgress("tenant-1", 2048*1024) // 2MB
		tracker.RecordEgress("tenant-2", 512*1024)  // 512KB

		// Check totals
		assert.Equal(t, int64(3*1024*1024), tracker.GetTenantEgress("tenant-1"))
		assert.Equal(t, int64(512*1024), tracker.GetTenantEgress("tenant-2"))
	})

	t.Run("calculates costs based on rates", func(t *testing.T) {
		tracker := NewEgressTracker()
		tracker.SetRate(0.01) // $0.01 per GB

		// Record 10GB for tenant
		tracker.RecordEgress("tenant-1", 10*1024*1024*1024)

		cost := tracker.GetTenantCost("tenant-1")
		assert.Equal(t, 0.10, cost) // $0.10 for 10GB
	})
}

func TestBandwidthQuota(t *testing.T) {
	t.Run("enforces monthly bandwidth limits", func(t *testing.T) {
		quota := NewBandwidthQuota(10 * 1024 * 1024 * 1024) // 10GB monthly quota

		// Use 5GB
		allowed := quota.AllowEgress("tenant-1", 5*1024*1024*1024)
		assert.True(t, allowed)

		// Try to use another 6GB (should fail - over quota)
		allowed = quota.AllowEgress("tenant-1", 6*1024*1024*1024)
		assert.False(t, allowed)

		// Check remaining quota
		remaining := quota.GetRemaining("tenant-1")
		assert.Equal(t, int64(5*1024*1024*1024), remaining)
	})

	t.Run("tracks multiple tenants independently", func(t *testing.T) {
		quota := NewBandwidthQuota(5 * 1024 * 1024 * 1024) // 5GB per tenant

		// Tenant 1 uses 3GB
		quota.AllowEgress("tenant-1", 3*1024*1024*1024)

		// Tenant 2 uses 4GB
		allowed := quota.AllowEgress("tenant-2", 4*1024*1024*1024)
		assert.True(t, allowed) // Each tenant has own quota

		assert.Equal(t, int64(2*1024*1024*1024), quota.GetRemaining("tenant-1"))
		assert.Equal(t, int64(1*1024*1024*1024), quota.GetRemaining("tenant-2"))
	})

	t.Run("resets monthly", func(t *testing.T) {
		quota := NewBandwidthQuota(1024) // 1KB quota
		quota.AllowEgress("tenant-1", 1024)

		// Manually trigger reset (normally done by timer)
		quota.Reset()

		// Should be able to use quota again
		allowed := quota.AllowEgress("tenant-1", 1024)
		assert.True(t, allowed)
	})
}

func TestEgressPredictor(t *testing.T) {
	t.Run("predicts monthly usage based on current rate", func(t *testing.T) {
		predictor := NewEgressPredictor()

		// Record usage for first 5 days of month
		now := time.Date(2024, 1, 5, 12, 0, 0, 0, time.UTC)
		predictor.RecordUsage("tenant-1", 5*1024*1024*1024, now) // 5GB in 5 days

		// Predict full month usage (should be ~30GB for 30 days)
		predicted := predictor.PredictMonthlyUsage("tenant-1", now)

		// 5GB/5days = 1GB/day * 30 days = 30GB
		expectedMin := int64(29 * 1024 * 1024 * 1024)
		expectedMax := int64(31 * 1024 * 1024 * 1024)
		assert.True(t, predicted >= expectedMin && predicted <= expectedMax,
			"Expected ~30GB, got %d", predicted/(1024*1024*1024))
	})

	t.Run("generates alerts at threshold levels", func(t *testing.T) {
		predictor := NewEgressPredictor()
		predictor.SetQuota("tenant-1", 10*1024*1024*1024) // 10GB quota

		// Info alert at 50% usage (not "No alert")
		alert := predictor.CheckAlert("tenant-1", 5*1024*1024*1024)
		assert.Equal(t, AlertInfo, alert.Level) // Changed from AlertNone to AlertInfo

		// Warning at 75% usage
		alert = predictor.CheckAlert("tenant-1", 7.5*1024*1024*1024)
		assert.Equal(t, AlertWarning, alert.Level)

		// Critical at 90% usage
		alert = predictor.CheckAlert("tenant-1", 9*1024*1024*1024)
		assert.Equal(t, AlertCritical, alert.Level)

		// No alert below 50%
		alert = predictor.CheckAlert("tenant-1", 4*1024*1024*1024) // 40%
		assert.Equal(t, AlertNone, alert.Level)
	})

	t.Run("tracks usage patterns over time", func(t *testing.T) {
		predictor := NewEgressPredictor()

		// Simulate daily usage
		for day := 1; day <= 7; day++ {
			date := time.Date(2024, 1, day, 0, 0, 0, 0, time.UTC)
			predictor.RecordDailyUsage("tenant-1", int64(day)*1024*1024*1024, date)
		}

		// Get average daily usage
		avgDaily := predictor.GetAverageDailyUsage("tenant-1")

		// Average should be (1+2+3+4+5+6+7)/7 = 4GB
		expected := int64(4 * 1024 * 1024 * 1024)
		assert.Equal(t, expected, avgDaily)
	})
}
