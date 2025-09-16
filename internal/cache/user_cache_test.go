// internal/cache/user_cache_test.go
package cache

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUserCacheProfile_Creation(t *testing.T) {
	cache, _ := NewSSDCache(1024*1024, 10*1024*1024, t.TempDir())
	manager := NewUserAwareCacheManager(cache, 100*1024) // 100KB per user

	profile := manager.GetUserProfile("user1")
	require.NotNil(t, profile)
	assert.Equal(t, "user1", profile.UserID)
	assert.Equal(t, int64(100*1024), profile.CacheQuota)
}

func TestUserCacheProfile_TrackAccess(t *testing.T) {
	cache, _ := NewSSDCache(1024*1024, 10*1024*1024, t.TempDir())
	manager := NewUserAwareCacheManager(cache, 100*1024)

	// Track multiple accesses
	manager.TrackUserAccess("user1", "file1.txt", 1024)
	manager.TrackUserAccess("user1", "file2.txt", 2048)
	manager.TrackUserAccess("user1", "file3.txt", 1024)

	profile := manager.GetUserProfile("user1")
	assert.Contains(t, profile.HotKeys, "file3.txt")
	assert.Contains(t, profile.HotKeys, "file2.txt")

	// Verify proper average: (1024 + 2048 + 1024) / 3 = 1365
	assert.Equal(t, int64(1365), profile.AvgObjectSize)
	assert.Equal(t, int64(3), profile.AccessCount)
	assert.Equal(t, int64(4096), profile.TotalSize)
}

func TestUserCacheQuota(t *testing.T) {
	cache, _ := NewSSDCache(1024*1024, 10*1024*1024, t.TempDir())
	manager := NewUserAwareCacheManager(cache, 10*1024) // 10KB limit

	// Should allow caching within quota
	assert.True(t, manager.ShouldCacheForUser("user1", 5*1024))

	// Simulate usage
	profile := manager.GetUserProfile("user1")
	profile.CurrentUsage = 8 * 1024

	// Should not allow exceeding quota
	assert.False(t, manager.ShouldCacheForUser("user1", 5*1024))

	// Should allow small item that fits
	assert.True(t, manager.ShouldCacheForUser("user1", 1024))
}
