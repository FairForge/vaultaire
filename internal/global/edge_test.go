// internal/global/edge_test.go
package global

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestNewEdgeManager(t *testing.T) {
	m := NewEdgeManager(nil)
	if m == nil {
		t.Fatal("expected non-nil manager")
	}
	if m.config == nil {
		t.Error("expected default config")
	}
}

func TestEdgeManagerWithConfig(t *testing.T) {
	config := &EdgeConfig{
		HealthCheckInterval: time.Minute,
		MaxLatency:          100 * time.Millisecond,
	}
	m := NewEdgeManager(config)

	if m.config.HealthCheckInterval != time.Minute {
		t.Error("config not applied")
	}
}

func TestEdgeManagerRegisterLocation(t *testing.T) {
	m := NewEdgeManager(nil)

	err := m.RegisterLocation(&EdgeLocation{
		ID:      "test-1",
		Name:    "Test Location",
		Region:  "us-east",
		Country: "US",
		Enabled: true,
	})
	if err != nil {
		t.Fatalf("failed to register: %v", err)
	}

	loc, ok := m.GetLocation("test-1")
	if !ok {
		t.Fatal("location not found")
	}
	if loc.Name != "Test Location" {
		t.Errorf("expected 'Test Location', got '%s'", loc.Name)
	}
}

func TestEdgeManagerRegisterLocationNoID(t *testing.T) {
	m := NewEdgeManager(nil)

	err := m.RegisterLocation(&EdgeLocation{
		Name: "No ID",
	})
	if err == nil {
		t.Error("expected error for missing ID")
	}
}

func TestEdgeManagerUnregisterLocation(t *testing.T) {
	m := NewEdgeManager(nil)

	_ = m.RegisterLocation(&EdgeLocation{ID: "test-1", Enabled: true})
	m.UnregisterLocation("test-1")

	_, ok := m.GetLocation("test-1")
	if ok {
		t.Error("location should be removed")
	}
}

func TestEdgeManagerGetLocations(t *testing.T) {
	m := NewEdgeManager(nil)

	_ = m.RegisterLocation(&EdgeLocation{ID: "loc-1", Enabled: true})
	_ = m.RegisterLocation(&EdgeLocation{ID: "loc-2", Enabled: true})
	_ = m.RegisterLocation(&EdgeLocation{ID: "loc-3", Enabled: false})

	all := m.GetLocations()
	if len(all) != 3 {
		t.Errorf("expected 3 locations, got %d", len(all))
	}

	enabled := m.GetEnabledLocations()
	if len(enabled) != 2 {
		t.Errorf("expected 2 enabled locations, got %d", len(enabled))
	}
}

func TestEdgeManagerGetHealthyLocations(t *testing.T) {
	m := NewEdgeManager(nil)

	_ = m.RegisterLocation(&EdgeLocation{ID: "healthy", Enabled: true})
	_ = m.RegisterLocation(&EdgeLocation{ID: "unhealthy", Enabled: true})

	m.SetLocationHealth("healthy", true, 10*time.Millisecond, nil)
	m.SetLocationHealth("unhealthy", false, 0, nil)

	healthy := m.GetHealthyLocations()
	if len(healthy) != 1 {
		t.Errorf("expected 1 healthy location, got %d", len(healthy))
	}
	if healthy[0].ID != "healthy" {
		t.Error("expected 'healthy' location")
	}
}

func TestEdgeManagerGetLocationsByRegion(t *testing.T) {
	m := NewEdgeManager(nil)

	_ = m.RegisterLocation(&EdgeLocation{ID: "us-1", Region: "us-east", Enabled: true})
	_ = m.RegisterLocation(&EdgeLocation{ID: "us-2", Region: "us-east", Enabled: true})
	_ = m.RegisterLocation(&EdgeLocation{ID: "eu-1", Region: "eu-west", Enabled: true})

	usLocs := m.GetLocationsByRegion("us-east")
	if len(usLocs) != 2 {
		t.Errorf("expected 2 us-east locations, got %d", len(usLocs))
	}

	euLocs := m.GetLocationsByRegion("eu-west")
	if len(euLocs) != 1 {
		t.Errorf("expected 1 eu-west location, got %d", len(euLocs))
	}
}

