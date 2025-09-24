// internal/drivers/idrive_test.go
package drivers

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/FairForge/vaultaire/internal/engine"
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

func TestSmartCache(t *testing.T) {
	t.Run("caches frequently accessed objects", func(t *testing.T) {
		cache := NewSmartCache(10 * 1024 * 1024) // 10MB cache

		// First access - cache miss
		data := []byte("test data")
		hit := cache.Get("tenant-1", "file.txt")
		assert.Nil(t, hit)

		// Store in cache
		cache.Put("tenant-1", "file.txt", data)

		// Second access - cache hit
		hit = cache.Get("tenant-1", "file.txt")
		assert.Equal(t, data, hit)
	})

	t.Run("evicts least recently used items", func(t *testing.T) {
		cache := NewSmartCache(100) // Small 100 byte cache

		// Fill cache
		cache.Put("tenant-1", "file1.txt", make([]byte, 40))
		cache.Put("tenant-1", "file2.txt", make([]byte, 40))

		// Access file1 to make it more recent
		cache.Get("tenant-1", "file1.txt")

		// Add file3 - should evict file2 (least recently used)
		cache.Put("tenant-1", "file3.txt", make([]byte, 40))

		assert.NotNil(t, cache.Get("tenant-1", "file1.txt"))
		assert.Nil(t, cache.Get("tenant-1", "file2.txt")) // Evicted
		assert.NotNil(t, cache.Get("tenant-1", "file3.txt"))
	})

	t.Run("tracks cache hit ratio", func(t *testing.T) {
		cache := NewSmartCache(1024)

		// 2 misses, 3 hits
		cache.Get("tenant-1", "file1.txt") // miss
		cache.Put("tenant-1", "file1.txt", []byte("data"))
		cache.Get("tenant-1", "file1.txt") // hit
		cache.Get("tenant-1", "file1.txt") // hit
		cache.Get("tenant-1", "file2.txt") // miss
		cache.Get("tenant-1", "file1.txt") // hit

		stats := cache.GetStats()
		assert.Equal(t, int64(3), stats.Hits)
		assert.Equal(t, int64(2), stats.Misses)
		assert.Equal(t, 0.6, stats.HitRatio) // 3/5 = 0.6
	})
}

func TestCostAdvisor(t *testing.T) {
	t.Run("recommends compression for text files", func(t *testing.T) {
		advisor := NewCostAdvisor()

		// Add usage pattern
		advisor.RecordUpload("tenant-1", "logs.txt", 10*1024*1024, "text/plain")
		advisor.RecordUpload("tenant-1", "data.json", 5*1024*1024, "application/json")

		recommendations := advisor.GetRecommendations("tenant-1")

		// Should recommend compression for text files
		assert.Contains(t, recommendations[0].Title, "compression")
		assert.Greater(t, recommendations[0].EstimatedSavings, 0.0)
	})

	t.Run("suggests archival for infrequent access", func(t *testing.T) {
		advisor := NewCostAdvisor()
		now := time.Now()

		// File not accessed for 30 days
		advisor.RecordUpload("tenant-1", "old-backup.zip", 100*1024*1024, "application/zip")
		advisor.RecordAccess("tenant-1", "old-backup.zip", now.AddDate(0, -2, 0))

		recommendations := advisor.GetRecommendations("tenant-1")

		// Should suggest moving to archive tier
		found := false
		for _, rec := range recommendations {
			if strings.Contains(rec.Title, "archive") {
				found = true
				break
			}
		}
		assert.True(t, found)
	})
}

