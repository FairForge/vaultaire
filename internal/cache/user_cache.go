// internal/cache/user_cache.go
package cache

import (
	"sync"
	"time"
)

// UserCacheProfile tracks per-user cache behavior
type UserCacheProfile struct {
	UserID          string
	HotKeys         []string
	AvgObjectSize   int64
	TotalSize       int64 // Track total size for proper averaging
	AccessCount     int64 // Track access count for proper averaging
	AccessFrequency float64
	LastAccess      time.Time
	CacheQuota      int64 // Bytes allocated to this user
	CurrentUsage    int64
}

// UserAwareCacheManager manages user-specific caching
type UserAwareCacheManager struct {
	mu          sync.RWMutex
	profiles    map[string]*UserCacheProfile
	globalCache *SSDCache
	maxPerUser  int64
}

// NewUserAwareCacheManager creates a user-aware cache manager
func NewUserAwareCacheManager(cache *SSDCache, maxPerUser int64) *UserAwareCacheManager {
	return &UserAwareCacheManager{
		profiles:    make(map[string]*UserCacheProfile),
		globalCache: cache,
		maxPerUser:  maxPerUser,
	}
}

// GetUserProfile returns or creates a user's cache profile
func (u *UserAwareCacheManager) GetUserProfile(userID string) *UserCacheProfile {
	u.mu.Lock()
	defer u.mu.Unlock()

	if profile, exists := u.profiles[userID]; exists {
		profile.LastAccess = time.Now()
		return profile
	}

	// Create new profile
	profile := &UserCacheProfile{
		UserID:     userID,
		HotKeys:    make([]string, 0),
		CacheQuota: u.maxPerUser,
		LastAccess: time.Now(),
	}
	u.profiles[userID] = profile
	return profile
}

// TrackUserAccess records a user's data access
func (u *UserAwareCacheManager) TrackUserAccess(userID, key string, size int64) {
	profile := u.GetUserProfile(userID)

	u.mu.Lock()
	defer u.mu.Unlock()

	// Update hot keys (keep last 10)
	profile.HotKeys = append([]string{key}, profile.HotKeys...)
	if len(profile.HotKeys) > 10 {
		profile.HotKeys = profile.HotKeys[:10]
	}

	// Proper averaging
	profile.TotalSize += size
	profile.AccessCount++
	if profile.AccessCount > 0 {
		profile.AvgObjectSize = profile.TotalSize / profile.AccessCount
	}

	profile.AccessFrequency++
}

// ShouldCacheForUser determines if data should be cached for this user
func (u *UserAwareCacheManager) ShouldCacheForUser(userID string, size int64) bool {
	profile := u.GetUserProfile(userID)

	u.mu.RLock()
	defer u.mu.RUnlock()

	// Check if user has quota
	if profile.CurrentUsage+size > profile.CacheQuota {
		return false
	}

	return true
}
