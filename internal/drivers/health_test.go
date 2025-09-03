// internal/drivers/health_test.go
package drivers

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"go.uber.org/zap"
)

func TestHealthChecker(t *testing.T) {
	t.Run("reports healthy when all checks pass", func(t *testing.T) {
		// Arrange
		checker := NewHealthChecker(zap.NewNop())

		checker.RegisterCheck("database", func(ctx context.Context) error {
			return nil // Healthy
		})

		checker.RegisterCheck("storage", func(ctx context.Context) error {
			return nil // Healthy
		})

		// Act
		status := checker.Check(context.Background())

		// Assert
		assert.Equal(t, HealthStatusHealthy, status.Status)
		assert.Len(t, status.Checks, 2)
		assert.Equal(t, "healthy", status.Checks["database"])
		assert.Equal(t, "healthy", status.Checks["storage"])
	})

	t.Run("reports unhealthy when any check fails", func(t *testing.T) {
		// Arrange
		checker := NewHealthChecker(zap.NewNop())

		checker.RegisterCheck("database", func(ctx context.Context) error {
			return nil // Healthy
		})

		checker.RegisterCheck("storage", func(ctx context.Context) error {
			return errors.New("connection failed")
		})

		// Act
		status := checker.Check(context.Background())

		// Assert
		assert.Equal(t, HealthStatusUnhealthy, status.Status)
		assert.Equal(t, "healthy", status.Checks["database"])
		assert.Equal(t, "unhealthy: connection failed", status.Checks["storage"])
	})

	t.Run("respects check timeout", func(t *testing.T) {
		// Arrange
		checker := NewHealthChecker(
			zap.NewNop(),
			WithCheckTimeout(50*time.Millisecond),
		)

		checker.RegisterCheck("slow", func(ctx context.Context) error {
			time.Sleep(100 * time.Millisecond)
			return nil
		})

		// Act
		start := time.Now()
		status := checker.Check(context.Background())
		duration := time.Since(start)

		// Assert
		assert.Equal(t, HealthStatusUnhealthy, status.Status)
		assert.Contains(t, status.Checks["slow"], "timeout")
		assert.Less(t, duration, 75*time.Millisecond)
	})

	t.Run("runs checks in parallel", func(t *testing.T) {
		// Arrange
		checker := NewHealthChecker(zap.NewNop())

		for i := 0; i < 5; i++ {
			name := string(rune('a' + i))
			checker.RegisterCheck(name, func(ctx context.Context) error {
				time.Sleep(50 * time.Millisecond)
				return nil
			})
		}

		// Act
		start := time.Now()
		status := checker.Check(context.Background())
		duration := time.Since(start)

		// Assert - should take ~50ms not 250ms
		assert.Equal(t, HealthStatusHealthy, status.Status)
		assert.Less(t, duration, 100*time.Millisecond)
	})
}