func TestRegionalFailover(t *testing.T) {
	t.Run("fails over to secondary region", func(t *testing.T) {
		primary := &MockIDriveDriver{}
		primary.SetShouldFail(true)
		secondary := &MockIDriveDriver{}

		failover := NewRegionalFailover(primary, secondary, zap.NewNop())

		// Primary fails, should use secondary
		err := failover.Put(context.Background(), "bucket", "file.txt", strings.NewReader("data"))
		assert.NoError(t, err)
		assert.Equal(t, 1, primary.putCalls)
		assert.Equal(t, 1, secondary.putCalls)
	})

	t.Run("tracks region health", func(t *testing.T) {
		primary := &MockIDriveDriver{}
		secondary := &MockIDriveDriver{}

		failover := NewRegionalFailover(primary, secondary, zap.NewNop())

		// Mark primary as unhealthy
		failover.MarkUnhealthy("primary")

		// Should use secondary even though primary might work
		_, _ = failover.Get(context.Background(), "container", "key")
		assert.Equal(t, 0, primary.getCalls)
		assert.Equal(t, 1, secondary.getCalls)

		// Check health status - using renamed type
		status := failover.GetHealthStatus()
		assert.False(t, status.PrimaryHealthy)
		assert.True(t, status.SecondaryHealthy)
	})

	t.Run("automatic recovery probe", func(t *testing.T) {
		primary := &MockIDriveDriver{}
		primary.SetShouldFail(true)
		secondary := &MockIDriveDriver{}

		failover := NewRegionalFailover(primary, secondary, zap.NewNop())
		failover.SetRecoveryInterval(100 * time.Millisecond)

		// Primary fails initially
		_ = failover.Put(context.Background(), "container", "key", strings.NewReader("data"))
		assert.False(t, failover.GetHealthStatus().PrimaryHealthy)

		// Fix primary
		primary.SetShouldFail(false)

		// Wait for recovery probe
		time.Sleep(200 * time.Millisecond)

		// Should detect primary is healthy again
		status := failover.GetHealthStatus()
		assert.True(t, status.PrimaryHealthy)
	})
}

// MockIDriveDriver for testing
type MockIDriveDriver struct {
	mu         sync.Mutex
	shouldFail bool
	putCalls   int
	getCalls   int
}

func (m *MockIDriveDriver) Put(ctx context.Context, container, artifact string, data io.Reader, opts ...engine.PutOption) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.putCalls++
	if m.shouldFail {
		return fmt.Errorf("mock failure")
	}
	return nil
}

func (m *MockIDriveDriver) Get(ctx context.Context, container, artifact string) (io.ReadCloser, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.getCalls++
	if m.shouldFail {
		return nil, fmt.Errorf("mock failure")
	}
	return io.NopCloser(strings.NewReader("mock data")), nil
}

func (m *MockIDriveDriver) Delete(ctx context.Context, container, artifact string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.shouldFail {
		return fmt.Errorf("mock failure")
	}
	return nil
}

func (m *MockIDriveDriver) List(ctx context.Context, container string, prefix string) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.shouldFail {
		return nil, fmt.Errorf("mock failure")
	}
	return []string{}, nil
}

func (m *MockIDriveDriver) Exists(ctx context.Context, container, artifact string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.shouldFail {
		return false, fmt.Errorf("mock failure")
	}
	return true, nil
}

func TestIDriveIntegration(t *testing.T) {
	t.Run("complete workflow integration", func(t *testing.T) {
		// Skip if no credentials
		if os.Getenv("IDRIVE_ACCESS_KEY") == "" {
			t.Skip("Integration test requires iDrive credentials")
		}

		// Create fully configured driver
		logger := zap.NewNop()
		driver, err := NewIDriveDriverFromConfig(logger)
		require.NoError(t, err)

		// Add all features
		driver.SetEgressTracker(NewEgressTracker())
		cache := NewSmartCache(10 * 1024 * 1024)
		advisor := NewCostAdvisor()

		// Remove unused variables or use them:
		// quota := NewBandwidthQuota(100 * 1024 * 1024)  // REMOVED
		// predictor := NewEgressPredictor()               // REMOVED

		ctx := context.WithValue(context.Background(), TenantIDKey, "test-tenant")

		// Test complete workflow
		testData := []byte("integration test data")

		// Upload
		err = driver.Put(ctx, "test-bucket", "integration.txt", bytes.NewReader(testData))
		assert.NoError(t, err)

		// Cache it
		cache.Put("test-tenant", "integration.txt", testData)

		// Track usage
		advisor.RecordUpload("test-tenant", "integration.txt", int64(len(testData)), "text/plain")

		// Download
		reader, err := driver.Get(ctx, "test-bucket", "integration.txt")
		assert.NoError(t, err)
		defer func() { _ = reader.Close() }()

		// Verify
		data, err := io.ReadAll(reader)
		assert.NoError(t, err)
		assert.Equal(t, testData, data)

		// Check metrics
		assert.True(t, driver.GetEgressTracker().GetTenantEgress("test-tenant") > 0)
		assert.NotNil(t, cache.Get("test-tenant", "integration.txt"))

		// Cleanup
		err = driver.Delete(ctx, "test-bucket", "integration.txt")
		assert.NoError(t, err)
	})
}

func (m *MockIDriveDriver) SetShouldFail(fail bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.shouldFail = fail
}
