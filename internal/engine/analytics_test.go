// internal/engine/analytics_test.go
package engine

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestAnalytics_RecordOperation(t *testing.T) {
	analytics := NewAnalytics()

	// Record some operations
	analytics.RecordOperation("backend1", "GET", 100*time.Millisecond, 1024, nil)
	analytics.RecordOperation("backend1", "PUT", 200*time.Millisecond, 2048, nil)

	stats := analytics.GetStats("backend1")
	assert.Equal(t, 2, stats.TotalOperations)
	assert.Equal(t, int64(3072), stats.TotalBytes)
}

func TestAnalytics_CalculatePercentiles(t *testing.T) {
	analytics := NewAnalytics()

	// Record operations with various latencies
	for i := 0; i < 100; i++ {
		latency := time.Duration(i) * time.Millisecond
		analytics.RecordOperation("backend1", "GET", latency, 1024, nil)
	}

	stats := analytics.GetStats("backend1")
	assert.InDelta(t, 50.0, stats.P50Latency.Milliseconds(), 5)
	assert.InDelta(t, 95.0, stats.P95Latency.Milliseconds(), 5)
}
