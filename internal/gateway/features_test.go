// internal/gateway/features_test.go
package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRequestTransformer_Validate(t *testing.T) {
	t.Run("valid config passes", func(t *testing.T) {
		config := &TransformConfig{
			AddHeaders:    map[string]string{"X-Request-ID": "{{uuid}}"},
			RemoveHeaders: []string{"X-Internal"},
		}
		err := config.Validate()
		assert.NoError(t, err)
	})
}

func TestRequestTransformer_Transform(t *testing.T) {
	transformer := NewRequestTransformer(&TransformConfig{
		AddHeaders: map[string]string{
			"X-Gateway":    "vaultaire",
			"X-Request-ID": "{{uuid}}",
		},
		RemoveHeaders: []string{"X-Internal-Secret"},
		RewritePath:   "/api/v2{{path}}",
	})

	t.Run("adds headers", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/users", nil)
		transformed := transformer.TransformRequest(req)
		assert.Equal(t, "vaultaire", transformed.Header.Get("X-Gateway"))
	})

	t.Run("generates UUID for template", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/users", nil)
		transformed := transformer.TransformRequest(req)
		requestID := transformed.Header.Get("X-Request-ID")
		assert.NotEmpty(t, requestID)
		assert.Len(t, requestID, 36) // UUID format
	})

	t.Run("removes headers", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/users", nil)
		req.Header.Set("X-Internal-Secret", "secret123")
		transformed := transformer.TransformRequest(req)
		assert.Empty(t, transformed.Header.Get("X-Internal-Secret"))
	})

	t.Run("rewrites path", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/users/123", nil)
		transformed := transformer.TransformRequest(req)
		assert.Equal(t, "/api/v2/users/123", transformed.URL.Path)
	})
}

func TestResponseTransformer_Transform(t *testing.T) {
	transformer := NewResponseTransformer(&TransformConfig{
		AddHeaders:    map[string]string{"X-Served-By": "vaultaire-gateway"},
		RemoveHeaders: []string{"X-Backend-Server"},
	})

	t.Run("adds response headers", func(t *testing.T) {
		resp := &http.Response{
			Header: make(http.Header),
		}
		transformed := transformer.TransformResponse(resp)
		assert.Equal(t, "vaultaire-gateway", transformed.Header.Get("X-Served-By"))
	})

	t.Run("removes response headers", func(t *testing.T) {
		resp := &http.Response{
			Header: make(http.Header),
		}
		resp.Header.Set("X-Backend-Server", "internal-node-1")
		transformed := transformer.TransformResponse(resp)
		assert.Empty(t, transformed.Header.Get("X-Backend-Server"))
	})
}

func TestCircuitBreaker_States(t *testing.T) {
	cb := NewCircuitBreaker(&CircuitBreakerConfig{
		FailureThreshold: 3,
		SuccessThreshold: 2,
		Timeout:          100 * time.Millisecond,
	})

	t.Run("starts closed", func(t *testing.T) {
		assert.Equal(t, CircuitClosed, cb.State())
	})

	t.Run("opens after failures", func(t *testing.T) {
		cb.RecordFailure()
		cb.RecordFailure()
		cb.RecordFailure()
		assert.Equal(t, CircuitOpen, cb.State())
	})

	t.Run("rejects requests when open", func(t *testing.T) {
		err := cb.Allow()
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "circuit open")
	})

	t.Run("transitions to half-open after timeout", func(t *testing.T) {
		time.Sleep(150 * time.Millisecond)
		assert.Equal(t, CircuitHalfOpen, cb.State())
	})

	t.Run("closes after successes in half-open", func(t *testing.T) {
		_ = cb.Allow() // Allowed in half-open
		cb.RecordSuccess()
		cb.RecordSuccess()
		assert.Equal(t, CircuitClosed, cb.State())
	})
}

func TestCircuitBreaker_Execute(t *testing.T) {
	cb := NewCircuitBreaker(&CircuitBreakerConfig{
		FailureThreshold: 2,
		SuccessThreshold: 1,
		Timeout:          50 * time.Millisecond,
	})

	t.Run("executes successful function", func(t *testing.T) {
		result, err := cb.Execute(func() (interface{}, error) {
			return "success", nil
		})
		assert.NoError(t, err)
		assert.Equal(t, "success", result)
	})

	t.Run("records failures", func(t *testing.T) {
		_, _ = cb.Execute(func() (interface{}, error) {
			return nil, assert.AnError
		})
		_, _ = cb.Execute(func() (interface{}, error) {
			return nil, assert.AnError
		})
		assert.Equal(t, CircuitOpen, cb.State())
	})
}

