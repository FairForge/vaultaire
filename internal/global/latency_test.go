// internal/global/latency_test.go
package global

import (
	"context"
	"testing"
	"time"
)

func TestNewLatencyOptimizer(t *testing.T) {
	em := NewEdgeManager(nil)
	lo := NewLatencyOptimizer(em, nil)

	if lo == nil {
		t.Fatal("expected non-nil optimizer")
	}
	if lo.config == nil {
		t.Error("expected default config")
	}
}

func TestLatencyOptimizerWithConfig(t *testing.T) {
	em := NewEdgeManager(nil)
	config := &LatencyConfig{
		MaxSamples:   50,
		SampleWindow: 30 * time.Minute,
	}
	lo := NewLatencyOptimizer(em, config)

	if lo.config.MaxSamples != 50 {
		t.Errorf("expected 50 samples, got %d", lo.config.MaxSamples)
	}
}

func TestLatencyOptimizerRecordMeasurement(t *testing.T) {
	em := NewEdgeManager(nil)
	lo := NewLatencyOptimizer(em, nil)

	m := LatencyMeasurement{
		LocationID: "us-east-1",
		Latency:    50 * time.Millisecond,
		Success:    true,
	}
	lo.RecordMeasurement(m)

	measurements := lo.GetMeasurements("us-east-1")
	if len(measurements) != 1 {
		t.Errorf("expected 1 measurement, got %d", len(measurements))
	}
	if measurements[0].Latency != 50*time.Millisecond {
		t.Error("latency not recorded correctly")
	}
}

func TestLatencyOptimizerStatsCalculation(t *testing.T) {
	em := NewEdgeManager(nil)
	config := &LatencyConfig{
		MaxSamples:         100,
		SampleWindow:       time.Hour,
		MinSamplesForStats: 3,
	}
	lo := NewLatencyOptimizer(em, config)

	// Record multiple measurements
	latencies := []time.Duration{10, 20, 30, 40, 50, 60, 70, 80, 90, 100}
	for _, l := range latencies {
		lo.RecordMeasurement(LatencyMeasurement{
			LocationID: "us-east-1",
			Latency:    l * time.Millisecond,
			Success:    true,
		})
	}

	stats, ok := lo.GetStats("us-east-1")
	if !ok {
		t.Fatal("expected stats")
	}

	if stats.SampleCount != 10 {
		t.Errorf("expected 10 samples, got %d", stats.SampleCount)
	}
	if stats.Min != 10*time.Millisecond {
		t.Errorf("expected min 10ms, got %v", stats.Min)
	}
	if stats.Max != 100*time.Millisecond {
		t.Errorf("expected max 100ms, got %v", stats.Max)
	}
	if stats.Avg != 55*time.Millisecond {
		t.Errorf("expected avg 55ms, got %v", stats.Avg)
	}
	if stats.SuccessRate != 1.0 {
		t.Errorf("expected 100%% success, got %v", stats.SuccessRate)
	}
}

func TestLatencyOptimizerSuccessRate(t *testing.T) {
	em := NewEdgeManager(nil)
	config := &LatencyConfig{
		SampleWindow:       time.Hour,
		MinSamplesForStats: 2,
	}
	lo := NewLatencyOptimizer(em, config)

	// 3 successes, 2 failures
	for i := 0; i < 3; i++ {
		lo.RecordMeasurement(LatencyMeasurement{
			LocationID: "loc-1",
			Latency:    50 * time.Millisecond,
			Success:    true,
		})
	}
	for i := 0; i < 2; i++ {
		lo.RecordMeasurement(LatencyMeasurement{
			LocationID: "loc-1",
			Success:    false,
			Error:      "timeout",
		})
	}

	stats, ok := lo.GetStats("loc-1")
	if !ok {
		t.Fatal("expected stats")
	}
	if stats.SuccessRate != 0.6 {
		t.Errorf("expected 60%% success rate, got %v", stats.SuccessRate)
	}
}

func TestLatencyOptimizerGetAllStats(t *testing.T) {
	em := NewEdgeManager(nil)
	config := &LatencyConfig{
		SampleWindow:       time.Hour,
		MinSamplesForStats: 1,
	}
	lo := NewLatencyOptimizer(em, config)

	lo.RecordMeasurement(LatencyMeasurement{LocationID: "loc-1", Latency: 10 * time.Millisecond, Success: true})
	lo.RecordMeasurement(LatencyMeasurement{LocationID: "loc-2", Latency: 20 * time.Millisecond, Success: true})

	allStats := lo.GetAllStats()
	if len(allStats) != 2 {
		t.Errorf("expected 2 locations, got %d", len(allStats))
	}
}

