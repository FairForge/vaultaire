// Package loadtest provides infrastructure for load, stress, and chaos testing.
package loadtest

import (
	"fmt"
	"time"
)

// SLA defines a Service Level Agreement with performance targets.
type SLA struct {
	Name        string
	Description string
	Objectives  []SLO
}

// SLO defines a Service Level Objective - a specific measurable target.
type SLO struct {
	Name       string
	Metric     SLOMetric
	Target     float64
	Comparator Comparator
	Window     time.Duration // Measurement window (e.g., "per month")
	Priority   SLOPriority
}

// SLOMetric identifies what metric the SLO measures.
type SLOMetric string

const (
	MetricAvailability SLOMetric = "availability"
	MetricLatencyP50   SLOMetric = "latency_p50"
	MetricLatencyP95   SLOMetric = "latency_p95"
	MetricLatencyP99   SLOMetric = "latency_p99"
	MetricLatencyMax   SLOMetric = "latency_max"
	MetricErrorRate    SLOMetric = "error_rate"
	MetricThroughput   SLOMetric = "throughput"
	MetricSuccessRate  SLOMetric = "success_rate"
)

// Comparator defines how to compare metric against target.
type Comparator string

const (
	ComparatorLessThan       Comparator = "<"
	ComparatorLessOrEqual    Comparator = "<="
	ComparatorGreaterThan    Comparator = ">"
	ComparatorGreaterOrEqual Comparator = ">="
)

// SLOPriority indicates the importance of an SLO.
type SLOPriority string

const (
	PriorityCritical SLOPriority = "critical"
	PriorityHigh     SLOPriority = "high"
	PriorityMedium   SLOPriority = "medium"
	PriorityLow      SLOPriority = "low"
)

// SLAResult captures the result of validating against an SLA.
type SLAResult struct {
	SLA              *SLA
	Timestamp        time.Time
	Duration         time.Duration
	ObjectiveResults []SLOResult
	OverallPass      bool
	CriticalPass     bool
	Score            float64 // Percentage of SLOs met
}

// SLOResult captures the result of a single SLO check.
type SLOResult struct {
	SLO         SLO
	ActualValue float64
	TargetMet   bool
	Margin      float64 // How far from target (negative = failed)
	Message     string
}

// DefaultStorageSLA returns recommended SLA for stored.ge.
func DefaultStorageSLA() *SLA {
	return &SLA{
		Name:        "stored.ge-production",
		Description: "Production SLA for stored.ge S3-compatible storage",
		Objectives: []SLO{
			{
				Name:       "Availability",
				Metric:     MetricAvailability,
				Target:     99.9,
				Comparator: ComparatorGreaterOrEqual,
				Window:     30 * 24 * time.Hour, // Monthly
				Priority:   PriorityCritical,
			},
			{
				Name:       "P50 Latency",
				Metric:     MetricLatencyP50,
				Target:     50, // milliseconds
				Comparator: ComparatorLessOrEqual,
				Window:     time.Hour,
				Priority:   PriorityMedium,
			},
			{
				Name:       "P95 Latency",
				Metric:     MetricLatencyP95,
				Target:     200, // milliseconds
				Comparator: ComparatorLessOrEqual,
				Window:     time.Hour,
				Priority:   PriorityHigh,
			},
			{
				Name:       "P99 Latency",
				Metric:     MetricLatencyP99,
				Target:     500, // milliseconds
				Comparator: ComparatorLessOrEqual,
				Window:     time.Hour,
				Priority:   PriorityCritical,
			},
			{
				Name:       "Error Rate",
				Metric:     MetricErrorRate,
				Target:     0.1, // 0.1%
				Comparator: ComparatorLessOrEqual,
				Window:     time.Hour,
				Priority:   PriorityCritical,
			},
			{
				Name:       "Throughput",
				Metric:     MetricThroughput,
				Target:     100, // RPS per instance
				Comparator: ComparatorGreaterOrEqual,
				Window:     time.Hour,
				Priority:   PriorityHigh,
			},
		},
	}
}

// SLAValidator validates test results against SLAs.
type SLAValidator struct {
	sla *SLA
}

// NewSLAValidator creates a validator for the given SLA.
func NewSLAValidator(sla *SLA) *SLAValidator {
	return &SLAValidator{sla: sla}
}

// Validate checks a test summary against the SLA.
func (v *SLAValidator) Validate(summary *Summary) *SLAResult {
	result := &SLAResult{
		SLA:              v.sla,
		Timestamp:        time.Now(),
		Duration:         summary.EndTime.Sub(summary.StartTime),
		ObjectiveResults: make([]SLOResult, 0, len(v.sla.Objectives)),
		OverallPass:      true,
		CriticalPass:     true,
	}

	passCount := 0

	for _, slo := range v.sla.Objectives {
		sloResult := v.checkSLO(slo, summary)
		result.ObjectiveResults = append(result.ObjectiveResults, sloResult)

		if sloResult.TargetMet {
			passCount++
		} else {
			result.OverallPass = false
			if slo.Priority == PriorityCritical {
				result.CriticalPass = false
			}
		}
	}

	if len(v.sla.Objectives) > 0 {
		result.Score = float64(passCount) / float64(len(v.sla.Objectives)) * 100
	}

	return result
}

