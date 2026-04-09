package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestLoginRateLimit_AllowsUnderLimit(t *testing.T) {
	rl := NewLoginRateLimiter(5, 1) // 5 per minute, burst 1

	handler := rl.Limit(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("POST", "/login", nil)
	req.RemoteAddr = "192.168.1.1:12345"

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
}

func TestLoginRateLimit_BlocksOverLimit(t *testing.T) {
	rl := NewLoginRateLimiter(2, 2) // 2 per minute, burst 2

	handler := rl.Limit(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// Exhaust the burst.
	for i := 0; i < 2; i++ {
		req := httptest.NewRequest("POST", "/login", nil)
		req.RemoteAddr = "10.0.0.1:9999"
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		assert.Equal(t, http.StatusOK, w.Code)
	}

	// Next request should be rate limited.
	req := httptest.NewRequest("POST", "/login", nil)
	req.RemoteAddr = "10.0.0.1:9999"
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusTooManyRequests, w.Code)
	assert.Contains(t, w.Body.String(), "Too many login attempts")
}

func TestLoginRateLimit_DifferentIPsIndependent(t *testing.T) {
	rl := NewLoginRateLimiter(1, 1) // 1 per minute, burst 1

	handler := rl.Limit(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// First IP uses its token.
	req1 := httptest.NewRequest("POST", "/login", nil)
	req1.RemoteAddr = "10.0.0.1:1111"
	w1 := httptest.NewRecorder()
	handler.ServeHTTP(w1, req1)
	assert.Equal(t, http.StatusOK, w1.Code)

	// Second IP should still be allowed.
	req2 := httptest.NewRequest("POST", "/login", nil)
	req2.RemoteAddr = "10.0.0.2:2222"
	w2 := httptest.NewRecorder()
	handler.ServeHTTP(w2, req2)
	assert.Equal(t, http.StatusOK, w2.Code)
}

func TestLoginRateLimit_XForwardedFor(t *testing.T) {
	rl := NewLoginRateLimiter(1, 1)

	handler := rl.Limit(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// First request from forwarded IP.
	req1 := httptest.NewRequest("POST", "/login", nil)
	req1.RemoteAddr = "127.0.0.1:8000"
	req1.Header.Set("X-Forwarded-For", "203.0.113.50")
	w1 := httptest.NewRecorder()
	handler.ServeHTTP(w1, req1)
	assert.Equal(t, http.StatusOK, w1.Code)

	// Second request from same forwarded IP should be blocked.
	req2 := httptest.NewRequest("POST", "/login", nil)
	req2.RemoteAddr = "127.0.0.1:8000"
	req2.Header.Set("X-Forwarded-For", "203.0.113.50")
	w2 := httptest.NewRecorder()
	handler.ServeHTTP(w2, req2)
	assert.Equal(t, http.StatusTooManyRequests, w2.Code)
}

func TestLoginRateLimit_CleanupRemovesStaleEntries(t *testing.T) {
	rl := NewLoginRateLimiter(5, 5)

	// Add some entries.
	for i := 0; i < 10; i++ {
		req := httptest.NewRequest("POST", "/login", nil)
		req.RemoteAddr = "10.0.0." + string(rune('0'+i)) + ":1234"
		w := httptest.NewRecorder()
		rl.Limit(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		})).ServeHTTP(w, req)
	}

	// Cleanup should not panic.
	rl.Cleanup()
}
