// internal/cache/patterns_test.go
package cache

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPatternAnalyzer_TrackAccess(t *testing.T) {
	analyzer := NewPatternAnalyzer(1 * time.Minute)

	// Track multiple accesses
	analyzer.TrackAccess("key1")
	time.Sleep(10 * time.Millisecond)
	analyzer.TrackAccess("key1")
	time.Sleep(10 * time.Millisecond)
	analyzer.TrackAccess("key1")

	pattern := analyzer.GetPattern("key1")
	require.NotNil(t, pattern)
	assert.Equal(t, int64(3), pattern.AccessCount)
	assert.WithinDuration(t, time.Now(), pattern.LastAccess, 100*time.Millisecond)
}

func TestPatternAnalyzer_CalculateFrequency(t *testing.T) {
	analyzer := NewPatternAnalyzer(1 * time.Minute)

	// Regular interval accesses
	for i := 0; i < 5; i++ {
		analyzer.TrackAccess("regular")
		time.Sleep(20 * time.Millisecond)
	}

	pattern := analyzer.GetPattern("regular")
	assert.InDelta(t, 20.0, pattern.AccessInterval.Milliseconds(), 10.0)
	assert.Greater(t, pattern.Predictability, 0.7) // High predictability
}

func TestPatternAnalyzer_DetectsHotKeys(t *testing.T) {
	analyzer := NewPatternAnalyzer(1 * time.Minute)

	// Simulate various access patterns
	for i := 0; i < 100; i++ {
		analyzer.TrackAccess("hot-key-1")
	}
	for i := 0; i < 50; i++ {
		analyzer.TrackAccess("hot-key-2")
	}
	for i := 0; i < 10; i++ {
		analyzer.TrackAccess("warm-key")
	}
	analyzer.TrackAccess("cold-key")

	hotKeys := analyzer.GetHotKeys(2)
	assert.Contains(t, hotKeys, "hot-key-1")
	assert.Contains(t, hotKeys, "hot-key-2")
	assert.NotContains(t, hotKeys, "cold-key")
}
