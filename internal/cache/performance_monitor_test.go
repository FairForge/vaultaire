// internal/cache/performance_monitor_test.go
package cache

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSSDCache_PerformanceMonitoring(t *testing.T) {
	t.Run("tracks_operation_latencies", func(t *testing.T) {
		cache, err := NewSSDCache(1024, 10*1024*1024, t.TempDir())
		require.NoError(t, err)

		// Enable performance monitoring
		cache.EnableMonitoring()

		// Perform operations
		_ = cache.Put("key1", []byte("data"))
		cache.Get("key1")

		// Get metrics
		metrics := cache.GetPerformanceMetrics()

		assert.Greater(t, metrics.PutLatencyP50, time.Duration(0))
		assert.Greater(t, metrics.GetLatencyP50, time.Duration(0))
		assert.Equal(t, int64(1), metrics.PutCount)
		assert.Equal(t, int64(1), metrics.GetCount)
	})

	t.Run("tracks_hit_rates", func(t *testing.T) {
		cache, err := NewSSDCache(1024, 10*1024*1024, t.TempDir())
		require.NoError(t, err)

		cache.EnableMonitoring()

		// Create hits and misses
		_ = cache.Put("exists", []byte("data"))

		cache.Get("exists")     // hit
		cache.Get("not-exists") // miss
		cache.Get("exists")     // hit

		metrics := cache.GetPerformanceMetrics()

		assert.Equal(t, int64(2), metrics.CacheHits)
		assert.Equal(t, int64(1), metrics.CacheMisses)
		assert.InDelta(t, 0.666, metrics.HitRate, 0.01)
	})
}
