// Package loadtest provides infrastructure for load, stress, and chaos testing.
package loadtest

import (
	"fmt"
	"math"
	"time"
)

// CapacityModel represents a system's capacity characteristics.
type CapacityModel struct {
	Name              string
	CreatedAt         time.Time
	BaselineRPS       float64 // Known sustainable RPS
	BaselineLatencyMs float64 // Latency at baseline RPS
	BreakingPointRPS  float64 // RPS where system degrades
	MaxRPS            float64 // Absolute maximum observed

	// Resource coefficients (derived from testing)
	CPUPerRequest    float64 // CPU units per request
	MemoryPerRequest float64 // Bytes per request
	IOPerRequest     float64 // IO operations per request

	// Scaling characteristics
	ScalingFactor    float64 // How well system scales (0-1)
	ConcurrencyLimit int     // Max effective concurrency
}

// CapacityEstimate provides capacity projections.
type CapacityEstimate struct {
	TargetRPS         float64
	EstimatedLatency  time.Duration
	RequiredInstances int
	RequiredCPU       float64
	RequiredMemoryGB  float64
	Confidence        float64 // 0-1 confidence in estimate
	Feasible          bool
	Warnings          []string
	Recommendations   []string
}

// ResourceRequirements specifies resource needs.
type ResourceRequirements struct {
	CPUCores    float64
	MemoryGB    float64
	StorageGB   float64
	NetworkMbps float64
	Instances   int
}

// CapacityPlanner helps estimate resource requirements.
type CapacityPlanner struct {
	model *CapacityModel
}

// NewCapacityPlanner creates a planner with the given model.
func NewCapacityPlanner(model *CapacityModel) *CapacityPlanner {
	return &CapacityPlanner{model: model}
}

// BuildModelFromStress creates a capacity model from stress test results.
func BuildModelFromStress(name string, result *StressResult) *CapacityModel {
	model := &CapacityModel{
		Name:             name,
		CreatedAt:        time.Now(),
		BreakingPointRPS: float64(result.BreakingPointRPS),
		MaxRPS:           float64(result.MaxRPSAchieved),
	}

	// Estimate baseline as 70% of breaking point
	if result.BreakingPointRPS > 0 {
		model.BaselineRPS = float64(result.BreakingPointRPS) * 0.7
	} else {
		model.BaselineRPS = result.RequestsPerSec
	}

	model.BaselineLatencyMs = float64(result.AvgLatency.Milliseconds())

	// Estimate scaling factor based on how close max is to breaking point
	if result.MaxRPSAchieved > 0 && result.BreakingPointRPS > 0 {
		model.ScalingFactor = float64(result.BreakingPointRPS) / float64(result.MaxRPSAchieved)
	} else {
		model.ScalingFactor = 0.8 // Default assumption
	}

	return model
}

// BuildModelFromSoak creates a capacity model from soak test results.
func BuildModelFromSoak(name string, result *SoakResult, targetRPS float64) *CapacityModel {
	model := &CapacityModel{
		Name:              name,
		CreatedAt:         time.Now(),
		BaselineRPS:       result.RequestsPerSec,
		BaselineLatencyMs: float64(result.AvgLatency.Milliseconds()),
		ScalingFactor:     0.8,
	}

	// Estimate breaking point as 1.5x baseline if system was stable
	if result.Stable {
		model.BreakingPointRPS = result.RequestsPerSec * 1.5
		model.MaxRPS = result.RequestsPerSec * 2
	} else {
		model.BreakingPointRPS = result.RequestsPerSec
		model.MaxRPS = result.RequestsPerSec * 1.2
	}

	// Calculate per-request resource usage
	if result.TotalRequests > 0 {
		model.MemoryPerRequest = float64(result.PeakMemory) / float64(result.TotalRequests)
	}

	return model
}

