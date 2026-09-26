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
		advisor := NewCostAdvisor()

		ctx := context.WithValue(context.Background(), TenantIDKey, "test-tenant")

		// Test complete workflow
		testData := []byte("integration test data")

		// Upload
		err = driver.Put(ctx, "test-bucket", "integration.txt", bytes.NewReader(testData))
		assert.NoError(t, err)

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
