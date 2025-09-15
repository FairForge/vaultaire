package cache

import (
	"container/list"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// AccessPattern tracks access patterns for cache items
type AccessPattern struct {
	Count      int
	LastAccess time.Time
	Window     time.Duration
}

// SSDCache implements a tiered cache with memory and SSD storage
type SSDCache struct {
	// Memory tier
	memMu        sync.RWMutex
	memMaxBytes  int64
	memCurrBytes int64
	memItems     map[string]*list.Element
	memLRU       *list.List

	// SSD tier
	ssdPath     string
	maxSSDSize  int64
	currentSize int64

	// Wear leveling
	shardCount   int
	writeCounter int64
	shardMu      sync.RWMutex
	shardWrites  map[int]int64 // Track writes per shard

	// Access tracking
	accessMu  sync.RWMutex
	accessLog map[string]*AccessPattern

	mu    sync.RWMutex
	index map[string]*SSDEntry
}

// SSDEntry represents an item stored on SSD
type SSDEntry struct {
	Key        string
	Size       int64
	Path       string
	AccessTime time.Time
}

// ssdMemItem represents an item in memory cache
type ssdMemItem struct {
	key  string
	data []byte
	size int64
}

// NewSSDCache creates a new SSD-backed cache
func NewSSDCache(memSize, ssdSize int64, ssdPath string) (*SSDCache, error) {
	// Create SSD cache directory
	if err := os.MkdirAll(ssdPath, 0755); err != nil {
		return nil, fmt.Errorf("failed to create SSD cache dir: %w", err)
	}

	// Create shard directories for wear leveling
	shardCount := 8 // Use 8 shards by default
	for i := 0; i < shardCount; i++ {
		shardPath := filepath.Join(ssdPath, fmt.Sprintf("shard-%d", i))
		if err := os.MkdirAll(shardPath, 0755); err != nil {
			return nil, fmt.Errorf("failed to create shard dir: %w", err)
		}
	}

	return &SSDCache{
		memMaxBytes: memSize,
		memItems:    make(map[string]*list.Element),
		memLRU:      list.New(),
		ssdPath:     ssdPath,
		maxSSDSize:  ssdSize,
		index:       make(map[string]*SSDEntry),
		accessLog:   make(map[string]*AccessPattern),
		shardCount:  shardCount,
		shardWrites: make(map[int]int64),
	}, nil
}

// Get retrieves from memory or SSD
func (c *SSDCache) Get(key string) ([]byte, bool) {
	// Track access
	c.recordAccess(key)

	// Try memory first
	c.memMu.Lock()
	if elem, ok := c.memItems[key]; ok {
		// Move to front (LRU)
		c.memLRU.MoveToFront(elem)
		item := elem.Value.(*ssdMemItem)
		c.memMu.Unlock()
		return item.data, true
	}
	c.memMu.Unlock()

	// Try SSD
	c.mu.RLock()
	entry, exists := c.index[key]
	c.mu.RUnlock()

	if !exists {
		return nil, false
	}

	// Read from SSD
	data, err := os.ReadFile(entry.Path)
	if err != nil {
		return nil, false
	}

	// Update access time
	c.mu.Lock()
	entry.AccessTime = time.Now()
	c.mu.Unlock()

	// Check if this should be promoted to memory
	if c.IsHot(key) {
		c.promoteToMemory(key, data)
	}

	return data, true
}

// Put stores in memory and potentially demotes to SSD
func (c *SSDCache) Put(key string, data []byte) error {
	size := int64(len(data))

	c.memMu.Lock()
	defer c.memMu.Unlock()

	// Check if item already exists
	if elem, ok := c.memItems[key]; ok {
		// Update existing item
		oldItem := elem.Value.(*ssdMemItem)
		c.memCurrBytes -= oldItem.size
		c.memLRU.MoveToFront(elem)
		elem.Value = &ssdMemItem{key: key, data: data, size: size}
		c.memCurrBytes += size
	} else {
		// Add new item
		elem := c.memLRU.PushFront(&ssdMemItem{key: key, data: data, size: size})
		c.memItems[key] = elem
		c.memCurrBytes += size
	}

	// Evict items if over memory limit
	for c.memCurrBytes > c.memMaxBytes && c.memLRU.Len() > 1 {
		elem := c.memLRU.Back()
		if elem != nil {
			item := elem.Value.(*ssdMemItem)
			c.memLRU.Remove(elem)
			delete(c.memItems, item.key)
			c.memCurrBytes -= item.size

			// Demote to SSD synchronously
			if err := c.demoteToSSD(item.key, item.data); err != nil {
				return fmt.Errorf("failed to demote to SSD: %w", err)
			}
		}
	}

	return nil
}

// promoteToMemory moves hot data from SSD to memory
func (c *SSDCache) promoteToMemory(key string, data []byte) {
	size := int64(len(data))

	c.memMu.Lock()
	defer c.memMu.Unlock()

	// Don't promote if it would exceed memory limit by too much
	if c.memCurrBytes+size > c.memMaxBytes*2 {
		return
	}

	// Add to memory
	elem := c.memLRU.PushFront(&ssdMemItem{key: key, data: data, size: size})
	c.memItems[key] = elem
	c.memCurrBytes += size

	// Remove from SSD
	c.mu.Lock()
	if entry, ok := c.index[key]; ok {
		_ = os.Remove(entry.Path)
		c.currentSize -= entry.Size
		delete(c.index, key)
	}
	c.mu.Unlock()
}

// demoteToSSD moves data from memory to SSD with wear leveling
func (c *SSDCache) demoteToSSD(key string, data []byte) error {
	size := int64(len(data))

	// Determine shard for this key
	shard := c.GetShardForKey(key)

	// Write to sharded path
	shardPath := filepath.Join(c.ssdPath, fmt.Sprintf("shard-%d", shard))
	path := filepath.Join(shardPath, fmt.Sprintf("%s.cache", key))

	if err := os.WriteFile(path, data, 0644); err != nil {
		return err
	}

	// Track writes per shard for monitoring
	c.shardMu.Lock()
	c.shardWrites[shard]++
	c.writeCounter++
	c.shardMu.Unlock()

	// Update index
	c.mu.Lock()
	c.index[key] = &SSDEntry{
		Key:        key,
		Size:       size,
		Path:       path,
		AccessTime: time.Now(),
	}
	c.currentSize += size
	c.mu.Unlock()

	return nil
}

// GetShardForKey returns the shard number for a given key
func (c *SSDCache) GetShardForKey(key string) int {
	// Simple hash-based sharding
	hash := 0
	for _, ch := range key {
		hash = (hash*31 + int(ch)) % c.shardCount
	}
	if hash < 0 {
		hash = -hash
	}
	return hash % c.shardCount
}

// recordAccess tracks access patterns
func (c *SSDCache) recordAccess(key string) {
	c.accessMu.Lock()
	defer c.accessMu.Unlock()

	now := time.Now()
	if pattern, ok := c.accessLog[key]; ok {
		// Reset count if outside window (default 1 minute)
		if now.Sub(pattern.LastAccess) > time.Minute {
			pattern.Count = 1
		} else {
			pattern.Count++
		}
		pattern.LastAccess = now
	} else {
		c.accessLog[key] = &AccessPattern{
			Count:      1,
			LastAccess: now,
			Window:     time.Minute,
		}
	}
}

// IsHot returns true if the key has been accessed frequently
func (c *SSDCache) IsHot(key string) bool {
	c.accessMu.RLock()
	defer c.accessMu.RUnlock()

	pattern, ok := c.accessLog[key]
	if !ok {
		return false
	}

	// Hot if accessed >3 times in the last minute
	if time.Since(pattern.LastAccess) <= pattern.Window && pattern.Count > 3 {
		return true
	}

	return false
}

// GetWriteStats returns wear leveling statistics
func (c *SSDCache) GetWriteStats() map[string]interface{} {
	c.shardMu.RLock()
	defer c.shardMu.RUnlock()

	stats := make(map[string]interface{})
	stats["total_writes"] = c.writeCounter
	stats["shards"] = c.shardCount

	shardStats := make(map[int]int64)
	for shard, writes := range c.shardWrites {
		shardStats[shard] = writes
	}
	stats["shard_writes"] = shardStats

	return stats
}

// Stats returns cache statistics
func (c *SSDCache) Stats() map[string]interface{} {
	c.memMu.RLock()
	memUsed := c.memCurrBytes
	memItems := len(c.memItems)
	c.memMu.RUnlock()

	c.mu.RLock()
	ssdUsed := c.currentSize
	ssdItems := len(c.index)
	c.mu.RUnlock()

	return map[string]interface{}{
		"memory_used":     memUsed,
		"memory_capacity": c.memMaxBytes,
		"ssd_used":        ssdUsed,
		"ssd_capacity":    c.maxSSDSize,
		"memory_items":    memItems,
		"ssd_items":       ssdItems,
	}
}