func TestRequestCoalescer(t *testing.T) {
	coalescer := NewRequestCoalescer()

	t.Run("coalesces identical requests", func(t *testing.T) {
		callCount := 0
		fetch := func() (interface{}, error) {
			callCount++
			time.Sleep(50 * time.Millisecond)
			return "result", nil
		}

		// Start two concurrent requests for same key
		done := make(chan string, 2)
		for i := 0; i < 2; i++ {
			go func() {
				result, _ := coalescer.Do("same-key", fetch)
				done <- result.(string)
			}()
		}

		// Both should get result
		r1 := <-done
		r2 := <-done
		assert.Equal(t, "result", r1)
		assert.Equal(t, "result", r2)
		assert.Equal(t, 1, callCount) // Only called once
	})
}

func TestAPIComposer(t *testing.T) {
	// Mock backend servers
	userServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"id":   "user-1",
			"name": "John",
		})
	}))
	defer userServer.Close()

	orderServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]interface{}{
			{"id": "order-1", "total": 100},
			{"id": "order-2", "total": 200},
		})
	}))
	defer orderServer.Close()

	composer := NewAPIComposer()

	t.Run("composes multiple API calls", func(t *testing.T) {
		composition := &CompositionConfig{
			Endpoints: []EndpointConfig{
				{Name: "user", URL: userServer.URL, Method: "GET"},
				{Name: "orders", URL: orderServer.URL, Method: "GET"},
			},
		}

		result, err := composer.Compose(context.Background(), composition)
		require.NoError(t, err)

		data := result.(map[string]interface{})
		assert.Contains(t, data, "user")
		assert.Contains(t, data, "orders")
	})

	t.Run("handles parallel execution", func(t *testing.T) {
		composition := &CompositionConfig{
			Parallel: true,
			Endpoints: []EndpointConfig{
				{Name: "user", URL: userServer.URL, Method: "GET"},
				{Name: "orders", URL: orderServer.URL, Method: "GET"},
			},
		}

		start := time.Now()
		_, err := composer.Compose(context.Background(), composition)
		duration := time.Since(start)

		require.NoError(t, err)
		// Parallel should be faster than sequential
		assert.Less(t, duration, 200*time.Millisecond)
	})
}

func TestRateLimitPolicy(t *testing.T) {
	policy := NewRateLimitPolicy(&RateLimitPolicyConfig{
		DefaultLimit: 100,
		BurstLimit:   10,
		Window:       time.Minute,
		ByEndpoint: map[string]int{
			"/api/expensive": 10,
		},
		ByTier: map[string]int{
			"free":       50,
			"pro":        500,
			"enterprise": 5000,
		},
	})

	t.Run("returns default limit", func(t *testing.T) {
		limit := policy.GetLimit("/api/users", "basic")
		assert.Equal(t, 100, limit)
	})

	t.Run("returns endpoint-specific limit", func(t *testing.T) {
		limit := policy.GetLimit("/api/expensive", "basic")
		assert.Equal(t, 10, limit)
	})

	t.Run("returns tier-specific limit", func(t *testing.T) {
		limit := policy.GetLimit("/api/users", "enterprise")
		assert.Equal(t, 5000, limit)
	})
}

func TestRequestValidator(t *testing.T) {
	validator := NewRequestValidator()

	t.Run("validates required headers", func(t *testing.T) {
		rule := &ValidationRule{
			RequiredHeaders: []string{"Authorization", "Content-Type"},
		}
		validator.AddRule("/api/*", rule)

		req := httptest.NewRequest("POST", "/api/users", nil)
		req.Header.Set("Authorization", "Bearer token")
		// Missing Content-Type

		errors := validator.Validate(req)
		assert.NotEmpty(t, errors)
		assert.Contains(t, errors[0], "Content-Type")
	})

	t.Run("validates content type", func(t *testing.T) {
		rule := &ValidationRule{
			AllowedContentTypes: []string{"application/json"},
		}
		validator.AddRule("/api/*", rule)

		req := httptest.NewRequest("POST", "/api/users", strings.NewReader("data"))
		req.Header.Set("Content-Type", "text/plain")

		errors := validator.Validate(req)
		assert.NotEmpty(t, errors)
	})

	t.Run("validates max body size", func(t *testing.T) {
		rule := &ValidationRule{
			MaxBodySize: 100,
		}
		validator.AddRule("/api/*", rule)

		body := strings.Repeat("x", 200)
		req := httptest.NewRequest("POST", "/api/users", strings.NewReader(body))

		errors := validator.Validate(req)
		assert.NotEmpty(t, errors)
		assert.Contains(t, errors[0], "body size")
	})
}

