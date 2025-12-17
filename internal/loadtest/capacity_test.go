package loadtest

import (
	"testing"
	"time"
)

func TestBuildModelFromStress(t *testing.T) {
	result := &StressResult{
		Summary: &Summary{
			TestName:       "stress-test",
			RequestsPerSec: 100,
			AvgLatency:     50 * time.Millisecond,
		},
		MaxRPSAchieved:   150,
		BreakingPointRPS: 120,
	}

	model := BuildModelFromStress("test-model", result)

	if model.Name != "test-model" {
		t.Errorf("expected name 'test-model', got %q", model.Name)
	}
	if model.BreakingPointRPS != 120 {
		t.Errorf("expected breaking point 120, got %.0f", model.BreakingPointRPS)
	}
	if model.BaselineRPS != 84 { // 70% of 120
		t.Errorf("expected baseline 84, got %.0f", model.BaselineRPS)
	}

	t.Logf("Model: baseline=%.0f, breaking=%.0f, max=%.0f, scaling=%.2f",
		model.BaselineRPS, model.BreakingPointRPS, model.MaxRPS, model.ScalingFactor)
}

func TestBuildModelFromSoak(t *testing.T) {
	result := &SoakResult{
		Summary: &Summary{
			TestName:       "soak-test",
			RequestsPerSec: 50,
			TotalRequests:  10000,
			AvgLatency:     30 * time.Millisecond,
		},
		Stable:     true,
		PeakMemory: 500 * 1024 * 1024, // 500MB
	}

	model := BuildModelFromSoak("soak-model", result, 50)

	if model.BaselineRPS != 50 {
		t.Errorf("expected baseline 50, got %.0f", model.BaselineRPS)
	}
	if model.BreakingPointRPS != 75 { // 1.5x baseline for stable system
		t.Errorf("expected breaking point 75, got %.0f", model.BreakingPointRPS)
	}

	t.Logf("Soak model: baseline=%.0f, breaking=%.0f, memory_per_req=%.0f bytes",
		model.BaselineRPS, model.BreakingPointRPS, model.MemoryPerRequest)
}

func TestNewCapacityPlanner(t *testing.T) {
	model := &CapacityModel{
		Name:        "test",
		BaselineRPS: 100,
	}

	planner := NewCapacityPlanner(model)

	if planner.model != model {
		t.Error("expected model to be set")
	}
}

func TestCapacityPlanner_EstimateCapacity(t *testing.T) {
	model := &CapacityModel{
		Name:              "test",
		BaselineRPS:       100,
		BaselineLatencyMs: 50,
		BreakingPointRPS:  150,
		MaxRPS:            200,
		ScalingFactor:     0.8,
	}

	planner := NewCapacityPlanner(model)

	// Estimate for baseline RPS
	estimate := planner.EstimateCapacity(100)

	if !estimate.Feasible {
		t.Error("expected feasible at baseline RPS")
	}
	if estimate.RequiredInstances < 1 {
		t.Error("expected at least 1 instance")
	}
	if estimate.Confidence < 0.8 {
		t.Errorf("expected high confidence at baseline, got %.2f", estimate.Confidence)
	}

	t.Logf("Estimate for 100 RPS: instances=%d, latency=%v, confidence=%.2f",
		estimate.RequiredInstances, estimate.EstimatedLatency, estimate.Confidence)
}

func TestCapacityPlanner_EstimateCapacity_HighLoad(t *testing.T) {
	model := &CapacityModel{
		Name:              "test",
		BaselineRPS:       100,
		BaselineLatencyMs: 50,
		BreakingPointRPS:  150,
		MaxRPS:            200,
		ScalingFactor:     0.8,
	}

	planner := NewCapacityPlanner(model)

	// Estimate for high RPS
	estimate := planner.EstimateCapacity(300)

	if estimate.RequiredInstances < 2 {
		t.Errorf("expected multiple instances for 300 RPS, got %d", estimate.RequiredInstances)
	}
	if estimate.Confidence > 0.7 {
		t.Error("expected lower confidence for extrapolated estimate")
	}

	t.Logf("Estimate for 300 RPS: instances=%d, latency=%v, confidence=%.2f, feasible=%v",
		estimate.RequiredInstances, estimate.EstimatedLatency, estimate.Confidence, estimate.Feasible)
}

func TestCapacityPlanner_EstimateCapacity_Infeasible(t *testing.T) {
	model := &CapacityModel{
		Name:              "test",
		BaselineRPS:       100,
		BaselineLatencyMs: 50,
		BreakingPointRPS:  150,
		MaxRPS:            200,
		ScalingFactor:     0.8,
	}

	planner := NewCapacityPlanner(model)

	// Request way beyond capacity
	estimate := planner.EstimateCapacity(500)

	if estimate.Feasible {
		t.Error("expected infeasible for 500 RPS with max 200")
	}
	if len(estimate.Warnings) == 0 {
		t.Error("expected warnings for infeasible estimate")
	}

	t.Logf("Infeasible estimate: warnings=%v", estimate.Warnings)
}

func TestCapacityPlanner_EstimateCapacity_NilModel(t *testing.T) {
	planner := NewCapacityPlanner(nil)
	estimate := planner.EstimateCapacity(100)

	if estimate.Feasible {
		t.Error("expected infeasible with nil model")
	}
	if len(estimate.Warnings) == 0 {
		t.Error("expected warnings with nil model")
	}
}

