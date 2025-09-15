package cache

import (
	"bytes"
	"compress/gzip"
	"container/list"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/golang/snappy"
)

// AccessPattern tracks access patterns for cache items
type AccessPattern struct {
	Count      int
	LastAccess time.Time
	Window     time.Duration
}

// PromotionPolicy defines when to promote data from SSD to memory
type PromotionPolicy struct {
	FrequencyThreshold int           // Min accesses to trigger promotion
	TimeWindow         time.Duration // Time window for counting accesses
	SizeLimit          int64         // Max size to promote (avoid huge files in memory)
}

// DemotionPolicy defines when to demote data from memory to SSD
type DemotionPolicy struct {
	MaxAge        time.Duration // Max time in memory without access
	LowWaterMark  int64         // Start demotion when memory usage exceeds this
	HighWaterMark int64         // Aggressively demote when exceeding this
}

// PerformanceMetrics tracks cache performance
type PerformanceMetrics struct {
	// Latency tracking
	PutLatencyP50 time.Duration
	GetLatencyP50 time.Duration

	// Operation counts
	PutCount int64
	GetCount int64

	// Hit rate tracking
	CacheHits   int64
	CacheMisses int64
	HitRate     float64
}

// CompressionStats tracks compression effectiveness
type CompressionStats struct {
	OriginalSize     int64
	CompressedSize   int64
	BytesSaved       int64
	CompressionRatio float64
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

	// Policies
	promotionPolicy *PromotionPolicy
	demotionPolicy  *DemotionPolicy

	// Performance monitoring
	monitoringEnabled bool
	perfMu            sync.RWMutex
	putLatencies      []time.Duration
	getLatencies      []time.Duration
	putCount          int64
	getCount          int64
	cacheHits         int64
	cacheMisses       int64

	// Compression
	compressionEnabled  bool
	compressionAlgo     string
	compressionMu       sync.RWMutex
	totalOriginalSize   int64
	totalCompressedSize int64

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
		memMaxBytes:  memSize,
		memItems:     make(map[string]*list.Element),
		memLRU:       list.New(),
		ssdPath:      ssdPath,
		maxSSDSize:   ssdSize,
		index:        make(map[string]*SSDEntry),
		accessLog:    make(map[string]*AccessPattern),
		shardCount:   shardCount,
		shardWrites:  make(map[int]int64),
		putLatencies: make([]time.Duration, 0, 1000),
		getLatencies: make([]time.Duration, 0, 1000),
	}, nil
}

// Get retrieves from memory or SSD
func (c *SSDCache) Get(key string) ([]byte, bool) {
	start := time.Now()
	defer func() {
		if c.monitoringEnabled {
			c.recordLatency("get", time.Since(start))
		}
	}()

	// Track access
	c.recordAccess(key)

	// Try memory first
	c.memMu.Lock()
	if elem, ok := c.memItems[key]; ok {
		// Move to front (LRU)
		c.memLRU.MoveToFront(elem)
		item := elem.Value.(*ssdMemItem)
		c.memMu.Unlock()

		// Record hit
		if c.monitoringEnabled {
			c.perfMu.Lock()
			c.cacheHits++
			c.perfMu.Unlock()
		}

		return item.data, true
	}
	c.memMu.Unlock()

	// Try SSD
	c.mu.RLock()
	entry, exists := c.index[key]
	c.mu.RUnlock()

	if !exists {
		// Record miss
		if c.monitoringEnabled {
			c.perfMu.Lock()
			c.cacheMisses++
			c.perfMu.Unlock()
		}
		return nil, false
	}

	// Read from SSD
	compressedData, err := os.ReadFile(entry.Path)
	if err != nil {
		if c.monitoringEnabled {
			c.perfMu.Lock()
			c.cacheMisses++
			c.perfMu.Unlock()
		}
		return nil, false
	}

	// Decompress if needed
	data, err := c.decompressData(compressedData)
	if err != nil {
		if c.monitoringEnabled {
			c.perfMu.Lock()
			c.cacheMisses++
			c.perfMu.Unlock()
		}
		return nil, false
	}

	// Update access time
	c.mu.Lock()
	entry.AccessTime = time.Now()
	c.mu.Unlock()

	// Record hit
	if c.monitoringEnabled {
		c.perfMu.Lock()
		c.cacheHits++
		c.perfMu.Unlock()
	}

	// Check if this should be promoted to memory
	if c.IsHot(key) {
		c.promoteToMemory(key, data)
	}

	return data, true
}

