// internal/gateway/ratelimit_integration_test.go
package gateway

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestRateLimitIntegration(t *testing.T) {
	t.Run("integrates rate limiting with gateway", func(t *testing.T) {
		// Arrange
		gw := NewGateway()
		limiter := NewRateLimiter(2, time.Second) // 2 requests per second

		handler := gw.WithRateLimit(limiter, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))

		// Act - make 3 requests quickly
		for i := 0; i < 3; i++ {
			req := httptest.NewRequest("GET", "/test", nil)
			req.Header.Set("X-Tenant-ID", "tenant-1")
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if i < 2 {
				assert.Equal(t, http.StatusOK, rec.Code)
			} else {
				assert.Equal(t, http.StatusTooManyRequests, rec.Code)
			}
		}
	})
}
