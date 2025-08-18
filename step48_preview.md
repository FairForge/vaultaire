# Step 48: Rate Limiting Middleware

## What We'll Build:
- Per-tenant rate limiting using token bucket algorithm
- Uses the tenant context from Step 47
- Returns HTTP 429 when limit exceeded
- Configurable limits per tenant plan

## Key Components:
1. internal/api/ratelimit.go - Rate limiter implementation
2. Use golang.org/x/time/rate package
3. Store limiters per tenant ID
4. Check limits before processing requests

## Example Implementation:
```go
type RateLimiter struct {
    limiters map[string]*rate.Limiter
    mu       sync.RWMutex
}

func (rl *RateLimiter) Allow(tenantID string, limit int) bool {
    limiter := rl.getLimiter(tenantID, limit)
    return limiter.Allow()
}
Testing:

Rapid requests should get 429 after limit
Different tenants have independent limits
Limits reset over time
