package cache

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestMetricsTracker(t *testing.T) {
	t.Run("tracks hit rate", func(t *testing.T) {
		tracker := NewMetricsTracker()

		// Record some hits and misses
		tracker.RecordHit("key1", 1*time.Millisecond, 100)
		tracker.RecordHit("key1", 1*time.Millisecond, 100)
		tracker.RecordMiss("key2", 1*time.Millisecond)

		hitRate := tracker.GetHitRate()
		assert.Equal(t, float64(2.0/3.0), hitRate)
	})

	t.Run("tracks top keys", func(t *testing.T) {
		tracker := NewMetricsTracker()

		// Access keys different amounts
		for i := 0; i < 5; i++ {
			tracker.RecordHit("popular", 1*time.Millisecond, 100)
		}
		for i := 0; i < 2; i++ {
			tracker.RecordHit("medium", 1*time.Millisecond, 100)
		}
		tracker.RecordHit("rare", 1*time.Millisecond, 100)

		top := tracker.GetTopKeys(2)
		assert.Equal(t, []string{"popular", "medium"}, top)
	})
}

func TestHotDataTracker(t *testing.T) {
	t.Run("identifies hot data", func(t *testing.T) {
		tracker := &HotDataTracker{
			accessCounts: make(map[string]int64),
			lastAccess:   make(map[string]time.Time),
			threshold:    3,
		}

		// Access key multiple times
		for i := 0; i < 5; i++ {
			tracker.RecordAccess("hot-key")
		}
		tracker.RecordAccess("cold-key")

		assert.True(t, tracker.IsHot("hot-key"))
		assert.False(t, tracker.IsHot("cold-key"))
	})
}

func TestEvictionPolicies(t *testing.T) {
	t.Run("LRU selects oldest", func(t *testing.T) {
		policy := &LRUPolicy{}
		items := map[string]*CacheItem{
			"old": {
				LastAccessed: time.Now().Add(-1 * time.Hour),
			},
			"new": {
				LastAccessed: time.Now(),
			},
		}

		victim := policy.SelectVictim(items)
		assert.Equal(t, "old", victim)
	})

	t.Run("LFU selects least frequent", func(t *testing.T) {
		policy := &LFUPolicy{
			accessCounts: map[string]int64{
				"popular": 10,
				"rare":    1,
			},
		}
		items := map[string]*CacheItem{
			"popular": {},
			"rare":    {},
		}

		victim := policy.SelectVictim(items)
		assert.Equal(t, "rare", victim)
	})
}

func TestCacheManager(t *testing.T) {
	t.Run("integrates all features", func(t *testing.T) {
		manager := NewCacheManager(1024 * 1024) // 1MB
		ctx := context.Background()

		// Use the cache
		_, hit, err := manager.Get(ctx, "test", "artifact")
		assert.NoError(t, err)
		assert.False(t, hit)

		// Check metrics were recorded
		hitRate := manager.metrics.GetHitRate()
		assert.Equal(t, float64(0), hitRate) // All misses
	})
}
