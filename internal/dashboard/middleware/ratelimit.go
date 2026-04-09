package middleware

import (
	"net"
	"net/http"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// LoginRateLimiter limits login attempts per IP address.
type LoginRateLimiter struct {
	mu       sync.Mutex
	limiters map[string]*visitorLimiter
	rps      rate.Limit // tokens per second
	burst    int
}

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
		ip := clientIP(r)
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
		v = &visitorLimiter{
			limiter:  rate.NewLimiter(rl.rps, rl.burst),
			lastSeen: time.Now(),
		}
		rl.limiters[ip] = v
	}
	v.lastSeen = time.Now()
	return v.limiter
}

// Cleanup removes entries not seen in the last 5 minutes.
// Call periodically from a goroutine.
func (rl *LoginRateLimiter) Cleanup() {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	cutoff := time.Now().Add(-5 * time.Minute)
	for ip, v := range rl.limiters {
		if v.lastSeen.Before(cutoff) {
			delete(rl.limiters, ip)
		}
	}
}

// clientIP extracts the client IP, preferring X-Forwarded-For (first entry)
// since HAProxy sits in front of Vaultaire.
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		// Take the first IP (client), ignore proxies.
		for i := 0; i < len(xff); i++ {
			if xff[i] == ',' {
				return xff[:i]
			}
		}
		return xff
	}
	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return ip
}
