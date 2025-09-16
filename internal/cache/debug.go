// internal/cache/debug.go
package cache

import (
	"fmt"
	"runtime"
	"sort"
	"sync"
	"time"
)

// CacheDebugger provides debugging tools for cache inspection
type CacheDebugger struct {
	mu        sync.RWMutex
	cache     *SSDCache
	traces    []*OperationTrace
	maxTraces int
	enabled   bool
}

// OperationTrace records cache operation details
type OperationTrace struct {
	Timestamp  time.Time
	Operation  string
	Key        string
	Size       int64
	Duration   time.Duration
	Hit        bool
	Error      error
	StackTrace string
}

// NewCacheDebugger creates a cache debugging tool
func NewCacheDebugger(cache *SSDCache) *CacheDebugger {
	return &CacheDebugger{
		cache:     cache,
		traces:    make([]*OperationTrace, 0),
		maxTraces: 10000,
		enabled:   false,
	}
}

// Enable turns on debug tracing
func (d *CacheDebugger) Enable() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.enabled = true
}

// TraceOperation records a cache operation
func (d *CacheDebugger) TraceOperation(op string, key string, size int64, duration time.Duration, hit bool, err error) {
	if !d.enabled {
		return
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	trace := &OperationTrace{
		Timestamp: time.Now(),
		Operation: op,
		Key:       key,
		Size:      size,
		Duration:  duration,
		Hit:       hit,
		Error:     err,
	}

	// Capture stack trace for errors
	if err != nil {
		buf := make([]byte, 4096)
		n := runtime.Stack(buf, false)
		trace.StackTrace = string(buf[:n])
	}

	d.traces = append(d.traces, trace)
	if len(d.traces) > d.maxTraces {
		d.traces = d.traces[1:]
	}
}

// GetCacheStats returns current cache statistics
func (d *CacheDebugger) GetCacheStats() map[string]interface{} {
	d.mu.RLock()
	defer d.mu.RUnlock()

	stats := make(map[string]interface{})

	// Calculate hit rate
	var hits, misses int
	for _, trace := range d.traces {
		if trace.Operation == "GET" {
			if trace.Hit {
				hits++
			} else {
				misses++
			}
		}
	}

	total := hits + misses
	if total > 0 {
		stats["hit_rate"] = float64(hits) / float64(total)
	}

	stats["total_operations"] = len(d.traces)
	stats["hits"] = hits
	stats["misses"] = misses

	return stats
}

// GetSlowOperations returns operations slower than threshold
func (d *CacheDebugger) GetSlowOperations(threshold time.Duration) []*OperationTrace {
	d.mu.RLock()
	defer d.mu.RUnlock()

	var slow []*OperationTrace
	for _, trace := range d.traces {
		if trace.Duration > threshold {
			slow = append(slow, trace)
		}
	}

	// Sort by duration (slowest first)
	sort.Slice(slow, func(i, j int) bool {
		return slow[i].Duration > slow[j].Duration
	})

	return slow
}

// DumpCache returns a snapshot of cache contents
func (d *CacheDebugger) DumpCache() map[string]string {
	// Simplified - would access cache internals in production
	dump := make(map[string]string)
	dump["memory_usage"] = fmt.Sprintf("%d MB", runtime.MemStats{}.Alloc/1024/1024)
	dump["goroutines"] = fmt.Sprintf("%d", runtime.NumGoroutine())
	return dump
}
