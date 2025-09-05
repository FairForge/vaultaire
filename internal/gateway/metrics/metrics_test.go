// internal/gateway/metrics/metrics_test.go
package metrics

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
)

func TestCollector_RecordRequest(t *testing.T) {
	collector := NewCollector()

	// Get initial counts
	initialCount := testutil.ToFloat64(requestsTotal.WithLabelValues("GET", "/bucket/object", "2xx"))

	// Record some requests
	collector.RecordRequest("GET", "/bucket/object", 200, 100*time.Millisecond, 1024, 2048)
	collector.RecordRequest("PUT", "/bucket/object", 201, 200*time.Millisecond, 4096, 512)
	collector.RecordRequest("GET", "/bucket/object", 404, 50*time.Millisecond, 512, 128)
	collector.RecordRequest("GET", "/", 500, 10*time.Millisecond, 0, 0)

	// Verify counters increased by expected amount
	assert.Equal(t, initialCount+1, testutil.ToFloat64(requestsTotal.WithLabelValues("GET", "/bucket/object", "2xx")))
	assert.GreaterOrEqual(t, testutil.ToFloat64(requestsTotal.WithLabelValues("GET", "/bucket/object", "4xx")), float64(1))
	assert.GreaterOrEqual(t, testutil.ToFloat64(requestsTotal.WithLabelValues("GET", "/", "5xx")), float64(1))
}

func TestCollector_ErrorTracking(t *testing.T) {
	collector := NewCollector()

	initial := testutil.ToFloat64(errorsTotal.WithLabelValues("client_error", "/bucket/object"))

	// Record errors
	collector.RecordError("client_error", "/bucket/object")
	collector.RecordError("client_error", "/bucket/object")
	collector.RecordError("server_error", "/")

	// Verify error counters increased
	assert.Equal(t, initial+2, testutil.ToFloat64(errorsTotal.WithLabelValues("client_error", "/bucket/object")))
	assert.GreaterOrEqual(t, testutil.ToFloat64(errorsTotal.WithLabelValues("server_error", "/")), float64(1))
}

func TestCollector_CacheMetrics(t *testing.T) {
	collector := NewCollector()

	initialHits := testutil.ToFloat64(cacheHits)
	initialMisses := testutil.ToFloat64(cacheMisses)

	// Record cache operations
	for i := 0; i < 10; i++ {
		collector.RecordCacheHit()
	}

	for i := 0; i < 5; i++ {
		collector.RecordCacheMiss()
	}

	// Verify cache counters increased
	assert.Equal(t, initialHits+10, testutil.ToFloat64(cacheHits))
	assert.Equal(t, initialMisses+5, testutil.ToFloat64(cacheMisses))
}

func TestCollector_ConnectionTracking(t *testing.T) {
	collector := NewCollector()

	initial := testutil.ToFloat64(activeConnections)

	// Simulate connections
	collector.IncrementConnections()
	collector.IncrementConnections()
	collector.IncrementConnections()

	assert.Equal(t, initial+3, testutil.ToFloat64(activeConnections))

	collector.DecrementConnections()

	assert.Equal(t, initial+2, testutil.ToFloat64(activeConnections))
}

func TestCollector_Uptime(t *testing.T) {
	collector := NewCollector()

	// Wait a bit
	time.Sleep(100 * time.Millisecond)

	uptime := collector.Uptime()
	assert.True(t, uptime >= 100*time.Millisecond)
	assert.True(t, uptime < 200*time.Millisecond)
}

func TestMiddleware_RecordsMetrics(t *testing.T) {
	collector := NewCollector()

	// Get initial count
	initial := testutil.ToFloat64(requestsTotal.WithLabelValues("GET", "/bucket/object", "2xx"))

	// Create test handler
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("test response"))
	})

	// Wrap with middleware
	wrapped := Middleware(collector)(handler)

	// Make request
	req := httptest.NewRequest("GET", "/bucket/test-object", nil)
	rec := httptest.NewRecorder()

	wrapped.ServeHTTP(rec, req)

	// Verify response
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "test response", rec.Body.String())

	// Verify metrics increased
	time.Sleep(10 * time.Millisecond) // Give metrics time to update
	assert.Equal(t, initial+1, testutil.ToFloat64(requestsTotal.WithLabelValues("GET", "/bucket/object", "2xx")))
}

func TestMiddleware_TracksErrors(t *testing.T) {
	collector := NewCollector()

	initial := testutil.ToFloat64(errorsTotal.WithLabelValues("server_error", "/bucket"))

	// Create handler that returns error
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("error"))
	})

	wrapped := Middleware(collector)(handler)

	req := httptest.NewRequest("POST", "/bucket", nil)
	rec := httptest.NewRecorder()

	wrapped.ServeHTTP(rec, req)

	// Verify error was recorded
	assert.Equal(t, initial+1, testutil.ToFloat64(errorsTotal.WithLabelValues("server_error", "/bucket")))
}

func TestNormalizePath(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"/", "/"},
		{"/bucket", "/bucket"},
		{"/bucket/", "/bucket"},
		{"/bucket/object", "/bucket/object"},
		{"/bucket/object/key", "/bucket/object"},
		{"/v1/bucket", "/v1"},
		{"", "/"},
	}

	for _, test := range tests {
		t.Run(test.input, func(t *testing.T) {
			result := normalizePath(test.input)
			assert.Equal(t, test.expected, result)
		})
	}
}

func TestCacheMiddleware(t *testing.T) {
	collector := NewCollector()

	t.Run("records cache hit", func(t *testing.T) {
		initial := testutil.ToFloat64(cacheHits)

		handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Cache", "HIT")
			w.WriteHeader(http.StatusOK)
		})

		wrapped := CacheMiddleware(collector)(handler)

		req := httptest.NewRequest("GET", "/test", nil)
		rec := httptest.NewRecorder()

		wrapped.ServeHTTP(rec, req)

		assert.Equal(t, initial+1, testutil.ToFloat64(cacheHits))
	})

	t.Run("records cache miss", func(t *testing.T) {
		initial := testutil.ToFloat64(cacheMisses)

		handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Cache", "MISS")
			w.WriteHeader(http.StatusOK)
		})

		wrapped := CacheMiddleware(collector)(handler)

		req := httptest.NewRequest("GET", "/test", nil)
		rec := httptest.NewRecorder()

		wrapped.ServeHTTP(rec, req)

		assert.Equal(t, initial+1, testutil.ToFloat64(cacheMisses))
	})
}

func TestMetricsIntegration(t *testing.T) {
	collector := NewCollector()

	// Simulate a series of operations
	for i := 0; i < 100; i++ {
		if i%10 == 0 {
			collector.RecordRequest("GET", "/", 200, 10*time.Millisecond, 512, 1024)
		} else if i%5 == 0 {
			collector.RecordRequest("PUT", "/bucket/object", 201, 50*time.Millisecond, 4096, 512)
		} else {
			collector.RecordRequest("GET", "/bucket/object", 200, 20*time.Millisecond, 512, 2048)
		}
	}

	// Just verify we recorded something
	assert.True(t, testutil.ToFloat64(requestsTotal.WithLabelValues("GET", "/", "2xx")) > 0)
	assert.True(t, testutil.ToFloat64(requestsTotal.WithLabelValues("PUT", "/bucket/object", "2xx")) > 0)
	assert.True(t, testutil.ToFloat64(requestsTotal.WithLabelValues("GET", "/bucket/object", "2xx")) > 0)
}
