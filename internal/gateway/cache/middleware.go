// internal/gateway/cache/middleware.go
package cache

import (
	"bytes"
	"net/http"
	"time"
)

// CacheOptions configures the cache middleware
type CacheOptions struct {
	TTL               time.Duration
	Methods           []string
	InvalidateOnWrite bool
	VaryHeaders       []string
}

// responseRecorder captures the response for caching
type responseRecorder struct {
	http.ResponseWriter
	status int
	body   *bytes.Buffer
}

func (r *responseRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

func (r *responseRecorder) Write(data []byte) (int, error) {
	if r.body != nil {
		r.body.Write(data)
	}
	return r.ResponseWriter.Write(data)
}

// CacheMiddleware creates HTTP caching middleware
func CacheMiddleware(cache *ResponseCache, options CacheOptions) func(http.Handler) http.Handler {
	// Create method lookup map
	methodMap := make(map[string]bool)
	for _, method := range options.Methods {
		methodMap[method] = true
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Only cache configured methods
			if !methodMap[r.Method] {
				next.ServeHTTP(w, r)
				return
			}

			// Generate cache key
			var cacheKey string
			if len(options.VaryHeaders) > 0 {
				cacheKey = cache.GenerateKeyWithVary(r, options.VaryHeaders)
			} else {
				cacheKey = cache.GenerateKey(r)
			}

			// Check for write operations that should invalidate
			if options.InvalidateOnWrite && (r.Method == "POST" || r.Method == "PUT" || r.Method == "DELETE" || r.Method == "PATCH") {
				// Invalidate related cache entries
				cache.InvalidatePattern(r.Context(), "*"+r.URL.Path+"*")
				next.ServeHTTP(w, r)
				return
			}

			// Try to get from cache
			if entry, found := cache.Get(r.Context(), cacheKey); found {
				// Check ETag
				if cache.ValidateETag(r, entry) {
					w.Header().Set("X-Cache", "HIT")
					w.WriteHeader(http.StatusNotModified)
					return
				}

				// Return cached response
				for key, values := range entry.Headers {
					for _, value := range values {
						w.Header().Add(key, value)
					}
				}
				w.Header().Set("X-Cache", "HIT")
				w.Header().Set("ETag", entry.ETag)
				w.WriteHeader(entry.StatusCode)
				if _, err := w.Write(entry.Data); err != nil {
					return
				}
				// Log error but don't fail - response is already committed
				// This is best-effort since headers are already sent
				return
			}

			// Record the response
			recorder := &responseRecorder{
				ResponseWriter: w,
				status:         http.StatusOK,
				body:           &bytes.Buffer{},
			}

			// Set cache miss header
			w.Header().Set("X-Cache", "MISS")

			// Call next handler
			next.ServeHTTP(recorder, r)

			// Cache successful responses
			if recorder.status >= 200 && recorder.status < 300 {
				data := recorder.body.Bytes()
				entry := &CacheEntry{
					Data:       data,
					Headers:    recorder.Header().Clone(),
					StatusCode: recorder.status,
					ETag:       cache.GenerateETag(data),
					CachedAt:   time.Now(),
				}

				cache.Set(r.Context(), cacheKey, entry, options.TTL)
			}
		})
	}
}
