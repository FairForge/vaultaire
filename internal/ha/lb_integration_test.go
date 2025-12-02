// internal/ha/lb_integration_test.go
// Step 363: Load Balancing Integration Tests
package ha

import (
	"context"
	"testing"
	"time"

	"github.com/FairForge/vaultaire/internal/engine"
)

func TestHALoadBalancer_ExcludesFailedBackends(t *testing.T) {
	// Setup
	lb := engine.NewLoadBalancer(engine.RoundRobin)
	lb.AddBackend("backend-1", 1.0)
	lb.AddBackend("backend-2", 1.0)
	lb.AddBackend("backend-3", 1.0)

	orchestrator := NewHAOrchestrator()
	orchestrator.RegisterBackend("backend-1", BackendConfig{FailureThreshold: 2})
	orchestrator.RegisterBackend("backend-2", BackendConfig{FailureThreshold: 2})
	orchestrator.RegisterBackend("backend-3", BackendConfig{FailureThreshold: 2})

	haLB := NewHALoadBalancer(lb, orchestrator)
	defer haLB.Stop()

	// Mark backend-2 as failed
	orchestrator.ReportHealthCheck("backend-2", false, 0, nil)
	orchestrator.ReportHealthCheck("backend-2", false, 0, nil) // Threshold reached

	// Wait for event processing
	time.Sleep(50 * time.Millisecond)

	// Verify failed backend is excluded
	ctx := context.Background()
	selections := make(map[string]int)
	for i := 0; i < 100; i++ {
		selected := haLB.NextHealthyBackend(ctx)
		selections[selected]++
	}

	if selections["backend-2"] > 0 {
		t.Errorf("Failed backend should not be selected, got %d selections", selections["backend-2"])
	}
	if selections["backend-1"] == 0 || selections["backend-3"] == 0 {
		t.Error("Healthy backends should be selected")
	}
}

func TestHALoadBalancer_ReducesWeightForDegraded(t *testing.T) {
	lb := engine.NewLoadBalancer(engine.WeightedRandom)
	lb.AddBackend("backend-1", 1.0)
	lb.AddBackend("backend-2", 1.0)

	orchestrator := NewHAOrchestrator()
	orchestrator.RegisterBackend("backend-1", BackendConfig{FailureThreshold: 3})
	orchestrator.RegisterBackend("backend-2", BackendConfig{FailureThreshold: 3})

	haLB := NewHALoadBalancer(lb, orchestrator)
	defer haLB.Stop()

	// Mark backend-1 as degraded (fail once, below threshold)
	orchestrator.ReportHealthCheck("backend-1", false, 0, nil)
	orchestrator.ReportHealthCheck("backend-1", false, 0, nil)

	time.Sleep(50 * time.Millisecond)

	// Check weight was reduced
	weight := haLB.GetEffectiveWeight("backend-1")
	if weight >= 1.0 {
		t.Errorf("Degraded backend weight should be reduced, got %f", weight)
	}
	if weight == 0 {
		t.Error("Degraded backend should not have zero weight")
	}
}

func TestHALoadBalancer_RestoresWeightOnRecovery(t *testing.T) {
	lb := engine.NewLoadBalancer(engine.RoundRobin)
	lb.AddBackend("backend-1", 1.0)

	orchestrator := NewHAOrchestrator()
	orchestrator.RegisterBackend("backend-1", BackendConfig{
		FailureThreshold:  2,
		RecoveryThreshold: 2,
	})

	haLB := NewHALoadBalancer(lb, orchestrator)
	defer haLB.Stop()

	// Fail the backend
	orchestrator.ReportHealthCheck("backend-1", false, 0, nil)
	orchestrator.ReportHealthCheck("backend-1", false, 0, nil)
	time.Sleep(50 * time.Millisecond)

	initialWeight := haLB.GetEffectiveWeight("backend-1")
	if initialWeight != 0 {
		t.Errorf("Failed backend should have weight 0, got %f", initialWeight)
	}

	// Recover the backend
	orchestrator.ReportHealthCheck("backend-1", true, time.Millisecond, nil)
	orchestrator.ReportHealthCheck("backend-1", true, time.Millisecond, nil)
	time.Sleep(50 * time.Millisecond)

	recoveredWeight := haLB.GetEffectiveWeight("backend-1")
	if recoveredWeight != 1.0 {
		t.Errorf("Recovered backend should have full weight, got %f", recoveredWeight)
	}
}

func TestHALoadBalancer_AllBackendsFailed(t *testing.T) {
	lb := engine.NewLoadBalancer(engine.RoundRobin)
	lb.AddBackend("backend-1", 1.0)

	orchestrator := NewHAOrchestrator()
	orchestrator.RegisterBackend("backend-1", BackendConfig{FailureThreshold: 1})

	haLB := NewHALoadBalancer(lb, orchestrator)
	defer haLB.Stop()

	// Fail the only backend
	orchestrator.ReportHealthCheck("backend-1", false, 0, nil)
	time.Sleep(50 * time.Millisecond)

	ctx := context.Background()
	selected := haLB.NextHealthyBackend(ctx)

	// Should return empty when no healthy backends
	if selected != "" {
		t.Errorf("Expected empty string when all backends failed, got %s", selected)
	}
}

func TestHALoadBalancer_RecoveringBackendGetsReducedWeight(t *testing.T) {
	lb := engine.NewLoadBalancer(engine.WeightedRandom)
	lb.AddBackend("backend-1", 1.0)

	orchestrator := NewHAOrchestrator()
	orchestrator.RegisterBackend("backend-1", BackendConfig{
		FailureThreshold:  2,
		RecoveryThreshold: 3, // Needs 3 successes to fully recover
	})

	haLB := NewHALoadBalancer(lb, orchestrator)
	defer haLB.Stop()

	// Fail then start recovering
	orchestrator.ReportHealthCheck("backend-1", false, 0, nil)
	orchestrator.ReportHealthCheck("backend-1", false, 0, nil)
	time.Sleep(50 * time.Millisecond)

	// First recovery check
	orchestrator.ReportHealthCheck("backend-1", true, time.Millisecond, nil)
	time.Sleep(50 * time.Millisecond)

	weight := haLB.GetEffectiveWeight("backend-1")
	// Recovering should have reduced weight (cautious)
	if weight <= 0 || weight >= 1.0 {
		t.Errorf("Recovering backend should have partial weight, got %f", weight)
	}
}

func TestLoadBalancer_SetWeight(t *testing.T) {
	lb := engine.NewLoadBalancer(engine.WeightedRandom)
	lb.AddBackend("backend-1", 1.0)

	// Test the new SetWeight method
	lb.SetWeight("backend-1", 0.5)

	backends := lb.GetBackends()
	found := false
	for _, b := range backends {
		if b.ID == "backend-1" {
			found = true
			if b.Weight != 0.5 {
				t.Errorf("Expected weight 0.5, got %f", b.Weight)
			}
		}
	}
	if !found {
		t.Error("Backend not found")
	}
}

func TestLoadBalancer_RemoveBackend(t *testing.T) {
	lb := engine.NewLoadBalancer(engine.RoundRobin)
	lb.AddBackend("backend-1", 1.0)
	lb.AddBackend("backend-2", 1.0)

	lb.RemoveBackend("backend-1")

	backends := lb.GetBackends()
	if len(backends) != 1 {
		t.Errorf("Expected 1 backend after removal, got %d", len(backends))
	}
	if backends[0].ID != "backend-2" {
		t.Errorf("Expected backend-2 to remain, got %s", backends[0].ID)
	}
}
