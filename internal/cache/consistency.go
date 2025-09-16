// internal/cache/consistency.go
package cache

import (
	"sync"
	"time"
)

// ConsistencyLevel defines cache consistency guarantees
type ConsistencyLevel int

const (
	EventualConsistency ConsistencyLevel = iota
	StrongConsistency
	BoundedStaleness
)

// CacheVersion tracks cache entry versions
type CacheVersion struct {
	Key       string
	Version   int64
	Timestamp time.Time
	Checksum  string
	Source    string
}

// ConsistencyManager ensures cache consistency across nodes
type ConsistencyManager struct {
	mu            sync.RWMutex
	versions      map[string]*CacheVersion
	invalidations chan string
	level         ConsistencyLevel
	maxStaleness  time.Duration
}

// NewConsistencyManager creates a consistency controller
func NewConsistencyManager(level ConsistencyLevel) *ConsistencyManager {
	return &ConsistencyManager{
		versions:      make(map[string]*CacheVersion),
		invalidations: make(chan string, 1000),
		level:         level,
		maxStaleness:  5 * time.Minute,
	}
}

// ValidateConsistency checks if cached data is consistent
func (c *ConsistencyManager) ValidateConsistency(key string, localVersion int64) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()

	version, exists := c.versions[key]
	if !exists {
		// Handle non-existent keys based on consistency level
		if c.level == EventualConsistency {
			return true // Will sync eventually
		}
		return false
	}

	switch c.level {
	case StrongConsistency:
		return localVersion == version.Version
	case BoundedStaleness:
		return time.Since(version.Timestamp) < c.maxStaleness
	case EventualConsistency:
		return true // Always valid, will sync eventually
	default:
		return false
	}
}

// Invalidate marks cache entry as invalid
func (c *ConsistencyManager) Invalidate(key string) {
	c.mu.Lock()
	version := c.versions[key]
	if version != nil {
		version.Version++
		version.Timestamp = time.Now()
	}
	c.mu.Unlock()

	// Broadcast invalidation
	select {
	case c.invalidations <- key:
	default:
		// Channel full, skip
	}
}

// UpdateVersion updates the version info for a key
func (c *ConsistencyManager) UpdateVersion(key string, version int64, checksum string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.versions[key] = &CacheVersion{
		Key:       key,
		Version:   version,
		Timestamp: time.Now(),
		Checksum:  checksum,
	}
}
