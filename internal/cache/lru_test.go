package cache

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLRUCache_Basic(t *testing.T) {
	t.Run("put and get item", func(t *testing.T) {
		// Arrange
		cache := NewLRU(3) // capacity of 3 items
		ctx := context.Background()

		// Act
		err := cache.Put(ctx, "container1", "artifact1", strings.NewReader("data1"), 5)
		require.NoError(t, err)

		reader, hit, err := cache.Get(ctx, "container1", "artifact1")

		// Assert
		require.NoError(t, err)
		assert.True(t, hit, "should be a cache hit")

		data, err := io.ReadAll(reader)
		require.NoError(t, err)
		assert.Equal(t, "data1", string(data))
	})

	t.Run("cache miss returns false", func(t *testing.T) {
		// Arrange
		cache := NewLRU(3)
		ctx := context.Background()

		// Act
		_, hit, err := cache.Get(ctx, "container1", "missing")

		// Assert
		require.NoError(t, err)
		assert.False(t, hit, "should be a cache miss")
	})

	t.Run("evicts least recently used", func(t *testing.T) {
		// Arrange
		cache := NewLRU(2) // capacity of only 2
		ctx := context.Background()

		// Add 3 items to a cache of size 2
		require.NoError(t, cache.Put(ctx, "c", "a1", strings.NewReader("data1"), 5))
		require.NoError(t, cache.Put(ctx, "c", "a2", strings.NewReader("data2"), 5))
		require.NoError(t, cache.Put(ctx, "c", "a3", strings.NewReader("data3"), 5))

		// Act - a1 should be evicted (LRU)
		_, hit1, _ := cache.Get(ctx, "c", "a1")
		_, hit2, _ := cache.Get(ctx, "c", "a2")
		_, hit3, _ := cache.Get(ctx, "c", "a3")

		// Assert
		assert.False(t, hit1, "a1 should be evicted")
		assert.True(t, hit2, "a2 should still be in cache")
		assert.True(t, hit3, "a3 should still be in cache")
	})

	t.Run("accessing item moves to front", func(t *testing.T) {
		// Arrange
		cache := NewLRU(2)
		ctx := context.Background()

		require.NoError(t, cache.Put(ctx, "c", "a1", strings.NewReader("data1"), 5))
		require.NoError(t, cache.Put(ctx, "c", "a2", strings.NewReader("data2"), 5))

		// Access a1 to move it to front
		_, _, _ = cache.Get(ctx, "c", "a1")

		// Add a3, which should evict a2 (not a1)
		require.NoError(t, cache.Put(ctx, "c", "a3", strings.NewReader("data3"), 5))

		// Act
		_, hit1, _ := cache.Get(ctx, "c", "a1")
		_, hit2, _ := cache.Get(ctx, "c", "a2")
		_, hit3, _ := cache.Get(ctx, "c", "a3")

		// Assert
		assert.True(t, hit1, "a1 should still be in cache (was accessed)")
		assert.False(t, hit2, "a2 should be evicted")
		assert.True(t, hit3, "a3 should be in cache")
	})

	t.Run("delete removes from cache", func(t *testing.T) {
		// Arrange
		cache := NewLRU(3)
		ctx := context.Background()

		require.NoError(t, cache.Put(ctx, "c", "a1", strings.NewReader("data1"), 5))

		// Act
		err := cache.Delete(ctx, "c", "a1")
		require.NoError(t, err)

		_, hit, _ := cache.Get(ctx, "c", "a1")

		// Assert
		assert.False(t, hit, "deleted item should not be in cache")
	})

	t.Run("concurrent access is safe", func(t *testing.T) {
		// Arrange
		cache := NewLRU(100)
		ctx := context.Background()
		done := make(chan bool)

		// Act - concurrent writes and reads
		for i := 0; i < 10; i++ {
			go func(id int) {
				key := string(rune('a' + id))
				_ = cache.Put(ctx, "c", key, strings.NewReader(key), 1)
				_, _, _ = cache.Get(ctx, "c", key)
				done <- true
			}(i)
		}

		// Wait for all goroutines
		for i := 0; i < 10; i++ {
			<-done
		}

		// Assert - should complete without panic
		stats := cache.Stats()
		assert.True(t, stats.Items > 0, "should have cached items")
	})
}

func TestLRUCache_Stats(t *testing.T) {
	t.Run("tracks hit and miss rates", func(t *testing.T) {
		// Arrange
		cache := NewLRU(2)
		ctx := context.Background()

		_ = cache.Put(ctx, "c", "a1", strings.NewReader("data"), 4)

		// Act
		_, _, _ = cache.Get(ctx, "c", "a1")   // hit
		_, _, _ = cache.Get(ctx, "c", "a1")   // hit
		_, _, _ = cache.Get(ctx, "c", "miss") // miss

		stats := cache.Stats()

		// Assert
		assert.Equal(t, int64(2), stats.Hits)
		assert.Equal(t, int64(1), stats.Misses)
		assert.Equal(t, float64(2.0/3.0), stats.HitRate())
	})
}
