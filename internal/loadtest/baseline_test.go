package loadtest

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestNewBaselineManager(t *testing.T) {
	bm := NewBaselineManager("/tmp/baselines")

	if bm.storePath != "/tmp/baselines" {
		t.Errorf("expected storePath '/tmp/baselines', got %q", bm.storePath)
	}
	if len(bm.thresholds) == 0 {
		t.Error("expected default thresholds to be set")
	}
}

func TestBaselineManager_SetThreshold(t *testing.T) {
	bm := NewBaselineManager("")

	bm.SetThreshold("custom_metric", 5.0)

	bm.mu.RLock()
	val, ok := bm.thresholds["custom_metric"]
	bm.mu.RUnlock()

	if !ok || val != 5.0 {
		t.Errorf("expected threshold 5.0, got %v", val)
	}
}

func TestBaselineManager_CreateBaseline(t *testing.T) {
	bm := NewBaselineManager("")

	summary := &Summary{
		TestName:       "test",
		StartTime:      time.Now().Add(-time.Minute),
		EndTime:        time.Now(),
		TotalRequests:  1000,
		SuccessCount:   990,
		FailureCount:   10,
		RequestsPerSec: 16.67,
		ErrorRate:      0.01,
		AvgLatency:     50 * time.Millisecond,
		P50Latency:     45 * time.Millisecond,
		P95Latency:     100 * time.Millisecond,
		P99Latency:     150 * time.Millisecond,
		MaxLatency:     200 * time.Millisecond,
	}

	baseline := bm.CreateBaseline("v1.0", "Initial baseline", "prod", "1.0.0", summary)

	if baseline.Name != "v1.0" {
		t.Errorf("expected name 'v1.0', got %q", baseline.Name)
	}
	if baseline.Metrics.RequestsPerSec != 16.67 {
		t.Errorf("expected RPS 16.67, got %f", baseline.Metrics.RequestsPerSec)
	}
	if baseline.Metrics.ErrorRate != 0.01 {
		t.Errorf("expected error rate 0.01, got %f", baseline.Metrics.ErrorRate)
	}
}

func TestBaselineManager_GetBaseline(t *testing.T) {
	bm := NewBaselineManager("")

	summary := &Summary{
		StartTime:      time.Now().Add(-time.Minute),
		EndTime:        time.Now(),
		RequestsPerSec: 100,
	}

	bm.CreateBaseline("test", "", "", "", summary)

	// Get existing
	baseline, ok := bm.GetBaseline("test")
	if !ok || baseline == nil {
		t.Error("expected to find baseline 'test'")
	}

	// Get non-existing
	_, ok = bm.GetBaseline("nonexistent")
	if ok {
		t.Error("should not find 'nonexistent' baseline")
	}
}

func TestBaselineManager_ListBaselines(t *testing.T) {
	bm := NewBaselineManager("")

	summary := &Summary{
		StartTime: time.Now().Add(-time.Minute),
		EndTime:   time.Now(),
	}

	bm.CreateBaseline("baseline1", "", "", "", summary)
	bm.CreateBaseline("baseline2", "", "", "", summary)
	bm.CreateBaseline("baseline3", "", "", "", summary)

	names := bm.ListBaselines()

	if len(names) != 3 {
		t.Errorf("expected 3 baselines, got %d", len(names))
	}
}

func TestBaselineManager_Compare_Pass(t *testing.T) {
	bm := NewBaselineManager("")

	// Create baseline
	baselineSummary := &Summary{
		StartTime:      time.Now().Add(-time.Minute),
		EndTime:        time.Now(),
		RequestsPerSec: 100,
		ErrorRate:      0.01,
		AvgLatency:     50 * time.Millisecond,
		P95Latency:     100 * time.Millisecond,
		P99Latency:     150 * time.Millisecond,
	}
	bm.CreateBaseline("baseline", "", "", "", baselineSummary)

	// Compare with similar current
	currentSummary := &Summary{
		StartTime:      time.Now().Add(-time.Minute),
		EndTime:        time.Now(),
		RequestsPerSec: 98, // Within 10% threshold
		ErrorRate:      0.012,
		AvgLatency:     52 * time.Millisecond,
		P95Latency:     105 * time.Millisecond,
		P99Latency:     155 * time.Millisecond,
	}

	comparison, err := bm.Compare("baseline", currentSummary)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if comparison.OverallStatus != StatusPass {
		t.Errorf("expected pass, got %s", comparison.OverallStatus)
	}
}

