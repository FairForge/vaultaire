// internal/ratelimit/headers.go
package ratelimit

import (
	"fmt"
	"net/http"
	"strconv"
	"time"

	"golang.org/x/time/rate"
)

// HeaderLimiter adds rate limit headers to HTTP responses
type HeaderLimiter struct {
	limiter *rate.Limiter
	limit   int
	useIETF bool
}

// NewHeaderLimiter creates a new header-aware limiter
func NewHeaderLimiter(ratePerSecond, burst int) *HeaderLimiter {
	return &HeaderLimiter{
		limiter: rate.NewLimiter(rate.Limit(ratePerSecond), burst),
		limit:   burst,
	}
}

// UseIETFDraft enables IETF draft headers
func (hl *HeaderLimiter) UseIETFDraft(use bool) {
	hl.useIETF = use
}

// Middleware wraps an HTTP handler with rate limiting
func (hl *HeaderLimiter) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Try to consume a token
		allowed := hl.limiter.Allow()

		// Calculate remaining tokens
		remaining := int(hl.limiter.Tokens())
		if remaining < 0 {
			remaining = 0
		}

		// Calculate reset time (1 second from now for simplicity)
		resetTime := time.Now().Add(time.Second).Unix()

		// Add headers based on format
		if hl.useIETF {
			// IETF draft headers (no X- prefix)
			w.Header().Set("RateLimit-Limit", strconv.Itoa(hl.limit))
			w.Header().Set("RateLimit-Remaining", strconv.Itoa(remaining))
			w.Header().Set("RateLimit-Reset", strconv.FormatInt(resetTime, 10))
		} else {
			// Traditional X- headers
			w.Header().Set("X-RateLimit-Limit", strconv.Itoa(hl.limit))
			w.Header().Set("X-RateLimit-Remaining", strconv.Itoa(remaining))
			w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(resetTime, 10))
		}

		if !allowed {
			// Add Retry-After header
			w.Header().Set("Retry-After", "1")

			// Return 429 Too Many Requests
			http.Error(w, "Too Many Requests", http.StatusTooManyRequests)
			return
		}

		// Continue to next handler
		next.ServeHTTP(w, r)
	})
}

// GetInfo returns current rate limit info
func (hl *HeaderLimiter) GetInfo() RateLimitInfo {
	return RateLimitInfo{
		Limit:     hl.limit,
		Remaining: int(hl.limiter.Tokens()),
		Reset:     time.Now().Add(time.Second).Unix(),
	}
}

// RateLimitInfo contains rate limit information
type RateLimitInfo struct {
	Limit     int   `json:"limit"`
	Remaining int   `json:"remaining"`
	Reset     int64 `json:"reset"`
}

// SetHeaders adds rate limit headers to a response
func SetHeaders(w http.ResponseWriter, info RateLimitInfo, useIETF bool) {
	if useIETF {
		w.Header().Set("RateLimit-Limit", strconv.Itoa(info.Limit))
		w.Header().Set("RateLimit-Remaining", strconv.Itoa(info.Remaining))
		w.Header().Set("RateLimit-Reset", strconv.FormatInt(info.Reset, 10))
	} else {
		w.Header().Set("X-RateLimit-Limit", strconv.Itoa(info.Limit))
		w.Header().Set("X-RateLimit-Remaining", strconv.Itoa(info.Remaining))
		w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(info.Reset, 10))
	}
}

// FormatRateLimitError formats a rate limit error response
func FormatRateLimitError(w http.ResponseWriter, retryAfter int) {
	w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusTooManyRequests)

	errorMsg := fmt.Sprintf(`{"error":"Rate limit exceeded","retry_after":%d}`, retryAfter)
	_, _ = w.Write([]byte(errorMsg))
}
