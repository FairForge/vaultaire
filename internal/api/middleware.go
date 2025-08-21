package api

import (
	"fmt"
	"net/http"
	"time"
)

// Middleware is a function that wraps an HTTP handler
type Middleware func(http.Handler) http.Handler

// RateLimitMiddleware creates middleware that enforces rate limits
func RateLimitMiddleware(limiter *RateLimiter) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Extract tenant ID from header
			tenantID := r.Header.Get("X-Tenant-ID")
			if tenantID == "" {
				tenantID = "default" // Default tenant for requests without ID
			}
			
			// Set rate limit headers (always set, even on success)
			w.Header().Set("X-RateLimit-Limit", "100")
			w.Header().Set("X-RateLimit-Remaining", "99") // Simplified for now
			w.Header().Set("X-RateLimit-Reset", fmt.Sprintf("%d", time.Now().Add(time.Second).Unix()))
			
			// Check rate limit
			if !limiter.Allow(tenantID) {
				// Rate limit exceeded
				w.WriteHeader(http.StatusTooManyRequests)
				// Handle error to satisfy gosec
				_, _ = w.Write([]byte("Rate limit exceeded"))
				return
			}
			
			// Continue to next handler
			next.ServeHTTP(w, r)
		})
	}
}
