// internal/cache/time_strategies.go
package cache

import (
	"sync"
	"time"
)

// TimeBasedStrategy handles time-aware caching decisions
type TimeBasedStrategy struct {
	mu                sync.RWMutex
	timePatterns      map[int]*HourlyPattern // hour -> pattern
	ttlOverrides      map[string]time.Duration
	peakHours         []int
	offPeakMultiplier float64
}

// HourlyPattern tracks usage patterns by hour
type HourlyPattern struct {
	Hour         int
	AvgRequests  float64
	AvgCacheSize int64
	HotKeys      []string
}

// NewTimeBasedStrategy creates a time-aware strategy
func NewTimeBasedStrategy() *TimeBasedStrategy {
	return &TimeBasedStrategy{
		timePatterns:      make(map[int]*HourlyPattern),
		ttlOverrides:      make(map[string]time.Duration),
		peakHours:         []int{9, 10, 11, 14, 15, 16}, // Business hours
		offPeakMultiplier: 0.5,                          // Reduce cache during off-peak
	}
}

// GetOptimalTTL returns TTL based on time of day
func (t *TimeBasedStrategy) GetOptimalTTL(key string, baselineTTL time.Duration) time.Duration {
	t.mu.RLock()
	defer t.mu.RUnlock()

	// Check for specific override
	if override, exists := t.ttlOverrides[key]; exists {
		return override
	}

	hour := time.Now().Hour()

	// During peak hours, cache longer
	for _, peakHour := range t.peakHours {
		if hour == peakHour {
			return baselineTTL * 2
		}
	}

	// Off-peak, use shorter TTL
	return time.Duration(float64(baselineTTL) * t.offPeakMultiplier)
}

// RecordHourlyPattern records usage for the current hour
func (t *TimeBasedStrategy) RecordHourlyPattern(requests int, cacheSize int64, hotKeys []string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	hour := time.Now().Hour()

	if pattern, exists := t.timePatterns[hour]; exists {
		// Update running average
		pattern.AvgRequests = (pattern.AvgRequests + float64(requests)) / 2
		pattern.AvgCacheSize = (pattern.AvgCacheSize + cacheSize) / 2
		pattern.HotKeys = hotKeys
	} else {
		t.timePatterns[hour] = &HourlyPattern{
			Hour:         hour,
			AvgRequests:  float64(requests),
			AvgCacheSize: cacheSize,
			HotKeys:      hotKeys,
		}
	}
}
