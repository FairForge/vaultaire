package loadtest

import (
	"testing"
	"time"
)

func TestDefaultStorageSLA(t *testing.T) {
	sla := DefaultStorageSLA()

	if sla.Name == "" {
		t.Error("expected SLA name to be set")
	}
	if len(sla.Objectives) == 0 {
		t.Error("expected SLA to have objectives")
	}

	// Check for critical objectives
	hasCritical := false
	for _, obj := range sla.Objectives {
		if obj.Priority == PriorityCritical {
			hasCritical = true
			break
		}
	}
	if !hasCritical {
		t.Error("expected at least one critical objective")
	}

	t.Logf("Default SLA has %d objectives", len(sla.Objectives))
}

func TestNewSLAValidator(t *testing.T) {
	sla := DefaultStorageSLA()
	validator := NewSLAValidator(sla)

	if validator.sla != sla {
		t.Error("expected SLA to be set")
	}
}

func TestSLAValidator_Validate_AllPass(t *testing.T) {
	sla := DefaultStorageSLA()
	validator := NewSLAValidator(sla)

	// Create a summary that passes all SLOs
	summary := &Summary{
		TestName:       "passing-test",
		StartTime:      time.Now().Add(-time.Hour),
		EndTime:        time.Now(),
		TotalRequests:  10000,
		SuccessCount:   9999,
		FailureCount:   1,
		RequestsPerSec: 150,
		ErrorRate:      0.0001, // 0.01%
		AvgLatency:     30 * time.Millisecond,
		P50Latency:     25 * time.Millisecond,
		P95Latency:     100 * time.Millisecond,
		P99Latency:     200 * time.Millisecond,
		MaxLatency:     500 * time.Millisecond,
	}

	result := validator.Validate(summary)

	if !result.OverallPass {
		t.Error("expected all SLOs to pass")
		for _, r := range result.GetAllFailed() {
			t.Logf("Failed: %s", r.Message)
		}
	}
	if !result.CriticalPass {
		t.Error("expected critical SLOs to pass")
	}
	if result.Score < 100 {
		t.Errorf("expected 100%% score, got %.1f%%", result.Score)
	}

	t.Logf("All pass: score=%.1f%%, passed=%d/%d",
		result.Score, result.countPassed(), len(result.ObjectiveResults))
}

func TestSLAValidator_Validate_SomeFail(t *testing.T) {
	sla := DefaultStorageSLA()
	validator := NewSLAValidator(sla)

	// Create a summary that fails some SLOs
	summary := &Summary{
		TestName:       "partial-test",
		StartTime:      time.Now().Add(-time.Hour),
		EndTime:        time.Now(),
		TotalRequests:  10000,
		SuccessCount:   9900,
		FailureCount:   100,
		RequestsPerSec: 150,
		ErrorRate:      0.01, // 1% - exceeds 0.1% target
		AvgLatency:     100 * time.Millisecond,
		P50Latency:     80 * time.Millisecond,  // Exceeds 50ms target
		P95Latency:     300 * time.Millisecond, // Exceeds 200ms target
		P99Latency:     400 * time.Millisecond, // Passes 500ms target
		MaxLatency:     800 * time.Millisecond,
	}

	result := validator.Validate(summary)

	if result.OverallPass {
		t.Error("expected some SLOs to fail")
	}
	if result.Score >= 100 {
		t.Error("expected score less than 100%")
	}

	failed := result.GetAllFailed()
	if len(failed) == 0 {
		t.Error("expected some failed objectives")
	}

	t.Logf("Partial pass: score=%.1f%%, failed=%d", result.Score, len(failed))
	for _, f := range failed {
		t.Logf("  Failed: %s", f.Message)
	}
}

func TestSLAValidator_Validate_CriticalFail(t *testing.T) {
	sla := DefaultStorageSLA()
	validator := NewSLAValidator(sla)

	// Create a summary that fails critical SLOs
	summary := &Summary{
		TestName:       "critical-fail-test",
		StartTime:      time.Now().Add(-time.Hour),
		EndTime:        time.Now(),
		TotalRequests:  10000,
		SuccessCount:   9000,
		FailureCount:   1000,
		RequestsPerSec: 150,
		ErrorRate:      0.10, // 10% - way exceeds 0.1% target
		AvgLatency:     200 * time.Millisecond,
		P50Latency:     150 * time.Millisecond,
		P95Latency:     400 * time.Millisecond,
		P99Latency:     800 * time.Millisecond, // Exceeds 500ms target
		MaxLatency:     2 * time.Second,
	}

	result := validator.Validate(summary)

	if result.CriticalPass {
		t.Error("expected critical SLOs to fail")
	}

	criticalFailed := result.GetFailedCritical()
	if len(criticalFailed) == 0 {
		t.Error("expected failed critical objectives")
	}

	t.Logf("Critical failures: %d", len(criticalFailed))
	for _, f := range criticalFailed {
		t.Logf("  Critical fail: %s", f.Message)
	}
}

