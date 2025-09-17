package cache

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCacheDebugger_TraceOperation(t *testing.T) {
	cache, _ := NewSSDCache(1024*1024, 10*1024*1024, t.TempDir())
	debugger := NewCacheDebugger(cache)
	debugger.Enable()

	// Trace some operations
	debugger.TraceOperation("GET", "key1", 1024, 100*time.Millisecond, true, nil)
	debugger.TraceOperation("PUT", "key2", 2048, 50*time.Millisecond, false, nil)
	debugger.TraceOperation("GET", "key3", 512, 200*time.Millisecond, false, errors.New("not found"))

	assert.Equal(t, 3, len(debugger.traces))

	// Check error trace has stack
	errorTrace := debugger.traces[2]
	assert.NotEmpty(t, errorTrace.StackTrace)
}

func TestCacheDebugger_GetCacheStats(t *testing.T) {
	cache, _ := NewSSDCache(1024*1024, 10*1024*1024, t.TempDir())
	debugger := NewCacheDebugger(cache)
	debugger.Enable()

	// Simulate cache hits and misses
	debugger.TraceOperation("GET", "key1", 1024, 10*time.Millisecond, true, nil)
	debugger.TraceOperation("GET", "key2", 1024, 10*time.Millisecond, false, nil)
	debugger.TraceOperation("GET", "key3", 1024, 10*time.Millisecond, true, nil)

	stats := debugger.GetCacheStats()

	assert.Equal(t, 2, stats["hits"])
	assert.Equal(t, 1, stats["misses"])
	assert.InDelta(t, 0.666, stats["hit_rate"], 0.01)
}

func TestCacheDebugger_GetSlowOperations(t *testing.T) {
	cache, _ := NewSSDCache(1024*1024, 10*1024*1024, t.TempDir())
	debugger := NewCacheDebugger(cache)
	debugger.Enable()

	// Mix of fast and slow operations
	debugger.TraceOperation("GET", "fast", 1024, 10*time.Millisecond, true, nil)
	debugger.TraceOperation("GET", "slow1", 1024, 200*time.Millisecond, false, nil)
	debugger.TraceOperation("PUT", "slow2", 1024, 150*time.Millisecond, false, nil)

	slow := debugger.GetSlowOperations(100 * time.Millisecond)

	require.Len(t, slow, 2)
	assert.Equal(t, "slow1", slow[0].Key) // 200ms is slowest
	assert.Equal(t, "slow2", slow[1].Key) // 150ms is second
}

func TestCacheDebugger_Disabled(t *testing.T) {
	cache, _ := NewSSDCache(1024*1024, 10*1024*1024, t.TempDir())
	debugger := NewCacheDebugger(cache)
	// Don't enable - should not trace

	debugger.TraceOperation("GET", "key1", 1024, 10*time.Millisecond, true, nil)

	assert.Empty(t, debugger.traces)
}
