package middleware

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

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

// R1-14: Cleanup had no caller, so every limiter grew one entry per IP for
// the life of the process. Idle entries are now swept inline as new IPs
// arrive; the map cannot grow without bound.
func TestLoginRateLimit_MapIsSweptInline(t *testing.T) {
	rl := NewLoginRateLimiter(5, 5)
	for i := 0; i < rateLimitSweepEvery-1; i++ {
		rl.getLimiter("10.1." + string(rune('0'+i%10)) + "." + string(rune('0'+(i/10)%10)) + string(rune('0'+(i/100)%10)))
	}
	// Age every entry past the idle window.
	rl.mu.Lock()
	for _, v := range rl.limiters {
		v.lastSeen = v.lastSeen.Add(-2 * rateLimitIdle)
	}
	rl.mu.Unlock()
	before := rl.Len()
	rl.getLimiter("203.0.113.9") // the Nth new IP triggers the sweep
	if rl.Len() >= before {
		t.Fatalf("expected idle entries to be swept: before=%d after=%d", before, rl.Len())
	}
}

// RP92-5: an IPv6 /64 is one key, and a map past its cap evicts its OLDEST
// entries — it used to be replaced whole, so 20,001 addresses zeroed every
// counter, a per-day cap included.
func TestRateLimiter_IPv6BySlash64AndEvictsOldest(t *testing.T) {
	rl := NewRateLimiter(24*time.Hour, 2)
	assert.True(t, rl.Allow("2001:db8:1:2::1"))
	assert.True(t, rl.Allow("2001:db8:1:2:ffff::7"))
	assert.False(t, rl.Allow("2001:db8:1:2:abcd::9"), "a third address of the same /64 is over the network's limit")
	assert.True(t, rl.Allow("2001:db8:1:3::1"), "another /64 is another key")

	// A flood of distinct networks past the cap.
	for i := 0; i <= rateLimitMaxEntries; i++ {
		rl.Allow(fmt.Sprintf("2001:db8:ffff:%x::1", i))
		if i == rateLimitMaxEntries-50 {
			// The spent network is seen again late in the flood: it is
			// not among the oldest, so it is not evicted.
			assert.False(t, rl.Allow("2001:db8:1:2::1"))
		}
	}
	assert.LessOrEqual(t, rl.Len(), rateLimitMaxEntries+1, "bounded")
	assert.False(t, rl.Allow("2001:db8:1:2::1"), "the spent network's counter survived the flood")
}