func TestSLAResult_GenerateReport(t *testing.T) {
	sla := DefaultStorageSLA()
	validator := NewSLAValidator(sla)

	summary := &Summary{
		TestName:       "report-test",
		StartTime:      time.Now().Add(-time.Hour),
		EndTime:        time.Now(),
		TotalRequests:  10000,
		SuccessCount:   9950,
		FailureCount:   50,
		RequestsPerSec: 120,
		ErrorRate:      0.005,
		AvgLatency:     40 * time.Millisecond,
		P50Latency:     35 * time.Millisecond,
		P95Latency:     150 * time.Millisecond,
		P99Latency:     350 * time.Millisecond,
		MaxLatency:     600 * time.Millisecond,
	}

	result := validator.Validate(summary)
	report := result.GenerateReport()

	if report == "" {
		t.Error("expected non-empty report")
	}
	if len(report) < 200 {
		t.Error("report seems too short")
	}

	t.Logf("Generated report:\n%s", report)
}

func TestCreateCustomSLA(t *testing.T) {
	sla := CreateCustomSLA(
		"custom-api",
		"Custom API SLA",
		NewLatencySLO("Fast P99", MetricLatencyP99, 100, PriorityCritical),
		NewErrorRateSLO("Low Errors", 0.01, PriorityCritical),
		NewThroughputSLO("High Throughput", 500, PriorityHigh),
	)

	if sla.Name != "custom-api" {
		t.Errorf("expected name 'custom-api', got %q", sla.Name)
	}
	if len(sla.Objectives) != 3 {
		t.Errorf("expected 3 objectives, got %d", len(sla.Objectives))
	}

	// Validate against a summary
	validator := NewSLAValidator(sla)
	summary := &Summary{
		StartTime:      time.Now().Add(-time.Hour),
		EndTime:        time.Now(),
		TotalRequests:  10000,
		RequestsPerSec: 600,
		ErrorRate:      0.0001,
		P99Latency:     80 * time.Millisecond,
	}

	result := validator.Validate(summary)
	if !result.OverallPass {
		t.Error("expected custom SLA to pass")
	}

	t.Logf("Custom SLA: %d objectives, score=%.1f%%", len(sla.Objectives), result.Score)
}

func TestNewLatencySLO(t *testing.T) {
	slo := NewLatencySLO("Test P99", MetricLatencyP99, 500, PriorityCritical)

	if slo.Name != "Test P99" {
		t.Error("Name not set correctly")
	}
	if slo.Metric != MetricLatencyP99 {
		t.Error("Metric not set correctly")
	}
	if slo.Target != 500 {
		t.Error("Target not set correctly")
	}
	if slo.Comparator != ComparatorLessOrEqual {
		t.Error("Comparator should be LessOrEqual for latency")
	}
	if slo.Priority != PriorityCritical {
		t.Error("Priority not set correctly")
	}
}

func TestNewErrorRateSLO(t *testing.T) {
	slo := NewErrorRateSLO("Low Errors", 0.1, PriorityHigh)

	if slo.Metric != MetricErrorRate {
		t.Error("Metric should be ErrorRate")
	}
	if slo.Target != 0.1 {
		t.Error("Target not set correctly")
	}
}

func TestNewThroughputSLO(t *testing.T) {
	slo := NewThroughputSLO("High RPS", 100, PriorityMedium)

	if slo.Metric != MetricThroughput {
		t.Error("Metric should be Throughput")
	}
	if slo.Comparator != ComparatorGreaterOrEqual {
		t.Error("Comparator should be GreaterOrEqual for throughput")
	}
}

func TestSLO(t *testing.T) {
	slo := SLO{
		Name:       "Test SLO",
		Metric:     MetricLatencyP95,
		Target:     200,
		Comparator: ComparatorLessOrEqual,
		Window:     time.Hour,
		Priority:   PriorityHigh,
	}

	if slo.Name != "Test SLO" {
		t.Error("Name not set correctly")
	}
	if slo.Window != time.Hour {
		t.Error("Window not set correctly")
	}
}

func TestSLOResult(t *testing.T) {
	result := SLOResult{
		SLO: SLO{
			Name:     "Test",
			Priority: PriorityCritical,
		},
		ActualValue: 150,
		TargetMet:   true,
		Margin:      50,
		Message:     "Test: 150 <= 200 ✓",
	}

	if !result.TargetMet {
		t.Error("TargetMet not set correctly")
	}
	if result.Margin != 50 {
		t.Error("Margin not set correctly")
	}
}
