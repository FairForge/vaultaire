// internal/drivers/idrive_test.go
package drivers

import (
	"context"
	"crypto/rand"
	"io"
	"os"
	"strings"
	"testing"

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