func TestEdgeManagerGetLocationsByCountry(t *testing.T) {
	m := NewEdgeManager(nil)

	_ = m.RegisterLocation(&EdgeLocation{ID: "us-1", Country: "US", Enabled: true})
	_ = m.RegisterLocation(&EdgeLocation{ID: "de-1", Country: "DE", Enabled: true})

	usLocs := m.GetLocationsByCountry("US")
	if len(usLocs) != 1 {
		t.Errorf("expected 1 US location, got %d", len(usLocs))
	}
}

func TestEdgeManagerUpdateStatus(t *testing.T) {
	m := NewEdgeManager(nil)

	_ = m.RegisterLocation(&EdgeLocation{ID: "test-1", Enabled: true})

	m.UpdateStatus(&EdgeStatus{
		LocationID:   "test-1",
		Healthy:      true,
		Latency:      50 * time.Millisecond,
		RequestCount: 100,
	})

	status, ok := m.GetStatus("test-1")
	if !ok {
		t.Fatal("status not found")
	}
	if !status.Healthy {
		t.Error("expected healthy")
	}
	if status.RequestCount != 100 {
		t.Errorf("expected 100 requests, got %d", status.RequestCount)
	}
}

func TestHaversineDistance(t *testing.T) {
	// New York to London
	nyLat, nyLon := 40.7128, -74.0060
	lonLat, lonLon := 51.5074, -0.1278

	dist := haversineDistance(nyLat, nyLon, lonLat, lonLon)

	// Should be approximately 5570 km
	if dist < 5500 || dist > 5600 {
		t.Errorf("expected ~5570 km, got %.2f km", dist)
	}
}

func TestHaversineDistanceSamePoint(t *testing.T) {
	dist := haversineDistance(40.7128, -74.0060, 40.7128, -74.0060)
	if dist != 0 {
		t.Errorf("expected 0, got %f", dist)
	}
}

func TestEdgeManagerFindNearestLocation(t *testing.T) {
	m := NewEdgeManager(nil)

	// Register NYC and London locations
	_ = m.RegisterLocation(&EdgeLocation{
		ID:        "nyc",
		Latitude:  40.7128,
		Longitude: -74.0060,
		Enabled:   true,
	})
	_ = m.RegisterLocation(&EdgeLocation{
		ID:        "london",
		Latitude:  51.5074,
		Longitude: -0.1278,
		Enabled:   true,
	})

	// Find nearest to Boston (should be NYC)
	nearest := m.FindNearestLocation(42.3601, -71.0589)
	if nearest == nil {
		t.Fatal("expected to find nearest")
	}
	if nearest.ID != "nyc" {
		t.Errorf("expected nyc, got %s", nearest.ID)
	}

	// Find nearest to Paris (should be London)
	nearest = m.FindNearestLocation(48.8566, 2.3522)
	if nearest == nil {
		t.Fatal("expected to find nearest")
	}
	if nearest.ID != "london" {
		t.Errorf("expected london, got %s", nearest.ID)
	}
}

func TestEdgeManagerFindNearestLocationSkipsUnhealthy(t *testing.T) {
	m := NewEdgeManager(nil)

	_ = m.RegisterLocation(&EdgeLocation{
		ID:        "close-unhealthy",
		Latitude:  40.7128,
		Longitude: -74.0060,
		Enabled:   true,
	})
	_ = m.RegisterLocation(&EdgeLocation{
		ID:        "far-healthy",
		Latitude:  51.5074,
		Longitude: -0.1278,
		Enabled:   true,
	})

	m.SetLocationHealth("close-unhealthy", false, 0, nil)
	m.SetLocationHealth("far-healthy", true, 10*time.Millisecond, nil)

	// Should skip unhealthy and return far one
	nearest := m.FindNearestLocation(40.7128, -74.0060)
	if nearest == nil {
		t.Fatal("expected to find nearest")
	}
	if nearest.ID != "far-healthy" {
		t.Errorf("expected far-healthy, got %s", nearest.ID)
	}
}

func TestEdgeManagerFindNearestLocations(t *testing.T) {
	m := NewEdgeManager(nil)

	_ = m.RegisterLocation(&EdgeLocation{ID: "loc-1", Latitude: 40.0, Longitude: -74.0, Enabled: true})
	_ = m.RegisterLocation(&EdgeLocation{ID: "loc-2", Latitude: 41.0, Longitude: -73.0, Enabled: true})
	_ = m.RegisterLocation(&EdgeLocation{ID: "loc-3", Latitude: 42.0, Longitude: -72.0, Enabled: true})

	nearest := m.FindNearestLocations(40.5, -73.5, 2)
	if len(nearest) != 2 {
		t.Errorf("expected 2 locations, got %d", len(nearest))
	}
}