func TestLatencyOptimizerClearMeasurements(t *testing.T) {
	em := NewEdgeManager(nil)
	lo := NewLatencyOptimizer(em, nil)

	lo.RecordMeasurement(LatencyMeasurement{LocationID: "loc-1", Success: true})
	lo.ClearMeasurements("loc-1")

	measurements := lo.GetMeasurements("loc-1")
	if len(measurements) != 0 {
		t.Error("expected measurements to be cleared")
	}
}

func TestLatencyOptimizerClearAllMeasurements(t *testing.T) {
	em := NewEdgeManager(nil)
	lo := NewLatencyOptimizer(em, nil)

	lo.RecordMeasurement(LatencyMeasurement{LocationID: "loc-1", Success: true})
	lo.RecordMeasurement(LatencyMeasurement{LocationID: "loc-2", Success: true})
	lo.ClearAllMeasurements()

	if len(lo.GetMeasurements("loc-1")) != 0 || len(lo.GetMeasurements("loc-2")) != 0 {
		t.Error("expected all measurements to be cleared")
	}
}

func TestLatencyOptimizerRankLocations(t *testing.T) {
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

	config := &LatencyConfig{
		SampleWindow:       time.Hour,
		MinSamplesForStats: 1,
	}
	lo := NewLatencyOptimizer(em, config)

	// Record latencies - US has better latency
	lo.RecordMeasurement(LatencyMeasurement{LocationID: "us-east", Latency: 20 * time.Millisecond, Success: true})
	lo.RecordMeasurement(LatencyMeasurement{LocationID: "eu-west", Latency: 100 * time.Millisecond, Success: true})

	// Rank from NYC area (closer to us-east)
	scores := lo.RankLocations(40.7, -74.0, DefaultScoreWeights())

	if len(scores) != 2 {
		t.Fatalf("expected 2 scores, got %d", len(scores))
	}

	// US-East should rank higher (better latency + closer)
	if scores[0].LocationID != "us-east" {
		t.Errorf("expected us-east first, got %s", scores[0].LocationID)
	}
}

func TestLatencyOptimizerGetOptimalLocation(t *testing.T) {
	em := NewEdgeManager(nil)
	_ = em.RegisterLocation(&EdgeLocation{ID: "loc-1", Latitude: 40, Longitude: -74, Enabled: true})
	_ = em.RegisterLocation(&EdgeLocation{ID: "loc-2", Latitude: 51, Longitude: 0, Enabled: true})

	lo := NewLatencyOptimizer(em, nil)

	// Without latency data, should return based on distance
	loc := lo.GetOptimalLocation(41, -73) // Near NYC
	if loc == nil {
		t.Fatal("expected location")
	}
	if loc.ID != "loc-1" {
		t.Errorf("expected loc-1 (closer), got %s", loc.ID)
	}
}

func TestLatencyOptimizerGetOptimalLocations(t *testing.T) {
	em := NewEdgeManager(nil)
	_ = em.RegisterLocation(&EdgeLocation{ID: "loc-1", Latitude: 40, Longitude: -74, Enabled: true})
	_ = em.RegisterLocation(&EdgeLocation{ID: "loc-2", Latitude: 51, Longitude: 0, Enabled: true})
	_ = em.RegisterLocation(&EdgeLocation{ID: "loc-3", Latitude: 35, Longitude: 139, Enabled: true})

	lo := NewLatencyOptimizer(em, nil)

	locs := lo.GetOptimalLocations(41, -73, 2)
	if len(locs) != 2 {
		t.Errorf("expected 2 locations, got %d", len(locs))
	}
}

func TestLatencyOptimizerProbeLocation(t *testing.T) {
	em := NewEdgeManager(nil)
	lo := NewLatencyOptimizer(em, nil)

	probe := func(ctx context.Context, locationID string) (time.Duration, error) {
		return 25 * time.Millisecond, nil
	}

	ctx := context.Background()
	m, err := lo.ProbeLocation(ctx, "test-loc", probe)

	if err != nil {
		t.Fatalf("probe error: %v", err)
	}
	if m.Latency != 25*time.Millisecond {
		t.Errorf("expected 25ms, got %v", m.Latency)
	}
	if !m.Success {
		t.Error("expected success")
	}
}

func TestLatencyOptimizerProbeLocationError(t *testing.T) {
	em := NewEdgeManager(nil)
	lo := NewLatencyOptimizer(em, nil)

	probe := func(ctx context.Context, locationID string) (time.Duration, error) {
		return 0, context.DeadlineExceeded
	}

	ctx := context.Background()
	m, err := lo.ProbeLocation(ctx, "test-loc", probe)

	if err == nil {
		t.Error("expected error")
	}
	if m.Success {
		t.Error("expected failure")
	}
	if m.Error == "" {
		t.Error("expected error message")
	}
}

