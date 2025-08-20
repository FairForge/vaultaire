package api

import (
	"golang.org/x/time/rate"
	"sync"
)

type RateLimiter struct {
	mu                sync.RWMutex
	limiters          map[string]*rate.Limiter
	requestsPerSecond int
	burstSize         int
}

func NewRateLimiter() *RateLimiter {
	return &RateLimiter{
		limiters:          make(map[string]*rate.Limiter),
		requestsPerSecond: 100,
		burstSize:         200,
	}
}

func (rl *RateLimiter) Allow(tenant string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	// MEMORY PROTECTION: Prevent unlimited growth
	if len(rl.limiters) >= 10000 {
		// Clear all limiters if we hit the limit
		rl.limiters = make(map[string]*rate.Limiter)
	}

	// Get or create limiter for this tenant
	limiter, exists := rl.limiters[tenant]
	if !exists {
		limiter = rate.NewLimiter(
			rate.Limit(rl.requestsPerSecond),
			rl.burstSize,
		)
		rl.limiters[tenant] = limiter
	}

	return limiter.Allow()
}
