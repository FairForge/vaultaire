// internal/gateway/cache/response_cache_test.go
package cache

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResponseCache_GenerateKey(t *testing.T) {
	tests := []struct {
		name    string
		method  string
		path    string
		query   string
		headers map[string]string
		wantKey string
	}{
		{
			name:    "simple GET request",
			method:  "GET",
			path:    "/v1/containers/test",
			query:   "",
			wantKey: "GET:/v1/containers/test",
		},
		{
			name:    "GET with query params",
			method:  "GET",
			path:    "/v1/artifacts",
			query:   "limit=10&offset=20",
			wantKey: "GET:/v1/artifacts?limit=10&offset=20",
		},
		{
			name:    "includes tenant in key",
			method:  "GET",
			path:    "/v1/data",
			headers: map[string]string{"X-Tenant-ID": "tenant-123"},
			wantKey: "tenant-123:GET:/v1/data",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cache := NewResponseCache(DefaultConfig())
			req := httptest.NewRequest(tt.method, tt.path+"?"+tt.query, nil)

			for k, v := range tt.headers {
				req.Header.Set(k, v)
			}

			key := cache.GenerateKey(req)
			assert.Equal(t, tt.wantKey, key)
		})
	}
}

func TestResponseCache_SetAndGet(t *testing.T) {
	cache := NewResponseCache(DefaultConfig())
	ctx := context.Background()

	t.Run("cache miss returns nil", func(t *testing.T) {
		entry, found := cache.Get(ctx, "nonexistent")
		assert.False(t, found)
		assert.Nil(t, entry)
	})

	t.Run("cache hit returns data", func(t *testing.T) {
		key := "test-key"
		data := []byte("test response data")
		headers := http.Header{
			"Content-Type": []string{"application/json"},
			"X-Custom":     []string{"value"},
		}

		entry := &CacheEntry{
			Data:       data,
			Headers:    headers,
			StatusCode: 200,
			ETag:       `"abc123"`,
			CachedAt:   time.Now(),
		}

		cache.Set(ctx, key, entry, 5*time.Minute)

		retrieved, found := cache.Get(ctx, key)
		require.True(t, found)
		assert.Equal(t, data, retrieved.Data)
		assert.Equal(t, headers, retrieved.Headers)
		assert.Equal(t, 200, retrieved.StatusCode)
		assert.Equal(t, `"abc123"`, retrieved.ETag)
	})

	t.Run("expired entries are not returned", func(t *testing.T) {
		key := "expire-test"
		entry := &CacheEntry{
			Data:     []byte("expires quickly"),
			CachedAt: time.Now(),
		}

		cache.Set(ctx, key, entry, 10*time.Millisecond)
		time.Sleep(20 * time.Millisecond)

		retrieved, found := cache.Get(ctx, key)
		assert.False(t, found)
		assert.Nil(t, retrieved)
	})
}

func TestResponseCache_Invalidation(t *testing.T) {
	cache := NewResponseCache(DefaultConfig())
	ctx := context.Background()

	t.Run("invalidate by key", func(t *testing.T) {
		key := "test-key"
		entry := &CacheEntry{Data: []byte("data")}

		cache.Set(ctx, key, entry, 5*time.Minute)
		_, found := cache.Get(ctx, key)
		assert.True(t, found)

		cache.Invalidate(ctx, key)
		_, found = cache.Get(ctx, key)
		assert.False(t, found)
	})

	t.Run("invalidate by pattern", func(t *testing.T) {
		// Set multiple entries
		cache.Set(ctx, "tenant-1:GET:/api/v1", &CacheEntry{Data: []byte("1")}, 5*time.Minute)
		cache.Set(ctx, "tenant-1:GET:/api/v2", &CacheEntry{Data: []byte("2")}, 5*time.Minute)
		cache.Set(ctx, "tenant-2:GET:/api/v1", &CacheEntry{Data: []byte("3")}, 5*time.Minute)

		// Invalidate all tenant-1 entries
		cache.InvalidatePattern(ctx, "tenant-1:*")

		_, found := cache.Get(ctx, "tenant-1:GET:/api/v1")
		assert.False(t, found)
		_, found = cache.Get(ctx, "tenant-1:GET:/api/v2")
		assert.False(t, found)
		_, found = cache.Get(ctx, "tenant-2:GET:/api/v1")
		assert.True(t, found)
	})
}

func TestResponseCache_ETags(t *testing.T) {
	cache := NewResponseCache(DefaultConfig())
	ctx := context.Background()

	t.Run("generates ETag from content", func(t *testing.T) {
		data := []byte("test content")
		etag := cache.GenerateETag(data)
		assert.NotEmpty(t, etag)
		assert.Contains(t, etag, `"`)
	})

	t.Run("validates If-None-Match", func(t *testing.T) {
		key := "etag-test"
		data := []byte("content")
		etag := cache.GenerateETag(data)

		entry := &CacheEntry{
			Data:     data,
			ETag:     etag,
			CachedAt: time.Now(),
		}
		cache.Set(ctx, key, entry, 5*time.Minute)

		// Request with matching ETag should return 304
		req := httptest.NewRequest("GET", "/test", nil)
		req.Header.Set("If-None-Match", etag)

		cached, found := cache.Get(ctx, key)
		require.True(t, found)

		shouldReturn304 := cache.ValidateETag(req, cached)
		assert.True(t, shouldReturn304)
	})
}

