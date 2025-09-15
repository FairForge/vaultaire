package cache

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSSDCache_TieredStorage(t *testing.T) {
	t.Run("initializes_with_memory_and_ssd_tiers", func(t *testing.T) {
		// Arrange
		tmpDir := t.TempDir()
		memSize := int64(1024 * 1024)      // 1MB memory
		ssdSize := int64(10 * 1024 * 1024) // 10MB SSD

		// Act
		cache, err := NewSSDCache(memSize, ssdSize, tmpDir)

		// Assert
		require.NoError(t, err)
		assert.NotNil(t, cache)
		assert.DirExists(t, tmpDir)

		stats := cache.Stats()
		assert.Equal(t, int64(0), stats["memory_used"].(int64))
		assert.Equal(t, int64(0), stats["ssd_used"].(int64))
		assert.Equal(t, memSize, stats["memory_capacity"].(int64))
		assert.Equal(t, ssdSize, stats["ssd_capacity"].(int64))
	})

	t.Run("stores_in_memory_tier_first", func(t *testing.T) {
		// Arrange
		tmpDir := t.TempDir()
		cache, err := NewSSDCache(1024*1024, 10*1024*1024, tmpDir)
		require.NoError(t, err)

		key := "test-key"
		data := []byte("test data")

		// Act
		err = cache.Put(key, data)
		require.NoError(t, err)

		// Assert
		retrieved, ok := cache.Get(key)
		assert.True(t, ok)
		assert.Equal(t, data, retrieved)

		stats := cache.Stats()
		assert.Greater(t, stats["memory_used"].(int64), int64(0))
		assert.Equal(t, int64(0), stats["ssd_used"].(int64))
	})

	t.Run("demotes_to_ssd_on_memory_pressure", func(t *testing.T) {
		// Arrange
		tmpDir := t.TempDir()
		cache, err := NewSSDCache(1024, 10*1024*1024, tmpDir) // Small memory
		require.NoError(t, err)

		// Act - Fill memory beyond capacity
		for i := 0; i < 10; i++ {
			key := fmt.Sprintf("key-%d", i)
			data := make([]byte, 200) // Each item is 200 bytes
			err := cache.Put(key, data)
			require.NoError(t, err)
		}

		// Assert - Some data should be on SSD
		stats := cache.Stats()
		assert.Greater(t, stats["ssd_used"].(int64), int64(0))
		assert.Less(t, stats["memory_used"].(int64), int64(2000)) // Not all in memory

		// Verify we can still retrieve all items
		for i := 0; i < 10; i++ {
			key := fmt.Sprintf("key-%d", i)
			_, ok := cache.Get(key)
			assert.True(t, ok, "Should retrieve %s", key)
		}
	})
}