func TestBaselineManager_Compare_Regression(t *testing.T) {
	bm := NewBaselineManager("")

	// Create baseline
	baselineSummary := &Summary{
		StartTime:      time.Now().Add(-time.Minute),
		EndTime:        time.Now(),
		RequestsPerSec: 100,
		ErrorRate:      0.01,
		AvgLatency:     50 * time.Millisecond,
		P95Latency:     100 * time.Millisecond,
		P99Latency:     150 * time.Millisecond,
	}
	bm.CreateBaseline("baseline", "", "", "", baselineSummary)

	// Compare with degraded current
	currentSummary := &Summary{
		StartTime:      time.Now().Add(-time.Minute),
		EndTime:        time.Now(),
		RequestsPerSec: 70,                    // 30% decrease - regression
		ErrorRate:      0.05,                  // 5x increase - regression
		AvgLatency:     80 * time.Millisecond, // 60% increase - regression
		P95Latency:     200 * time.Millisecond,
		P99Latency:     300 * time.Millisecond,
	}

	comparison, err := bm.Compare("baseline", currentSummary)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if comparison.OverallStatus != StatusRegression {
		t.Errorf("expected regression, got %s", comparison.OverallStatus)
	}
	if len(comparison.Regressions) == 0 {
		t.Error("expected regressions to be reported")
	}

	t.Logf("Regressions detected: %v", comparison.Regressions)
}

func TestBaselineManager_Compare_Improved(t *testing.T) {
	bm := NewBaselineManager("")

	// Create baseline
	baselineSummary := &Summary{
		StartTime:      time.Now().Add(-time.Minute),
		EndTime:        time.Now(),
		RequestsPerSec: 100,
		ErrorRate:      0.05,
		AvgLatency:     100 * time.Millisecond,
		P95Latency:     200 * time.Millisecond,
		P99Latency:     300 * time.Millisecond,
	}
	bm.CreateBaseline("baseline", "", "", "", baselineSummary)

	// Compare with improved current
	currentSummary := &Summary{
		StartTime:      time.Now().Add(-time.Minute),
		EndTime:        time.Now(),
		RequestsPerSec: 150,                   // 50% increase - improvement
		ErrorRate:      0.01,                  // 80% decrease - improvement
		AvgLatency:     50 * time.Millisecond, // 50% decrease - improvement
		P95Latency:     100 * time.Millisecond,
		P99Latency:     150 * time.Millisecond,
	}

	comparison, err := bm.Compare("baseline", currentSummary)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(comparison.Improvements) == 0 {
		t.Error("expected improvements to be detected")
	}

	t.Logf("Improvements detected: %v", comparison.Improvements)
}

func TestBaselineManager_Compare_NotFound(t *testing.T) {
	bm := NewBaselineManager("")

	summary := &Summary{
		StartTime: time.Now().Add(-time.Minute),
		EndTime:   time.Now(),
	}

	_, err := bm.Compare("nonexistent", summary)

	if err == nil {
		t.Error("expected error for nonexistent baseline")
	}
}

