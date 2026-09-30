package middleware

import (
	"net/http"
	"sync"
	"time"

	"github.com/FairForge/vaultaire/internal/clientip"
	"golang.org/x/time/rate"
)

// LoginRateLimiter limits login attempts per IP address.
type LoginRateLimiter struct {
	mu       sync.Mutex
	limiters map[string]*visitorLimiter
	rps      rate.Limit // tokens per second
	burst    int
	inserts  int // new-IP inserts since the last sweep (see getLimiter)
}

// Bound the per-IP map without a goroutine: every rateLimitSweepEvery new
// IPs (or when it grows past rateLimitMaxEntries) the idle entries are
// swept inline. Cleanup used to exist but nothing ever called it, so the
// three limiter instances grew one entry per IP forever (Review R1-14).
const (
	rateLimitSweepEvery = 512
	rateLimitMaxEntries = 20000
	rateLimitIdle       = 5 * time.Minute
)

type visitorLimiter struct {
	limiter  *rate.Limiter
	lastSeen time.Time
}

// NewLoginRateLimiter creates a rate limiter allowing perMinute attempts
// with the given burst size per IP.
func NewLoginRateLimiter(perMinute int, burst int) *LoginRateLimiter {
	return &LoginRateLimiter{
		limiters: make(map[string]*visitorLimiter),
		rps:      rate.Limit(float64(perMinute) / 60.0),
		burst:    burst,
	}
}

// Limit returns middleware that rate-limits requests by client IP.
func (rl *LoginRateLimiter) Limit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := ClientIP(r)
		limiter := rl.getLimiter(ip)

		if !limiter.Allow() {
			w.Header().Set("Retry-After", "60")
			http.Error(w, "Too many login attempts. Please try again in a minute.", http.StatusTooManyRequests)
			return
		}

		next.ServeHTTP(w, r)
	})
}

func (rl *LoginRateLimiter) getLimiter(ip string) *rate.Limiter {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	v, exists := rl.limiters[ip]
	if !exists {
		rl.inserts++
		if rl.inserts%rateLimitSweepEvery == 0 || len(rl.limiters) > rateLimitMaxEntries {
			rl.cleanupLocked(time.Now())
		}
		v = &visitorLimiter{
			limiter:  rate.NewLimiter(rl.rps, rl.burst),
			lastSeen: time.Now(),
		}
		rl.limiters[ip] = v
	}
	v.lastSeen = time.Now()
	return v.limiter
}

// Cleanup removes entries not seen in the last 5 minutes. getLimiter calls
// it inline; it stays exported for tests and operators' tooling.
func (rl *LoginRateLimiter) Cleanup() {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	rl.cleanupLocked(time.Now())
}

// cleanupLocked drops idle entries; a runaway map is reset outright. Caller
// holds mu.
func (rl *LoginRateLimiter) cleanupLocked(now time.Time) {
	if len(rl.limiters) > rateLimitMaxEntries {
		rl.limiters = make(map[string]*visitorLimiter)
		return
	}
	cutoff := now.Add(-rateLimitIdle)
	for ip, v := range rl.limiters {
		if v.lastSeen.Before(cutoff) {
			delete(rl.limiters, ip)
		}
	}
}

// Len is the number of tracked IPs (tests).
func (rl *LoginRateLimiter) Len() int {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	return len(rl.limiters)
}

// ClientIP returns the client IP used to key the login/reset/abuse limiters
// and session records. Proxy-header trust lives in internal/clientip (the
// first X-Forwarded-For entry this used to take is client-writable — R1-01).
func ClientIP(r *http.Request) string {
	return clientip.FromRequest(r)
}
