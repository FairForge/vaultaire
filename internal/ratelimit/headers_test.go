// internal/ratelimit/headers_test.go
package ratelimit

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRateLimitHeaders(t *testing.T) {
	t.Run("adds standard rate limit headers", func(t *testing.T) {
		// Arrange
		limiter := NewHeaderLimiter(10, 20) // 10 req/s, burst 20
		handler := limiter.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))

		// Act
		req := httptest.NewRequest("GET", "/test", nil)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)

		// Assert
		assert.Equal(t, "20", w.Header().Get("X-RateLimit-Limit"))
		assert.Equal(t, "19", w.Header().Get("X-RateLimit-Remaining"))
		assert.NotEmpty(t, w.Header().Get("X-RateLimit-Reset"))
	})

	t.Run("shows retry-after when rate limited", func(t *testing.T) {
		// Arrange
		limiter := NewHeaderLimiter(1, 1) // Very restrictive
		handler := limiter.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))

		// First request succeeds
		req1 := httptest.NewRequest("GET", "/test", nil)
		w1 := httptest.NewRecorder()
		handler.ServeHTTP(w1, req1)
		assert.Equal(t, http.StatusOK, w1.Code)

		// Second request should be rate limited
		req2 := httptest.NewRequest("GET", "/test", nil)
		w2 := httptest.NewRecorder()
		handler.ServeHTTP(w2, req2)

		assert.Equal(t, http.StatusTooManyRequests, w2.Code)
		assert.NotEmpty(t, w2.Header().Get("Retry-After"))
	})

	t.Run("supports draft IETF headers", func(t *testing.T) {
		// Arrange
		limiter := NewHeaderLimiter(10, 20)
		limiter.UseIETFDraft(true)

		handler := limiter.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))

		req := httptest.NewRequest("GET", "/test", nil)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)

		// Assert IETF draft headers
		assert.NotEmpty(t, w.Header().Get("RateLimit-Limit"))
		assert.NotEmpty(t, w.Header().Get("RateLimit-Remaining"))
		assert.NotEmpty(t, w.Header().Get("RateLimit-Reset"))
	})
}
