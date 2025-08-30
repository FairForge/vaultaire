package drivers

import (
	"context"
	"fmt"
	"io"
	"math"
	"sync"
	"time"
)

// Backend interface for fallback chain
type Backend interface {
	Get(ctx context.Context, container, artifact string) (io.ReadCloser, error)
	Put(ctx context.Context, container, artifact string, data io.Reader) error
	Delete(ctx context.Context, container, artifact string) error
	List(ctx context.Context, container, prefix string) ([]string, error)
}

// ExponentialBackoff implements retry with exponential backoff
type ExponentialBackoff struct {
	BaseDelay  time.Duration
	MaxDelay   time.Duration
	MaxRetries int
}

func (e *ExponentialBackoff) Retry(ctx context.Context, fn func() error) error {
	var lastErr error
	for i := 0; i < e.MaxRetries; i++ {
		if err := fn(); err == nil {
			return nil
		} else {
			lastErr = err
		}

		delay := time.Duration(math.Min(
			float64(e.BaseDelay)*math.Pow(2, float64(i)),
			float64(e.MaxDelay),
		))

		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return fmt.Errorf("max retries exceeded: %w", lastErr)
}

// CircuitBreaker prevents cascading failures
type CircuitBreaker struct {
	mu           sync.Mutex
	failures     int
	lastFailTime time.Time
	state        string // "closed", "open", "half-open"
	threshold    int
	timeout      time.Duration
}

func NewCircuitBreaker(threshold int, timeout time.Duration) *CircuitBreaker {
	return &CircuitBreaker{
		state:     "closed",
		threshold: threshold,
		timeout:   timeout,
	}
}

func (cb *CircuitBreaker) Call(fn func() error) error {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	// Check if circuit should be half-open
	if cb.state == "open" && time.Since(cb.lastFailTime) > cb.timeout {
		cb.state = "half-open"
		cb.failures = 0
	}

	if cb.state == "open" {
		return fmt.Errorf("circuit breaker is open")
	}

	err := fn()
	if err != nil {
		cb.failures++
		cb.lastFailTime = time.Now()

		if cb.failures >= cb.threshold {
			cb.state = "open"
		}
		return err
	}

	// Success - reset if half-open
	if cb.state == "half-open" {
		cb.state = "closed"
		cb.failures = 0
	}

	return nil
}

// RequestQueue manages request queuing with priorities
type RequestQueue struct {
	queue   chan Request
	workers int
}

type Request struct {
	Fn       func() error
	Priority int
	Result   chan error
}

func NewRequestQueue(size, workers int) *RequestQueue {
	q := &RequestQueue{
		queue:   make(chan Request, size),
		workers: workers,
	}

	for i := 0; i < workers; i++ {
		go q.worker()
	}

	return q
}

func (q *RequestQueue) worker() {
	for req := range q.queue {
		req.Result <- req.Fn()
	}
}

func (q *RequestQueue) Submit(fn func() error, priority int) error {
	result := make(chan error, 1)
	q.queue <- Request{
		Fn:       fn,
		Priority: priority,
		Result:   result,
	}
	return <-result
}

// FallbackChain tries multiple backends in order
type FallbackChain struct {
	backends []Backend
}

func NewFallbackChain(backends ...Backend) *FallbackChain {
	return &FallbackChain{backends: backends}
}

func (f *FallbackChain) Get(ctx context.Context, container, artifact string) (io.ReadCloser, error) {
	var lastErr error
	for _, backend := range f.backends {
		result, err := backend.Get(ctx, container, artifact)
		if err == nil {
			return result, nil
		}
		lastErr = err
	}
	return nil, fmt.Errorf("all backends failed: %w", lastErr)
}