func TestEdgeManagerSelectLocation(t *testing.T) {
	m := NewEdgeManager(nil)

	_ = m.RegisterLocation(&EdgeLocation{
		ID:        "loc-1",
		Region:    "us-east",
		Latitude:  40.0,
		Longitude: -74.0,
		Enabled:   true,
		Weight:    10,
	})
	_ = m.RegisterLocation(&EdgeLocation{
		ID:        "loc-2",
		Region:    "eu-west",
		Latitude:  51.0,
		Longitude: -0.1,
		Enabled:   true,
		Weight:    5,
	})

	m.SetLocationHealth("loc-1", true, 20*time.Millisecond, nil)
	m.SetLocationHealth("loc-2", true, 100*time.Millisecond, nil)

	// Select with region constraint
	opts := &SelectionOptions{
		RequiredRegion: "us-east",
	}
	selected := m.SelectLocation(40.5, -73.5, opts)
	if selected == nil {
		t.Fatal("expected selection")
	}
	if selected.ID != "loc-1" {
		t.Errorf("expected loc-1, got %s", selected.ID)
	}
}

func TestEdgeManagerSelectLocationNoMatch(t *testing.T) {
	m := NewEdgeManager(nil)

	_ = m.RegisterLocation(&EdgeLocation{
		ID:      "loc-1",
		Region:  "us-east",
		Enabled: true,
	})

	opts := &SelectionOptions{
		RequiredRegion: "ap-southeast",
	}
	selected := m.SelectLocation(0, 0, opts)
	if selected != nil {
		t.Error("expected no selection")
	}
}

func TestDefaultSelectionOptions(t *testing.T) {
	opts := DefaultSelectionOptions()

	if !opts.PreferLowLatency {
		t.Error("expected PreferLowLatency true")
	}
	if opts.WeightDistance+opts.WeightLatency+opts.WeightCapacity+opts.WeightLoad != 1.0 {
		t.Error("weights should sum to 1.0")
	}
}

func TestDefaultEdgeConfig(t *testing.T) {
	config := DefaultEdgeConfig()

	if config.HealthCheckInterval != 30*time.Second {
		t.Errorf("unexpected health check interval: %v", config.HealthCheckInterval)
	}
	if config.MaxLatency != 500*time.Millisecond {
		t.Errorf("unexpected max latency: %v", config.MaxLatency)
	}
}

func TestPredefinedEdgeLocations(t *testing.T) {
	locations := PredefinedEdgeLocations()

	if len(locations) == 0 {
		t.Fatal("expected predefined locations")
	}

	// Check for expected regions
	regions := make(map[string]bool)
	for _, loc := range locations {
		regions[loc.Region] = true

		// Validate coordinates
		if loc.Latitude < -90 || loc.Latitude > 90 {
			t.Errorf("invalid latitude for %s: %f", loc.ID, loc.Latitude)
		}
		if loc.Longitude < -180 || loc.Longitude > 180 {
			t.Errorf("invalid longitude for %s: %f", loc.ID, loc.Longitude)
		}
	}

	expectedRegions := []string{"us-east", "us-west", "eu-west", "ap-northeast"}
	for _, r := range expectedRegions {
		if !regions[r] {
			t.Errorf("missing expected region: %s", r)
		}
	}
}

func TestGetRegions(t *testing.T) {
	regions := GetRegions()

	if len(regions) == 0 {
		t.Fatal("expected regions")
	}

	// Check EU regions have GDPR
	euWest, ok := regions["eu-west"]
	if !ok {
		t.Fatal("expected eu-west region")
	}

	hasGDPR := false
	for _, reg := range euWest.Regulations {
		if reg == "GDPR" {
			hasGDPR = true
			break
		}
	}
	if !hasGDPR {
		t.Error("expected GDPR regulation for eu-west")
	}
}

