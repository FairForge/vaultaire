// internal/engine/health_test.go
package engine

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestHealthScorer_CalculateScore(t *testing.T) {
	tests := []struct {
		name     string
		metrics  HealthMetrics
		expected float64
	}{
		{
			name: "perfect health",
			metrics: HealthMetrics{
				Latency:    10 * time.Millisecond,
				ErrorRate:  0.0,
				Uptime:     1.0,
				Throughput: 100 * 1024 * 1024, // 100MB/s
			},
			expected: 100.0,
		},
		{
			name: "degraded health",
			metrics: HealthMetrics{
				Latency:    500 * time.Millisecond,
				ErrorRate:  0.05, // 5% errors
				Uptime:     0.95,
				Throughput: 10 * 1024 * 1024, // 10MB/s
			},
			expected: 65.0, // Approximate
		},
	}

	scorer := NewHealthScorer()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			score := scorer.CalculateScore(tt.metrics)
			assert.InDelta(t, tt.expected, score, 5.0)
		})
	}
}
