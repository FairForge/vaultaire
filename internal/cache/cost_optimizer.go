// internal/cache/cost_optimizer.go
package cache

import (
	"sync"
	"time"
)

// StorageTier represents different storage cost tiers
type StorageTier struct {
	Name         string
	CostPerGB    float64 // $ per GB per month
	AccessCost   float64 // $ per 1000 requests
	Latency      time.Duration
	Capacity     int64
	CurrentUsage int64
}

// CacheValue represents the value/cost analysis of cached data
type CacheValue struct {
	Key           string
	Size          int64
	AccessCount   int64
	LastAccess    time.Time
	StorageCost   float64
	AccessSavings float64
	NetValue      float64
}

// CostOptimizer optimizes cache usage based on cost/benefit
type CostOptimizer struct {
	mu           sync.RWMutex
	tiers        []*StorageTier
	values       map[string]*CacheValue
	budget       float64
	currentSpend float64
}

// NewCostOptimizer creates a cost-aware cache optimizer
func NewCostOptimizer(budget float64) *CostOptimizer {
	return &CostOptimizer{
		tiers: []*StorageTier{
			{Name: "memory", CostPerGB: 10.0, AccessCost: 0.001, Latency: 1 * time.Microsecond},
			{Name: "ssd", CostPerGB: 1.0, AccessCost: 0.01, Latency: 100 * time.Microsecond},
			{Name: "cloud", CostPerGB: 0.023, AccessCost: 0.4, Latency: 100 * time.Millisecond},
		},
		values: make(map[string]*CacheValue),
		budget: budget,
	}
}

// AnalyzeValue calculates the value of caching specific data
func (c *CostOptimizer) AnalyzeValue(key string, size int64, accessCount int64) *CacheValue {
	c.mu.Lock()
	defer c.mu.Unlock()

	value := &CacheValue{
		Key:         key,
		Size:        size,
		AccessCount: accessCount,
		LastAccess:  time.Now(),
	}

	// Calculate monthly storage cost (assuming memory tier)
	gbSize := float64(size) / (1024 * 1024 * 1024)
	value.StorageCost = gbSize * c.tiers[0].CostPerGB

	// Calculate access savings vs cloud storage
	cloudAccessCost := float64(accessCount) / 1000 * c.tiers[2].AccessCost
	cacheAccessCost := float64(accessCount) / 1000 * c.tiers[0].AccessCost
	value.AccessSavings = cloudAccessCost - cacheAccessCost

	value.NetValue = value.AccessSavings - value.StorageCost

	c.values[key] = value
	return value
}

// ShouldCache determines if data is worth caching based on cost
func (c *CostOptimizer) ShouldCache(key string, size int64, predictedAccess int64) bool {
	value := c.AnalyzeValue(key, size, predictedAccess)

	c.mu.RLock()
	defer c.mu.RUnlock()

	// Cache if positive net value and within budget
	if value.NetValue > 0 && c.currentSpend+value.StorageCost <= c.budget {
		return true
	}

	return false
}

// GetOptimalTier returns the most cost-effective tier for data
func (c *CostOptimizer) GetOptimalTier(accessFreq float64, size int64) *StorageTier {
	c.mu.RLock()
	defer c.mu.RUnlock()

	var bestTier *StorageTier
	bestCost := float64(999999)

	for _, tier := range c.tiers {
		if tier.CurrentUsage+size > tier.Capacity {
			continue
		}

		// Calculate total cost for this tier
		gbSize := float64(size) / (1024 * 1024 * 1024)
		monthlyStorage := gbSize * tier.CostPerGB
		monthlyAccess := accessFreq * 30 / 1000 * tier.AccessCost
		totalCost := monthlyStorage + monthlyAccess

		if totalCost < bestCost {
			bestCost = totalCost
			bestTier = tier
		}
	}

	return bestTier
}
