// internal/drivers/retry_test.go
package drivers

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRetryPolicy(t *testing.T) {
	t.Run("retries transient failures", func(t *testing.T) {
		// Arrange
		attempts := 0
		failingFunc := func() error {
			attempts++
			if attempts < 3 {
				return errors.New("transient error")
			}
			return nil
		}

		policy := NewRetryPolicy(
			WithMaxAttempts(5),
			WithInitialDelay(10*time.Millisecond),
			WithMaxDelay(100*time.Millisecond),
			WithJitter(true),
		)

		// Act
		err := policy.Execute(context.Background(), failingFunc)

		// Assert
		require.NoError(t, err)
		assert.Equal(t, 3, attempts, "Should succeed on third attempt")
	})

	t.Run("respects context cancellation", func(t *testing.T) {
		// Arrange
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()

		slowFunc := func() error {
			time.Sleep(100 * time.Millisecond)
			return errors.New("still failing")
		}

		policy := NewRetryPolicy(WithMaxAttempts(10))

		// Act
		err := policy.Execute(ctx, slowFunc)

		// Assert
		assert.ErrorIs(t, err, context.DeadlineExceeded)
	})

	t.Run("applies exponential backoff with jitter", func(t *testing.T) {
		// Arrange
		var delays []time.Duration
		start := time.Now()

		failingFunc := func() error {
			delays = append(delays, time.Since(start))
			start = time.Now()
			return errors.New("keep failing")
		}

		policy := NewRetryPolicy(
			WithMaxAttempts(4),
			WithInitialDelay(10*time.Millisecond),
			WithJitter(true),
		)

		// Act
		_ = policy.Execute(context.Background(), failingFunc)

		// Assert - delays should increase exponentially with jitter
		require.Len(t, delays, 4)

		// First attempt is immediate
		assert.Less(t, delays[0], 5*time.Millisecond)

		// Subsequent attempts have increasing delays with jitter
		for i := 1; i < len(delays)-1; i++ {
			// Each delay should be roughly 2x the previous (accounting for jitter)
			ratio := float64(delays[i+1]) / float64(delays[i])
			assert.InDelta(t, 2.0, ratio, 1.0, "Delays should roughly double")
		}
	})
}