// EstimateCapacity estimates resources needed for target RPS.
func (p *CapacityPlanner) EstimateCapacity(targetRPS float64) *CapacityEstimate {
	estimate := &CapacityEstimate{
		TargetRPS:       targetRPS,
		Warnings:        make([]string, 0),
		Recommendations: make([]string, 0),
	}

	if p.model == nil {
		estimate.Feasible = false
		estimate.Warnings = append(estimate.Warnings, "No capacity model available")
		return estimate
	}

	// Check feasibility
	if targetRPS > p.model.MaxRPS*2 {
		estimate.Feasible = false
		estimate.Warnings = append(estimate.Warnings,
			fmt.Sprintf("Target RPS (%.0f) exceeds 2x observed maximum (%.0f)", targetRPS, p.model.MaxRPS))
	} else {
		estimate.Feasible = true
	}

	// Calculate required instances
	if p.model.BaselineRPS > 0 {
		rawInstances := targetRPS / p.model.BaselineRPS
		// Apply scaling factor (diminishing returns)
		scaledInstances := rawInstances / p.model.ScalingFactor
		estimate.RequiredInstances = int(math.Ceil(scaledInstances))

		if estimate.RequiredInstances < 1 {
			estimate.RequiredInstances = 1
		}
	} else {
		estimate.RequiredInstances = 1
	}

	// Estimate latency using Little's Law approximation
	if p.model.BaselineRPS > 0 {
		// Latency increases as we approach capacity
		loadFactor := targetRPS / p.model.BreakingPointRPS
		if loadFactor > 1 {
			loadFactor = 1
		}

		// Exponential latency increase as load increases
		latencyMultiplier := 1.0 + (loadFactor*loadFactor*loadFactor)*10
		estimatedMs := p.model.BaselineLatencyMs * latencyMultiplier
		estimate.EstimatedLatency = time.Duration(estimatedMs) * time.Millisecond
	}

	// Calculate resource requirements
	if p.model.CPUPerRequest > 0 {
		estimate.RequiredCPU = targetRPS * p.model.CPUPerRequest
	} else {
		// Estimate: 1 CPU core per 100 RPS as baseline
		estimate.RequiredCPU = targetRPS / 100
	}

	if p.model.MemoryPerRequest > 0 {
		estimate.RequiredMemoryGB = (targetRPS * p.model.MemoryPerRequest) / (1024 * 1024 * 1024)
	} else {
		// Estimate: 100MB per 100 RPS as baseline
		estimate.RequiredMemoryGB = (targetRPS / 100) * 0.1
	}

	// Calculate confidence
	estimate.Confidence = p.calculateConfidence(targetRPS)

	// Generate recommendations
	p.generateRecommendations(estimate)

	return estimate
}

// calculateConfidence determines confidence in the estimate.
func (p *CapacityPlanner) calculateConfidence(targetRPS float64) float64 {
	if p.model == nil || p.model.BaselineRPS == 0 {
		return 0
	}

	// High confidence for targets within tested range
	if targetRPS <= p.model.BaselineRPS {
		return 0.95
	}

	// Decreasing confidence as we extrapolate beyond tested range
	if targetRPS <= p.model.BreakingPointRPS {
		return 0.85
	}

	if targetRPS <= p.model.MaxRPS {
		return 0.70
	}

	// Extrapolating beyond tested range
	ratio := targetRPS / p.model.MaxRPS
	if ratio > 2 {
		return 0.3
	}
	return 0.5
}

// generateRecommendations creates actionable recommendations.
func (p *CapacityPlanner) generateRecommendations(estimate *CapacityEstimate) {
	if estimate.RequiredInstances > 1 {
		estimate.Recommendations = append(estimate.Recommendations,
			fmt.Sprintf("Deploy %d instances behind a load balancer", estimate.RequiredInstances))
	}

	if estimate.EstimatedLatency > 500*time.Millisecond {
		estimate.Recommendations = append(estimate.Recommendations,
			"Consider adding caching layer to reduce latency")
	}

	if estimate.RequiredMemoryGB > 4 {
		estimate.Recommendations = append(estimate.Recommendations,
			"Enable memory profiling to optimize allocations")
	}

	if estimate.Confidence < 0.5 {
		estimate.Recommendations = append(estimate.Recommendations,
			"Run additional load tests at higher RPS to improve estimate accuracy")
	}

	if estimate.TargetRPS > p.model.BreakingPointRPS*0.8 {
		estimate.Recommendations = append(estimate.Recommendations,
			"Target is near system limits - implement circuit breakers and graceful degradation")
	}
}

