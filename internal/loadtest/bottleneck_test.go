package loadtest

import (
	"testing"
	"time"
)

func TestDefaultAnalysisConfig(t *testing.T) {
	config := DefaultAnalysisConfig()

	if config.P99LatencyThreshold <= 0 {
		t.Error("expected positive P99LatencyThreshold")
	}
	if config.ErrorRateThreshold <= 0 {
		t.Error("expected positive ErrorRateThreshold")
	}
}

func TestNewBottleneckAnalyzer(t *testing.T) {
	// With nil config
	analyzer := NewBottleneckAnalyzer(nil)
	if analyzer.config == nil {
		t.Error("expected default config when nil provided")
	}

	// With custom config
	config := &AnalysisConfig{
		P99LatencyThreshold: time.Second,
	}
	analyzer = NewBottleneckAnalyzer(config)
	if analyzer.config.P99LatencyThreshold != time.Second {
		t.Error("expected custom config to be used")
	}
}

func TestBottleneckAnalyzer_AnalyzeLoadTest_Healthy(t *testing.T) {
	analyzer := NewBottleneckAnalyzer(nil)

	summary := &Summary{
		TestName:       "healthy-test",
		RequestsPerSec: 100,
		ErrorRate:      0.001,
		AvgLatency:     10 * time.Millisecond,
		P50Latency:     8 * time.Millisecond,
		P95Latency:     50 * time.Millisecond,
		P99Latency:     100 * time.Millisecond,
		MaxLatency:     150 * time.Millisecond,
	}

	analysis := analyzer.AnalyzeLoadTest(summary)

	if analysis.HealthScore < 90 {
		t.Errorf("expected high health score for healthy system, got %.0f", analysis.HealthScore)
	}

	t.Logf("Healthy system: score=%.0f, bottlenecks=%d",
		analysis.HealthScore, len(analysis.Bottlenecks))
}

func TestBottleneckAnalyzer_AnalyzeLoadTest_LatencyIssues(t *testing.T) {
	analyzer := NewBottleneckAnalyzer(nil)

	summary := &Summary{
		TestName:       "latency-test",
		RequestsPerSec: 100,
		ErrorRate:      0.001,
		AvgLatency:     200 * time.Millisecond,
		P50Latency:     150 * time.Millisecond,
		P95Latency:     500 * time.Millisecond,
		P99Latency:     2 * time.Second,
		MaxLatency:     5 * time.Second,
	}

	analysis := analyzer.AnalyzeLoadTest(summary)

	// Should detect latency bottlenecks
	hasLatencyBottleneck := false
	for _, b := range analysis.Bottlenecks {
		if b.Type == BottleneckLatency {
			hasLatencyBottleneck = true
			t.Logf("Latency bottleneck: %s (severity: %s)", b.Description, b.Severity)
		}
	}

	if !hasLatencyBottleneck {
		t.Error("expected latency bottleneck to be detected")
	}
}

func TestBottleneckAnalyzer_AnalyzeLoadTest_ThroughputIssues(t *testing.T) {
	analyzer := NewBottleneckAnalyzer(nil)

	summary := &Summary{
		TestName:       "throughput-test",
		RequestsPerSec: 2, // Very low
		ErrorRate:      0.001,
		AvgLatency:     10 * time.Millisecond,
		P95Latency:     50 * time.Millisecond,
		P99Latency:     100 * time.Millisecond,
	}

	analysis := analyzer.AnalyzeLoadTest(summary)

	hasThroughputBottleneck := false
	for _, b := range analysis.Bottlenecks {
		if b.Type == BottleneckThroughput {
			hasThroughputBottleneck = true
			t.Logf("Throughput bottleneck: %s", b.Description)
		}
	}

	if !hasThroughputBottleneck {
		t.Error("expected throughput bottleneck to be detected")
	}
}

