package drivers

import (
	"sync"
	"time"
)

// EgressTracker tracks bandwidth usage and costs
type EgressTracker struct {
	mu        sync.RWMutex
	usage     map[string]int64   // tenant -> bytes
	costs     map[string]float64 // tenant -> cost
	rate      float64            // cost per GB
	window    time.Duration      // tracking window
	lastReset time.Time
}

// NewEgressTracker creates a new egress tracker
func NewEgressTracker() *EgressTracker {
	return &EgressTracker{
		usage:     make(map[string]int64),
		costs:     make(map[string]float64),
		rate:      0.009, // iDrive E2 rate: $0.009/GB
		window:    24 * time.Hour,
		lastReset: time.Now(),
	}
}

// RecordEgress records bandwidth usage for a tenant
func (e *EgressTracker) RecordEgress(tenantID string, bytes int64) {
	e.mu.Lock()
	defer e.mu.Unlock()

	e.usage[tenantID] += bytes

	// Calculate cost (bytes to GB * rate)
	gbUsed := float64(bytes) / (1024 * 1024 * 1024)
	e.costs[tenantID] += gbUsed * e.rate
}

// GetTenantEgress returns total egress for a tenant
func (e *EgressTracker) GetTenantEgress(tenantID string) int64 {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.usage[tenantID]
}

// GetTenantCost returns the cost for a tenant's egress
func (e *EgressTracker) GetTenantCost(tenantID string) float64 {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.costs[tenantID]
}

// SetRate updates the cost per GB
func (e *EgressTracker) SetRate(rate float64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.rate = rate
}

// GetAllTenantUsage returns usage for all tenants
func (e *EgressTracker) GetAllTenantUsage() map[string]int64 {
	e.mu.RLock()
	defer e.mu.RUnlock()

	result := make(map[string]int64)
	for k, v := range e.usage {
		result[k] = v
	}
	return result
}

// Reset clears the tracking data
func (e *EgressTracker) Reset() {
	e.mu.Lock()
	defer e.mu.Unlock()

	e.usage = make(map[string]int64)
	e.costs = make(map[string]float64)
	e.lastReset = time.Now()
}