// PlanForGrowth creates a scaling plan for projected growth.
func (p *CapacityPlanner) PlanForGrowth(currentRPS, growthRatePercent float64, months int) []CapacityEstimate {
	plans := make([]CapacityEstimate, months)

	rps := currentRPS
	monthlyGrowth := 1 + (growthRatePercent / 100)

	for i := 0; i < months; i++ {
		rps *= monthlyGrowth
		estimate := p.EstimateCapacity(rps)
		plans[i] = *estimate
	}

	return plans
}

// CalculateHeadroom determines how much capacity headroom exists.
func (p *CapacityPlanner) CalculateHeadroom(currentRPS float64) *HeadroomAnalysis {
	analysis := &HeadroomAnalysis{
		CurrentRPS: currentRPS,
	}

	if p.model == nil {
		return analysis
	}

	// Headroom to recommended operating point (70% of breaking point)
	recommendedMax := p.model.BreakingPointRPS * 0.7
	if currentRPS < recommendedMax {
		analysis.RecommendedHeadroom = recommendedMax - currentRPS
		analysis.RecommendedHeadroomPct = (analysis.RecommendedHeadroom / currentRPS) * 100
	}

	// Headroom to breaking point
	if currentRPS < p.model.BreakingPointRPS {
		analysis.AbsoluteHeadroom = p.model.BreakingPointRPS - currentRPS
		analysis.AbsoluteHeadroomPct = (analysis.AbsoluteHeadroom / currentRPS) * 100
	}

	// Utilization percentage
	if p.model.BreakingPointRPS > 0 {
		analysis.Utilization = (currentRPS / p.model.BreakingPointRPS) * 100
	}

	// Risk assessment
	if analysis.Utilization >= 90 {
		analysis.RiskLevel = "critical"
		analysis.Recommendation = "Immediate scaling required"
	} else if analysis.Utilization >= 70 {
		analysis.RiskLevel = "high"
		analysis.Recommendation = "Plan scaling within 1-2 weeks"
	} else if analysis.Utilization >= 50 {
		analysis.RiskLevel = "medium"
		analysis.Recommendation = "Monitor closely, plan for growth"
	} else {
		analysis.RiskLevel = "low"
		analysis.Recommendation = "Adequate headroom available"
	}

	return analysis
}

// HeadroomAnalysis shows remaining capacity.
type HeadroomAnalysis struct {
	CurrentRPS             float64
	RecommendedHeadroom    float64
	RecommendedHeadroomPct float64
	AbsoluteHeadroom       float64
	AbsoluteHeadroomPct    float64
	Utilization            float64 // Percentage of capacity used
	RiskLevel              string
	Recommendation         string
}

// GenerateCapacityReport creates a formatted capacity report.
func (p *CapacityPlanner) GenerateCapacityReport() string {
	if p.model == nil {
		return "No capacity model available"
	}

	report := "Capacity Planning Report\n"
	report += "========================\n\n"
	report += fmt.Sprintf("Model: %s\n", p.model.Name)
	report += fmt.Sprintf("Created: %s\n\n", p.model.CreatedAt.Format(time.RFC3339))

	report += "System Characteristics:\n"
	report += "-----------------------\n"
	report += fmt.Sprintf("Baseline RPS: %.2f\n", p.model.BaselineRPS)
	report += fmt.Sprintf("Baseline Latency: %.2f ms\n", p.model.BaselineLatencyMs)
	report += fmt.Sprintf("Breaking Point: %.2f RPS\n", p.model.BreakingPointRPS)
	report += fmt.Sprintf("Maximum Observed: %.2f RPS\n", p.model.MaxRPS)
	report += fmt.Sprintf("Scaling Factor: %.2f\n\n", p.model.ScalingFactor)

	report += "Capacity Projections:\n"
	report += "---------------------\n"

	// Show estimates for various RPS targets
	targets := []float64{
		p.model.BaselineRPS * 0.5,
		p.model.BaselineRPS,
		p.model.BaselineRPS * 1.5,
		p.model.BaselineRPS * 2,
		p.model.BaselineRPS * 3,
	}

	for _, target := range targets {
		est := p.EstimateCapacity(target)
		feasible := "✓"
		if !est.Feasible {
			feasible = "✗"
		}
		report += fmt.Sprintf("%s %.0f RPS: %d instances, ~%.0f ms latency (%.0f%% confidence)\n",
			feasible, target, est.RequiredInstances, float64(est.EstimatedLatency.Milliseconds()), est.Confidence*100)
	}

	return report
}
