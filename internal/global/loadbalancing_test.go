// internal/global/loadbalancing_test.go
package global

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestNewGlobalLoadBalancer(t *testing.T) {
	em := NewEdgeManager(nil)
	lb := NewGlobalLoadBalancer(em, nil)

	if lb == nil {
		t.Fatal("expected non-nil load balancer")
	}
	if lb.config == nil {
		t.Error("expected default config")
	}
}

func TestLoadBalancerAddBackend(t *testing.T) {
	em := NewEdgeManager(nil)
	lb := NewGlobalLoadBalancer(em, nil)

	err := lb.AddBackend(&Backend{
		ID:      "backend-1",
		Address: "10.0.0.1",
		Port:    8080,
	})

	if err != nil {
		t.Fatalf("failed to add backend: %v", err)
	}

	backend, ok := lb.GetBackend("backend-1")
	if !ok {
		t.Fatal("backend not found")
	}
	if backend.Weight != 1 {
		t.Error("expected default weight of 1")
	}
	if backend.State != BackendStateHealthy {
		t.Error("expected default state healthy")
	}
}

func TestLoadBalancerAddBackendNoID(t *testing.T) {
	em := NewEdgeManager(nil)
	lb := NewGlobalLoadBalancer(em, nil)

	err := lb.AddBackend(&Backend{Address: "10.0.0.1"})
	if err == nil {
		t.Error("expected error for missing ID")
	}
}

func TestLoadBalancerRemoveBackend(t *testing.T) {
	em := NewEdgeManager(nil)
	lb := NewGlobalLoadBalancer(em, nil)

	_ = lb.AddBackend(&Backend{ID: "backend-1"})

	removed := lb.RemoveBackend("backend-1")
	if !removed {
		t.Error("expected backend to be removed")
	}

	_, ok := lb.GetBackend("backend-1")
	if ok {
		t.Error("backend should not exist")
	}

	removed = lb.RemoveBackend("nonexistent")
	if removed {
		t.Error("should not remove nonexistent")
	}
}

func TestLoadBalancerGetBackends(t *testing.T) {
	em := NewEdgeManager(nil)
	lb := NewGlobalLoadBalancer(em, nil)

	_ = lb.AddBackend(&Backend{ID: "backend-1"})
	_ = lb.AddBackend(&Backend{ID: "backend-2"})

	backends := lb.GetBackends()
	if len(backends) != 2 {
		t.Errorf("expected 2 backends, got %d", len(backends))
	}
}

func TestLoadBalancerGetHealthyBackends(t *testing.T) {
	em := NewEdgeManager(nil)
	lb := NewGlobalLoadBalancer(em, nil)

	_ = lb.AddBackend(&Backend{ID: "healthy", State: BackendStateHealthy})
	_ = lb.AddBackend(&Backend{ID: "unhealthy", State: BackendStateUnhealthy})

	healthy := lb.GetHealthyBackends()
	if len(healthy) != 1 {
		t.Errorf("expected 1 healthy, got %d", len(healthy))
	}
}

func TestLoadBalancerSetBackendState(t *testing.T) {
	em := NewEdgeManager(nil)
	lb := NewGlobalLoadBalancer(em, nil)

	_ = lb.AddBackend(&Backend{ID: "backend-1"})

	err := lb.SetBackendState("backend-1", BackendStateUnhealthy)
	if err != nil {
		t.Fatalf("failed to set state: %v", err)
	}

	backend, _ := lb.GetBackend("backend-1")
	if backend.State != BackendStateUnhealthy {
		t.Error("state not updated")
	}

	err = lb.SetBackendState("nonexistent", BackendStateHealthy)
	if err == nil {
		t.Error("expected error for nonexistent backend")
	}
}

func TestLoadBalancerSetAlgorithm(t *testing.T) {
	em := NewEdgeManager(nil)
	lb := NewGlobalLoadBalancer(em, nil)

	lb.SetAlgorithm(AlgorithmLeastConnections)

	if lb.GetAlgorithm() != AlgorithmLeastConnections {
		t.Error("algorithm not updated")
	}
}

