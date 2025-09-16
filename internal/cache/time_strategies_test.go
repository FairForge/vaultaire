// internal/cache/time_strategies_test.go
package cache

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestTimeBasedStrategy_GetOptimalTTL(t *testing.T) {
	strategy := NewTimeBasedStrategy()
	baselineTTL := 1 * time.Hour

	// Test during peak hours (if running during business hours)
	// This is simplified - production would mock time
	ttl := strategy.GetOptimalTTL("test-key", baselineTTL)
	assert.Greater(t, ttl, time.Duration(0))

	// Test with override
	strategy.ttlOverrides["special-key"] = 5 * time.Minute
	specialTTL := strategy.GetOptimalTTL("special-key", baselineTTL)
	assert.Equal(t, 5*time.Minute, specialTTL)
}

func TestTimeBasedStrategy_RecordHourlyPattern(t *testing.T) {
	strategy := NewTimeBasedStrategy()

	// Record pattern for current hour
	strategy.RecordHourlyPattern(100, 1024*1024, []string{"hot1", "hot2"})

	hour := time.Now().Hour()
	pattern := strategy.timePatterns[hour]

	assert.NotNil(t, pattern)
	assert.Equal(t, float64(100), pattern.AvgRequests)
	assert.Equal(t, int64(1024*1024), pattern.AvgCacheSize)

	// Record again, should average
	strategy.RecordHourlyPattern(200, 2*1024*1024, []string{"hot3"})
	assert.Equal(t, float64(150), pattern.AvgRequests) // (100+200)/2
}
