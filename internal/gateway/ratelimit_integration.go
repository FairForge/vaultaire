package gateway

import (
    "net/http"
    "sync"
    "time"
)

// RateLimiter provides rate limiting functionality
type RateLimiter struct {
    mu       sync.Mutex
    requests map[string][]time.Time
    limit    int
    window   time.Duration
}

// NewRateLimiter creates a new rate limiter
func NewRateLimiter(limit int, window time.Duration) *RateLimiter {
    return &RateLimiter{
        requests: make(map[string][]time.Time),
        limit:    limit,
        window:   window,
    }
}

// Allow checks if a request should be allowed
func (rl *RateLimiter) Allow(key string) bool {
    rl.mu.Lock()
    defer rl.mu.Unlock()
    
    now := time.Now()
    windowStart := now.Add(-rl.window)
    
    // Get existing requests
    requests := rl.requests[key]
    
    // Filter out old requests
    var valid []time.Time
    for _, reqTime := range requests {
        if reqTime.After(windowStart) {
            valid = append(valid, reqTime)
        }
    }
    
    // Check if under limit
    if len(valid) >= rl.limit {
        rl.requests[key] = valid
        return false
    }
    
    // Add new request
    valid = append(valid, now)
    rl.requests[key] = valid
    return true
}

// WithRateLimit adds rate limiting to the gateway
func (g *Gateway) WithRateLimit(limiter *RateLimiter, handler http.Handler) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        // Extract tenant ID for rate limiting
        tenantID := r.Header.Get("X-Tenant-ID")
        if tenantID == "" {
            tenantID = "anonymous"
        }
        
        if !limiter.Allow(tenantID) {
            http.Error(w, "Rate limit exceeded", http.StatusTooManyRequests)
            return
        }
        
        handler.ServeHTTP(w, r)
    })
}