// Put stores in memory and potentially demotes to SSD
func (c *SSDCache) Put(key string, data []byte) error {
	start := time.Now()
	defer func() {
		if c.monitoringEnabled {
			c.recordLatency("put", time.Since(start))
		}
	}()

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

// demoteToSSD moves data from memory to SSD with wear leveling and compression
func (c *SSDCache) demoteToSSD(key string, data []byte) error {
	originalSize := int64(len(data))

	// Compress data if enabled
	compressed, err := c.compressData(data)
	if err != nil {
		return fmt.Errorf("compression failed: %w", err)
	}

	compressedSize := int64(len(compressed))

	// Track compression stats
	if c.compressionEnabled {
		c.compressionMu.Lock()
		c.totalOriginalSize += originalSize
		c.totalCompressedSize += compressedSize
		c.compressionMu.Unlock()
	}

	// Determine shard for this key
	shard := c.GetShardForKey(key)

	// Write to sharded path
	shardPath := filepath.Join(c.ssdPath, fmt.Sprintf("shard-%d", shard))
	path := filepath.Join(shardPath, fmt.Sprintf("%s.cache", key))

	if err := os.WriteFile(path, compressed, 0644); err != nil {
		return err
	}

	// Track writes per shard for monitoring
	c.shardMu.Lock()
	c.shardWrites[shard]++
	c.writeCounter++
	c.shardMu.Unlock()

	// Update index - store original size for accounting
	c.mu.Lock()
	c.index[key] = &SSDEntry{
		Key:        key,
		Size:       originalSize, // Store original size
		Path:       path,
		AccessTime: time.Now(),
	}
	c.currentSize += originalSize
	c.mu.Unlock()

	return nil
}

// compressData compresses data using the configured algorithm
func (c *SSDCache) compressData(data []byte) ([]byte, error) {
	if !c.compressionEnabled || c.compressionAlgo == "none" {
		return data, nil
	}

	switch c.compressionAlgo {
	case "gzip":
		var buf bytes.Buffer
		gw := gzip.NewWriter(&buf)
		if _, err := gw.Write(data); err != nil {
			return nil, err
		}
		if err := gw.Close(); err != nil {
			return nil, err
		}
		return buf.Bytes(), nil

	case "snappy":
		return snappy.Encode(nil, data), nil

	default:
		return data, nil
	}
}

// decompressData decompresses data using the configured algorithm
func (c *SSDCache) decompressData(data []byte) ([]byte, error) {
	if !c.compressionEnabled || c.compressionAlgo == "none" {
		return data, nil
	}

	switch c.compressionAlgo {
	case "gzip":
		gr, err := gzip.NewReader(bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
		defer func() { _ = gr.Close() }()
		return io.ReadAll(gr)

	case "snappy":
		return snappy.Decode(nil, data)

	default:
		return data, nil
	}
}

// EnableCompression turns on data compression for SSD storage
func (c *SSDCache) EnableCompression(algorithm string) {
	c.compressionMu.Lock()
	defer c.compressionMu.Unlock()

	c.compressionEnabled = true
	c.compressionAlgo = algorithm
}

// GetCompressionStats returns compression statistics
func (c *SSDCache) GetCompressionStats() *CompressionStats {
	c.compressionMu.RLock()
	defer c.compressionMu.RUnlock()

	stats := &CompressionStats{
		OriginalSize:   c.totalOriginalSize,
		CompressedSize: c.totalCompressedSize,
		BytesSaved:     c.totalOriginalSize - c.totalCompressedSize,
	}

	if c.totalOriginalSize > 0 {
		stats.CompressionRatio = float64(c.totalCompressedSize) / float64(c.totalOriginalSize)
	}

	return stats
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

	// Use promotion policy if configured
	if c.promotionPolicy != nil {
		window := c.promotionPolicy.TimeWindow
		threshold := c.promotionPolicy.FrequencyThreshold

		if time.Since(pattern.LastAccess) <= window && pattern.Count >= threshold {
			return true
		}
		return false
	}

	// Default behavior
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

// SetPromotionPolicy configures when data moves from SSD to memory
func (c *SSDCache) SetPromotionPolicy(policy *PromotionPolicy) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.promotionPolicy = policy
}

// SetDemotionPolicy configures when data moves from memory to SSD
func (c *SSDCache) SetDemotionPolicy(policy *DemotionPolicy) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.demotionPolicy = policy
}

// GetMemoryKeys returns keys currently in memory (for testing)
func (c *SSDCache) GetMemoryKeys() []string {
	c.memMu.RLock()
	defer c.memMu.RUnlock()

	keys := make([]string, 0, len(c.memItems))
	for key := range c.memItems {
		keys = append(keys, key)
	}
	return keys
}

// ApplyDemotionPolicy actively demotes old items based on policy
func (c *SSDCache) ApplyDemotionPolicy() {
	if c.demotionPolicy == nil {
		return
	}

	c.memMu.Lock()
	defer c.memMu.Unlock()

	now := time.Now()
	toEvict := []*ssdMemItem{}

	// Find items older than MaxAge
	for key, elem := range c.memItems {
		item := elem.Value.(*ssdMemItem)

		// Check access time from access log
		c.accessMu.RLock()
		pattern, exists := c.accessLog[key]
		c.accessMu.RUnlock()

		if !exists || now.Sub(pattern.LastAccess) > c.demotionPolicy.MaxAge {
			toEvict = append(toEvict, item)
		}
	}

	// Evict old items
	for _, item := range toEvict {
		if elem, ok := c.memItems[item.key]; ok {
			c.memLRU.Remove(elem)
			delete(c.memItems, item.key)
			c.memCurrBytes -= item.size

			// Demote to SSD
			go func(key string, data []byte) {
				_ = c.demoteToSSD(key, data)
			}(item.key, item.data)
		}
	}
}

// EnableMonitoring turns on performance tracking
func (c *SSDCache) EnableMonitoring() {
	c.perfMu.Lock()
	defer c.perfMu.Unlock()
	c.monitoringEnabled = true
}

// recordLatency tracks operation latencies
func (c *SSDCache) recordLatency(operation string, duration time.Duration) {
	if !c.monitoringEnabled {
		return
	}

	c.perfMu.Lock()
	defer c.perfMu.Unlock()

	switch operation {
	case "put":
		c.putLatencies = append(c.putLatencies, duration)
		c.putCount++
		// Keep only last 1000 samples
		if len(c.putLatencies) > 1000 {
			c.putLatencies = c.putLatencies[len(c.putLatencies)-1000:]
		}
	case "get":
		c.getLatencies = append(c.getLatencies, duration)
		c.getCount++
		if len(c.getLatencies) > 1000 {
			c.getLatencies = c.getLatencies[len(c.getLatencies)-1000:]
		}
	}
}

// GetPerformanceMetrics returns current performance stats
func (c *SSDCache) GetPerformanceMetrics() *PerformanceMetrics {
	c.perfMu.RLock()
	defer c.perfMu.RUnlock()

	metrics := &PerformanceMetrics{
		PutCount:    c.putCount,
		GetCount:    c.getCount,
		CacheHits:   c.cacheHits,
		CacheMisses: c.cacheMisses,
	}

	// Calculate P50 latencies
	if len(c.putLatencies) > 0 {
		metrics.PutLatencyP50 = c.calculateP50(c.putLatencies)
	}
	if len(c.getLatencies) > 0 {
		metrics.GetLatencyP50 = c.calculateP50(c.getLatencies)
	}

	// Calculate hit rate
	total := float64(c.cacheHits + c.cacheMisses)
	if total > 0 {
		metrics.HitRate = float64(c.cacheHits) / total
	}

	return metrics
}

// calculateP50 finds the median latency
func (c *SSDCache) calculateP50(latencies []time.Duration) time.Duration {
	if len(latencies) == 0 {
		return 0
	}

	// Simple median calculation
	sorted := make([]time.Duration, len(latencies))
	copy(sorted, latencies)

	// Basic bubble sort for simplicity
	for i := 0; i < len(sorted); i++ {
		for j := i + 1; j < len(sorted); j++ {
			if sorted[i] > sorted[j] {
				sorted[i], sorted[j] = sorted[j], sorted[i]
			}
		}
	}

	return sorted[len(sorted)/2]
}
