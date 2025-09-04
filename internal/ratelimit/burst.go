// internal/ratelimit/burst.go
package ratelimit

import (
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// BurstLimiter handles burst capacity with token bucket
type BurstLimiter struct {
	limiter *rate.Limiter
}

// NewBurstLimiter creates a limiter with burst handling
func NewBurstLimiter(ratePerSecond, burst int) *BurstLimiter {
	return &BurstLimiter{
		limiter: rate.NewLimiter(rate.Limit(ratePerSecond), burst),
	}
}

// Allow checks if a request can proceed
func (bl *BurstLimiter) Allow() bool {
	return bl.limiter.Allow()
}

// AdaptiveBurstLimiter adjusts burst based on behavior
type AdaptiveBurstLimiter struct {
	mu        sync.RWMutex
	baseRate  int
	baseBurst int
	maxBurst  int
	limiters  map[string]*rate.Limiter
	behavior  map[string]*behaviorStats
}

type behaviorStats struct {
	goodRequests int
	violations   int
	currentBurst int
	lastReset    time.Time
}

// NewAdaptiveBurstLimiter creates an adaptive burst limiter
func NewAdaptiveBurstLimiter(ratePerSecond, baseBurst, maxBurst int) *AdaptiveBurstLimiter {
	return &AdaptiveBurstLimiter{
		baseRate:  ratePerSecond,
		baseBurst: baseBurst,
		maxBurst:  maxBurst,
		limiters:  make(map[string]*rate.Limiter),
		behavior:  make(map[string]*behaviorStats),
	}
}

// Allow checks if a tenant can proceed
func (abl *AdaptiveBurstLimiter) Allow(tenantID string) bool {
	abl.mu.Lock()
	defer abl.mu.Unlock()

	// Get or create stats
	stats, exists := abl.behavior[tenantID]
	if !exists {
		stats = &behaviorStats{
			currentBurst: abl.baseBurst,
			lastReset:    time.Now(),
		}
		abl.behavior[tenantID] = stats
	}

	// Get or create limiter
	limiter, exists := abl.limiters[tenantID]
	if !exists {
		limiter = rate.NewLimiter(rate.Limit(abl.baseRate), stats.currentBurst)
		abl.limiters[tenantID] = limiter
	}

	allowed := limiter.Allow()

	if allowed {
		stats.goodRequests++

		// Increase burst for consistently good behavior
		if stats.goodRequests >= 20 && stats.violations == 0 {
			newBurst := stats.currentBurst + 5
			if newBurst <= abl.maxBurst {
				stats.currentBurst = newBurst
				// Update limiter burst
				limiter.SetBurst(newBurst)
			}
			stats.goodRequests = 0 // Reset counter
		}
	} else {
		stats.violations++

		// Decrease burst for violations
		if stats.violations > 5 {
			newBurst := stats.currentBurst - 5
			if newBurst >= abl.baseBurst {
				stats.currentBurst = newBurst
				limiter.SetBurst(newBurst)
			}
			stats.violations = 0 // Reset counter
		}
	}

	return allowed
}

// GetBurst returns current burst for a tenant
func (abl *AdaptiveBurstLimiter) GetBurst(tenantID string) int {
	abl.mu.RLock()
	defer abl.mu.RUnlock()

	if stats, exists := abl.behavior[tenantID]; exists {
		return stats.currentBurst
	}
	return abl.baseBurst
}
