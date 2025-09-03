// internal/drivers/circuit_breaker_test.go
package drivers

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCircuitBreaker(t *testing.T) {
	t.Run("opens after consecutive failures", func(t *testing.T) {
		// Arrange
		attempts := 0
		failingFunc := func() error {
			attempts++
			return errors.New("service unavailable")
		}

		cb := NewCircuitBreaker(
			WithFailureThreshold(3),
			WithTimeout(100*time.Millisecond),
			WithResetTimeout(200*time.Millisecond),
		)

		// Act - trigger failures to open circuit
		for i := 0; i < 3; i++ {
			_ = cb.Execute(context.Background(), failingFunc)
		}

		// Circuit should be open now
		err := cb.Execute(context.Background(), failingFunc)

		// Assert
		assert.ErrorIs(t, err, ErrCircuitOpen)
		assert.Equal(t, 3, attempts, "Should not call function when circuit is open")
	})

	t.Run("closes after reset timeout", func(t *testing.T) {
		// Arrange
		attempts := 0
		workingFunc := func() error {
			attempts++
			return nil
		}

		cb := NewCircuitBreaker(
			WithFailureThreshold(2),
			WithResetTimeout(100*time.Millisecond),
		)

		// Open the circuit
		for i := 0; i < 2; i++ {
			_ = cb.Execute(context.Background(), func() error {
				return errors.New("fail")
			})
		}

		// Circuit is open
		err := cb.Execute(context.Background(), workingFunc)
		require.ErrorIs(t, err, ErrCircuitOpen)
		require.Equal(t, 0, attempts)

		// Wait for reset timeout
		time.Sleep(150 * time.Millisecond)

		// Circuit should be half-open, allowing one attempt
		err = cb.Execute(context.Background(), workingFunc)

		// Assert
		require.NoError(t, err)
		assert.Equal(t, 1, attempts, "Should allow attempt after reset timeout")
	})

	t.Run("tracks success rate", func(t *testing.T) {
		// Arrange
		cb := NewCircuitBreaker(
			WithFailureThreshold(3),
			WithSuccessThreshold(2), // Need 2 successes to close
		)

		// Some successes
		for i := 0; i < 5; i++ {
			_ = cb.Execute(context.Background(), func() error { return nil })
		}

		// Then failures to open circuit
		for i := 0; i < 3; i++ {
			_ = cb.Execute(context.Background(), func() error {
				return errors.New("fail")
			})
		}

		// Assert
		assert.Equal(t, StateOpen, cb.State())
	})
}
