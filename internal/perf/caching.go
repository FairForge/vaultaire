// internal/perf/caching.go
package perf

import (
	"container/list"
	"hash/fnv"
	"sync"
	"time"
)

// CacheStrategy defines caching eviction strategy
type CacheStrategy string

const (
	StrategyLRU  CacheStrategy = "lru"
	StrategyLFU  CacheStrategy = "lfu"
	StrategyTTL  CacheStrategy = "ttl"
	StrategyARC  CacheStrategy = "arc"
	StrategyFIFO CacheStrategy = "fifo"
)

// CacheEntry represents a cached item
type CacheEntry struct {
	Key         string
	Value       interface{}
	Size        int64
	CreatedAt   time.Time
	AccessedAt  time.Time
	ExpiresAt   time.Time
	AccessCount int64
}

// CacheStats tracks cache statistics
type CacheStats struct {
	Hits      int64
	Misses    int64
	Evictions int64
	Size      int64
	Count     int64
	MaxSize   int64
	HitRate   float64
}

// Cache is a generic cache interface
type Cache interface {
	Get(key string) (interface{}, bool)
	Set(key string, value interface{}, ttl time.Duration)
	Delete(key string)
	Clear()
	Stats() *CacheStats
}

// LRUCache implements least-recently-used eviction
type LRUCache struct {
	mu       sync.RWMutex
	capacity int
	items    map[string]*list.Element
	order    *list.List
	stats    CacheStats
}

type lruEntry struct {
	key       string
	value     interface{}
	expiresAt time.Time
}

// NewLRUCache creates a new LRU cache
func NewLRUCache(capacity int) *LRUCache {
	return &LRUCache{
		capacity: capacity,
		items:    make(map[string]*list.Element),
		order:    list.New(),
		stats:    CacheStats{MaxSize: int64(capacity)},
	}
}

// Get retrieves a value from the cache
func (c *LRUCache) Get(key string) (interface{}, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if elem, ok := c.items[key]; ok {
		entry := elem.Value.(*lruEntry)

		// Check expiration
		if !entry.expiresAt.IsZero() && time.Now().After(entry.expiresAt) {
			c.removeElement(elem)
			c.stats.Misses++
			return nil, false
		}

		c.order.MoveToFront(elem)
		c.stats.Hits++
		c.updateHitRate()
		return entry.value, true
	}

	c.stats.Misses++
	c.updateHitRate()
	return nil, false
}

// Set adds or updates a value in the cache
func (c *LRUCache) Set(key string, value interface{}, ttl time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()

	var expiresAt time.Time
	if ttl > 0 {
		expiresAt = time.Now().Add(ttl)
	}

	if elem, ok := c.items[key]; ok {
		c.order.MoveToFront(elem)
		entry := elem.Value.(*lruEntry)
		entry.value = value
		entry.expiresAt = expiresAt
		return
	}

	// Evict if at capacity
	for c.order.Len() >= c.capacity {
		c.evictOldest()
	}

	entry := &lruEntry{
		key:       key,
		value:     value,
		expiresAt: expiresAt,
	}
	elem := c.order.PushFront(entry)
	c.items[key] = elem
	c.stats.Count++
}

func (c *LRUCache) evictOldest() {
	elem := c.order.Back()
	if elem != nil {
		c.removeElement(elem)
		c.stats.Evictions++
	}
}

func (c *LRUCache) removeElement(elem *list.Element) {
	c.order.Remove(elem)
	entry := elem.Value.(*lruEntry)
	delete(c.items, entry.key)
	c.stats.Count--
}

// Delete removes a key from the cache
func (c *LRUCache) Delete(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if elem, ok := c.items[key]; ok {
		c.removeElement(elem)
	}
}

// Clear removes all entries
func (c *LRUCache) Clear() {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.items = make(map[string]*list.Element)
	c.order = list.New()
	c.stats.Count = 0
}

// Stats returns cache statistics
func (c *LRUCache) Stats() *CacheStats {
	c.mu.RLock()
	defer c.mu.RUnlock()

	stats := c.stats
	stats.Size = c.stats.Count
	return &stats
}

func (c *LRUCache) updateHitRate() {
	total := c.stats.Hits + c.stats.Misses
	if total > 0 {
		c.stats.HitRate = float64(c.stats.Hits) / float64(total) * 100
	}
}

// Len returns number of items in cache
func (c *LRUCache) Len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.order.Len()
}

// TTLCache implements time-based expiration
type TTLCache struct {
	mu         sync.RWMutex
	items      map[string]*ttlEntry
	defaultTTL time.Duration
	stats      CacheStats
	maxSize    int
}

type ttlEntry struct {
	value     interface{}
	expiresAt time.Time
}

// NewTTLCache creates a new TTL cache
func NewTTLCache(defaultTTL time.Duration, maxSize int) *TTLCache {
	c := &TTLCache{
		items:      make(map[string]*ttlEntry),
		defaultTTL: defaultTTL,
		maxSize:    maxSize,
		stats:      CacheStats{MaxSize: int64(maxSize)},
	}

	// Start cleanup goroutine
	go c.cleanup()

	return c
}

func (c *TTLCache) cleanup() {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()

	for range ticker.C {
		c.mu.Lock()
		now := time.Now()
		for key, entry := range c.items {
			if now.After(entry.expiresAt) {
				delete(c.items, key)
				c.stats.Evictions++
				c.stats.Count--
			}
		}
		c.mu.Unlock()
	}
}

