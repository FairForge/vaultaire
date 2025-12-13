// internal/global/failover_test.go
package global

import (
	"context"
	"testing"
	"time"
)

func TestNewFailoverManager(t *testing.T) {
	em := NewEdgeManager(nil)
	fm := NewFailoverManager(em, nil)

	if fm == nil {
		t.Fatal("expected non-nil manager")
	}
	if fm.config == nil {
		t.Error("expected default config")
	}
	if fm.state != FailoverStateNormal {
		t.Error("expected normal state")
	}
}

func TestFailoverManagerAddPolicy(t *testing.T) {
	em := NewEdgeManager(nil)
	fm := NewFailoverManager(em, nil)

	err := fm.AddPolicy(&FailoverPolicy{
		ID:            "test-policy",
		Name:          "Test Policy",
		SourceRegion:  "us-east",
		TargetRegions: []string{"us-west"},
		Enabled:       true,
	})

	if err != nil {
		t.Fatalf("failed to add policy: %v", err)
	}

	policy, ok := fm.GetPolicy("test-policy")
	if !ok {
		t.Fatal("policy not found")
	}
	if policy.Name != "Test Policy" {
		t.Error("policy not saved correctly")
	}
	if policy.HealthThreshold == 0 {
		t.Error("expected default health threshold")
	}
}

func TestFailoverManagerAddPolicyValidation(t *testing.T) {
	em := NewEdgeManager(nil)
	fm := NewFailoverManager(em, nil)

	// No ID
	err := fm.AddPolicy(&FailoverPolicy{SourceRegion: "us-east", TargetRegions: []string{"us-west"}})
	if err == nil {
		t.Error("expected error for missing ID")
	}

	// No source region
	err = fm.AddPolicy(&FailoverPolicy{ID: "test", TargetRegions: []string{"us-west"}})
	if err == nil {
		t.Error("expected error for missing source region")
	}

	// No target regions
	err = fm.AddPolicy(&FailoverPolicy{ID: "test", SourceRegion: "us-east"})
	if err == nil {
		t.Error("expected error for missing target regions")
	}
}

func TestFailoverManagerRemovePolicy(t *testing.T) {
	em := NewEdgeManager(nil)
	fm := NewFailoverManager(em, nil)

	_ = fm.AddPolicy(&FailoverPolicy{
		ID:            "test",
		SourceRegion:  "us-east",
		TargetRegions: []string{"us-west"},
	})

	removed := fm.RemovePolicy("test")
	if !removed {
		t.Error("expected policy to be removed")
	}

	_, ok := fm.GetPolicy("test")
	if ok {
		t.Error("policy should not exist")
	}

	removed = fm.RemovePolicy("nonexistent")
	if removed {
		t.Error("should not remove nonexistent")
	}
}

func TestFailoverManagerGetPolicies(t *testing.T) {
	em := NewEdgeManager(nil)
	fm := NewFailoverManager(em, nil)

	_ = fm.AddPolicy(&FailoverPolicy{ID: "p1", SourceRegion: "us-east", TargetRegions: []string{"us-west"}})
	_ = fm.AddPolicy(&FailoverPolicy{ID: "p2", SourceRegion: "eu-west", TargetRegions: []string{"eu-central"}})

	policies := fm.GetPolicies()
	if len(policies) != 2 {
		t.Errorf("expected 2 policies, got %d", len(policies))
	}
}

func TestFailoverManagerEnableDisablePolicy(t *testing.T) {
	em := NewEdgeManager(nil)
	fm := NewFailoverManager(em, nil)

	_ = fm.AddPolicy(&FailoverPolicy{
		ID:            "test",
		SourceRegion:  "us-east",
		TargetRegions: []string{"us-west"},
		Enabled:       true,
	})

	err := fm.DisablePolicy("test")
	if err != nil {
		t.Fatalf("failed to disable: %v", err)
	}

	policy, _ := fm.GetPolicy("test")
	if policy.Enabled {
		t.Error("expected disabled")
	}

	err = fm.EnablePolicy("test")
	if err != nil {
		t.Fatalf("failed to enable: %v", err)
	}

	policy, _ = fm.GetPolicy("test")
	if !policy.Enabled {
		t.Error("expected enabled")
	}
}