// checkSLO validates a single SLO against the summary.
func (v *SLAValidator) checkSLO(slo SLO, summary *Summary) SLOResult {
	result := SLOResult{
		SLO: slo,
	}

	// Extract the actual value for this metric
	switch slo.Metric {
	case MetricAvailability:
		if summary.TotalRequests > 0 {
			result.ActualValue = (1 - summary.ErrorRate) * 100
		}
	case MetricLatencyP50:
		result.ActualValue = float64(summary.P50Latency.Milliseconds())
	case MetricLatencyP95:
		result.ActualValue = float64(summary.P95Latency.Milliseconds())
	case MetricLatencyP99:
		result.ActualValue = float64(summary.P99Latency.Milliseconds())
	case MetricLatencyMax:
		result.ActualValue = float64(summary.MaxLatency.Milliseconds())
	case MetricErrorRate:
		result.ActualValue = summary.ErrorRate * 100 // Convert to percentage
	case MetricThroughput:
		result.ActualValue = summary.RequestsPerSec
	case MetricSuccessRate:
		if summary.TotalRequests > 0 {
			result.ActualValue = (1 - summary.ErrorRate) * 100
		}
	}

	// Check if target is met
	result.TargetMet = v.compareValues(result.ActualValue, slo.Target, slo.Comparator)

	// Calculate margin
	switch slo.Comparator {
	case ComparatorLessThan, ComparatorLessOrEqual:
		result.Margin = slo.Target - result.ActualValue
	case ComparatorGreaterThan, ComparatorGreaterOrEqual:
		result.Margin = result.ActualValue - slo.Target
	}

	// Generate message
	if result.TargetMet {
		result.Message = fmt.Sprintf("%s: %.2f %s %.2f ✓",
			slo.Name, result.ActualValue, slo.Comparator, slo.Target)
	} else {
		result.Message = fmt.Sprintf("%s: %.2f %s %.2f ✗ (margin: %.2f)",
			slo.Name, result.ActualValue, slo.Comparator, slo.Target, result.Margin)
	}

	return result
}

// compareValues checks if actual meets target based on comparator.
func (v *SLAValidator) compareValues(actual, target float64, comp Comparator) bool {
	switch comp {
	case ComparatorLessThan:
		return actual < target
	case ComparatorLessOrEqual:
		return actual <= target
	case ComparatorGreaterThan:
		return actual > target
	case ComparatorGreaterOrEqual:
		return actual >= target
	default:
		return false
	}
}

// GenerateReport creates a human-readable SLA validation report.
func (r *SLAResult) GenerateReport() string {
	report := "SLA Validation Report\n"
	report += "=====================\n\n"
	report += fmt.Sprintf("SLA: %s\n", r.SLA.Name)
	report += fmt.Sprintf("Description: %s\n", r.SLA.Description)
	report += fmt.Sprintf("Validated: %s\n", r.Timestamp.Format(time.RFC3339))
	report += fmt.Sprintf("Test Duration: %v\n\n", r.Duration)

	status := "PASS"
	if !r.OverallPass {
		status = "FAIL"
	}
	report += fmt.Sprintf("Overall Status: %s\n", status)
	report += fmt.Sprintf("Score: %.1f%% (%d/%d objectives met)\n",
		r.Score, r.countPassed(), len(r.ObjectiveResults))

	if !r.CriticalPass {
		report += "⚠️  CRITICAL OBJECTIVES FAILED\n"
	}
	report += "\n"

	report += "Objective Results:\n"
	report += "------------------\n"

	// Group by priority
	for _, priority := range []SLOPriority{PriorityCritical, PriorityHigh, PriorityMedium, PriorityLow} {
		hasResults := false
		for _, res := range r.ObjectiveResults {
			if res.SLO.Priority == priority {
				if !hasResults {
					report += fmt.Sprintf("\n[%s]\n", priority)
					hasResults = true
				}
				report += fmt.Sprintf("  %s\n", res.Message)
			}
		}
	}

	return report
}

// countPassed returns the number of passed objectives.
func (r *SLAResult) countPassed() int {
	count := 0
	for _, res := range r.ObjectiveResults {
		if res.TargetMet {
			count++
		}
	}
	return count
}

// GetFailedCritical returns failed critical SLOs.
func (r *SLAResult) GetFailedCritical() []SLOResult {
	failed := make([]SLOResult, 0)
	for _, res := range r.ObjectiveResults {
		if !res.TargetMet && res.SLO.Priority == PriorityCritical {
			failed = append(failed, res)
		}
	}
	return failed
}

// GetAllFailed returns all failed SLOs.
func (r *SLAResult) GetAllFailed() []SLOResult {
	failed := make([]SLOResult, 0)
	for _, res := range r.ObjectiveResults {
		if !res.TargetMet {
			failed = append(failed, res)
		}
	}
	return failed
}

// CreateCustomSLA builds a custom SLA from objectives.
func CreateCustomSLA(name, description string, objectives ...SLO) *SLA {
	return &SLA{
		Name:        name,
		Description: description,
		Objectives:  objectives,
	}
}

// NewLatencySLO creates a latency-based SLO.
func NewLatencySLO(name string, metric SLOMetric, targetMs float64, priority SLOPriority) SLO {
	return SLO{
		Name:       name,
		Metric:     metric,
		Target:     targetMs,
		Comparator: ComparatorLessOrEqual,
		Window:     time.Hour,
		Priority:   priority,
	}
}

// NewErrorRateSLO creates an error rate SLO.
func NewErrorRateSLO(name string, targetPercent float64, priority SLOPriority) SLO {
	return SLO{
		Name:       name,
		Metric:     MetricErrorRate,
		Target:     targetPercent,
		Comparator: ComparatorLessOrEqual,
		Window:     time.Hour,
		Priority:   priority,
	}
}

// NewThroughputSLO creates a throughput SLO.
func NewThroughputSLO(name string, targetRPS float64, priority SLOPriority) SLO {
	return SLO{
		Name:       name,
		Metric:     MetricThroughput,
		Target:     targetRPS,
		Comparator: ComparatorGreaterOrEqual,
		Window:     time.Hour,
		Priority:   priority,
	}
}
