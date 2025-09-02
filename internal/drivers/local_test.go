package drivers

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"go.uber.org/zap"
)

// TestLocalDriver_HealthCheck tests health check functionality
func TestLocalDriver_HealthCheck(t *testing.T) {
	ctx := context.Background()

	t.Run("HealthyDriver", func(t *testing.T) {
		tmpDir := t.TempDir()
		driver := NewLocalDriver(tmpDir, zap.NewNop())

		err := driver.HealthCheck(ctx)
		assert.NoError(t, err, "Health check should pass for valid path")
	})

	t.Run("UnhealthyDriver", func(t *testing.T) {
		// Use non-existent path
		driver := NewLocalDriver("/nonexistent/path/12345", zap.NewNop())

		err := driver.HealthCheck(ctx)
		assert.Error(t, err, "Health check should fail for invalid path")
		assert.Contains(t, err.Error(), "health check failed")
	})
}