func TestBottleneckAnalyzer_AnalyzeLoadTest_ErrorIssues(t *testing.T) {
	analyzer := NewBottleneckAnalyzer(nil)

	summary := &Summary{
		TestName:       "error-test",
		RequestsPerSec: 100,
		TotalRequests:  1000,
		FailureCount:   200,
		ErrorRate:      0.20, // 20% error rate
		AvgLatency:     10 * time.Millisecond,
		P95Latency:     50 * time.Millisecond,
		P99Latency:     100 * time.Millisecond,
	}

	analysis := analyzer.AnalyzeLoadTest(summary)

	hasErrorBottleneck := false
	for _, b := range analysis.Bottlenecks {
		if b.Description == "Error rate exceeds acceptable threshold" {
			hasErrorBottleneck = true
			if b.Severity != SeverityCritical {
				t.Errorf("expected critical severity for 20%% error rate, got %s", b.Severity)
			}
		}
	}

	if !hasErrorBottleneck {
		t.Error("expected error bottleneck to be detected")
	}
}

func TestBottleneckAnalyzer_AnalyzeSoakTest_MemoryLeak(t *testing.T) {
	analyzer := NewBottleneckAnalyzer(nil)

	result := &SoakResult{
		Summary: &Summary{
			TestName:       "memory-test",
			RequestsPerSec: 100,
			ErrorRate:      0.001,
			AvgLatency:     10 * time.Millisecond,
			P95Latency:     50 * time.Millisecond,
			P99Latency:     100 * time.Millisecond,
		},
		MemoryGrowth:   80,                 // 80% growth
		PeakMemory:     1024 * 1024 * 1024, // 1GB
		PeakGoroutines: 100,
	}

	analysis := analyzer.AnalyzeSoakTest(result)

	hasMemoryBottleneck := false
	for _, b := range analysis.Bottlenecks {
		if b.Type == BottleneckMemory {
			hasMemoryBottleneck = true
			t.Logf("Memory bottleneck: %s", b.Description)
		}
	}

	if !hasMemoryBottleneck {
		t.Error("expected memory bottleneck to be detected")
	}
}

func TestBottleneckAnalyzer_AnalyzeSoakTest_GoroutineLeak(t *testing.T) {
	analyzer := NewBottleneckAnalyzer(nil)

	result := &SoakResult{
		Summary: &Summary{
			TestName:       "goroutine-test",
			RequestsPerSec: 100,
			ErrorRate:      0.001,
			AvgLatency:     10 * time.Millisecond,
			P95Latency:     50 * time.Millisecond,
			P99Latency:     100 * time.Millisecond,
		},
		MemoryGrowth:    10,
		PeakMemory:      100 * 1024 * 1024,
		PeakGoroutines:  5000, // High goroutine count
		GoroutineGrowth: 100,  // 100% growth - leak
	}

	analysis := analyzer.AnalyzeSoakTest(result)

	hasGoroutineBottleneck := false
	for _, b := range analysis.Bottlenecks {
		if b.Type == BottleneckGoroutine {
			hasGoroutineBottleneck = true
			t.Logf("Goroutine bottleneck: %s", b.Description)
		}
	}

	if !hasGoroutineBottleneck {
		t.Error("expected goroutine bottleneck to be detected")
	}
}

func TestBottleneckAnalyzer_AnalyzeStressTest_BreakingPoint(t *testing.T) {
	analyzer := NewBottleneckAnalyzer(nil)

	result := &StressResult{
		Summary: &Summary{
			TestName:       "stress-test",
			RequestsPerSec: 50,
			ErrorRate:      0.15,
			AvgLatency:     100 * time.Millisecond,
			P95Latency:     300 * time.Millisecond,
			P99Latency:     600 * time.Millisecond,
		},
		MaxRPSAchieved:   200,
		BreakingPointRPS: 50,
		StopReason:       "error rate exceeded threshold",
	}

	analysis := analyzer.AnalyzeStressTest(result)

	hasConcurrencyBottleneck := false
	for _, b := range analysis.Bottlenecks {
		if b.Type == BottleneckConcurrency {
			hasConcurrencyBottleneck = true
			t.Logf("Concurrency bottleneck: %s", b.Description)
		}
	}

	if !hasConcurrencyBottleneck {
		t.Error("expected concurrency bottleneck to be detected")
	}
}

