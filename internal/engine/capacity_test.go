// internal/engine/capacity_test.go
package engine

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestCapacityPlanner_PredictFullDate(t *testing.T) {
	planner := NewCapacityPlanner()

	// Add historical data points
	planner.AddDataPoint("backend1", time.Now().Add(-30*24*time.Hour), 100*1024*1024*1024) // 100GB 30 days ago
	planner.AddDataPoint("backend1", time.Now().Add(-15*24*time.Hour), 150*1024*1024*1024) // 150GB 15 days ago
	planner.AddDataPoint("backend1", time.Now(), 200*1024*1024*1024)                       // 200GB now

	// Set capacity
	planner.SetCapacity("backend1", 1024*1024*1024*1024) // 1TB capacity

	prediction := planner.PredictFullDate("backend1")
	assert.NotNil(t, prediction)
	assert.True(t, prediction.After(time.Now()))
}

func TestCapacityPlanner_GetRecommendations(t *testing.T) {
	planner := NewCapacityPlanner()

	planner.SetCapacity("backend1", 1024*1024*1024*1024)             // 1TB
	planner.AddDataPoint("backend1", time.Now(), 900*1024*1024*1024) // 900GB used

	recommendations := planner.GetRecommendations("backend1")
	assert.Contains(t, recommendations, "WARNING")
	assert.Contains(t, recommendations, "87.9%")
}
