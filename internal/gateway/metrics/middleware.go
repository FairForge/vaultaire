// internal/gateway/metrics/middleware.go
package metrics

import (
	"net/http"
	"strings"
	"time"
)

// responseWriter wraps http.ResponseWriter to capture status and size
type responseWriter struct {
	http.ResponseWriter
	status int
	size   int64
}

func (rw *responseWriter) WriteHeader(status int) {
	rw.status = status
	rw.ResponseWriter.WriteHeader(status)
}

func (rw *responseWriter) Write(b []byte) (int, error) {
	n, err := rw.ResponseWriter.Write(b)
	rw.size += int64(n)
	return n, err
}

// Middleware provides metrics collection for HTTP requests
func Middleware(collector *Collector) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()

			// Track active connections
			collector.IncrementConnections()
			defer collector.DecrementConnections()

			// Get request size
			reqSize := r.ContentLength
			if reqSize < 0 {
				reqSize = 0
			}

			// Wrap response writer
			wrapped := &responseWriter{
				ResponseWriter: w,
				status:         200,
			}

			// Extract endpoint (normalize path)
			endpoint := normalizePath(r.URL.Path)

			// Process request
			next.ServeHTTP(wrapped, r)

			// Record metrics
			duration := time.Since(start)
			collector.RecordRequest(
				r.Method,
				endpoint,
				wrapped.status,
				duration,
				reqSize,
				wrapped.size,
			)

			// Record errors if status >= 400
			if wrapped.status >= 400 {
				errorType := "client_error"
				if wrapped.status >= 500 {
					errorType = "server_error"
				}
				collector.RecordError(errorType, endpoint)
			}
		})
	}
}

// normalizePath normalizes the path for metrics
func normalizePath(path string) string {
	// Remove trailing slash
	path = strings.TrimSuffix(path, "/")

	if path == "" {
		return "/"
	}

	parts := strings.Split(path, "/")

	// For paths with 3+ segments starting with /, normalize to /bucket/object
	if len(parts) >= 3 && parts[0] == "" {
		// Special case for /v1/... paths
		if parts[1] == "v1" || parts[1] == "v2" {
			return "/" + parts[1]
		}
		return "/bucket/object"
	}

	// For /bucket paths
	if len(parts) == 2 && parts[0] == "" {
		return path
	}

	return path
}

// CacheMiddleware tracks cache metrics
func CacheMiddleware(collector *Collector) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Check if response includes cache hit header
			next.ServeHTTP(w, r)

			// Check cache status from response header
			if w.Header().Get("X-Cache") == "HIT" {
				collector.RecordCacheHit()
			} else if w.Header().Get("X-Cache") == "MISS" {
				collector.RecordCacheMiss()
			}
		})
	}
}