func TestLoadBalancerRoundRobin(t *testing.T) {
	em := NewEdgeManager(nil)
	config := &LoadBalancerConfig{Algorithm: AlgorithmRoundRobin}
	lb := NewGlobalLoadBalancer(em, config)

	_ = lb.AddBackend(&Backend{ID: "b1"})
	_ = lb.AddBackend(&Backend{ID: "b2"})
	_ = lb.AddBackend(&Backend{ID: "b3"})

	ctx := context.Background()
	selected := make(map[string]int)

	for i := 0; i < 9; i++ {
		backend, err := lb.SelectBackend(ctx, &RequestContext{})
		if err != nil {
			t.Fatalf("select error: %v", err)
		}
		selected[backend.ID]++
	}

	// Each should be selected 3 times
	for id, count := range selected {
		if count != 3 {
			t.Errorf("expected %s selected 3 times, got %d", id, count)
		}
	}
}

func TestLoadBalancerWeightedRoundRobin(t *testing.T) {
	em := NewEdgeManager(nil)
	config := &LoadBalancerConfig{Algorithm: AlgorithmWeightedRoundRobin}
	lb := NewGlobalLoadBalancer(em, config)

	_ = lb.AddBackend(&Backend{ID: "heavy", Weight: 3})
	_ = lb.AddBackend(&Backend{ID: "light", Weight: 1})

	ctx := context.Background()
	selected := make(map[string]int)

	for i := 0; i < 40; i++ {
		backend, _ := lb.SelectBackend(ctx, &RequestContext{})
		selected[backend.ID]++
	}

	// Heavy should get roughly 3x more requests
	if selected["heavy"] < selected["light"] {
		t.Error("expected heavy to get more requests")
	}
}

func TestLoadBalancerLeastConnections(t *testing.T) {
	em := NewEdgeManager(nil)
	config := &LoadBalancerConfig{Algorithm: AlgorithmLeastConnections}
	lb := NewGlobalLoadBalancer(em, config)

	_ = lb.AddBackend(&Backend{ID: "busy", ActiveConns: 100})
	_ = lb.AddBackend(&Backend{ID: "free", ActiveConns: 10})

	ctx := context.Background()
	backend, err := lb.SelectBackend(ctx, &RequestContext{})

	if err != nil {
		t.Fatalf("select error: %v", err)
	}
	if backend.ID != "free" {
		t.Errorf("expected free, got %s", backend.ID)
	}
}

func TestLoadBalancerLeastResponseTime(t *testing.T) {
	em := NewEdgeManager(nil)
	config := &LoadBalancerConfig{Algorithm: AlgorithmLeastResponseTime}
	lb := NewGlobalLoadBalancer(em, config)

	_ = lb.AddBackend(&Backend{ID: "slow", AvgResponseTime: 500 * time.Millisecond})
	_ = lb.AddBackend(&Backend{ID: "fast", AvgResponseTime: 50 * time.Millisecond})

	ctx := context.Background()
	backend, _ := lb.SelectBackend(ctx, &RequestContext{})

	if backend.ID != "fast" {
		t.Errorf("expected fast, got %s", backend.ID)
	}
}

func TestLoadBalancerIPHash(t *testing.T) {
	em := NewEdgeManager(nil)
	config := &LoadBalancerConfig{Algorithm: AlgorithmIPHash}
	lb := NewGlobalLoadBalancer(em, config)

	_ = lb.AddBackend(&Backend{ID: "b1"})
	_ = lb.AddBackend(&Backend{ID: "b2"})

	ctx := context.Background()
	reqCtx := &RequestContext{ClientIP: "192.168.1.100"}

	// Same IP should always go to same backend
	first, _ := lb.SelectBackend(ctx, reqCtx)
	for i := 0; i < 10; i++ {
		backend, _ := lb.SelectBackend(ctx, reqCtx)
		if backend.ID != first.ID {
			t.Error("IP hash not consistent")
		}
	}
}

