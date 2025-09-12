package cache

import (
	"bytes"
	"container/list"
	"context"
	"fmt"
	"io"
	"sync"
	"time"
)

// CacheItem represents a single cached artifact
type CacheItem struct {
	Container    string
	Artifact     string
	Data         []byte
	Size         int64
	LastAccessed time.Time
}

// LRU implements a Least Recently Used cache
type LRU struct {
	mu       sync.RWMutex
	capacity int
	items    map[string]*list.Element
	lruList  *list.List

	// Statistics
	hits      int64
	misses    int64
	evictions int64
}

// NewLRU creates a new LRU cache with the given capacity
func NewLRU(capacity int) *LRU {
	return &LRU{
		capacity: capacity,
		items:    make(map[string]*list.Element),
		lruList:  list.New(),
	}
}

// cacheKey generates a unique key for container/artifact pair
func cacheKey(container, artifact string) string {
	return fmt.Sprintf("%s/%s", container, artifact)
}

// Get retrieves an item from the cache
func (c *LRU) Get(ctx context.Context, container, artifact string) (io.Reader, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	key := cacheKey(container, artifact)
	elem, exists := c.items[key]

	if !exists {
		c.misses++
		return nil, false, nil
	}

	// Move to front (most recently used)
	c.lruList.MoveToFront(elem)
	item := elem.Value.(*CacheItem)
	item.LastAccessed = time.Now()

	c.hits++
	return bytes.NewReader(item.Data), true, nil
}

// Put adds an item to the cache
func (c *LRU) Put(ctx context.Context, container, artifact string, reader io.Reader, size int64) error {
	// Read the data into memory
	data, err := io.ReadAll(reader)
	if err != nil {
		return fmt.Errorf("cache put: reading data: %w", err)
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	key := cacheKey(container, artifact)

	// Check if item already exists
	if elem, exists := c.items[key]; exists {
		// Update existing item and move to front
		c.lruList.MoveToFront(elem)
		item := elem.Value.(*CacheItem)
		item.Data = data
		item.Size = int64(len(data))
		item.LastAccessed = time.Now()
		return nil
	}

	// Add new item
	item := &CacheItem{
		Container:    container,
		Artifact:     artifact,
		Data:         data,
		Size:         int64(len(data)),
		LastAccessed: time.Now(),
	}

	elem := c.lruList.PushFront(item)
	c.items[key] = elem

	// Check capacity and evict if necessary
	if c.lruList.Len() > c.capacity {
		c.evictOldest()
	}

	return nil
}

// Delete removes an item from the cache
func (c *LRU) Delete(ctx context.Context, container, artifact string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	key := cacheKey(container, artifact)

	if elem, exists := c.items[key]; exists {
		c.lruList.Remove(elem)
		delete(c.items, key)
	}

	return nil
}

// evictOldest removes the least recently used item
func (c *LRU) evictOldest() {
	elem := c.lruList.Back()
	if elem == nil {
		return
	}

	c.lruList.Remove(elem)
	item := elem.Value.(*CacheItem)
	key := cacheKey(item.Container, item.Artifact)
	delete(c.items, key)
	c.evictions++
}

// CacheStats holds cache statistics
type CacheStats struct {
	Items     int
	Hits      int64
	Misses    int64
	Evictions int64
	Capacity  int
}

// HitRate calculates the cache hit rate
func (s *CacheStats) HitRate() float64 {
	total := s.Hits + s.Misses
	if total == 0 {
		return 0
	}
	return float64(s.Hits) / float64(total)
}

// Stats returns current cache statistics
func (c *LRU) Stats() *CacheStats {
	c.mu.RLock()
	defer c.mu.RUnlock()

	return &CacheStats{
		Items:     c.lruList.Len(),
		Hits:      c.hits,
		Misses:    c.misses,
		Evictions: c.evictions,
		Capacity:  c.capacity,
	}
}

// Clear removes all items from the cache
func (c *LRU) Clear() {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.items = make(map[string]*list.Element)
	c.lruList = list.New()
	c.hits = 0
	c.misses = 0
	c.evictions = 0
}