func TestLatencyOptimizerIsStale(t *testing.T) {
	em := NewEdgeManager(nil)
	config := &LatencyConfig{
		SampleWindow:       time.Hour,
		StaleThreshold:     time.Minute,
		MinSamplesForStats: 1,
	}
	lo := NewLatencyOptimizer(em, config)

	// No data - should be stale
	if !lo.IsStale("unknown") {
		t.Error("expected unknown location to be stale")
	}

	// Fresh data
	lo.RecordMeasurement(LatencyMeasurement{LocationID: "fresh", Latency: 10 * time.Millisecond, Success: true})
	if lo.IsStale("fresh") {
		t.Error("expected fresh location not to be stale")
	}
}

func TestLatencyOptimizerGetStaleLocations(t *testing.T) {
	em := NewEdgeManager(nil)
	_ = em.RegisterLocation(&EdgeLocation{ID: "loc-1", Enabled: true})
	_ = em.RegisterLocation(&EdgeLocation{ID: "loc-2", Enabled: true})

	config := &LatencyConfig{
		SampleWindow:       time.Hour,
		StaleThreshold:     time.Minute,
		MinSamplesForStats: 1,
	}
	lo := NewLatencyOptimizer(em, config)

	// Only loc-1 has data
	lo.RecordMeasurement(LatencyMeasurement{LocationID: "loc-1", Latency: 10 * time.Millisecond, Success: true})

	stale := lo.GetStaleLocations()
	if len(stale) != 1 {
		t.Errorf("expected 1 stale location, got %d", len(stale))
	}
	if stale[0] != "loc-2" {
		t.Errorf("expected loc-2 stale, got %s", stale[0])
	}
}

func TestLatencyOptimizerGenerateReport(t *testing.T) {
	em := NewEdgeManager(nil)
	_ = em.RegisterLocation(&EdgeLocation{ID: "loc-1", Enabled: true})
	_ = em.RegisterLocation(&EdgeLocation{ID: "loc-2", Enabled: true})

	config := &LatencyConfig{
		SampleWindow:       time.Hour,
		MinSamplesForStats: 1,
	}
	lo := NewLatencyOptimizer(em, config)

	lo.RecordMeasurement(LatencyMeasurement{LocationID: "loc-1", Latency: 20 * time.Millisecond, Success: true})
	lo.RecordMeasurement(LatencyMeasurement{LocationID: "loc-2", Latency: 50 * time.Millisecond, Success: true})

	report := lo.GenerateReport()

	if report.TotalLocations != 2 {
		t.Errorf("expected 2 locations, got %d", report.TotalLocations)
	}
	if report.LocationsWithData != 2 {
		t.Errorf("expected 2 locations with data, got %d", report.LocationsWithData)
	}
	if report.BestLocation != "loc-1" {
		t.Errorf("expected loc-1 best, got %s", report.BestLocation)
	}
	if report.WorstLocation != "loc-2" {
		t.Errorf("expected loc-2 worst, got %s", report.WorstLocation)
	}
}

func TestLatencyOptimizerMeetsBudget(t *testing.T) {
	em := NewEdgeManager(nil)
	config := &LatencyConfig{
		SampleWindow:       time.Hour,
		MinSamplesForStats: 1,
	}
	lo := NewLatencyOptimizer(em, config)

	lo.RecordMeasurement(LatencyMeasurement{LocationID: "fast", Latency: 10 * time.Millisecond, Success: true})
	lo.RecordMeasurement(LatencyMeasurement{LocationID: "slow", Latency: 200 * time.Millisecond, Success: true})

	budget := LatencyBudget{
		MaxP50:         50 * time.Millisecond,
		MinSuccessRate: 0.9,
	}

	if !lo.MeetsBudget("fast", budget) {
		t.Error("expected fast to meet budget")
	}
	if lo.MeetsBudget("slow", budget) {
		t.Error("expected slow not to meet budget")
	}
	if lo.MeetsBudget("unknown", budget) {
		t.Error("expected unknown not to meet budget")
	}
}

func TestLatencyOptimizerGetLocationsWithinBudget(t *testing.T) {
	em := NewEdgeManager(nil)
	_ = em.RegisterLocation(&EdgeLocation{ID: "fast", Enabled: true})
	_ = em.RegisterLocation(&EdgeLocation{ID: "slow", Enabled: true})

	config := &LatencyConfig{
		SampleWindow:       time.Hour,
		MinSamplesForStats: 1,
	}
	lo := NewLatencyOptimizer(em, config)

	lo.RecordMeasurement(LatencyMeasurement{LocationID: "fast", Latency: 10 * time.Millisecond, Success: true})
	lo.RecordMeasurement(LatencyMeasurement{LocationID: "slow", Latency: 200 * time.Millisecond, Success: true})

	budget := LatencyBudget{MaxP50: 50 * time.Millisecond}
	locs := lo.GetLocationsWithinBudget(budget)

	if len(locs) != 1 {
		t.Errorf("expected 1 location, got %d", len(locs))
	}
	if locs[0].ID != "fast" {
		t.Error("expected fast location")
	}
}

