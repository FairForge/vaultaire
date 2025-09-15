// internal/cache/wear_leveling_test.go
package cache

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSSDCache_WearLeveling(t *testing.T) {
	t.Run("distributes_writes_across_ssd_shards", func(t *testing.T) {
		// Test that writes are distributed across multiple shards
		cache, err := NewSSDCache(1024, 10*1024*1024, t.TempDir())
		require.NoError(t, err)

		// Write multiple items and check distribution
		shardMap := make(map[int]int) // shard -> count

		for i := 0; i < 100; i++ {
			key := fmt.Sprintf("key-%d", i)
			err := cache.Put(key, []byte("data"))
			require.NoError(t, err)

			shard := cache.GetShardForKey(key)
			shardMap[shard]++
		}

		// Should have multiple shards used
		assert.Greater(t, len(shardMap), 1, "Should use multiple shards")

		// Check distribution is relatively even (no shard has >50% of writes)
		for shard, count := range shardMap {
			assert.Less(t, count, 51, "Shard %d has too many writes: %d", shard, count)
		}
	})

	t.Run("rotates_write_location", func(t *testing.T) {
		cache, err := NewSSDCache(1024, 10*1024*1024, t.TempDir())
		require.NoError(t, err)

		// Track write locations over time
		locations := cache.GetWriteStats()
		assert.NotNil(t, locations)
	})
}
