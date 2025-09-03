// internal/drivers/retry.go
package drivers

import (
	"context"
	"io"
	"math"
	"math/rand"
	"time"

	"github.com/FairForge/vaultaire/internal/engine"
	"go.uber.org/zap"
)

// RetryPolicy defines how to retry failed operations
type RetryPolicy struct {
	maxAttempts  int
	initialDelay time.Duration
	maxDelay     time.Duration
	multiplier   float64
	jitter       bool
	logger       *zap.Logger
}

// RetryOption configures retry behavior
type RetryOption func(*RetryPolicy)

// WithMaxAttempts sets maximum retry attempts
func WithMaxAttempts(n int) RetryOption {
	return func(p *RetryPolicy) {
		p.maxAttempts = n
	}
}

// WithInitialDelay sets the initial retry delay
func WithInitialDelay(d time.Duration) RetryOption {
	return func(p *RetryPolicy) {
		p.initialDelay = d
	}
}

// WithMaxDelay sets the maximum retry delay
func WithMaxDelay(d time.Duration) RetryOption {
	return func(p *RetryPolicy) {
		p.maxDelay = d
	}
}

// WithJitter enables jitter to prevent thundering herd
func WithJitter(enabled bool) RetryOption {
	return func(p *RetryPolicy) {
		p.jitter = enabled
	}
}

// WithLogger adds logging to retry attempts
func WithLogger(logger *zap.Logger) RetryOption {
	return func(p *RetryPolicy) {
		p.logger = logger
	}
}

// NewRetryPolicy creates a new retry policy
func NewRetryPolicy(opts ...RetryOption) *RetryPolicy {
	p := &RetryPolicy{
		maxAttempts:  3,
		initialDelay: 100 * time.Millisecond,
		maxDelay:     30 * time.Second,
		multiplier:   2.0,
		jitter:       true,
		logger:       zap.NewNop(),
	}

	for _, opt := range opts {
		opt(p)
	}

	return p
}

// Execute runs a function with retry logic
func (p *RetryPolicy) Execute(ctx context.Context, fn func() error) error {
	var lastErr error

	for attempt := 0; attempt < p.maxAttempts; attempt++ {
		// Check context before attempting
		if ctx.Err() != nil {
			return ctx.Err()
		}

		// Try the operation
		if err := fn(); err == nil {
			if attempt > 0 {
				p.logger.Debug("operation succeeded after retry",
					zap.Int("attempt", attempt+1),
					zap.Int("maxAttempts", p.maxAttempts))
			}
			return nil
		} else {
			lastErr = err
		}

		// Don't delay after the last attempt
		if attempt == p.maxAttempts-1 {
			break
		}

		// Calculate delay with exponential backoff
		delay := p.calculateDelay(attempt)

		p.logger.Debug("operation failed, retrying",
			zap.Error(lastErr),
			zap.Int("attempt", attempt+1),
			zap.Int("maxAttempts", p.maxAttempts),
			zap.Duration("delay", delay))

		// Wait with context cancellation support
		select {
		case <-time.After(delay):
			// Continue to next attempt
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	p.logger.Error("operation failed after all retries",
		zap.Error(lastErr),
		zap.Int("attempts", p.maxAttempts))

	return lastErr
}

// calculateDelay computes the delay for the given attempt
func (p *RetryPolicy) calculateDelay(attempt int) time.Duration {
	// Exponential backoff: delay = initial * (multiplier ^ attempt)
	delay := float64(p.initialDelay) * math.Pow(p.multiplier, float64(attempt))

	// Cap at max delay
	if delay > float64(p.maxDelay) {
		delay = float64(p.maxDelay)
	}

	// Apply jitter to prevent thundering herd
	if p.jitter {
		// Jitter between 0.5x and 1.5x the delay
		jitter := 0.5 + rand.Float64()
		delay = delay * jitter
	}

	return time.Duration(delay)
}

// RetryableDriver wraps a driver with retry logic
type RetryableDriver struct {
	driver Driver
	policy *RetryPolicy
}

// NewRetryableDriver creates a driver with retry capability
func NewRetryableDriver(driver Driver, policy *RetryPolicy) *RetryableDriver {
	return &RetryableDriver{
		driver: driver,
		policy: policy,
	}
}

// Get with retry
func (r *RetryableDriver) Get(ctx context.Context, container, artifact string) (io.ReadCloser, error) {
	var result io.ReadCloser
	err := r.policy.Execute(ctx, func() error {
		var err error
		result, err = r.driver.Get(ctx, container, artifact)
		return err
	})
	return result, err
}

// Put with retry (be careful - may not be idempotent!)
func (r *RetryableDriver) Put(ctx context.Context, container, artifact string,
	data io.Reader, opts ...engine.PutOption) error {
	// Note: PUT operations may not be safe to retry if partially completed
	// Consider using resumable uploads instead
	return r.policy.Execute(ctx, func() error {
		return r.driver.Put(ctx, container, artifact, data, opts...)
	})
}

// Delete with retry (idempotent)
func (r *RetryableDriver) Delete(ctx context.Context, container, artifact string) error {
	return r.policy.Execute(ctx, func() error {
		return r.driver.Delete(ctx, container, artifact)
	})
}

// List with retry
func (r *RetryableDriver) List(ctx context.Context, container, prefix string) ([]string, error) {
	var result []string
	err := r.policy.Execute(ctx, func() error {
		var err error
		result, err = r.driver.List(ctx, container, prefix)
		return err
	})
	return result, err
}

// Exists with retry
func (r *RetryableDriver) Exists(ctx context.Context, container, artifact string) (bool, error) {
	var result bool
	err := r.policy.Execute(ctx, func() error {
		var err error
		result, err = r.driver.Exists(ctx, container, artifact)
		return err
	})
	return result, err
}