func TestCapacityPlanner_PlanForGrowth(t *testing.T) {
	model := &CapacityModel{
		Name:              "test",
		BaselineRPS:       100,
		BaselineLatencyMs: 50,
		BreakingPointRPS:  200,
		MaxRPS:            300,
		ScalingFactor:     0.8,
	}

	planner := NewCapacityPlanner(model)

	// Plan for 6 months with 20% monthly growth
	plans := planner.PlanForGrowth(50, 20, 6)

	if len(plans) != 6 {
		t.Errorf("expected 6 monthly plans, got %d", len(plans))
	}

	// RPS should be increasing
	prevRPS := float64(50)
	for i, plan := range plans {
		if plan.TargetRPS <= prevRPS {
			t.Errorf("month %d: RPS should increase", i+1)
		}
		prevRPS = plan.TargetRPS
		t.Logf("Month %d: %.0f RPS, %d instances, %.2f confidence",
			i+1, plan.TargetRPS, plan.RequiredInstances, plan.Confidence)
	}
}

func TestCapacityPlanner_CalculateHeadroom(t *testing.T) {
	model := &CapacityModel{
		Name:             "test",
		BaselineRPS:      100,
		BreakingPointRPS: 200,
		MaxRPS:           250,
	}

	planner := NewCapacityPlanner(model)

	// Test with low utilization
	analysis := planner.CalculateHeadroom(50)

	if analysis.Utilization >= 50 {
		t.Errorf("expected <50%% utilization at 50 RPS, got %.0f%%", analysis.Utilization)
	}
	if analysis.RiskLevel != "low" {
		t.Errorf("expected low risk, got %s", analysis.RiskLevel)
	}

	t.Logf("Low load: utilization=%.0f%%, headroom=%.0f RPS, risk=%s",
		analysis.Utilization, analysis.AbsoluteHeadroom, analysis.RiskLevel)
}

func TestCapacityPlanner_CalculateHeadroom_HighLoad(t *testing.T) {
	model := &CapacityModel{
		Name:             "test",
		BaselineRPS:      100,
		BreakingPointRPS: 200,
		MaxRPS:           250,
	}

	planner := NewCapacityPlanner(model)

	// Test with high utilization
	analysis := planner.CalculateHeadroom(180)

	if analysis.Utilization < 80 {
		t.Errorf("expected >80%% utilization at 180 RPS, got %.0f%%", analysis.Utilization)
	}
	if analysis.RiskLevel == "low" {
		t.Error("expected higher risk at high utilization")
	}

	t.Logf("High load: utilization=%.0f%%, headroom=%.0f RPS, risk=%s, recommendation=%s",
		analysis.Utilization, analysis.AbsoluteHeadroom, analysis.RiskLevel, analysis.Recommendation)
}

func TestCapacityPlanner_GenerateCapacityReport(t *testing.T) {
	model := &CapacityModel{
		Name:              "production-api",
		CreatedAt:         time.Now(),
		BaselineRPS:       100,
		BaselineLatencyMs: 50,
		BreakingPointRPS:  150,
		MaxRPS:            200,
		ScalingFactor:     0.8,
	}

	planner := NewCapacityPlanner(model)
	report := planner.GenerateCapacityReport()

	if report == "" {
		t.Error("expected non-empty report")
	}
	if len(report) < 200 {
		t.Error("report seems too short")
	}

	t.Logf("Capacity report:\n%s", report)
}

func TestCapacityPlanner_GenerateCapacityReport_NilModel(t *testing.T) {
	planner := NewCapacityPlanner(nil)
	report := planner.GenerateCapacityReport()

	if report != "No capacity model available" {
		t.Errorf("expected 'No capacity model available', got %q", report)
	}
}

func TestCapacityModel(t *testing.T) {
	model := CapacityModel{
		Name:              "test",
		CreatedAt:         time.Now(),
		BaselineRPS:       100,
		BaselineLatencyMs: 50,
		BreakingPointRPS:  150,
		MaxRPS:            200,
		CPUPerRequest:     0.01,
		MemoryPerRequest:  1024,
		IOPerRequest:      2,
		ScalingFactor:     0.85,
		ConcurrencyLimit:  1000,
	}

	if model.Name != "test" {
		t.Error("Name not set correctly")
	}
	if model.ScalingFactor != 0.85 {
		t.Error("ScalingFactor not set correctly")
	}
}

func TestCapacityEstimate(t *testing.T) {
	estimate := CapacityEstimate{
		TargetRPS:         200,
		EstimatedLatency:  100 * time.Millisecond,
		RequiredInstances: 2,
		RequiredCPU:       4.0,
		RequiredMemoryGB:  8.0,
		Confidence:        0.85,
		Feasible:          true,
		Warnings:          []string{"Warning 1"},
		Recommendations:   []string{"Recommendation 1"},
	}

	if estimate.TargetRPS != 200 {
		t.Error("TargetRPS not set correctly")
	}
	if !estimate.Feasible {
		t.Error("Feasible not set correctly")
	}
}

func TestHeadroomAnalysis(t *testing.T) {
	analysis := HeadroomAnalysis{
		CurrentRPS:             100,
		RecommendedHeadroom:    40,
		RecommendedHeadroomPct: 40,
		AbsoluteHeadroom:       100,
		AbsoluteHeadroomPct:    100,
		Utilization:            50,
		RiskLevel:              "medium",
		Recommendation:         "Monitor closely",
	}

	if analysis.CurrentRPS != 100 {
		t.Error("CurrentRPS not set correctly")
	}
	if analysis.RiskLevel != "medium" {
		t.Error("RiskLevel not set correctly")
	}
}
