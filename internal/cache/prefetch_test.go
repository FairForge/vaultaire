package cache

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestSequentialPrefetchStrategy(t *testing.T) {
	strategy := &SequentialPrefetchStrategy{windowSize: 3}

	pattern := &AccessPatternAnalysis{
		AccessCount:    10,
		Predictability: 0.9,
	}

	assert.True(t, strategy.ShouldPrefetch("key1", pattern))

	// Low predictability shouldn't prefetch
	pattern.Predictability = 0.3
	assert.False(t, strategy.ShouldPrefetch("key1", pattern))
}

func TestPredictivePrefetcher_Integration(t *testing.T) {
	cache, _ := NewSSDCache(1024*1024, 10*1024*1024, t.TempDir())
	analyzer := NewPatternAnalyzer(1 * time.Minute)

	// Track sequential access pattern
	for i := 0; i < 10; i++ {
		analyzer.TrackAccess("seq-1")
		time.Sleep(10 * time.Millisecond)
	}

	pattern := analyzer.GetPattern("seq-1")
	assert.Greater(t, pattern.AccessCount, int64(5))

	// Test prefetcher
	strategy := &SequentialPrefetchStrategy{windowSize: 3}
	prefetcher := NewPredictivePrefetcher(analyzer, strategy)

	// Should trigger prefetch for frequently accessed key
	prefetcher.TriggerPrefetch("seq-1")

	_ = cache // Use cache variable to avoid unused warning
}