// Get retrieves a value
func (c *TTLCache) Get(key string) (interface{}, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	entry, ok := c.items[key]
	if !ok {
		c.mu.RUnlock()
		c.mu.Lock()
		c.stats.Misses++
		c.updateHitRate()
		c.mu.Unlock()
		c.mu.RLock()
		return nil, false
	}

	if time.Now().After(entry.expiresAt) {
		c.mu.RUnlock()
		c.mu.Lock()
		delete(c.items, key)
		c.stats.Misses++
		c.stats.Count--
		c.updateHitRate()
		c.mu.Unlock()
		c.mu.RLock()
		return nil, false
	}

	c.mu.RUnlock()
	c.mu.Lock()
	c.stats.Hits++
	c.updateHitRate()
	c.mu.Unlock()
	c.mu.RLock()

	return entry.value, true
}

// Set adds a value
func (c *TTLCache) Set(key string, value interface{}, ttl time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if ttl == 0 {
		ttl = c.defaultTTL
	}

	_, exists := c.items[key]
	c.items[key] = &ttlEntry{
		value:     value,
		expiresAt: time.Now().Add(ttl),
	}

	if !exists {
		c.stats.Count++
	}

	// Evict oldest if over max size
	if c.maxSize > 0 && int(c.stats.Count) > c.maxSize {
		c.evictOldest()
	}
}

func (c *TTLCache) evictOldest() {
	var oldestKey string
	var oldestTime time.Time

	for key, entry := range c.items {
		if oldestKey == "" || entry.expiresAt.Before(oldestTime) {
			oldestKey = key
			oldestTime = entry.expiresAt
		}
	}

	if oldestKey != "" {
		delete(c.items, oldestKey)
		c.stats.Evictions++
		c.stats.Count--
	}
}

// Delete removes a key
func (c *TTLCache) Delete(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if _, ok := c.items[key]; ok {
		delete(c.items, key)
		c.stats.Count--
	}
}

// Clear removes all entries
func (c *TTLCache) Clear() {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.items = make(map[string]*ttlEntry)
	c.stats.Count = 0
}

// Stats returns statistics
func (c *TTLCache) Stats() *CacheStats {
	c.mu.RLock()
	defer c.mu.RUnlock()

	stats := c.stats
	stats.Size = c.stats.Count
	return &stats
}

func (c *TTLCache) updateHitRate() {
	total := c.stats.Hits + c.stats.Misses
	if total > 0 {
		c.stats.HitRate = float64(c.stats.Hits) / float64(total) * 100
	}
}

// Len returns number of items
func (c *TTLCache) Len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.items)
}

// ShardedCache is a cache with multiple shards for better concurrency
type ShardedCache struct {
	shards    []*LRUCache
	shardMask uint32
}

// NewShardedCache creates a new sharded cache
func NewShardedCache(shardCount, capacityPerShard int) *ShardedCache {
	// Round up to power of 2
	shardCount = nextPowerOf2(shardCount)

	shards := make([]*LRUCache, shardCount)
	for i := 0; i < shardCount; i++ {
		shards[i] = NewLRUCache(capacityPerShard)
	}

	return &ShardedCache{
		shards:    shards,
		shardMask: uint32(shardCount - 1),
	}
}

func nextPowerOf2(n int) int {
	n--
	n |= n >> 1
	n |= n >> 2
	n |= n >> 4
	n |= n >> 8
	n |= n >> 16
	n++
	return n
}

func (c *ShardedCache) getShard(key string) *LRUCache {
	h := fnv.New32a()
	_, _ = h.Write([]byte(key))
	return c.shards[h.Sum32()&c.shardMask]
}

// Get retrieves a value
func (c *ShardedCache) Get(key string) (interface{}, bool) {
	return c.getShard(key).Get(key)
}

// Set adds a value
func (c *ShardedCache) Set(key string, value interface{}, ttl time.Duration) {
	c.getShard(key).Set(key, value, ttl)
}

// Delete removes a key
func (c *ShardedCache) Delete(key string) {
	c.getShard(key).Delete(key)
}

// Clear removes all entries
func (c *ShardedCache) Clear() {
	for _, shard := range c.shards {
		shard.Clear()
	}
}

// Stats returns aggregated statistics
func (c *ShardedCache) Stats() *CacheStats {
	stats := &CacheStats{}
	for _, shard := range c.shards {
		s := shard.Stats()
		stats.Hits += s.Hits
		stats.Misses += s.Misses
		stats.Evictions += s.Evictions
		stats.Size += s.Size
		stats.Count += s.Count
		stats.MaxSize += s.MaxSize
	}

	total := stats.Hits + stats.Misses
	if total > 0 {
		stats.HitRate = float64(stats.Hits) / float64(total) * 100
	}

	return stats
}

// CacheWarmer pre-populates cache
type CacheWarmer struct {
	cache  Cache
	loader func(key string) (interface{}, error)
}

// NewCacheWarmer creates a new cache warmer
func NewCacheWarmer(cache Cache, loader func(string) (interface{}, error)) *CacheWarmer {
	return &CacheWarmer{
		cache:  cache,
		loader: loader,
	}
}

// Warm pre-loads keys into the cache
func (w *CacheWarmer) Warm(keys []string, ttl time.Duration) error {
	for _, key := range keys {
		value, err := w.loader(key)
		if err != nil {
			return err
		}
		w.cache.Set(key, value, ttl)
	}
	return nil
}

// WarmAsync pre-loads keys asynchronously
func (w *CacheWarmer) WarmAsync(keys []string, ttl time.Duration, workers int) <-chan error {
	errCh := make(chan error, 1)
	keyCh := make(chan string, len(keys))

	for _, key := range keys {
		keyCh <- key
	}
	close(keyCh)

	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for key := range keyCh {
				value, err := w.loader(key)
				if err != nil {
					select {
					case errCh <- err:
					default:
					}
					return
				}
				w.cache.Set(key, value, ttl)
			}
		}()
	}

	go func() {
		wg.Wait()
		close(errCh)
	}()

	return errCh
}