func TestFailoverManagerEnableNonexistent(t *testing.T) {
	em := NewEdgeManager(nil)
	fm := NewFailoverManager(em, nil)

	err := fm.EnablePolicy("nonexistent")
	if err == nil {
		t.Error("expected error")
	}

	err = fm.DisablePolicy("nonexistent")
	if err == nil {
		t.Error("expected error")
	}
}

func TestFailoverManagerUpdateRegionHealth(t *testing.T) {
	em := NewEdgeManager(nil)
	fm := NewFailoverManager(em, nil)

	fm.UpdateRegionHealth(&RegionHealth{
		Region:      "us-east",
		Healthy:     true,
		HealthScore: 0.95,
	})

	health, ok := fm.GetRegionHealth("us-east")
	if !ok {
		t.Fatal("health not found")
	}
	if !health.Healthy {
		t.Error("expected healthy")
	}
	if health.ConsecutiveOK != 1 {
		t.Errorf("expected 1 consecutive OK, got %d", health.ConsecutiveOK)
	}
}

func TestFailoverManagerConsecutiveHealthUpdates(t *testing.T) {
	em := NewEdgeManager(nil)
	fm := NewFailoverManager(em, nil)

	// Multiple healthy updates
	for i := 0; i < 5; i++ {
		fm.UpdateRegionHealth(&RegionHealth{Region: "us-east", Healthy: true})
	}

	health, _ := fm.GetRegionHealth("us-east")
	if health.ConsecutiveOK != 5 {
		t.Errorf("expected 5 consecutive OK, got %d", health.ConsecutiveOK)
	}

	// One failure resets
	fm.UpdateRegionHealth(&RegionHealth{Region: "us-east", Healthy: false})
	health, _ = fm.GetRegionHealth("us-east")
	if health.ConsecutiveFails != 1 {
		t.Errorf("expected 1 consecutive fail, got %d", health.ConsecutiveFails)
	}
	if health.ConsecutiveOK != 0 {
		t.Errorf("expected 0 consecutive OK, got %d", health.ConsecutiveOK)
	}
}

func TestFailoverManagerGetAllRegionHealth(t *testing.T) {
	em := NewEdgeManager(nil)
	fm := NewFailoverManager(em, nil)

	fm.UpdateRegionHealth(&RegionHealth{Region: "us-east", Healthy: true})
	fm.UpdateRegionHealth(&RegionHealth{Region: "eu-west", Healthy: false})

	all := fm.GetAllRegionHealth()
	if len(all) != 2 {
		t.Errorf("expected 2 regions, got %d", len(all))
	}
}

func TestFailoverManagerIsRegionHealthy(t *testing.T) {
	em := NewEdgeManager(nil)
	fm := NewFailoverManager(em, nil)

	// Unknown region assumed healthy
	if !fm.IsRegionHealthy("unknown") {
		t.Error("expected unknown region to be healthy")
	}

	fm.UpdateRegionHealth(&RegionHealth{Region: "us-east", Healthy: true})
	if !fm.IsRegionHealthy("us-east") {
		t.Error("expected healthy")
	}

	fm.UpdateRegionHealth(&RegionHealth{Region: "us-east", Healthy: false})
	if fm.IsRegionHealthy("us-east") {
		t.Error("expected unhealthy")
	}
}

func TestFailoverManagerInitiateFailover(t *testing.T) {
	em := NewEdgeManager(nil)
	fm := NewFailoverManager(em, nil)

	_ = fm.AddPolicy(&FailoverPolicy{
		ID:            "test-policy",
		SourceRegion:  "us-east",
		TargetRegions: []string{"us-west"},
		Enabled:       true,
	})

	// Mark target as healthy
	fm.UpdateRegionHealth(&RegionHealth{Region: "us-west", Healthy: true})

	ctx := context.Background()
	err := fm.InitiateFailover(ctx, "test-policy")
	if err != nil {
		t.Fatalf("failover failed: %v", err)
	}

	// Wait for async failover
	time.Sleep(50 * time.Millisecond)

	activeFailovers := fm.GetActiveFailovers()
	if len(activeFailovers) != 1 {
		t.Errorf("expected 1 active failover, got %d", len(activeFailovers))
	}
}

