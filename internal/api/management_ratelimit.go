package api

import (
	"fmt"
	"net/http"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// ManagementRateLimiter is the per-tenant token bucket for the JSON APIs
// (100 requests/min, burst 10), keyed by the JWT tenant only — no header or
// address is consulted. One instance serves /api/v1/manage, /api/v1/webhooks
// and /api/v1/events (Review R11-14: a limiter per Route block gave every
// tenant two independent budgets).
type ManagementRateLimiter struct {
	mu       sync.Mutex
	limiters map[string]*rate.Limiter
	rps      rate.Limit
	burst    int
	now      func() time.Time
}

func NewManagementRateLimiter() *ManagementRateLimiter {
	return &ManagementRateLimiter{
		limiters: make(map[string]*rate.Limiter),
		rps:      rate.Limit(100.0 / 60.0), // 100 requests per minute
		burst:    10,
		now:      time.Now,
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
		now := rl.now()
		reservation := lim.ReserveN(now, 1)

		remaining := int(lim.TokensAt(now))
		if remaining < 0 {
			remaining = 0
		}

		// X-RateLimit-Reset is the time the caller can next send a request
		// (R10-31 / WP-R1-8: it used to be now + 1/rps whatever the state).
		// When the request is admitted, it is the moment the bucket is full
		// again — the earliest time a full burst is available.
		var resetAt time.Time
		delay := reservation.DelayFrom(now)
		if !reservation.OK() || delay > 0 {
			resetAt = now.Add(delay)
		} else {
			missing := float64(rl.burst) - lim.TokensAt(now)
			if missing < 0 {
				missing = 0
			}
			resetAt = now.Add(time.Duration(missing / float64(rl.rps) * float64(time.Second)))
		}

		w.Header().Set("X-RateLimit-Limit", "100")
		w.Header().Set("X-RateLimit-Remaining", fmt.Sprintf("%d", remaining))
		resetUnix := resetAt.Unix()
		if resetAt.Nanosecond() > 0 {
			resetUnix++ // round UP: a Reset in the past would be a lie
		}
		w.Header().Set("X-RateLimit-Reset", fmt.Sprintf("%d", resetUnix))

		if !reservation.OK() || delay > 0 {
			reservation.CancelAt(now)
			retry := int(delay.Seconds())
			if delay > 0 && delay < time.Second {
				retry = 1
			}
			w.Header().Set("Retry-After", fmt.Sprintf("%d", retry))
			writeManagementError(w, ErrTypeRateLimit, "rate_limit_exceeded",
				"too many requests, please retry later", "")
			return
		}

		next.ServeHTTP(w, r)
	})
}
