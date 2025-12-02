// internal/ha/lb_integration.go
// Step 363: Load Balancing Integration
package ha

import (
	"context"
	"sync"

	"github.com/FairForge/vaultaire/internal/engine"
)

// WeightMultiplier defines weight adjustments per state
var WeightMultiplier = map[BackendState]float64{
	StateHealthy:    1.0,
	StateDegraded:   0.5,
	StateRecovering: 0.3,
	StateFailed:     0.0,
	StateUnknown:    0.0,
}

// HALoadBalancer integrates LoadBalancer with HAOrchestrator
type HALoadBalancer struct {
	mu           sync.RWMutex
	lb           *engine.LoadBalancer
	orchestrator *HAOrchestrator
	baseWeights  map[string]float64 // Original weights before adjustment
	stopChan     chan struct{}
}

// NewHALoadBalancer creates a health-aware load balancer
func NewHALoadBalancer(lb *engine.LoadBalancer, orchestrator *HAOrchestrator) *HALoadBalancer {
	haLB := &HALoadBalancer{
		lb:           lb,
		orchestrator: orchestrator,
		baseWeights:  make(map[string]float64),
		stopChan:     make(chan struct{}),
	}

	// Store initial weights
	for _, b := range lb.GetBackends() {
		haLB.baseWeights[b.ID] = b.Weight
	}

	// Subscribe to health events
	orchestrator.Subscribe(haLB.handleHealthEvent)

	return haLB
}

// handleHealthEvent responds to HAOrchestrator events
func (h *HALoadBalancer) handleHealthEvent(event HAEvent) {
	// Query current state from orchestrator instead of relying on event
	// This prevents race conditions when events arrive out of order
	status := h.orchestrator.GetBackendStatus(event.Backend)
	h.updateWeight(event.Backend, status.State)
}

// updateWeight adjusts backend weight based on health state
func (h *HALoadBalancer) updateWeight(backendID string, state BackendState) {
	h.mu.Lock()
	defer h.mu.Unlock()

	baseWeight, exists := h.baseWeights[backendID]
	if !exists {
		baseWeight = 1.0
	}

	multiplier := WeightMultiplier[state]
	newWeight := baseWeight * multiplier

	h.lb.SetWeight(backendID, newWeight)
}

// NextHealthyBackend selects next backend, excluding unhealthy ones
func (h *HALoadBalancer) NextHealthyBackend(ctx context.Context) string {
	h.mu.RLock()
	defer h.mu.RUnlock()

	// Get healthy backends from orchestrator
	healthyIDs := h.orchestrator.GetHealthyBackends()
	if len(healthyIDs) == 0 {
		return ""
	}

	// Create a set for O(1) lookup
	healthySet := make(map[string]bool)
	for _, id := range healthyIDs {
		healthySet[id] = true
	}

	// Try to get a healthy backend
	// For round-robin, we may need multiple attempts
	for attempts := 0; attempts < len(h.lb.GetBackends()); attempts++ {
		selected := h.lb.NextBackend(ctx)
		if selected == "" {
			return ""
		}

		// Check if selected backend is healthy
		if healthySet[selected] {
			return selected
		}
	}

	// Fallback: return first healthy backend
	if len(healthyIDs) > 0 {
		return healthyIDs[0]
	}

	return ""
}

// GetEffectiveWeight returns the current effective weight for a backend
func (h *HALoadBalancer) GetEffectiveWeight(backendID string) float64 {
	return h.lb.GetWeight(backendID)
}

// SetBaseWeight sets the base weight for a backend (before health adjustments)
func (h *HALoadBalancer) SetBaseWeight(backendID string, weight float64) {
	h.mu.Lock()
	h.baseWeights[backendID] = weight
	h.mu.Unlock()

	// Recalculate effective weight based on current health
	status := h.orchestrator.GetBackendStatus(backendID)
	h.updateWeight(backendID, status.State)
}

// AddBackend adds a backend to both load balancer and tracking
func (h *HALoadBalancer) AddBackend(id string, weight float64) {
	h.mu.Lock()
	h.baseWeights[id] = weight
	h.mu.Unlock()

	h.lb.AddBackend(id, weight)
}

// RemoveBackend removes a backend from tracking
func (h *HALoadBalancer) RemoveBackend(id string) {
	h.mu.Lock()
	delete(h.baseWeights, id)
	h.mu.Unlock()

	h.lb.RemoveBackend(id)
}

// Stop shuts down the HA load balancer
func (h *HALoadBalancer) Stop() {
	close(h.stopChan)
}

// GetLoadBalancer returns the underlying load balancer
func (h *HALoadBalancer) GetLoadBalancer() *engine.LoadBalancer {
	return h.lb
}

// GetOrchestrator returns the underlying orchestrator
func (h *HALoadBalancer) GetOrchestrator() *HAOrchestrator {
	return h.orchestrator
}
