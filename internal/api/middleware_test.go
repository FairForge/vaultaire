package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRateLimitMiddleware_AllowsWithinLimit(t *testing.T) {
	// Create a rate limiter
	limiter := NewRateLimiter()

	// Create middleware
	middleware := RateLimitMiddleware(limiter)

	// Create a test handler that should be called
	called := false
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})

	// Wrap the handler with middleware
	wrapped := middleware(handler)

	// Create test request with tenant header
	req := httptest.NewRequest("GET", "/test", nil)
	req.Header.Set("X-Tenant-ID", "tenant1")
	rec := httptest.NewRecorder()

	// Execute
	wrapped.ServeHTTP(rec, req)

	// Assert
	if !called {
		t.Error("Handler should have been called")
	}
	if rec.Code != http.StatusOK {
		t.Errorf("Expected status 200, got %d", rec.Code)
	}
}

func TestRateLimitMiddleware_Returns429WhenLimited(t *testing.T) {
	// Create a rate limiter
	limiter := NewRateLimiter()

	// Exhaust the rate limit for a tenant
	tenant := "tenant2"
	for i := 0; i < 201; i++ { // Burst is 200
		limiter.Allow(tenant)
	}

	// Create middleware
	middleware := RateLimitMiddleware(limiter)

	// Create a test handler that should NOT be called
	called := false
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})

	// Wrap the handler
	wrapped := middleware(handler)

	// Create test request
	req := httptest.NewRequest("GET", "/test", nil)
	req.Header.Set("X-Tenant-ID", tenant)
	rec := httptest.NewRecorder()

	// Execute
	wrapped.ServeHTTP(rec, req)

	// Assert
	if called {
		t.Error("Handler should NOT have been called")
	}
	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("Expected status 429, got %d", rec.Code)
	}
}

func TestRateLimitMiddleware_SetsHeaders(t *testing.T) {
	// Create a rate limiter
	limiter := NewRateLimiter()

	// Create middleware
	middleware := RateLimitMiddleware(limiter)

	// Create a test handler
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	// Wrap the handler
	wrapped := middleware(handler)

	// Create test request
	req := httptest.NewRequest("GET", "/test", nil)
	req.Header.Set("X-Tenant-ID", "tenant3")
	rec := httptest.NewRecorder()

	// Execute
	wrapped.ServeHTTP(rec, req)

	// Assert headers exist
	if rec.Header().Get("X-RateLimit-Limit") == "" {
		t.Error("X-RateLimit-Limit header not set")
	}
	if rec.Header().Get("X-RateLimit-Remaining") == "" {
		t.Error("X-RateLimit-Remaining header not set")
	}
	if rec.Header().Get("X-RateLimit-Reset") == "" {
		t.Error("X-RateLimit-Reset header not set")
	}
}

func TestRateLimitMiddleware_IsolatesTenants(t *testing.T) {
	// Create a rate limiter
	limiter := NewRateLimiter()

	// Exhaust limit for tenant1
	for i := 0; i < 201; i++ {
		limiter.Allow("tenant-isolated-1")
	}

	// Create middleware
	middleware := RateLimitMiddleware(limiter)

	// Create test handler
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	wrapped := middleware(handler)

	// Request from tenant1 (exhausted) should fail
	req1 := httptest.NewRequest("GET", "/test", nil)
	req1.Header.Set("X-Tenant-ID", "tenant-isolated-1")
	rec1 := httptest.NewRecorder()
	wrapped.ServeHTTP(rec1, req1)

	if rec1.Code != http.StatusTooManyRequests {
		t.Errorf("Tenant1 should be rate limited, got %d", rec1.Code)
	}

	// Request from tenant2 (fresh) should succeed
	req2 := httptest.NewRequest("GET", "/test", nil)
	req2.Header.Set("X-Tenant-ID", "tenant-isolated-2")
	rec2 := httptest.NewRecorder()
	wrapped.ServeHTTP(rec2, req2)

	if rec2.Code != http.StatusOK {
		t.Errorf("Tenant2 should NOT be rate limited, got %d", rec2.Code)
	}
}