func TestBottleneckAnalysis_HealthScore(t *testing.T) {
	analyzer := NewBottleneckAnalyzer(nil)

	// Critical issues should severely impact score
	summary := &Summary{
		TestName:       "critical-test",
		RequestsPerSec: 1,    // Very low
		ErrorRate:      0.25, // Very high
		AvgLatency:     10 * time.Millisecond,
		P95Latency:     50 * time.Millisecond,
		P99Latency:     3 * time.Second, // Very high
	}

	analysis := analyzer.AnalyzeLoadTest(summary)

	if analysis.HealthScore > 50 {
		t.Errorf("expected low health score for critical issues, got %.0f", analysis.HealthScore)
	}

	t.Logf("Critical issues: score=%.0f, bottlenecks=%d",
		analysis.HealthScore, len(analysis.Bottlenecks))
}

func TestBottleneckAnalysis_TopIssue(t *testing.T) {
	analyzer := NewBottleneckAnalyzer(nil)

	summary := &Summary{
		TestName:       "multi-issue-test",
		RequestsPerSec: 5,
		ErrorRate:      0.30, // Critical
		AvgLatency:     10 * time.Millisecond,
		P95Latency:     300 * time.Millisecond, // Medium
		P99Latency:     100 * time.Millisecond,
	}

	analysis := analyzer.AnalyzeLoadTest(summary)

	if analysis.TopIssue == nil {
		t.Error("expected top issue to be identified")
	} else {
		// Top issue should be the most severe
		t.Logf("Top issue: %s (%s)", analysis.TopIssue.Description, analysis.TopIssue.Severity)
	}
}

func TestBottleneckAnalysis_GenerateReport(t *testing.T) {
	analyzer := NewBottleneckAnalyzer(nil)

	summary := &Summary{
		TestName:       "report-test",
		RequestsPerSec: 50,
		ErrorRate:      0.10,
		AvgLatency:     50 * time.Millisecond,
		P95Latency:     300 * time.Millisecond,
		P99Latency:     800 * time.Millisecond,
		MaxLatency:     2 * time.Second,
	}

	analysis := analyzer.AnalyzeLoadTest(summary)
	report := analysis.GenerateReport()

	if report == "" {
		t.Error("expected non-empty report")
	}
	if len(report) < 200 {
		t.Error("report seems too short")
	}

	t.Logf("Generated report:\n%s", report)
}

func TestSeverityRank(t *testing.T) {
	if severityRank(SeverityCritical) <= severityRank(SeverityHigh) {
		t.Error("critical should rank higher than high")
	}
	if severityRank(SeverityHigh) <= severityRank(SeverityMedium) {
		t.Error("high should rank higher than medium")
	}
	if severityRank(SeverityMedium) <= severityRank(SeverityLow) {
		t.Error("medium should rank higher than low")
	}
	if severityRank(SeverityLow) <= severityRank(SeverityInfo) {
		t.Error("low should rank higher than info")
	}
}

func TestBottleneck(t *testing.T) {
	b := Bottleneck{
		Type:        BottleneckLatency,
		Severity:    SeverityHigh,
		Description: "Test bottleneck",
		Evidence:    []string{"evidence 1", "evidence 2"},
		Impact:      "Test impact",
		Suggestion:  "Test suggestion",
		DetectedAt:  time.Now(),
		Metrics: map[string]float64{
			"test_metric": 123.45,
		},
	}

	if b.Type != BottleneckLatency {
		t.Error("Type not set correctly")
	}
	if len(b.Evidence) != 2 {
		t.Error("Evidence not set correctly")
	}
	if b.Metrics["test_metric"] != 123.45 {
		t.Error("Metrics not set correctly")
	}
}
