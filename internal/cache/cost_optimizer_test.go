package cache

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCostOptimizer_AnalyzeValue(t *testing.T) {
	optimizer := NewCostOptimizer(100.0) // $100 budget

	// 1GB file accessed 10000 times
	value := optimizer.AnalyzeValue("large-file", 1024*1024*1024, 10000)

	require.NotNil(t, value)
	assert.Equal(t, int64(1024*1024*1024), value.Size)
	assert.Equal(t, int64(10000), value.AccessCount)

	// Storage cost: 1GB * $10/GB = $10
	assert.InDelta(t, 10.0, value.StorageCost, 0.1)

	// Access savings: 10k requests
	// Cloud: 10 * $0.4 = $4
	// Cache: 10 * $0.001 = $0.01
	// Savings: ~$3.99
	assert.Greater(t, value.AccessSavings, 3.0)
}

func TestCostOptimizer_ShouldCache(t *testing.T) {
	optimizer := NewCostOptimizer(50.0)

	// High-value item (many accesses)
	shouldCache := optimizer.ShouldCache("popular", 100*1024*1024, 100000)
	assert.True(t, shouldCache)

	// Low-value item (few accesses, large size)
	shouldNotCache := optimizer.ShouldCache("rare", 10*1024*1024*1024, 10)
	assert.False(t, shouldNotCache)
}

func TestCostOptimizer_GetOptimalTier(t *testing.T) {
	optimizer := NewCostOptimizer(100.0)

	// Set tier capacities
	optimizer.tiers[0].Capacity = 1024 * 1024 * 1024        // 1GB memory
	optimizer.tiers[1].Capacity = 100 * 1024 * 1024 * 1024  // 100GB SSD
	optimizer.tiers[2].Capacity = 1024 * 1024 * 1024 * 1024 // 1TB cloud

	// Frequently accessed small file -> memory tier
	tier := optimizer.GetOptimalTier(1000.0, 10*1024*1024)
	assert.Equal(t, "memory", tier.Name)

	// Medium frequency, medium size -> SSD tier
	// Need higher access rate to justify SSD over cloud
	tier = optimizer.GetOptimalTier(100.0, 1*1024*1024*1024) // 100 accesses/day, 1GB
	assert.Equal(t, "ssd", tier.Name)

	// Low frequency, large size -> cloud tier
	tier = optimizer.GetOptimalTier(1.0, 10*1024*1024*1024) // 1 access/day, 10GB
	assert.Equal(t, "cloud", tier.Name)
}
