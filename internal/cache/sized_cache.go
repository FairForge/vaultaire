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

// SizedLRU implements an LRU cache with byte size limits
type SizedLRU struct {
	mu           sync.RWMutex
	maxBytes     int64
	currentBytes int64
	items        map[string]*list.Element
	lruList      *list.List

	// Statistics
	hits         int64
	misses       int64
	evictions    int64
	bytesEvicted int64
}

// SizedCacheItem includes size tracking
type SizedCacheItem struct {
	CacheItem
	ByteSize int64
}

// NewSizedLRU creates a cache with byte size limit
func NewSizedLRU(maxBytes int64) *SizedLRU {
	return &SizedLRU{
		maxBytes: maxBytes,
		items:    make(map[string]*list.Element),
		lruList:  list.New(),
	}
}

// Get retrieves an item from the cache
func (c *SizedLRU) Get(ctx context.Context, container, artifact string) (io.Reader, bool, error) {
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
	item := elem.Value.(*SizedCacheItem)
	item.LastAccessed = time.Now()

	c.hits++
	return bytes.NewReader(item.Data), true, nil
}

// Put adds an item to the cache with size tracking
func (c *SizedLRU) Put(ctx context.Context, container, artifact string, reader io.Reader, size int64) error {
	// Read the data into memory
	data, err := io.ReadAll(reader)
	if err != nil {
		return fmt.Errorf("sized cache put: reading data: %w", err)
	}

	dataSize := int64(len(data))

	// Don't cache if single item exceeds max size
	if dataSize > c.maxBytes {
		return fmt.Errorf("item size %d exceeds max cache size %d", dataSize, c.maxBytes)
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	key := cacheKey(container, artifact)

	// Check if item already exists
	if elem, exists := c.items[key]; exists {
		oldItem := elem.Value.(*SizedCacheItem)
		c.currentBytes -= oldItem.ByteSize

		// Update existing item and move to front
		c.lruList.MoveToFront(elem)
		oldItem.Data = data
		oldItem.ByteSize = dataSize
		oldItem.Size = dataSize
		oldItem.LastAccessed = time.Now()
		c.currentBytes += dataSize

		// Evict if necessary
		c.evictToSize()
		return nil
	}

	// Add new item
	item := &SizedCacheItem{
		CacheItem: CacheItem{
			Container:    container,
			Artifact:     artifact,
			Data:         data,
			Size:         dataSize,
			LastAccessed: time.Now(),
		},
		ByteSize: dataSize,
	}

	elem := c.lruList.PushFront(item)
	c.items[key] = elem
	c.currentBytes += dataSize

	// Evict items until we're under the limit
	c.evictToSize()

	return nil
}

// Delete removes an item from the cache
func (c *SizedLRU) Delete(ctx context.Context, container, artifact string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	key := cacheKey(container, artifact)

	if elem, exists := c.items[key]; exists {
		item := elem.Value.(*SizedCacheItem)
		c.currentBytes -= item.ByteSize
		c.lruList.Remove(elem)
		delete(c.items, key)
	}

	return nil
}

// evictToSize removes items until cache is under maxBytes
func (c *SizedLRU) evictToSize() {
	for c.currentBytes > c.maxBytes && c.lruList.Len() > 0 {
		elem := c.lruList.Back()
		if elem == nil {
			break
		}

		c.lruList.Remove(elem)
		item := elem.Value.(*SizedCacheItem)
		key := cacheKey(item.Container, item.Artifact)
		delete(c.items, key)

		c.currentBytes -= item.ByteSize
		c.bytesEvicted += item.ByteSize
		c.evictions++
	}
}

// SizedCacheStats includes byte size information
type SizedCacheStats struct {
	CacheStats
	CurrentBytes int64
	MaxBytes     int64
	BytesEvicted int64
}

// Stats returns current cache statistics
func (c *SizedLRU) Stats() *SizedCacheStats {
	c.mu.RLock()
	defer c.mu.RUnlock()

	return &SizedCacheStats{
		CacheStats: CacheStats{
			Items:     c.lruList.Len(),
			Hits:      c.hits,
			Misses:    c.misses,
			Evictions: c.evictions,
			Capacity:  -1, // Not item-based
		},
		CurrentBytes: c.currentBytes,
		MaxBytes:     c.maxBytes,
		BytesEvicted: c.bytesEvicted,
	}
}

// MemoryUsage returns current memory usage percentage
func (s *SizedCacheStats) MemoryUsage() float64 {
	if s.MaxBytes == 0 {
		return 0
	}
	return float64(s.CurrentBytes) / float64(s.MaxBytes) * 100
}