func TestFailoverManagerInitiateFailoverNoPolicy(t *testing.T) {
	em := NewEdgeManager(nil)
	fm := NewFailoverManager(em, nil)

	ctx := context.Background()
	err := fm.InitiateFailover(ctx, "nonexistent")
	if err == nil {
		t.Error("expected error for nonexistent policy")
	}
}

func TestFailoverManagerInitiateRecovery(t *testing.T) {
	em := NewEdgeManager(nil)
	fm := NewFailoverManager(em, nil)

	_ = fm.AddPolicy(&FailoverPolicy{
		ID:            "test-policy",
		SourceRegion:  "us-east",
		TargetRegions: []string{"us-west"},
		Enabled:       true,
	})

	fm.UpdateRegionHealth(&RegionHealth{Region: "us-west", Healthy: true})

	ctx := context.Background()
	_ = fm.InitiateFailover(ctx, "test-policy")
	time.Sleep(50 * time.Millisecond)

	err := fm.InitiateRecovery(ctx, "test-policy")
	if err != nil {
		t.Fatalf("recovery failed: %v", err)
	}

	time.Sleep(50 * time.Millisecond)

	activeFailovers := fm.GetActiveFailovers()
	if len(activeFailovers) != 0 {
		t.Errorf("expected 0 active failovers after recovery, got %d", len(activeFailovers))
	}

	if fm.GetState() != FailoverStateNormal {
		t.Error("expected normal state after recovery")
	}
}

func TestFailoverManagerInitiateRecoveryNoActive(t *testing.T) {
	em := NewEdgeManager(nil)
	fm := NewFailoverManager(em, nil)

	ctx := context.Background()
	err := fm.InitiateRecovery(ctx, "nonexistent")
	if err == nil {
		t.Error("expected error for no active failover")
	}
}

func TestFailoverManagerGetCurrentTarget(t *testing.T) {
	em := NewEdgeManager(nil)
	fm := NewFailoverManager(em, nil)

	// No failover, returns source
	target := fm.GetCurrentTarget("us-east")
	if target != "us-east" {
		t.Errorf("expected us-east, got %s", target)
	}

	_ = fm.AddPolicy(&FailoverPolicy{
		ID:            "test",
		SourceRegion:  "us-east",
		TargetRegions: []string{"us-west"},
		Enabled:       true,
	})
	fm.UpdateRegionHealth(&RegionHealth{Region: "us-west", Healthy: true})

	ctx := context.Background()
	_ = fm.InitiateFailover(ctx, "test")
	time.Sleep(50 * time.Millisecond)

	target = fm.GetCurrentTarget("us-east")
	if target != "us-west" {
		t.Errorf("expected us-west during failover, got %s", target)
	}
}

func TestFailoverManagerEvents(t *testing.T) {
	em := NewEdgeManager(nil)
	fm := NewFailoverManager(em, nil)

	_ = fm.AddPolicy(&FailoverPolicy{
		ID:            "test",
		SourceRegion:  "us-east",
		TargetRegions: []string{"us-west"},
		Enabled:       true,
	})
	fm.UpdateRegionHealth(&RegionHealth{Region: "us-west", Healthy: true})

	ctx := context.Background()
	_ = fm.InitiateFailover(ctx, "test")
	time.Sleep(50 * time.Millisecond)

	events := fm.GetEvents(10)
	if len(events) < 2 {
		t.Errorf("expected at least 2 events, got %d", len(events))
	}

	// Should have started and completed events
	hasStarted := false
	hasCompleted := false
	for _, e := range events {
		if e.Type == EventFailoverStarted {
			hasStarted = true
		}
		if e.Type == EventFailoverCompleted {
			hasCompleted = true
		}
	}
	if !hasStarted {
		t.Error("expected failover started event")
	}
	if !hasCompleted {
		t.Error("expected failover completed event")
	}
}

