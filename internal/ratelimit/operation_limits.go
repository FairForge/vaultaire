package ratelimit

import (
	"context"
	"sync"

	"golang.org/x/time/rate"
)

// OperationLimiter manages rate limits per operation type
type OperationLimiter struct {
	mu       sync.RWMutex
	limits   map[string]*OperationConfig // operation -> config
	limiters map[string]*rate.Limiter    // tenantID:operation -> limiter
}

// OperationConfig defines limits for an operation
type OperationConfig struct {
	RatePerSecond int
	Burst         int
}

// NewOperationLimiter creates a new operation-aware limiter
func NewOperationLimiter() *OperationLimiter {
	return &OperationLimiter{
		limits:   make(map[string]*OperationConfig),
		limiters: make(map[string]*rate.Limiter),
	}
}

// SetLimit configures rate limit for an operation
func (ol *OperationLimiter) SetLimit(operation string, ratePerSecond, burst int) {
	ol.mu.Lock()
	defer ol.mu.Unlock()

	ol.limits[operation] = &OperationConfig{
		RatePerSecond: ratePerSecond,
		Burst:         burst,
	}
}

// Allow checks if a tenant can perform an operation
func (ol *OperationLimiter) Allow(tenantID, operation string) bool {
	ol.mu.Lock()
	defer ol.mu.Unlock()

	// Get operation config
	config, exists := ol.limits[operation]
	if !exists {
		// No limit configured for this operation
		return true
	}

	// Get or create limiter for this tenant+operation
	key := tenantID + ":" + operation
	limiter, exists := ol.limiters[key]
	if !exists {
		limiter = rate.NewLimiter(rate.Limit(config.RatePerSecond), config.Burst)
		ol.limiters[key] = limiter
	}

	return limiter.Allow()
}

// Wait blocks until the operation can proceed
func (ol *OperationLimiter) Wait(ctx context.Context, tenantID, operation string) error {
	ol.mu.Lock()

	config, exists := ol.limits[operation]
	if !exists {
		ol.mu.Unlock()
		return nil // No limit
	}

	key := tenantID + ":" + operation
	limiter, exists := ol.limiters[key]
	if !exists {
		limiter = rate.NewLimiter(rate.Limit(config.RatePerSecond), config.Burst)
		ol.limiters[key] = limiter
	}
	ol.mu.Unlock()

	return limiter.Wait(ctx)
}

// GetLimit returns the configured limit for an operation
func (ol *OperationLimiter) GetLimit(operation string) (ratePerSecond, burst int, exists bool) {
	ol.mu.RLock()
	defer ol.mu.RUnlock()

	config, exists := ol.limits[operation]
	if !exists {
		return 0, 0, false
	}
	return config.RatePerSecond, config.Burst, true
}
