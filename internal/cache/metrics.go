package cache

import (
	"sync"
	"time"
)

// MetricsTracker tracks detailed cache metrics
type MetricsTracker struct {
	mu sync.RWMutex

	// Counters
	totalRequests int64
	hits          map[string]int64 // per key
	misses        map[string]int64 // per key

	// Timing
	getLatencies []time.Duration
	putLatencies []time.Duration

	// Access patterns
	accessTimes  map[string][]time.Time
	accessCounts map[string]int64

	// Size tracking
	totalBytesServed int64
	totalBytesCached int64
}

// NewMetricsTracker creates a new metrics tracker
func NewMetricsTracker() *MetricsTracker {
	return &MetricsTracker{
		hits:         make(map[string]int64),
		misses:       make(map[string]int64),
		accessTimes:  make(map[string][]time.Time),
		accessCounts: make(map[string]int64),
	}
}

// RecordHit records a cache hit
func (m *MetricsTracker) RecordHit(key string, latency time.Duration, bytes int64) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.totalRequests++
	m.hits[key]++
	m.accessCounts[key]++
	m.getLatencies = append(m.getLatencies, latency)
	m.accessTimes[key] = append(m.accessTimes[key], time.Now())
	m.totalBytesServed += bytes

	// Keep only last 1000 latencies
	if len(m.getLatencies) > 1000 {
		m.getLatencies = m.getLatencies[len(m.getLatencies)-1000:]
	}

	// Keep only last 100 access times per key
	if len(m.accessTimes[key]) > 100 {
		m.accessTimes[key] = m.accessTimes[key][len(m.accessTimes[key])-100:]
	}
}

// RecordMiss records a cache miss
func (m *MetricsTracker) RecordMiss(key string, latency time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.totalRequests++
	m.misses[key]++
	m.getLatencies = append(m.getLatencies, latency)
}

// RecordPut records a cache put operation
func (m *MetricsTracker) RecordPut(key string, latency time.Duration, bytes int64) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.putLatencies = append(m.putLatencies, latency)
	m.totalBytesCached += bytes

	// Keep only last 1000 latencies
	if len(m.putLatencies) > 1000 {
		m.putLatencies = m.putLatencies[len(m.putLatencies)-1000:]
	}
}

// GetTopKeys returns the most accessed keys
func (m *MetricsTracker) GetTopKeys(n int) []string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	// Simple implementation - in production use a heap
	type keyCount struct {
		key   string
		count int64
	}

	var items []keyCount
	for k, v := range m.accessCounts {
		items = append(items, keyCount{k, v})
	}

	// Sort by count (simple bubble sort for now)
	for i := 0; i < len(items); i++ {
		for j := i + 1; j < len(items); j++ {
			if items[j].count > items[i].count {
				items[i], items[j] = items[j], items[i]
			}
		}
	}

	result := make([]string, 0, n)
	for i := 0; i < n && i < len(items); i++ {
		result = append(result, items[i].key)
	}

	return result
}

// GetHitRate returns overall hit rate
func (m *MetricsTracker) GetHitRate() float64 {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if m.totalRequests == 0 {
		return 0
	}

	totalHits := int64(0)
	for _, count := range m.hits {
		totalHits += count
	}

	return float64(totalHits) / float64(m.totalRequests)
}
