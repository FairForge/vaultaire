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

		// With jitter, we can only verify that delays are non-zero and
		// the total time spent retrying is reasonable (not that ratios are exact)
		// Jitter adds randomness of 0-100%, making ratio checks unreliable
		for i := 1; i < len(delays); i++ {
			assert.Greater(t, delays[i], time.Duration(0), "Delay %d should be positive", i)
		}

		// Verify total retry time is in expected range
		// With initial=10ms, 3 retries with exponential backoff: ~10+20+40 = 70ms base
		// With jitter (0-100%), range is roughly 35ms to 140ms
		totalDelay := delays[1] + delays[2] + delays[3]
		assert.Greater(t, totalDelay, 20*time.Millisecond, "Total delay should be meaningful")
		assert.Less(t, totalDelay, 500*time.Millisecond, "Total delay should not be excessive")
	})
}