func TestCachePolicy(t *testing.T) {
	policy := NewCachePolicy(&CachePolicyConfig{
		DefaultTTL: time.Minute,
		ByStatus: map[int]time.Duration{
			200: 5 * time.Minute,
			404: 30 * time.Second,
		},
		ByPath: map[string]time.Duration{
			"/api/static/*": time.Hour,
		},
		VaryHeaders: []string{"Accept", "Accept-Language"},
	})

	t.Run("returns default TTL", func(t *testing.T) {
		ttl := policy.GetTTL("/api/users", 201)
		assert.Equal(t, time.Minute, ttl)
	})

	t.Run("returns status-specific TTL", func(t *testing.T) {
		ttl := policy.GetTTL("/api/users", 200)
		assert.Equal(t, 5*time.Minute, ttl)
	})

	t.Run("returns path-specific TTL", func(t *testing.T) {
		ttl := policy.GetTTL("/api/static/logo.png", 200)
		assert.Equal(t, time.Hour, ttl)
	})

	t.Run("generates cache key with vary headers", func(t *testing.T) {
		req1 := httptest.NewRequest("GET", "/api/users", nil)
		req1.Header.Set("Accept", "application/json")
		req1.Header.Set("Accept-Language", "en-US")

		req2 := httptest.NewRequest("GET", "/api/users", nil)
		req2.Header.Set("Accept", "text/html")
		req2.Header.Set("Accept-Language", "fr-FR")

		key1 := policy.GenerateCacheKey(req1)
		key2 := policy.GenerateCacheKey(req2)

		assert.Len(t, key1, 64)        // SHA256 hex
		assert.NotEqual(t, key1, key2) // Different headers = different keys
	})
}

func TestIPFilter(t *testing.T) {
	filter := NewIPFilter(&IPFilterConfig{
		Allowlist: []string{"10.0.0.0/8", "192.168.1.0/24"},
		Blocklist: []string{"10.0.0.1/32"},
	})

	t.Run("allows IP in allowlist", func(t *testing.T) {
		assert.True(t, filter.Allow("10.0.0.100"))
		assert.True(t, filter.Allow("192.168.1.50"))
	})

	t.Run("blocks IP in blocklist", func(t *testing.T) {
		assert.False(t, filter.Allow("10.0.0.1"))
	})

	t.Run("blocks IP not in allowlist", func(t *testing.T) {
		assert.False(t, filter.Allow("8.8.8.8"))
	})
}

func TestRetryPolicy(t *testing.T) {
	policy := NewRetryPolicy(&RetryPolicyConfig{
		MaxRetries:      3,
		InitialBackoff:  10 * time.Millisecond,
		MaxBackoff:      100 * time.Millisecond,
		BackoffFactor:   2.0,
		RetryableStatus: []int{502, 503, 504},
	})

	t.Run("calculates backoff with exponential growth", func(t *testing.T) {
		b1 := policy.GetBackoff(0)
		b2 := policy.GetBackoff(1)
		b3 := policy.GetBackoff(2)

		assert.Equal(t, 10*time.Millisecond, b1)
		assert.Equal(t, 20*time.Millisecond, b2)
		assert.Equal(t, 40*time.Millisecond, b3)
	})

	t.Run("caps at max backoff", func(t *testing.T) {
		b := policy.GetBackoff(10)
		assert.Equal(t, 100*time.Millisecond, b)
	})

	t.Run("identifies retryable status", func(t *testing.T) {
		assert.True(t, policy.IsRetryable(503))
		assert.False(t, policy.IsRetryable(400))
		assert.False(t, policy.IsRetryable(200))
	})
}
