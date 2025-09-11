// internal/engine/capacity.go
package engine

import (
	"fmt"
	"math"
	"sort"
	"sync"
	"time"
)

// CapacityPlanner predicts storage capacity needs
type CapacityPlanner struct {
	mu         sync.RWMutex
	capacities map[string]int64
	usage      map[string][]UsagePoint
}

// UsagePoint represents a point in time usage
type UsagePoint struct {
	Timestamp time.Time
	UsedBytes int64
}

// CapacityPrediction represents a capacity forecast
type CapacityPrediction struct {
	Backend         string
	CurrentUsage    int64
	TotalCapacity   int64
	UsagePercent    float64
	GrowthRateDaily float64
	PredictedFull   *time.Time
	DaysRemaining   int
}

// NewCapacityPlanner creates a planner
func NewCapacityPlanner() *CapacityPlanner {
	return &CapacityPlanner{
		capacities: make(map[string]int64),
		usage:      make(map[string][]UsagePoint),
	}
}

// SetCapacity sets total capacity for a backend
func (c *CapacityPlanner) SetCapacity(backend string, bytes int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.capacities[backend] = bytes
}

// AddDataPoint adds a usage data point
func (c *CapacityPlanner) AddDataPoint(backend string, timestamp time.Time, usedBytes int64) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if _, ok := c.usage[backend]; !ok {
		c.usage[backend] = []UsagePoint{}
	}

	c.usage[backend] = append(c.usage[backend], UsagePoint{
		Timestamp: timestamp,
		UsedBytes: usedBytes,
	})

	// Sort by timestamp
	sort.Slice(c.usage[backend], func(i, j int) bool {
		return c.usage[backend][i].Timestamp.Before(c.usage[backend][j].Timestamp)
	})

	// Keep only last 365 days of data
	if len(c.usage[backend]) > 365 {
		c.usage[backend] = c.usage[backend][len(c.usage[backend])-365:]
	}
}

// PredictFullDate predicts when backend will be full
func (c *CapacityPlanner) PredictFullDate(backend string) *time.Time {
	c.mu.RLock()
	defer c.mu.RUnlock()

	capacity, hasCapacity := c.capacities[backend]
	points, hasPoints := c.usage[backend]

	if !hasCapacity || !hasPoints || len(points) < 2 {
		return nil
	}

	// Calculate growth rate using linear regression
	growthRate := c.calculateGrowthRate(points)

	if growthRate <= 0 {
		return nil // Not growing
	}

	// Current usage
	currentUsage := points[len(points)-1].UsedBytes
	remainingCapacity := capacity - currentUsage

	if remainingCapacity <= 0 {
		now := time.Now()
		return &now // Already full
	}

	// Days until full
	daysUntilFull := float64(remainingCapacity) / growthRate
	predictedDate := time.Now().Add(time.Duration(daysUntilFull) * 24 * time.Hour)

	return &predictedDate
}

// calculateGrowthRate calculates daily growth rate
func (c *CapacityPlanner) calculateGrowthRate(points []UsagePoint) float64 {
	if len(points) < 2 {
		return 0
	}

	// Simple linear regression
	n := float64(len(points))
	var sumX, sumY, sumXY, sumX2 float64

	for i, p := range points {
		x := float64(i)
		y := float64(p.UsedBytes)
		sumX += x
		sumY += y
		sumXY += x * y
		sumX2 += x * x
	}

	// Calculate slope (bytes per day)
	slope := (n*sumXY - sumX*sumY) / (n*sumX2 - sumX*sumX)

	// Adjust for actual time differences
	firstTime := points[0].Timestamp
	lastTime := points[len(points)-1].Timestamp
	days := lastTime.Sub(firstTime).Hours() / 24

	if days > 0 {
		return slope * float64(len(points)) / days
	}

	return slope
}

// GetRecommendations provides capacity recommendations
func (c *CapacityPlanner) GetRecommendations(backend string) string {
	c.mu.RLock()
	defer c.mu.RUnlock()

	capacity, hasCapacity := c.capacities[backend]
	points, hasPoints := c.usage[backend]

	if !hasCapacity || !hasPoints || len(points) == 0 {
		return "Insufficient data for recommendations"
	}

	currentUsage := points[len(points)-1].UsedBytes
	usagePercent := float64(currentUsage) / float64(capacity) * 100

	var recommendation string

	if usagePercent > 90 {
		recommendation = fmt.Sprintf("CRITICAL: Backend at %.1f%% capacity. Immediate action required.", usagePercent)
	} else if usagePercent > 80 {
		recommendation = fmt.Sprintf("WARNING: Backend at %.1f%% capacity. Plan for expansion soon.", usagePercent)
	} else if usagePercent > 70 {
		recommendation = fmt.Sprintf("INFO: Backend at %.1f%% capacity. Monitor growth rate.", usagePercent)
	} else {
		recommendation = fmt.Sprintf("OK: Backend at %.1f%% capacity.", usagePercent)
	}

	// Add growth prediction
	if fullDate := c.PredictFullDate(backend); fullDate != nil {
		daysRemaining := int(time.Until(*fullDate).Hours() / 24)
		if daysRemaining < 30 {
			recommendation += fmt.Sprintf(" URGENT: Predicted to be full in %d days!", daysRemaining)
		} else if daysRemaining < 90 {
			recommendation += fmt.Sprintf(" Will be full in approximately %d days.", daysRemaining)
		}
	}

	return recommendation
}

// GetForecast generates a capacity forecast
func (c *CapacityPlanner) GetForecast(backend string) CapacityPrediction {
	c.mu.RLock()
	defer c.mu.RUnlock()

	prediction := CapacityPrediction{Backend: backend}

	if capacity, ok := c.capacities[backend]; ok {
		prediction.TotalCapacity = capacity
	}

	if points, ok := c.usage[backend]; ok && len(points) > 0 {
		prediction.CurrentUsage = points[len(points)-1].UsedBytes
		prediction.UsagePercent = float64(prediction.CurrentUsage) / float64(prediction.TotalCapacity) * 100

		if len(points) >= 2 {
			prediction.GrowthRateDaily = c.calculateGrowthRate(points)
			prediction.PredictedFull = c.PredictFullDate(backend)

			if prediction.PredictedFull != nil {
				prediction.DaysRemaining = int(math.Ceil(time.Until(*prediction.PredictedFull).Hours() / 24))
			}
		}
	}

	return prediction
}
