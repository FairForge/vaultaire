// internal/drivers/health.go
package drivers

import (
	"context"
	"fmt"
	"sync"
	"time"

	"go.uber.org/zap"
)

// HealthStatus represents the overall system health
type HealthStatus string

const (
	HealthStatusHealthy   HealthStatus = "healthy"
	HealthStatusUnhealthy HealthStatus = "unhealthy"
	HealthStatusDegraded  HealthStatus = "degraded"
)

// HealthCheck is a function that checks a component's health
type HealthCheck func(ctx context.Context) error

// HealthReport contains the overall health status
type HealthReport struct {
	Status    HealthStatus      `json:"status"`
	Checks    map[string]string `json:"checks"`
	Timestamp time.Time         `json:"timestamp"`
}

// HealthChecker manages health checks
type HealthChecker struct {
	mu      sync.RWMutex
	checks  map[string]HealthCheck
	timeout time.Duration
	logger  *zap.Logger
}

// HealthOption configures the health checker
type HealthOption func(*HealthChecker)

// WithCheckTimeout sets the timeout for each check
func WithCheckTimeout(d time.Duration) HealthOption {
	return func(h *HealthChecker) {
		h.timeout = d
	}
}

// NewHealthChecker creates a new health checker
func NewHealthChecker(logger *zap.Logger, opts ...HealthOption) *HealthChecker {
	h := &HealthChecker{
		checks:  make(map[string]HealthCheck),
		timeout: 5 * time.Second,
		logger:  logger,
	}

	for _, opt := range opts {
		opt(h)
	}

	return h
}

// RegisterCheck adds a health check
func (h *HealthChecker) RegisterCheck(name string, check HealthCheck) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.checks[name] = check
}

// Check runs all health checks
func (h *HealthChecker) Check(ctx context.Context) *HealthReport {
	h.mu.RLock()
	checks := make(map[string]HealthCheck, len(h.checks))
	for name, check := range h.checks {
		checks[name] = check
	}
	h.mu.RUnlock()

	report := &HealthReport{
		Status:    HealthStatusHealthy,
		Checks:    make(map[string]string),
		Timestamp: time.Now(),
	}

	// Run checks in parallel
	var wg sync.WaitGroup
	var mu sync.Mutex

	for name, check := range checks {
		wg.Add(1)
		go func(name string, check HealthCheck) {
			defer wg.Done()

			// Create timeout context for this check
			checkCtx, cancel := context.WithTimeout(ctx, h.timeout)
			defer cancel()

			// Run check in goroutine to handle timeout
			done := make(chan error, 1)
			go func() {
				done <- check(checkCtx)
			}()

			// Wait for check or timeout
			var err error
			select {
			case err = <-done:
				// Check completed
			case <-checkCtx.Done():
				// Timeout occurred
				err = fmt.Errorf("timeout after %v", h.timeout)
			}

			// Update report
			mu.Lock()
			defer mu.Unlock()

			if err != nil {
				report.Checks[name] = fmt.Sprintf("unhealthy: %v", err)
				report.Status = HealthStatusUnhealthy
			} else {
				report.Checks[name] = "healthy"
			}
		}(name, check)
	}

	wg.Wait()
	return report
}

// LivenessProbe checks if the service is alive
func (h *HealthChecker) LivenessProbe() error {
	// Basic liveness - we're responding
	return nil
}

// ReadinessProbe checks if the service is ready to serve traffic
func (h *HealthChecker) ReadinessProbe() error {
	report := h.Check(context.Background())
	if report.Status != HealthStatusHealthy {
		return fmt.Errorf("service not ready: %s", report.Status)
	}
	return nil
}
