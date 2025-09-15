package cache

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSSDCache_AccessTracking(t *testing.T) {
	t.Run("tracks_access_frequency", func(t *testing.T) {
		// Arrange
		cache, err := NewSSDCache(1024, 10*1024*1024, t.TempDir())
		require.NoError(t, err)

		// Act - Access same key multiple times
		err = cache.Put("hot-key", []byte("data"))
		require.NoError(t, err)

		for i := 0; i < 5; i++ {
			cache.Get("hot-key")
		}

		// Assert - Should be marked as hot
		assert.True(t, cache.IsHot("hot-key"))
	})

	t.Run("identifies_cold_data", func(t *testing.T) {
		// Arrange
		cache, err := NewSSDCache(1024, 10*1024*1024, t.TempDir())
		require.NoError(t, err)

		// Act - Put data but don't access it
		err = cache.Put("cold-key", []byte("data"))
		require.NoError(t, err)

		// Assert - Should not be hot
		assert.False(t, cache.IsHot("cold-key"))
	})
}
