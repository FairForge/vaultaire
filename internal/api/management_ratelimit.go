package api

import (
	"fmt"
	"net/http"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

type ManagementRateLimiter struct {
	mu       sync.Mutex
	limiters map[string]*rate.Limiter
	rps      rate.Limit
	burst    int
}

func NewManagementRateLimiter() *ManagementRateLimiter {
	return &ManagementRateLimiter{
		limiters: make(map[string]*rate.Limiter),
		rps:      rate.Limit(100.0 / 60.0), // 100 requests per minute
		burst:    10,
	}
}

func (rl *ManagementRateLimiter) getLimiter(tenantID string) *rate.Limiter {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	if len(rl.limiters) >= 10000 {
		rl.limiters = make(map[string]*rate.Limiter)
	}

	lim, ok := rl.limiters[tenantID]
	if !ok {
		lim = rate.NewLimiter(rl.rps, rl.burst)
		rl.limiters[tenantID] = lim
	}
	return lim
}

func (rl *ManagementRateLimiter) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tenantID, _ := r.Context().Value(tenantIDKey).(string)
		if tenantID == "" {
			next.ServeHTTP(w, r)
			return
		}

		lim := rl.getLimiter(tenantID)
		reservation := lim.Reserve()

		remaining := int(lim.Tokens())
		if remaining < 0 {
			remaining = 0
		}
		resetAt := time.Now().Add(time.Duration(float64(time.Second) / float64(rl.rps))).Unix()

		w.Header().Set("X-RateLimit-Limit", "100")
		w.Header().Set("X-RateLimit-Remaining", fmt.Sprintf("%d", remaining))
		w.Header().Set("X-RateLimit-Reset", fmt.Sprintf("%d", resetAt))

		if !reservation.OK() || reservation.Delay() > 0 {
			reservation.Cancel()
			w.Header().Set("Retry-After", fmt.Sprintf("%d", resetAt-time.Now().Unix()+1))
			writeManagementError(w, ErrTypeRateLimit, "rate_limit_exceeded",
				"too many requests, please retry later", "")
			return
		}

		next.ServeHTTP(w, r)
	})
}
