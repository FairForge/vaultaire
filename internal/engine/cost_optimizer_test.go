package engine

import (
	"context"
	"github.com/stretchr/testify/assert"
	"testing"
)

func TestCostOptimizer_SelectCheapest(t *testing.T) {
	optimizer := NewCostOptimizer()

	// Set backend costs (per GB)
	optimizer.SetCost("s3", 0.023)        // AWS S3 standard
	optimizer.SetCost("lyve", 0.0064)     // Seagate Lyve
	optimizer.SetCost("quotaless", 0.001) // Bulk storage

	tests := []struct {
		name      string
		operation string
		size      int64
		expected  string
	}{
		{
			name:      "small file prefers performance",
			operation: "PUT",
			size:      1024 * 1024, // 1MB
			expected:  "s3",        // Fast for small files
		},
		{
			name:      "large file prefers cheap",
			operation: "PUT",
			size:      10 * 1024 * 1024 * 1024, // 10GB
			expected:  "quotaless",             // Cheap for bulk
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			selected := optimizer.SelectOptimal(context.Background(), tt.operation, tt.size)
			assert.Equal(t, tt.expected, selected)
		})
	}
}

func TestCostOptimizer_CalculateMonthlyCost(t *testing.T) {
	optimizer := NewCostOptimizer()
	optimizer.SetCost("s3", 0.023)

	// 1TB for a month
	cost := optimizer.CalculateMonthlyCost("s3", 1024*1024*1024*1024)
	assert.InDelta(t, 23.552, cost, 0.01) // $23/TB for S3
}

func TestCostOptimizer_GetCostBreakdown(t *testing.T) {
	optimizer := NewCostOptimizer()
	optimizer.SetCost("s3", 0.023)
	optimizer.SetCost("lyve", 0.0064)

	// 1GB
	breakdown := optimizer.GetCostBreakdown(1024 * 1024 * 1024)

	assert.InDelta(t, 0.023, breakdown["s3"], 0.001)
	assert.InDelta(t, 0.0064, breakdown["lyve"], 0.001)
}
