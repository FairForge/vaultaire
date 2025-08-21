package api

import (
    "net/http"
)

// Middleware is a function that wraps an HTTP handler
type Middleware func(http.Handler) http.Handler

// RateLimitMiddleware creates middleware that enforces rate limits
func RateLimitMiddleware(limiter *RateLimiter) Middleware {
    return func(next http.Handler) http.Handler {
        return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
            // TODO: Implement rate limiting
            next.ServeHTTP(w, r)
        })
    }
}