func TestDefaultLatencyConfig(t *testing.T) {
	config := DefaultLatencyConfig()

	if config.MaxSamples != 100 {
		t.Errorf("expected 100 samples, got %d", config.MaxSamples)
	}
	if config.ProbeInterval != 30*time.Second {
		t.Error("unexpected probe interval")
	}
	if config.EnableAutoProbe {
		t.Error("auto probe should be disabled by default")
	}
}

func TestDefaultScoreWeights(t *testing.T) {
	weights := DefaultScoreWeights()

	total := weights.Latency + weights.Distance + weights.Capacity
	if total != 1.0 {
		t.Errorf("expected weights to sum to 1.0, got %v", total)
	}
}

func TestPercentile(t *testing.T) {
	sorted := []time.Duration{10, 20, 30, 40, 50, 60, 70, 80, 90, 100}

	p50 := percentile(sorted, 50)
	if p50 != 60 { // index 5
		t.Errorf("expected P50=60, got %v", p50)
	}

	p90 := percentile(sorted, 90)
	if p90 != 100 { // index 9
		t.Errorf("expected P90=100, got %v", p90)
	}

	// Empty slice
	p := percentile([]time.Duration{}, 50)
	if p != 0 {
		t.Error("expected 0 for empty slice")
	}
}

func TestLatencyOptimizerTrimOldMeasurements(t *testing.T) {
	em := NewEdgeManager(nil)
	config := &LatencyConfig{
		MaxSamples:   5,
		SampleWindow: time.Hour,
	}
	lo := NewLatencyOptimizer(em, config)

	// Record more than max samples
	for i := 0; i < 10; i++ {
		lo.RecordMeasurement(LatencyMeasurement{
			LocationID: "loc-1",
			Latency:    time.Duration(i) * time.Millisecond,
			Success:    true,
		})
	}

	measurements := lo.GetMeasurements("loc-1")
	if len(measurements) != 5 {
		t.Errorf("expected 5 measurements (max), got %d", len(measurements))
	}
}

func TestLatencyOptimizerCapacityScore(t *testing.T) {
	em := NewEdgeManager(nil)
	_ = em.RegisterLocation(&EdgeLocation{
		ID:        "loaded",
		Latitude:  40,
		Longitude: -74,
		Capacity:  100,
		Enabled:   true,
	})
	_ = em.RegisterLocation(&EdgeLocation{
		ID:        "free",
		Latitude:  40,
		Longitude: -74,
		Capacity:  100,
		Enabled:   true,
	})

	// Set status with different load percentages (CPU + Memory)
	em.UpdateStatus(&EdgeStatus{LocationID: "loaded", CPUPercent: 80, MemoryPercent: 80})
	em.UpdateStatus(&EdgeStatus{LocationID: "free", CPUPercent: 10, MemoryPercent: 10})

	lo := NewLatencyOptimizer(em, nil)

	// High capacity weight
	weights := ScoreWeights{
		Latency:  0.1,
		Distance: 0.1,
		Capacity: 0.8,
	}

	scores := lo.RankLocations(40, -74, weights)

	// Free location should rank higher due to capacity
	if scores[0].LocationID != "free" {
		t.Errorf("expected free first, got %s", scores[0].LocationID)
	}
}

func TestLatencyOptimizerNoLocations(t *testing.T) {
	em := NewEdgeManager(nil)
	lo := NewLatencyOptimizer(em, nil)

	loc := lo.GetOptimalLocation(40, -74)
	if loc != nil {
		t.Error("expected nil for no locations")
	}

	locs := lo.GetOptimalLocations(40, -74, 3)
	if len(locs) != 0 {
		t.Error("expected empty slice")
	}

	scores := lo.RankLocations(40, -74, DefaultScoreWeights())
	if len(scores) != 0 {
		t.Error("expected empty scores")
	}
}

func TestLatencyOptimizerAllFailures(t *testing.T) {
	em := NewEdgeManager(nil)
	config := &LatencyConfig{
		SampleWindow:       time.Hour,
		MinSamplesForStats: 2,
	}
	lo := NewLatencyOptimizer(em, config)

	// All failures
	for i := 0; i < 5; i++ {
		lo.RecordMeasurement(LatencyMeasurement{
			LocationID: "failing",
			Success:    false,
			Error:      "timeout",
		})
	}

	stats, ok := lo.GetStats("failing")
	if !ok {
		t.Fatal("expected stats")
	}
	if stats.SuccessRate != 0 {
		t.Errorf("expected 0%% success rate, got %v", stats.SuccessRate)
	}
}
