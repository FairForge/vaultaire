package cache

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSizedLRU(t *testing.T) {
	t.Run("respects byte size limit", func(t *testing.T) {
		// Arrange - cache with 100 byte limit
		cache := NewSizedLRU(100)
		ctx := context.Background()

		// Add 50 bytes
		data1 := strings.Repeat("a", 50)
		require.NoError(t, cache.Put(ctx, "c", "a1", strings.NewReader(data1), 50))

		// Add another 50 bytes (total 100)
		data2 := strings.Repeat("b", 50)
		require.NoError(t, cache.Put(ctx, "c", "a2", strings.NewReader(data2), 50))

		// Add 30 more bytes - should evict a1
		data3 := strings.Repeat("c", 30)
		require.NoError(t, cache.Put(ctx, "c", "a3", strings.NewReader(data3), 30))

		// Act
		_, hit1, _ := cache.Get(ctx, "c", "a1")
		_, hit2, _ := cache.Get(ctx, "c", "a2")
		_, hit3, _ := cache.Get(ctx, "c", "a3")

		// Assert
		assert.False(t, hit1, "a1 should be evicted")
		assert.True(t, hit2, "a2 should remain")
		assert.True(t, hit3, "a3 should remain")

		stats := cache.Stats()
		assert.LessOrEqual(t, stats.CurrentBytes, int64(100))
	})

	t.Run("rejects items larger than max size", func(t *testing.T) {
		// Arrange
		cache := NewSizedLRU(100)
		ctx := context.Background()

		// Try to add 200 bytes to 100 byte cache
		data := strings.Repeat("x", 200)

		// Act
		err := cache.Put(ctx, "c", "huge", strings.NewReader(data), 200)

		// Assert
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "exceeds max cache size")
	})

	t.Run("tracks memory usage", func(t *testing.T) {
		// Arrange
		cache := NewSizedLRU(1024 * 1024) // 1MB
		ctx := context.Background()

		// Add 512KB
		data := make([]byte, 512*1024)
		require.NoError(t, cache.Put(ctx, "c", "half", bytes.NewReader(data), int64(len(data))))

		// Act
		stats := cache.Stats()

		// Assert
		assert.Equal(t, int64(512*1024), stats.CurrentBytes)
		assert.Equal(t, float64(50), stats.MemoryUsage())
	})
}

func TestSizedLRU_256GB(t *testing.T) {
	t.Run("handles 256GB configuration", func(t *testing.T) {
		// Arrange - 256GB cache
		cache := NewSizedLRU(256 * 1024 * 1024 * 1024)

		// Act
		stats := cache.Stats()

		// Assert
		assert.Equal(t, int64(256*1024*1024*1024), stats.MaxBytes)
		assert.Equal(t, int64(0), stats.CurrentBytes)
		assert.Equal(t, float64(0), stats.MemoryUsage())
	})
}
