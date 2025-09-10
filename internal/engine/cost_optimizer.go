package engine

import (
	"context"
	"sort"
	"sync"
)

// CostOptimizer selects backends based on cost
type CostOptimizer struct {
	mu         sync.RWMutex
	costs      map[string]float64 // Cost per GB
	egress     map[string]float64 // Egress cost per GB
	thresholds CostThresholds
}

// CostThresholds defines size thresholds for routing
type CostThresholds struct {
	SmallFile int64 // Below this, prioritize speed
	LargeFile int64 // Above this, prioritize cost
}

// NewCostOptimizer creates an optimizer with defaults
func NewCostOptimizer() *CostOptimizer {
	return &CostOptimizer{
		costs:  make(map[string]float64),
		egress: make(map[string]float64),
		thresholds: CostThresholds{
			SmallFile: 10 * 1024 * 1024,         // 10MB
			LargeFile: 100 * 1024 * 1024 * 1024, // 100GB
		},
	}
}

// SetCost sets storage cost for a backend (per GB)
func (c *CostOptimizer) SetCost(backend string, costPerGB float64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.costs[backend] = costPerGB
}

// SetEgressCost sets egress cost for a backend
func (c *CostOptimizer) SetEgressCost(backend string, costPerGB float64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.egress[backend] = costPerGB
}

// SelectOptimal chooses backend based on size and cost
func (c *CostOptimizer) SelectOptimal(ctx context.Context, operation string, size int64) string {
	c.mu.RLock()
	defer c.mu.RUnlock()

	// For small files, prefer fastest (usually most expensive)
	if size < c.thresholds.SmallFile {
		return c.selectFastest()
	}

	// For large files, prefer cheapest
	if size > c.thresholds.LargeFile {
		return c.selectCheapest()
	}

	// Medium files: balance cost and performance
	return c.selectBalanced(size)
}

func (c *CostOptimizer) selectCheapest() string {
	type backend struct {
		name string
		cost float64
	}

	var backends []backend
	for name, cost := range c.costs {
		backends = append(backends, backend{name, cost})
	}

	sort.Slice(backends, func(i, j int) bool {
		return backends[i].cost < backends[j].cost
	})

	if len(backends) > 0 {
		return backends[0].name
	}
	return ""
}

func (c *CostOptimizer) selectFastest() string {
	// Assume S3/Lyve are fastest for now
	if _, ok := c.costs["s3"]; ok {
		return "s3"
	}
	if _, ok := c.costs["lyve"]; ok {
		return "lyve"
	}
	// Fallback to any available
	for name := range c.costs {
		return name
	}
	return ""
}

func (c *CostOptimizer) selectBalanced(size int64) string {
	// Calculate cost-performance score
	type score struct {
		name  string
		value float64
	}

	var scores []score
	sizeGB := float64(size) / (1024 * 1024 * 1024)

	for name, cost := range c.costs {
		// Lower cost = higher score
		// Add performance weight (hardcoded for now)
		perfWeight := 1.0
		if name == "s3" || name == "lyve" {
			perfWeight = 1.5 // Prefer these for medium files
		}

		scoreValue := perfWeight / (cost*sizeGB + 0.001) // Add small value to avoid divide by zero
		scores = append(scores, score{name, scoreValue})
	}

	sort.Slice(scores, func(i, j int) bool {
		return scores[i].value > scores[j].value
	})

	if len(scores) > 0 {
		return scores[0].name
	}
	return ""
}

// CalculateMonthlyCost estimates monthly storage cost
func (c *CostOptimizer) CalculateMonthlyCost(backend string, bytes int64) float64 {
	c.mu.RLock()
	defer c.mu.RUnlock()

	costPerGB, ok := c.costs[backend]
	if !ok {
		return 0
	}

	gb := float64(bytes) / (1024 * 1024 * 1024)
	return gb * costPerGB
}

// GetCostBreakdown returns detailed cost analysis
func (c *CostOptimizer) GetCostBreakdown(bytes int64) map[string]float64 {
	c.mu.RLock()
	defer c.mu.RUnlock()

	breakdown := make(map[string]float64)
	gb := float64(bytes) / (1024 * 1024 * 1024)

	for backend, costPerGB := range c.costs {
		breakdown[backend] = gb * costPerGB
	}

	return breakdown
}
