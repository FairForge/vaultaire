package engine

import (
	"context"
	"fmt"
	"sync"
	"time"

	"go.uber.org/zap"
)

type CircuitState int

const (
	StateClosed   CircuitState = iota // healthy — requests flow through
	StateOpen                         // broken — requests are rejected
	StateHalfOpen                     // probing — one request allowed to test recovery
)

func (s CircuitState) String() string {
	switch s {
	case StateClosed:
		return "closed"
	case StateOpen:
		return "open"
	case StateHalfOpen:
		return "half-open"
	default:
		return "unknown"
	}
}

const (
	failureThreshold = 5
	failureWindow    = 60 * time.Second
	openDuration     = 30 * time.Second
)

type BackendCircuitBreaker struct {
	mu           sync.Mutex
	state        CircuitState
	failures     []time.Time
	lastOpenedAt time.Time
}

func NewBackendCircuitBreaker() *BackendCircuitBreaker {
	return &BackendCircuitBreaker{
		state: StateClosed,
	}
}

func (b *BackendCircuitBreaker) Allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	switch b.state {
	case StateClosed:
		return true
	case StateOpen:
		if time.Since(b.lastOpenedAt) >= openDuration {
			b.state = StateHalfOpen
			return true
		}
		return false
	case StateHalfOpen:
		return true
	default:
		return true
	}
}

func (b *BackendCircuitBreaker) RecordSuccess() {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.state = StateClosed
	b.failures = nil
}

func (b *BackendCircuitBreaker) RecordFailure() {
	b.mu.Lock()
	defer b.mu.Unlock()

	now := time.Now()
	cutoff := now.Add(-failureWindow)
	var recent []time.Time
	for _, t := range b.failures {
		if t.After(cutoff) {
			recent = append(recent, t)
		}
	}
	recent = append(recent, now)
	b.failures = recent

	if len(recent) >= failureThreshold {
		b.state = StateOpen
		b.lastOpenedAt = now
	}
}

func (b *BackendCircuitBreaker) State() CircuitState {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.state == StateOpen && time.Since(b.lastOpenedAt) >= openDuration {
		b.state = StateHalfOpen
	}
	return b.state
}

type FailoverManager struct {
	mu       sync.RWMutex
	breakers map[string]*BackendCircuitBreaker
	logger   *zap.Logger
}

func NewFailoverManager(logger *zap.Logger) *FailoverManager {
	return &FailoverManager{
		breakers: make(map[string]*BackendCircuitBreaker),
		logger:   logger,
	}
}

func (f *FailoverManager) Register(backend string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, exists := f.breakers[backend]; !exists {
		f.breakers[backend] = NewBackendCircuitBreaker()
	}
}

func (f *FailoverManager) Execute(ctx context.Context, backends []string, fn func(driverName string) error) (string, error) {
	var lastErr error

	for _, backend := range backends {
		f.mu.RLock()
		breaker, exists := f.breakers[backend]
		f.mu.RUnlock()

		if !exists {
			continue
		}

		if !breaker.Allow() {
			f.logger.Debug("circuit breaker open, skipping backend",
				zap.String("backend", backend))
			continue
		}

		if err := fn(backend); err != nil {
			breaker.RecordFailure()
			lastErr = err
			f.logger.Warn("backend failed, trying next",
				zap.String("backend", backend),
				zap.Error(err))
			continue
		}

		breaker.RecordSuccess()
		return backend, nil
	}

	if lastErr != nil {
		return "", fmt.Errorf("all backends failed: %w", lastErr)
	}
	return "", ErrAllBackendsUnavailable
}

func (f *FailoverManager) GetStatus(backend string) string {
	f.mu.RLock()
	breaker, exists := f.breakers[backend]
	f.mu.RUnlock()
	if !exists {
		return "unknown"
	}
	return breaker.State().String()
}

func (f *FailoverManager) GetAllStatuses() map[string]string {
	f.mu.RLock()
	defer f.mu.RUnlock()

	statuses := make(map[string]string, len(f.breakers))
	for name, breaker := range f.breakers {
		statuses[name] = breaker.State().String()
	}
	return statuses
}