func TestLoadBalancerGeoProximity(t *testing.T) {
	em := NewEdgeManager(nil)
	_ = em.RegisterLocation(&EdgeLocation{
		ID:        "us-east",
		Latitude:  39.0,
		Longitude: -77.0,
		Enabled:   true,
	})
	_ = em.RegisterLocation(&EdgeLocation{
		ID:        "eu-west",
		Latitude:  53.0,
		Longitude: -6.0,
		Enabled:   true,
	})

	config := &LoadBalancerConfig{Algorithm: AlgorithmGeoProximity}
	lb := NewGlobalLoadBalancer(em, config)

	_ = lb.AddBackend(&Backend{ID: "us-backend", LocationID: "us-east"})
	_ = lb.AddBackend(&Backend{ID: "eu-backend", LocationID: "eu-west"})

	ctx := context.Background()

	// Request from NYC area
	reqCtx := &RequestContext{Latitude: 40.7, Longitude: -74.0}
	backend, _ := lb.SelectBackend(ctx, reqCtx)
	if backend.ID != "us-backend" {
		t.Errorf("expected us-backend for NYC, got %s", backend.ID)
	}

	// Request from London area
	reqCtx = &RequestContext{Latitude: 51.5, Longitude: -0.1}
	backend, _ = lb.SelectBackend(ctx, reqCtx)
	if backend.ID != "eu-backend" {
		t.Errorf("expected eu-backend for London, got %s", backend.ID)
	}
}

func TestLoadBalancerRandom(t *testing.T) {
	em := NewEdgeManager(nil)
	config := &LoadBalancerConfig{Algorithm: AlgorithmRandom}
	lb := NewGlobalLoadBalancer(em, config)

	_ = lb.AddBackend(&Backend{ID: "b1"})
	_ = lb.AddBackend(&Backend{ID: "b2"})

	ctx := context.Background()
	selected := make(map[string]int)

	for i := 0; i < 100; i++ {
		backend, _ := lb.SelectBackend(ctx, &RequestContext{})
		selected[backend.ID]++
	}

	// Both should be selected (random distribution)
	if selected["b1"] == 0 || selected["b2"] == 0 {
		t.Error("random should select all backends")
	}
}

func TestLoadBalancerNoHealthyBackends(t *testing.T) {
	em := NewEdgeManager(nil)
	lb := NewGlobalLoadBalancer(em, nil)

	_ = lb.AddBackend(&Backend{ID: "unhealthy", State: BackendStateUnhealthy})

	ctx := context.Background()
	_, err := lb.SelectBackend(ctx, &RequestContext{})

	if err == nil {
		t.Error("expected error for no healthy backends")
	}
}

func TestLoadBalancerRecordRequest(t *testing.T) {
	em := NewEdgeManager(nil)
	lb := NewGlobalLoadBalancer(em, nil)

	_ = lb.AddBackend(&Backend{ID: "backend-1"})

	lb.RecordRequest("backend-1", 100*time.Millisecond, true)
	lb.RecordRequest("backend-1", 200*time.Millisecond, true)
	lb.RecordRequest("backend-1", 50*time.Millisecond, false)

	backend, _ := lb.GetBackend("backend-1")
	if backend.TotalRequests != 3 {
		t.Errorf("expected 3 requests, got %d", backend.TotalRequests)
	}
	if backend.FailedRequests != 1 {
		t.Errorf("expected 1 failed, got %d", backend.FailedRequests)
	}
	if backend.AvgResponseTime == 0 {
		t.Error("expected avg response time to be set")
	}
}

func TestLoadBalancerConnectionTracking(t *testing.T) {
	em := NewEdgeManager(nil)
	lb := NewGlobalLoadBalancer(em, nil)

	_ = lb.AddBackend(&Backend{ID: "backend-1"})

	lb.IncrementConnections("backend-1")
	lb.IncrementConnections("backend-1")

	backend, _ := lb.GetBackend("backend-1")
	if backend.ActiveConns != 2 {
		t.Errorf("expected 2 active, got %d", backend.ActiveConns)
	}
	if backend.TotalConns != 2 {
		t.Errorf("expected 2 total, got %d", backend.TotalConns)
	}

	lb.DecrementConnections("backend-1")
	backend, _ = lb.GetBackend("backend-1")
	if backend.ActiveConns != 1 {
		t.Errorf("expected 1 active, got %d", backend.ActiveConns)
	}
}