func TestEdgeHealthChecker(t *testing.T) {
	m := NewEdgeManager(&EdgeConfig{
		HealthCheckInterval: 50 * time.Millisecond,
		HealthCheckTimeout:  10 * time.Millisecond,
		MaxLatency:          100 * time.Millisecond,
	})

	_ = m.RegisterLocation(&EdgeLocation{ID: "test-1", Enabled: true})

	checkCount := 0
	checker := func(ctx context.Context, loc *EdgeLocation) (time.Duration, error) {
		checkCount++
		return 5 * time.Millisecond, nil
	}

	hc := NewEdgeHealthChecker(m, checker)
	hc.Start()

	time.Sleep(150 * time.Millisecond)
	hc.Stop()

	if checkCount < 2 {
		t.Errorf("expected at least 2 checks, got %d", checkCount)
	}

	status, _ := m.GetStatus("test-1")
	if !status.Healthy {
		t.Error("expected healthy status")
	}
}

func TestCalculateLocationScore(t *testing.T) {
	m := NewEdgeManager(nil)

	loc := &EdgeLocation{
		ID:        "test",
		Latitude:  40.0,
		Longitude: -74.0,
		Capacity:  1000,
		UsedBytes: 200,
		MaxConns:  100,
		Weight:    10,
		Enabled:   true,
	}

	status := &EdgeStatus{
		LocationID:  "test",
		Healthy:     true,
		Latency:     50 * time.Millisecond,
		ActiveConns: 20,
	}

	opts := DefaultSelectionOptions()
	score := m.calculateLocationScore(loc, status, 40.0, -74.0, opts)

	if score <= 0 {
		t.Errorf("expected positive score, got %d", score)
	}
}

func TestEdgeManagerConcurrency(t *testing.T) {
	m := NewEdgeManager(nil)

	// Register locations
	for i := 0; i < 10; i++ {
		_ = m.RegisterLocation(&EdgeLocation{
			ID:      fmt.Sprintf("loc-%d", i),
			Enabled: true,
		})
	}

	// Concurrent reads and writes
	done := make(chan bool)
	for i := 0; i < 10; i++ {
		go func() {
			for j := 0; j < 100; j++ {
				_ = m.GetLocations()
				_ = m.GetHealthyLocations()
				m.SetLocationHealth("loc-0", true, time.Millisecond, nil)
			}
			done <- true
		}()
	}

	for i := 0; i < 10; i++ {
		<-done
	}
}

func TestFindNearestLocationNoLocations(t *testing.T) {
	m := NewEdgeManager(nil)

	nearest := m.FindNearestLocation(40.0, -74.0)
	if nearest != nil {
		t.Error("expected nil for empty manager")
	}
}

func TestFindNearestLocationsMoreThanAvailable(t *testing.T) {
	m := NewEdgeManager(nil)

	_ = m.RegisterLocation(&EdgeLocation{ID: "loc-1", Latitude: 40.0, Longitude: -74.0, Enabled: true})

	nearest := m.FindNearestLocations(40.0, -74.0, 10)
	if len(nearest) != 1 {
		t.Errorf("expected 1 location, got %d", len(nearest))
	}
}

func TestHaversineDistanceAntipodal(t *testing.T) {
	// Points on opposite sides of Earth
	dist := haversineDistance(0, 0, 0, 180)

	// Should be approximately half Earth circumference (~20000 km)
	if dist < 19000 || dist > 21000 {
		t.Errorf("expected ~20000 km, got %.2f km", dist)
	}
}

func TestSelectLocationWithCapacityWeight(t *testing.T) {
	m := NewEdgeManager(nil)

	_ = m.RegisterLocation(&EdgeLocation{
		ID:        "full",
		Latitude:  40.0,
		Longitude: -74.0,
		Capacity:  1000,
		UsedBytes: 950,
		Enabled:   true,
	})
	_ = m.RegisterLocation(&EdgeLocation{
		ID:        "empty",
		Latitude:  40.1,
		Longitude: -74.1,
		Capacity:  1000,
		UsedBytes: 100,
		Enabled:   true,
	})

	m.SetLocationHealth("full", true, 10*time.Millisecond, nil)
	m.SetLocationHealth("empty", true, 10*time.Millisecond, nil)

	opts := &SelectionOptions{
		WeightCapacity: 0.9,
		WeightDistance: 0.1,
	}

	selected := m.SelectLocation(40.0, -74.0, opts)
	if selected == nil {
		t.Fatal("expected selection")
	}
	// Should prefer empty even though full is closer
	if selected.ID != "empty" {
		t.Errorf("expected empty (more capacity), got %s", selected.ID)
	}
}
