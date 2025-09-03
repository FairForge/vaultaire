// internal/drivers/circuit_breaker.go
package drivers

import (
	"context"
	"errors"
	"sync"
	"time"

	"go.uber.org/zap"
)

// ErrCircuitOpen is returned when the circuit breaker is open
var ErrCircuitOpen = errors.New("circuit breaker is open")

// State represents the circuit breaker state
type State int

const (
	StateClosed   State = iota // Normal operation
	StateOpen                  // Failing, requests blocked
	StateHalfOpen              // Testing if service recovered
)

// CircuitBreaker protects against cascading failures
type CircuitBreaker struct {
	mu sync.RWMutex

	// Configuration
	failureThreshold int
	successThreshold int
	timeout          time.Duration
	resetTimeout     time.Duration

	// State
	state           State
	failures        int
	successes       int
	lastFailTime    time.Time
	lastAttemptTime time.Time

	logger *zap.Logger
}

// CircuitOption configures the circuit breaker
type CircuitOption func(*CircuitBreaker)

// WithFailureThreshold sets failures before opening
func WithFailureThreshold(n int) CircuitOption {
	return func(cb *CircuitBreaker) {
		cb.failureThreshold = n
	}
}

// WithSuccessThreshold sets successes before closing
func WithSuccessThreshold(n int) CircuitOption {
	return func(cb *CircuitBreaker) {
		cb.successThreshold = n
	}
}

// WithTimeout sets operation timeout
func WithTimeout(d time.Duration) CircuitOption {
	return func(cb *CircuitBreaker) {
		cb.timeout = d
	}
}

// WithResetTimeout sets time before trying again
func WithResetTimeout(d time.Duration) CircuitOption {
	return func(cb *CircuitBreaker) {
		cb.resetTimeout = d
	}
}

// WithCircuitLogger adds logging
func WithCircuitLogger(logger *zap.Logger) CircuitOption {
	return func(cb *CircuitBreaker) {
		cb.logger = logger
	}
}

// NewCircuitBreaker creates a new circuit breaker
func NewCircuitBreaker(opts ...CircuitOption) *CircuitBreaker {
	cb := &CircuitBreaker{
		failureThreshold: 5,
		successThreshold: 1,
		timeout:          10 * time.Second,
		resetTimeout:     60 * time.Second,
		state:            StateClosed,
		logger:           zap.NewNop(),
	}

	for _, opt := range opts {
		opt(cb)
	}

	return cb
}

// Execute runs a function with circuit breaker protection
func (cb *CircuitBreaker) Execute(ctx context.Context, fn func() error) error {
	cb.mu.Lock()

	// Check if we should transition from Open to Half-Open
	if cb.state == StateOpen {
		if time.Since(cb.lastFailTime) > cb.resetTimeout {
			cb.state = StateHalfOpen
			cb.failures = 0
			cb.successes = 0
			cb.logger.Info("circuit breaker half-open")
		} else {
			cb.mu.Unlock()
			return ErrCircuitOpen
		}
	}

	cb.lastAttemptTime = time.Now()
	cb.mu.Unlock()

	// Execute the function with timeout
	done := make(chan error, 1)
	go func() {
		done <- fn()
	}()

	select {
	case err := <-done:
		cb.recordResult(err)
		return err
	case <-time.After(cb.timeout):
		cb.recordResult(errors.New("timeout"))
		return errors.New("operation timeout")
	case <-ctx.Done():
		return ctx.Err()
	}
}

// recordResult updates circuit breaker state based on result
func (cb *CircuitBreaker) recordResult(err error) {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	if err != nil {
		cb.failures++
		cb.successes = 0
		cb.lastFailTime = time.Now()

		if cb.failures >= cb.failureThreshold {
			cb.state = StateOpen
			cb.logger.Error("circuit breaker opened",
				zap.Int("failures", cb.failures),
				zap.Error(err))
		}
	} else {
		cb.successes++
		cb.failures = 0

		if cb.state == StateHalfOpen && cb.successes >= cb.successThreshold {
			cb.state = StateClosed
			cb.logger.Info("circuit breaker closed",
				zap.Int("successes", cb.successes))
		}
	}
}

// State returns the current circuit breaker state
func (cb *CircuitBreaker) State() State {
	cb.mu.RLock()
	defer cb.mu.RUnlock()
	return cb.state
}

// Reset manually resets the circuit breaker
func (cb *CircuitBreaker) Reset() {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	cb.state = StateClosed
	cb.failures = 0
	cb.successes = 0
	cb.logger.Info("circuit breaker reset")
}