func TestLoadBalancerDrainBackend(t *testing.T) {
	em := NewEdgeManager(nil)
	lb := NewGlobalLoadBalancer(em, nil)

	_ = lb.AddBackend(&Backend{ID: "backend-1"})

	err := lb.DrainBackend("backend-1")
	if err != nil {
		t.Fatalf("failed to drain: %v", err)
	}

	backend, _ := lb.GetBackend("backend-1")
	if backend.State != BackendStateDraining {
		t.Error("expected draining state")
	}

	err = lb.DrainBackend("nonexistent")
	if err == nil {
		t.Error("expected error for nonexistent")
	}
}

func TestLoadBalancerGetStats(t *testing.T) {
	em := NewEdgeManager(nil)
	lb := NewGlobalLoadBalancer(em, nil)

	_ = lb.AddBackend(&Backend{ID: "b1", Region: "us-east", State: BackendStateHealthy})
	_ = lb.AddBackend(&Backend{ID: "b2", Region: "us-east", State: BackendStateUnhealthy})
	_ = lb.AddBackend(&Backend{ID: "b3", Region: "eu-west", State: BackendStateHealthy})

	lb.IncrementConnections("b1")
	lb.RecordRequest("b1", 100*time.Millisecond, true)

	stats := lb.GetStats()

	if stats.TotalBackends != 3 {
		t.Errorf("expected 3 backends, got %d", stats.TotalBackends)
	}
	if stats.HealthyBackends != 2 {
		t.Errorf("expected 2 healthy, got %d", stats.HealthyBackends)
	}
	if stats.UnhealthyBackends != 1 {
		t.Errorf("expected 1 unhealthy, got %d", stats.UnhealthyBackends)
	}
	if stats.BackendsByRegion["us-east"] != 2 {
		t.Error("expected 2 backends in us-east")
	}
}

func TestLoadBalancerGetBackendsByRegion(t *testing.T) {
	em := NewEdgeManager(nil)
	lb := NewGlobalLoadBalancer(em, nil)

	_ = lb.AddBackend(&Backend{ID: "b1", Region: "us-east"})
	_ = lb.AddBackend(&Backend{ID: "b2", Region: "us-east"})
	_ = lb.AddBackend(&Backend{ID: "b3", Region: "eu-west"})

	usBackends := lb.GetBackendsByRegion("us-east")
	if len(usBackends) != 2 {
		t.Errorf("expected 2 us-east backends, got %d", len(usBackends))
	}
}

func TestLoadBalancerSelectBackendInRegion(t *testing.T) {
	em := NewEdgeManager(nil)
	lb := NewGlobalLoadBalancer(em, nil)

	_ = lb.AddBackend(&Backend{ID: "us1", Region: "us-east", State: BackendStateHealthy})
	_ = lb.AddBackend(&Backend{ID: "us2", Region: "us-east", State: BackendStateHealthy})
	_ = lb.AddBackend(&Backend{ID: "eu1", Region: "eu-west", State: BackendStateHealthy})

	ctx := context.Background()
	backend, err := lb.SelectBackendInRegion(ctx, "us-east", &RequestContext{})

	if err != nil {
		t.Fatalf("select error: %v", err)
	}
	if backend.Region != "us-east" {
		t.Errorf("expected us-east backend, got %s", backend.Region)
	}
}

func TestLoadBalancerSelectBackendInRegionNoHealthy(t *testing.T) {
	em := NewEdgeManager(nil)
	lb := NewGlobalLoadBalancer(em, nil)

	_ = lb.AddBackend(&Backend{ID: "us1", Region: "us-east", State: BackendStateUnhealthy})

	ctx := context.Background()
	_, err := lb.SelectBackendInRegion(ctx, "us-east", &RequestContext{})

	if err == nil {
		t.Error("expected error for no healthy in region")
	}
}

