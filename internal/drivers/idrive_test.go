// internal/drivers/idrive_test.go
package drivers

import (
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