func TestResponseCache_VaryHeaders(t *testing.T) {
	cache := NewResponseCache(DefaultConfig())

	t.Run("generates different keys for Vary headers", func(t *testing.T) {
		req1 := httptest.NewRequest("GET", "/api", nil)
		req1.Header.Set("Accept", "application/json")

		req2 := httptest.NewRequest("GET", "/api", nil)
		req2.Header.Set("Accept", "application/xml")

		// With Vary on Accept
		key1 := cache.GenerateKeyWithVary(req1, []string{"Accept"})
		key2 := cache.GenerateKeyWithVary(req2, []string{"Accept"})

		assert.NotEqual(t, key1, key2)
	})

	t.Run("ignores non-vary headers", func(t *testing.T) {
		req1 := httptest.NewRequest("GET", "/api", nil)
		req1.Header.Set("Accept", "application/json")
		req1.Header.Set("User-Agent", "Mozilla")

		req2 := httptest.NewRequest("GET", "/api", nil)
		req2.Header.Set("Accept", "application/json")
		req2.Header.Set("User-Agent", "Chrome")

		// Only vary on Accept
		key1 := cache.GenerateKeyWithVary(req1, []string{"Accept"})
		key2 := cache.GenerateKeyWithVary(req2, []string{"Accept"})

		assert.Equal(t, key1, key2)
	})
}

func TestCacheMiddleware(t *testing.T) {
	cache := NewResponseCache(DefaultConfig())

	t.Run("caches successful GET responses", func(t *testing.T) {
		handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"result":"success"}`))
		})

		middleware := CacheMiddleware(cache, CacheOptions{
			TTL:     5 * time.Minute,
			Methods: []string{"GET"},
		})

		testHandler := middleware(handler)

		// First request - should hit handler
		req1 := httptest.NewRequest("GET", "/api/data", nil)
		rec1 := httptest.NewRecorder()
		testHandler.ServeHTTP(rec1, req1)

		assert.Equal(t, http.StatusOK, rec1.Code)
		assert.Equal(t, `{"result":"success"}`, rec1.Body.String())
		assert.Equal(t, "MISS", rec1.Header().Get("X-Cache"))

		// Second request - should hit cache
		req2 := httptest.NewRequest("GET", "/api/data", nil)
		rec2 := httptest.NewRecorder()
		testHandler.ServeHTTP(rec2, req2)

		assert.Equal(t, http.StatusOK, rec2.Code)
		assert.Equal(t, `{"result":"success"}`, rec2.Body.String())
		assert.Equal(t, "HIT", rec2.Header().Get("X-Cache"))
	})

	t.Run("does not cache POST requests", func(t *testing.T) {
		callCount := 0
		handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			callCount++
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("ok"))
		})

		middleware := CacheMiddleware(cache, CacheOptions{
			TTL:     5 * time.Minute,
			Methods: []string{"GET"},
		})

		testHandler := middleware(handler)

		// Two POST requests should both hit handler
		for i := 0; i < 2; i++ {
			req := httptest.NewRequest("POST", "/api/data", bytes.NewReader([]byte("{}")))
			rec := httptest.NewRecorder()
			testHandler.ServeHTTP(rec, req)
		}

		assert.Equal(t, 2, callCount)
	})

	t.Run("invalidates cache on write operations", func(t *testing.T) {
		handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == "DELETE" {
				// Invalidate related cache entries
				cache.InvalidatePattern(r.Context(), "*"+r.URL.Path+"*")
			}
			w.WriteHeader(http.StatusOK)
		})

		middleware := CacheMiddleware(cache, CacheOptions{
			TTL:               5 * time.Minute,
			Methods:           []string{"GET"},
			InvalidateOnWrite: true,
		})

		testHandler := middleware(handler)

		// Cache a GET response
		getReq := httptest.NewRequest("GET", "/api/item/123", nil)
		getRec := httptest.NewRecorder()
		testHandler.ServeHTTP(getRec, getReq)

		// DELETE should invalidate
		deleteReq := httptest.NewRequest("DELETE", "/api/item/123", nil)
		deleteRec := httptest.NewRecorder()
		testHandler.ServeHTTP(deleteRec, deleteReq)

		// Next GET should miss cache
		getReq2 := httptest.NewRequest("GET", "/api/item/123", nil)
		getRec2 := httptest.NewRecorder()
		testHandler.ServeHTTP(getRec2, getReq2)

		assert.Equal(t, "MISS", getRec2.Header().Get("X-Cache"))
	})
}

// Benchmark
func BenchmarkResponseCache_Get(b *testing.B) {
	cache := NewResponseCache(DefaultConfig())
	ctx := context.Background()

	// Pre-populate cache
	for i := 0; i < 1000; i++ {
		key := fmt.Sprintf("key-%d", i)
		cache.Set(ctx, key, &CacheEntry{
			Data: []byte("test data"),
		}, 5*time.Minute)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		key := fmt.Sprintf("key-%d", i%1000)
		cache.Get(ctx, key)
	}
}
