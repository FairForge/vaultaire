package ratelimit

import (
    "sync"
    "time"
)

// SlidingWindowLimiter implements sliding window algorithm
type SlidingWindowLimiter struct {
    mu       sync.Mutex
    limit    int
    window   time.Duration
    requests map[string][]time.Time
}

// NewSlidingWindowLimiter creates a sliding window rate limiter
func NewSlidingWindowLimiter(limit int, window time.Duration) *SlidingWindowLimiter {
    return &SlidingWindowLimiter{
        limit:    limit,
        window:   window,
        requests: make(map[string][]time.Time),
    }
}

// Allow checks if request can proceed
func (s *SlidingWindowLimiter) Allow(key string) bool {
    s.mu.Lock()
    defer s.mu.Unlock()
    
    now := time.Now()
    windowStart := now.Add(-s.window)
    
    // Get existing requests and clean old ones
    timestamps := s.requests[key]
    var validRequests []time.Time
    
    for _, t := range timestamps {
        if t.After(windowStart) {
            validRequests = append(validRequests, t)
        }
    }
    
    // Check if under limit
    if len(validRequests) < s.limit {
        validRequests = append(validRequests, now)
        s.requests[key] = validRequests
        return true
    }
    
    // Update with cleaned requests even if denied
    s.requests[key] = validRequests
    return false
}

// FixedWindowLimiter implements fixed window algorithm
type FixedWindowLimiter struct {
    mu         sync.Mutex
    limit      int
    window     time.Duration
    counters   map[string]*windowCounter
}

type windowCounter struct {
    count      int
    windowStart time.Time
}

// NewFixedWindowLimiter creates a fixed window rate limiter
func NewFixedWindowLimiter(limit int, window time.Duration) *FixedWindowLimiter {
    return &FixedWindowLimiter{
        limit:    limit,
        window:   window,
        counters: make(map[string]*windowCounter),
    }
}

// Allow checks if request can proceed
func (f *FixedWindowLimiter) Allow(key string) bool {
    f.mu.Lock()
    defer f.mu.Unlock()
    
    now := time.Now()
    
    // Get or create counter
    counter, exists := f.counters[key]
    if !exists {
        f.counters[key] = &windowCounter{
            count:       1,
            windowStart: now,
        }
        return true
    }
    
    // Check if we're in a new window
    if now.Sub(counter.windowStart) >= f.window {
        // Reset for new window
        counter.count = 1
        counter.windowStart = now
        return true
    }
    
    // Same window - check limit
    if counter.count < f.limit {
        counter.count++
        return true
    }
    
    return false
}

// Cleanup removes old entries (run periodically)
func (s *SlidingWindowLimiter) Cleanup() {
    s.mu.Lock()
    defer s.mu.Unlock()
    
    now := time.Now()
    windowStart := now.Add(-s.window)
    
    for key, timestamps := range s.requests {
        var validRequests []time.Time
        for _, t := range timestamps {
            if t.After(windowStart) {
                validRequests = append(validRequests, t)
            }
        }
        if len(validRequests) == 0 {
            delete(s.requests, key)
        } else {
            s.requests[key] = validRequests
        }
    }
}

// RedisLimiter for true distributed rate limiting (placeholder)
type RedisLimiter struct {
    // This would use Redis for distributed state
    // For MVP, the in-memory version is sufficient
}