func TestBaselineManager_SaveAndLoad(t *testing.T) {
	// Create temp directory
	tmpDir, err := os.MkdirTemp("", "baseline-test")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer func() {
		_ = os.RemoveAll(tmpDir)
	}()

	bm := NewBaselineManager(tmpDir) // ADD THIS LINE

	// Create and save baseline
	summary := &Summary{
		StartTime:      time.Now().Add(-time.Minute),
		EndTime:        time.Now(),
		RequestsPerSec: 123.45,
		ErrorRate:      0.02,
		AvgLatency:     55 * time.Millisecond,
		P50Latency:     50 * time.Millisecond,
		P95Latency:     110 * time.Millisecond,
		P99Latency:     160 * time.Millisecond,
		MaxLatency:     210 * time.Millisecond,
	}
	bm.CreateBaseline("test-save", "Test save/load", "test", "1.0", summary)

	err = bm.SaveToFile("test-save")
	if err != nil {
		t.Fatalf("failed to save: %v", err)
	}

	// Verify file exists
	filename := filepath.Join(tmpDir, "test-save.json")
	if _, err := os.Stat(filename); os.IsNotExist(err) {
		t.Error("expected file to exist")
	}

	// Create new manager and load
	bm2 := NewBaselineManager(tmpDir)
	err = bm2.LoadFromFile("test-save")
	if err != nil {
		t.Fatalf("failed to load: %v", err)
	}

	loaded, ok := bm2.GetBaseline("test-save")
	if !ok {
		t.Fatal("baseline not found after load")
	}
	if loaded.Metrics.RequestsPerSec != 123.45 {
		t.Errorf("expected RPS 123.45, got %f", loaded.Metrics.RequestsPerSec)
	}
}

func TestBaselineManager_DeleteBaseline(t *testing.T) {
	bm := NewBaselineManager("")

	summary := &Summary{
		StartTime: time.Now().Add(-time.Minute),
		EndTime:   time.Now(),
	}
	bm.CreateBaseline("to-delete", "", "", "", summary)

	// Delete existing
	deleted := bm.DeleteBaseline("to-delete")
	if !deleted {
		t.Error("expected delete to return true")
	}

	// Verify gone
	_, ok := bm.GetBaseline("to-delete")
	if ok {
		t.Error("baseline should be deleted")
	}

	// Delete non-existing
	deleted = bm.DeleteBaseline("nonexistent")
	if deleted {
		t.Error("expected delete to return false for nonexistent")
	}
}

func TestComparison_GenerateReport(t *testing.T) {
	bm := NewBaselineManager("")

	baselineSummary := &Summary{
		StartTime:      time.Now().Add(-time.Minute),
		EndTime:        time.Now(),
		RequestsPerSec: 100,
		ErrorRate:      0.01,
		AvgLatency:     50 * time.Millisecond,
		P95Latency:     100 * time.Millisecond,
		P99Latency:     150 * time.Millisecond,
	}
	bm.CreateBaseline("report-test", "Test report", "staging", "2.0", baselineSummary)

	currentSummary := &Summary{
		StartTime:      time.Now().Add(-time.Minute),
		EndTime:        time.Now(),
		RequestsPerSec: 110,
		ErrorRate:      0.008,
		AvgLatency:     45 * time.Millisecond,
		P95Latency:     90 * time.Millisecond,
		P99Latency:     140 * time.Millisecond,
	}

	comparison, _ := bm.Compare("report-test", currentSummary)

	report := comparison.GenerateReport()

	if report == "" {
		t.Error("expected non-empty report")
	}
	if len(report) < 100 {
		t.Error("report seems too short")
	}

	t.Logf("Generated report:\n%s", report)
}

func TestDifference(t *testing.T) {
	diff := Difference{
		Metric:      "requests_per_sec",
		BaselineVal: 100,
		CurrentVal:  90,
		DeltaAbs:    -10,
		DeltaPct:    -10.0,
		Status:      StatusPass,
		Threshold:   10.0,
	}

	if diff.Metric != "requests_per_sec" {
		t.Error("Metric not set correctly")
	}
	if diff.DeltaPct != -10.0 {
		t.Error("DeltaPct not set correctly")
	}
}

func TestBaselineMetrics(t *testing.T) {
	metrics := BaselineMetrics{
		RequestsPerSec: 100,
		BytesPerSec:    1000000,
		AvgLatencyMs:   50,
		P50LatencyMs:   45,
		P95LatencyMs:   100,
		P99LatencyMs:   150,
		MaxLatencyMs:   200,
		ErrorRate:      0.01,
		SuccessRate:    0.99,
		PeakMemoryMB:   512,
		AvgGoroutines:  50,
		Duration:       time.Minute,
		TargetRPS:      100,
		Concurrency:    20,
	}

	if metrics.RequestsPerSec != 100 {
		t.Error("RequestsPerSec not set correctly")
	}
	if metrics.SuccessRate != 0.99 {
		t.Error("SuccessRate not set correctly")
	}
}
