// internal/cache/promotion_policy_test.go
package cache

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSSDCache_PromotionPolicies(t *testing.T) {
	t.Run("promotes_based_on_frequency_threshold", func(t *testing.T) {
		cache, err := NewSSDCache(1024, 10*1024*1024, t.TempDir())
		require.NoError(t, err)

		// Configure promotion policy
		policy := &PromotionPolicy{
			FrequencyThreshold: 3,
			TimeWindow:         time.Minute,
		}
		cache.SetPromotionPolicy(policy)

		// Put item in cache (will go to memory first)
		err = cache.Put("test-key", []byte("data"))
		require.NoError(t, err)

		// Force demotion by filling memory
		for i := 0; i < 10; i++ {
			_ = cache.Put(fmt.Sprintf("filler-%d", i), make([]byte, 200))
		}

		// Access the demoted item multiple times
		for i := 0; i < 4; i++ {
			cache.Get("test-key")
		}

		// Should be back in memory
		memKeys := cache.GetMemoryKeys()
		assert.Contains(t, memKeys, "test-key")
	})

	t.Run("demotes_based_on_age", func(t *testing.T) {
		cache, err := NewSSDCache(1024, 10*1024*1024, t.TempDir())
		require.NoError(t, err)

		policy := &DemotionPolicy{
			MaxAge: time.Second,
		}
		cache.SetDemotionPolicy(policy)

		// Put item and wait
		_ = cache.Put("old-key", []byte("data"))
		time.Sleep(time.Second * 2)

		// Trigger cleanup
		cache.ApplyDemotionPolicy()

		// Should be on SSD now
		assert.NotContains(t, cache.GetMemoryKeys(), "old-key")
	})
}
