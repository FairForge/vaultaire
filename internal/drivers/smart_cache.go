package drivers

import (
	"container/list"
	"sync"
	"time"
)

// CacheEntry represents a cached object
type CacheEntry struct {
	Key         string
	TenantID    string
	Data        []byte
	Size        int64
	AccessTime  time.Time
	AccessCount int64
}

// CacheStats tracks cache performance
type CacheStats struct {
	Hits      int64
	Misses    int64
	Evictions int64
	HitRatio  float64
	Size      int64
	MaxSize   int64
}

// SmartCache implements LRU caching with tenant isolation
type SmartCache struct {
	mu          sync.RWMutex
	maxSize     int64
	currentSize int64
	entries     map[string]*list.Element
	lru         *list.List
	stats       CacheStats
}

// NewSmartCache creates a new smart cache
func NewSmartCache(maxSizeBytes int64) *SmartCache {
	return &SmartCache{
		maxSize: maxSizeBytes,
		entries: make(map[string]*list.Element),
		lru:     list.New(),
		stats:   CacheStats{MaxSize: maxSizeBytes},
	}
}

// Get retrieves an object from cache
func (c *SmartCache) Get(tenantID, key string) []byte {
	c.mu.Lock()
	defer c.mu.Unlock()

	cacheKey := c.buildKey(tenantID, key)

	if elem, exists := c.entries[cacheKey]; exists {
		// Move to front (most recently used)
		c.lru.MoveToFront(elem)
		entry := elem.Value.(*CacheEntry)
		entry.AccessTime = time.Now()
		entry.AccessCount++

		c.stats.Hits++
		c.updateHitRatio()

		// Return a copy to prevent modification
		result := make([]byte, len(entry.Data))
		copy(result, entry.Data)
		return result
	}

	c.stats.Misses++
	c.updateHitRatio()
	return nil
}

// Put stores an object in cache
func (c *SmartCache) Put(tenantID, key string, data []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()

	cacheKey := c.buildKey(tenantID, key)
	size := int64(len(data))

	// Don't cache if larger than max size
	if size > c.maxSize {
		return
	}

	// Check if already exists
	if elem, exists := c.entries[cacheKey]; exists {
		c.lru.MoveToFront(elem)
		oldEntry := elem.Value.(*CacheEntry)
		c.currentSize -= oldEntry.Size

		// Update entry
		oldEntry.Data = data
		oldEntry.Size = size
		oldEntry.AccessTime = time.Now()
		c.currentSize += size
		return
	}

	// Evict if necessary
	for c.currentSize+size > c.maxSize && c.lru.Len() > 0 {
		c.evictLRU()
	}

	// Add new entry
	entry := &CacheEntry{
		Key:         key,
		TenantID:    tenantID,
		Data:        data,
		Size:        size,
		AccessTime:  time.Now(),
		AccessCount: 0,
	}

	elem := c.lru.PushFront(entry)
	c.entries[cacheKey] = elem
	c.currentSize += size
	c.stats.Size = c.currentSize
}

// GetStats returns cache statistics
func (c *SmartCache) GetStats() CacheStats {
	c.mu.RLock()
	defer c.mu.RUnlock()

	stats := c.stats
	stats.Size = c.currentSize
	return stats
}

// Clear removes all entries from cache
func (c *SmartCache) Clear() {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.entries = make(map[string]*list.Element)
	c.lru = list.New()
	c.currentSize = 0
	c.stats.Size = 0
}

// GetTenantUsage returns cache usage for a specific tenant
func (c *SmartCache) GetTenantUsage(tenantID string) int64 {
	c.mu.RLock()
	defer c.mu.RUnlock()

	var usage int64
	for elem := c.lru.Front(); elem != nil; elem = elem.Next() {
		entry := elem.Value.(*CacheEntry)
		if entry.TenantID == tenantID {
			usage += entry.Size
		}
	}
	return usage
}

// Internal methods

func (c *SmartCache) buildKey(tenantID, key string) string {
	return tenantID + ":" + key
}

func (c *SmartCache) evictLRU() {
	elem := c.lru.Back()
	if elem == nil {
		return
	}

	c.lru.Remove(elem)
	entry := elem.Value.(*CacheEntry)
	delete(c.entries, c.buildKey(entry.TenantID, entry.Key))
	c.currentSize -= entry.Size
	c.stats.Evictions++
}

func (c *SmartCache) updateHitRatio() {
	total := c.stats.Hits + c.stats.Misses
	if total > 0 {
		c.stats.HitRatio = float64(c.stats.Hits) / float64(total)
	}
}

// PrefetchPopular pre-loads frequently accessed objects
func (c *SmartCache) PrefetchPopular(threshold int64) []string {
	c.mu.RLock()
	defer c.mu.RUnlock()

	var popular []string
	for elem := c.lru.Front(); elem != nil; elem = elem.Next() {
		entry := elem.Value.(*CacheEntry)
		if entry.AccessCount >= threshold {
			popular = append(popular, entry.Key)
		}
	}
	return popular
}