func TestLoadBalancerHealthChecks(t *testing.T) {
	em := NewEdgeManager(nil)
	config := &LoadBalancerConfig{
		EnableHealthChecks:  true,
		HealthCheckInterval: 50 * time.Millisecond,
		HealthCheckTimeout:  10 * time.Millisecond,
	}
	lb := NewGlobalLoadBalancer(em, config)

	_ = lb.AddBackend(&Backend{ID: "backend-1", State: BackendStateHealthy})

	checkCount := 0
	var mu sync.Mutex
	checker := func(ctx context.Context, b *Backend) bool {
		mu.Lock()
		checkCount++
		mu.Unlock()
		return true
	}

	lb.StartHealthChecks(checker)
	time.Sleep(150 * time.Millisecond)
	lb.StopHealthChecks()

	mu.Lock()
	if checkCount < 2 {
		t.Errorf("expected at least 2 health checks, got %d", checkCount)
	}
	mu.Unlock()
}

func TestLoadBalancerHealthCheckUpdatesState(t *testing.T) {
	em := NewEdgeManager(nil)
	config := &LoadBalancerConfig{
		EnableHealthChecks:  true,
		HealthCheckInterval: 50 * time.Millisecond,
		HealthCheckTimeout:  10 * time.Millisecond,
	}
	lb := NewGlobalLoadBalancer(em, config)

	_ = lb.AddBackend(&Backend{ID: "backend-1", State: BackendStateHealthy})

	checker := func(ctx context.Context, b *Backend) bool {
		return false // Always fail
	}

	lb.StartHealthChecks(checker)
	time.Sleep(100 * time.Millisecond)
	lb.StopHealthChecks()

	backend, _ := lb.GetBackend("backend-1")
	if backend.State != BackendStateUnhealthy {
		t.Error("expected unhealthy after failed checks")
	}
}

func TestLoadBalancerConcurrentAccess(t *testing.T) {
	em := NewEdgeManager(nil)
	lb := NewGlobalLoadBalancer(em, nil)

	_ = lb.AddBackend(&Backend{ID: "b1"})
	_ = lb.AddBackend(&Backend{ID: "b2"})

	var wg sync.WaitGroup
	ctx := context.Background()

	// Concurrent selects
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = lb.SelectBackend(ctx, &RequestContext{})
		}()
	}

	// Concurrent connection tracking
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			lb.IncrementConnections("b1")
			lb.DecrementConnections("b1")
		}()
	}

	wg.Wait()
}

func TestDefaultLoadBalancerConfig(t *testing.T) {
	config := DefaultLoadBalancerConfig()

	if config.Algorithm != AlgorithmRoundRobin {
		t.Error("expected round robin default")
	}
	if config.HealthCheckInterval != 30*time.Second {
		t.Error("unexpected health check interval")
	}
}

func TestDefaultHealthCheckConfig(t *testing.T) {
	config := DefaultHealthCheckConfig()

	if !config.Enabled {
		t.Error("expected enabled by default")
	}
	if config.Path != "/health" {
		t.Errorf("unexpected path: %s", config.Path)
	}
}

func TestBackendStates(t *testing.T) {
	states := []BackendState{
		BackendStateHealthy,
		BackendStateUnhealthy,
		BackendStateDraining,
		BackendStateDisabled,
	}

	for _, s := range states {
		if s == "" {
			t.Error("state should not be empty")
		}
	}
}

func TestLoadBalancerAlgorithms(t *testing.T) {
	algorithms := []LoadBalancerAlgorithm{
		AlgorithmRoundRobin,
		AlgorithmWeightedRoundRobin,
		AlgorithmLeastConnections,
		AlgorithmLeastResponseTime,
		AlgorithmIPHash,
		AlgorithmGeoProximity,
		AlgorithmRandom,
	}

	for _, a := range algorithms {
		if a == "" {
			t.Error("algorithm should not be empty")
		}
	}
}