func TestFailoverManagerAcknowledgeEvent(t *testing.T) {
	em := NewEdgeManager(nil)
	fm := NewFailoverManager(em, nil)

	_ = fm.AddPolicy(&FailoverPolicy{
		ID:            "test",
		SourceRegion:  "us-east",
		TargetRegions: []string{"us-west"},
		Enabled:       true,
	})
	fm.UpdateRegionHealth(&RegionHealth{Region: "us-west", Healthy: true})

	ctx := context.Background()
	_ = fm.InitiateFailover(ctx, "test")
	time.Sleep(50 * time.Millisecond)

	events := fm.GetEvents(1)
	if len(events) == 0 {
		t.Fatal("no events")
	}

	err := fm.AcknowledgeEvent(events[0].ID, "admin")
	if err != nil {
		t.Fatalf("acknowledge failed: %v", err)
	}

	events = fm.GetEvents(10)
	found := false
	for _, e := range events {
		if e.Acknowledged && e.AcknowledgedBy == "admin" {
			found = true
		}
	}
	if !found {
		t.Error("event not acknowledged")
	}
}

func TestFailoverManagerAcknowledgeNonexistent(t *testing.T) {
	em := NewEdgeManager(nil)
	fm := NewFailoverManager(em, nil)

	err := fm.AcknowledgeEvent("nonexistent", "admin")
	if err == nil {
		t.Error("expected error")
	}
}

func TestFailoverManagerGenerateReport(t *testing.T) {
	em := NewEdgeManager(nil)
	fm := NewFailoverManager(em, nil)

	_ = fm.AddPolicy(&FailoverPolicy{
		ID:            "p1",
		SourceRegion:  "us-east",
		TargetRegions: []string{"us-west"},
		Enabled:       true,
	})
	_ = fm.AddPolicy(&FailoverPolicy{
		ID:            "p2",
		SourceRegion:  "eu-west",
		TargetRegions: []string{"eu-central"},
		Enabled:       false,
	})

	fm.UpdateRegionHealth(&RegionHealth{Region: "us-east", Healthy: true})
	fm.UpdateRegionHealth(&RegionHealth{Region: "eu-west", Healthy: false})

	report := fm.GenerateReport()

	if report.TotalPolicies != 2 {
		t.Errorf("expected 2 policies, got %d", report.TotalPolicies)
	}
	if report.EnabledPolicies != 1 {
		t.Errorf("expected 1 enabled, got %d", report.EnabledPolicies)
	}
	if report.TotalRegions != 2 {
		t.Errorf("expected 2 regions, got %d", report.TotalRegions)
	}
	if report.HealthyRegions != 1 {
		t.Errorf("expected 1 healthy, got %d", report.HealthyRegions)
	}
	if report.UnhealthyRegions != 1 {
		t.Errorf("expected 1 unhealthy, got %d", report.UnhealthyRegions)
	}
}

func TestDefaultFailoverConfig(t *testing.T) {
	config := DefaultFailoverConfig()

	if config.HealthCheckInterval != 30*time.Second {
		t.Error("unexpected health check interval")
	}
	if !config.EnableAutoFailover {
		t.Error("expected auto failover enabled")
	}
	if !config.EnableAutoRecovery {
		t.Error("expected auto recovery enabled")
	}
}

func TestCommonFailoverPolicies(t *testing.T) {
	policies := CommonFailoverPolicies()

	if len(policies) == 0 {
		t.Fatal("expected common policies")
	}

	foundUS := false
	for _, p := range policies {
		if p.ID == "us-failover" {
			foundUS = true
			if p.SourceRegion != "us-east" {
				t.Error("expected us-east source")
			}
		}
	}
	if !foundUS {
		t.Error("expected US failover policy")
	}
}

func TestFailoverStates(t *testing.T) {
	states := []FailoverState{
		FailoverStateNormal,
		FailoverStateDetecting,
		FailoverStateFailover,
		FailoverStateRecovery,
	}

	for _, s := range states {
		if s == "" {
			t.Error("state should not be empty")
		}
	}
}

func TestFailoverEventTypes(t *testing.T) {
	types := []FailoverEventType{
		EventFailoverStarted,
		EventFailoverCompleted,
		EventFailoverFailed,
		EventRecoveryStarted,
		EventRecoveryCompleted,
		EventHealthDegraded,
		EventHealthRestored,
	}

	for _, et := range types {
		if et == "" {
			t.Error("event type should not be empty")
		}
	}
}

func TestFailoverManagerGetState(t *testing.T) {
	em := NewEdgeManager(nil)
	fm := NewFailoverManager(em, nil)

	if fm.GetState() != FailoverStateNormal {
		t.Error("expected initial state to be normal")
	}
}
