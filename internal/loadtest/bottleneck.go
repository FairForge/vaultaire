// Package loadtest provides infrastructure for load, stress, and chaos testing.
package loadtest

import (
	"fmt"
	"sort"
	"time"
)

// BottleneckType identifies the category of bottleneck.
type BottleneckType string

const (
	BottleneckCPU         BottleneckType = "cpu"
	BottleneckMemory      BottleneckType = "memory"
	BottleneckIO          BottleneckType = "io"
	BottleneckNetwork     BottleneckType = "network"
	BottleneckDatabase    BottleneckType = "database"
	BottleneckConcurrency BottleneckType = "concurrency"
	BottleneckLatency     BottleneckType = "latency"
	BottleneckThroughput  BottleneckType = "throughput"
	BottleneckGoroutine   BottleneckType = "goroutine"
	BottleneckGC          BottleneckType = "gc"
)

// Severity indicates how critical a bottleneck is.
type Severity string

const (
	SeverityCritical Severity = "critical"
	SeverityHigh     Severity = "high"
	SeverityMedium   Severity = "medium"
	SeverityLow      Severity = "low"
	SeverityInfo     Severity = "info"
)

// Bottleneck represents an identified performance bottleneck.
type Bottleneck struct {
	Type        BottleneckType
	Severity    Severity
	Description string
	Evidence    []string
	Impact      string
	Suggestion  string
	DetectedAt  time.Time
	Metrics     map[string]float64
}

// BottleneckAnalysis contains the complete analysis results.
type BottleneckAnalysis struct {
	Timestamp   time.Time
	TestName    string
	Bottlenecks []Bottleneck
	Summary     string
	HealthScore float64 // 0-100, higher is better
	TopIssue    *Bottleneck
}

// AnalysisConfig configures bottleneck detection thresholds.
type AnalysisConfig struct {
	// Latency thresholds
	P99LatencyThreshold time.Duration
	P95LatencyThreshold time.Duration
	AvgLatencyThreshold time.Duration

	// Throughput thresholds
	MinAcceptableRPS float64
	RPSDropThreshold float64 // Percentage drop that triggers alert

	// Error thresholds
	ErrorRateThreshold float64

	// Resource thresholds
	MemoryGrowthThreshold float64 // Percentage
	GoroutineThreshold    int
	GCPauseThreshold      time.Duration

	// Concurrency thresholds
	ConcurrencyEfficiency float64 // Expected RPS per concurrent worker
}

// DefaultAnalysisConfig returns sensible defaults for analysis.
func DefaultAnalysisConfig() *AnalysisConfig {
	return &AnalysisConfig{
		P99LatencyThreshold:   500 * time.Millisecond,
		P95LatencyThreshold:   200 * time.Millisecond,
		AvgLatencyThreshold:   100 * time.Millisecond,
		MinAcceptableRPS:      10,
		RPSDropThreshold:      20, // 20% drop
		ErrorRateThreshold:    0.05,
		MemoryGrowthThreshold: 50, // 50% growth
		GoroutineThreshold:    1000,
		GCPauseThreshold:      100 * time.Millisecond,
		ConcurrencyEfficiency: 2, // 2 RPS per worker
	}
}

// BottleneckAnalyzer identifies performance bottlenecks from test results.
type BottleneckAnalyzer struct {
	config *AnalysisConfig
}

// NewBottleneckAnalyzer creates a new analyzer with the given config.
func NewBottleneckAnalyzer(config *AnalysisConfig) *BottleneckAnalyzer {
	if config == nil {
		config = DefaultAnalysisConfig()
	}
	return &BottleneckAnalyzer{config: config}
}

// AnalyzeLoadTest analyzes load test results for bottlenecks.
func (a *BottleneckAnalyzer) AnalyzeLoadTest(summary *Summary) *BottleneckAnalysis {
	analysis := &BottleneckAnalysis{
		Timestamp:   time.Now(),
		TestName:    summary.TestName,
		Bottlenecks: make([]Bottleneck, 0),
	}

	// Check latency bottlenecks
	a.checkLatencyBottlenecks(summary, analysis)

	// Check throughput bottlenecks
	a.checkThroughputBottlenecks(summary, analysis)

	// Check error rate bottlenecks
	a.checkErrorBottlenecks(summary, analysis)

	// Calculate health score and summary
	a.calculateHealthScore(analysis)
	a.generateSummary(analysis)

	return analysis
}

// AnalyzeSoakTest analyzes soak test results for bottlenecks.
func (a *BottleneckAnalyzer) AnalyzeSoakTest(result *SoakResult) *BottleneckAnalysis {
	// Start with load test analysis
	analysis := a.AnalyzeLoadTest(result.Summary)

	// Add soak-specific checks
	a.checkMemoryBottlenecks(result, analysis)
	a.checkGoroutineBottlenecks(result, analysis)
	a.checkGCBottlenecks(result, analysis)

	// Recalculate after adding soak-specific bottlenecks
	a.calculateHealthScore(analysis)
	a.generateSummary(analysis)

	return analysis
}

// AnalyzeStressTest analyzes stress test results for bottlenecks.
func (a *BottleneckAnalyzer) AnalyzeStressTest(result *StressResult) *BottleneckAnalysis {
	analysis := a.AnalyzeLoadTest(result.Summary)

	// Add stress-specific checks
	a.checkBreakingPoint(result, analysis)

	// Recalculate
	a.calculateHealthScore(analysis)
	a.generateSummary(analysis)

	return analysis
}

// checkLatencyBottlenecks identifies latency-related issues.
func (a *BottleneckAnalyzer) checkLatencyBottlenecks(summary *Summary, analysis *BottleneckAnalysis) {
	// P99 latency check
	if summary.P99Latency > a.config.P99LatencyThreshold {
		severity := SeverityMedium
		if summary.P99Latency > a.config.P99LatencyThreshold*2 {
			severity = SeverityHigh
		}
		if summary.P99Latency > a.config.P99LatencyThreshold*5 {
			severity = SeverityCritical
		}

		analysis.Bottlenecks = append(analysis.Bottlenecks, Bottleneck{
			Type:        BottleneckLatency,
			Severity:    severity,
			Description: "P99 latency exceeds acceptable threshold",
			Evidence: []string{
				fmt.Sprintf("P99 latency: %v (threshold: %v)", summary.P99Latency, a.config.P99LatencyThreshold),
				fmt.Sprintf("P95 latency: %v", summary.P95Latency),
				fmt.Sprintf("Avg latency: %v", summary.AvgLatency),
			},
			Impact:     "Tail latency affects user experience for 1% of requests",
			Suggestion: "Profile slow requests, check for lock contention, optimize database queries",
			DetectedAt: time.Now(),
			Metrics: map[string]float64{
				"p99_latency_ms": float64(summary.P99Latency.Milliseconds()),
				"threshold_ms":   float64(a.config.P99LatencyThreshold.Milliseconds()),
			},
		})
	}

	// P95 latency check
	if summary.P95Latency > a.config.P95LatencyThreshold {
		severity := SeverityMedium
		if summary.P95Latency > a.config.P95LatencyThreshold*2 {
			severity = SeverityHigh
		}

		analysis.Bottlenecks = append(analysis.Bottlenecks, Bottleneck{
			Type:        BottleneckLatency,
			Severity:    severity,
			Description: "P95 latency exceeds acceptable threshold",
			Evidence: []string{
				fmt.Sprintf("P95 latency: %v (threshold: %v)", summary.P95Latency, a.config.P95LatencyThreshold),
			},
			Impact:     "5% of requests experience degraded performance",
			Suggestion: "Add caching, optimize hot paths, consider async processing",
			DetectedAt: time.Now(),
			Metrics: map[string]float64{
				"p95_latency_ms": float64(summary.P95Latency.Milliseconds()),
			},
		})
	}

	// Latency variance check (max vs avg)
	if summary.AvgLatency > 0 && summary.MaxLatency > summary.AvgLatency*10 {
		analysis.Bottlenecks = append(analysis.Bottlenecks, Bottleneck{
			Type:        BottleneckLatency,
			Severity:    SeverityMedium,
			Description: "High latency variance detected",
			Evidence: []string{
				fmt.Sprintf("Max latency: %v", summary.MaxLatency),
				fmt.Sprintf("Avg latency: %v", summary.AvgLatency),
				fmt.Sprintf("Ratio: %.1fx", float64(summary.MaxLatency)/float64(summary.AvgLatency)),
			},
			Impact:     "Unpredictable response times affect reliability",
			Suggestion: "Investigate outliers, check for GC pauses or resource contention",
			DetectedAt: time.Now(),
			Metrics: map[string]float64{
				"variance_ratio": float64(summary.MaxLatency) / float64(summary.AvgLatency),
			},
		})
	}
}

// checkThroughputBottlenecks identifies throughput-related issues.
func (a *BottleneckAnalyzer) checkThroughputBottlenecks(summary *Summary, analysis *BottleneckAnalysis) {
	if summary.RequestsPerSec < a.config.MinAcceptableRPS {
		severity := SeverityMedium
		if summary.RequestsPerSec < a.config.MinAcceptableRPS/2 {
			severity = SeverityHigh
		}
		if summary.RequestsPerSec < a.config.MinAcceptableRPS/5 {
			severity = SeverityCritical
		}

		analysis.Bottlenecks = append(analysis.Bottlenecks, Bottleneck{
			Type:        BottleneckThroughput,
			Severity:    severity,
			Description: "Throughput below minimum acceptable level",
			Evidence: []string{
				fmt.Sprintf("Actual RPS: %.2f", summary.RequestsPerSec),
				fmt.Sprintf("Minimum required: %.2f", a.config.MinAcceptableRPS),
			},
			Impact:     "System cannot handle expected load",
			Suggestion: "Scale horizontally, optimize critical paths, add caching",
			DetectedAt: time.Now(),
			Metrics: map[string]float64{
				"actual_rps":   summary.RequestsPerSec,
				"required_rps": a.config.MinAcceptableRPS,
			},
		})
	}
}

// checkErrorBottlenecks identifies error-related issues.
func (a *BottleneckAnalyzer) checkErrorBottlenecks(summary *Summary, analysis *BottleneckAnalysis) {
	if summary.ErrorRate > a.config.ErrorRateThreshold {
		severity := SeverityMedium
		if summary.ErrorRate > a.config.ErrorRateThreshold*2 {
			severity = SeverityHigh
		}
		if summary.ErrorRate >= 0.2 { // 20% errors is critical (changed > to >=)
			severity = SeverityCritical
		}

		analysis.Bottlenecks = append(analysis.Bottlenecks, Bottleneck{
			Type:        BottleneckThroughput,
			Severity:    severity,
			Description: "Error rate exceeds acceptable threshold",
			Evidence: []string{
				fmt.Sprintf("Error rate: %.2f%%", summary.ErrorRate*100),
				fmt.Sprintf("Threshold: %.2f%%", a.config.ErrorRateThreshold*100),
				fmt.Sprintf("Failed requests: %d", summary.FailureCount),
			},
			Impact:     "Users experiencing failures, potential data loss",
			Suggestion: "Review error logs, add circuit breakers, improve error handling",
			DetectedAt: time.Now(),
			Metrics: map[string]float64{
				"error_rate": summary.ErrorRate,
				"failures":   float64(summary.FailureCount),
			},
		})
	}
}

// checkMemoryBottlenecks identifies memory-related issues from soak tests.
func (a *BottleneckAnalyzer) checkMemoryBottlenecks(result *SoakResult, analysis *BottleneckAnalysis) {
	if result.MemoryGrowth > a.config.MemoryGrowthThreshold {
		severity := SeverityMedium
		if result.MemoryGrowth > a.config.MemoryGrowthThreshold*2 {
			severity = SeverityHigh
		}
		if result.MemoryGrowth > 100 { // 100% growth is critical
			severity = SeverityCritical
		}

		analysis.Bottlenecks = append(analysis.Bottlenecks, Bottleneck{
			Type:        BottleneckMemory,
			Severity:    severity,
			Description: "Potential memory leak detected",
			Evidence: []string{
				fmt.Sprintf("Memory growth: %.2f%%", result.MemoryGrowth),
				fmt.Sprintf("Peak memory: %d MB", result.PeakMemory/(1024*1024)),
			},
			Impact:     "System may run out of memory over time",
			Suggestion: "Profile memory allocation, check for unclosed resources, review object lifecycles",
			DetectedAt: time.Now(),
			Metrics: map[string]float64{
				"memory_growth_pct": result.MemoryGrowth,
				"peak_memory_mb":    float64(result.PeakMemory) / (1024 * 1024),
			},
		})
	}
}

// checkGoroutineBottlenecks identifies goroutine-related issues.
func (a *BottleneckAnalyzer) checkGoroutineBottlenecks(result *SoakResult, analysis *BottleneckAnalysis) {
	if result.PeakGoroutines > a.config.GoroutineThreshold {
		severity := SeverityMedium
		if result.PeakGoroutines > a.config.GoroutineThreshold*5 {
			severity = SeverityHigh
		}

		analysis.Bottlenecks = append(analysis.Bottlenecks, Bottleneck{
			Type:        BottleneckGoroutine,
			Severity:    severity,
			Description: "Excessive goroutine count",
			Evidence: []string{
				fmt.Sprintf("Peak goroutines: %d", result.PeakGoroutines),
				fmt.Sprintf("Threshold: %d", a.config.GoroutineThreshold),
			},
			Impact:     "High scheduling overhead, potential goroutine leaks",
			Suggestion: "Use worker pools, check for goroutine leaks, add context cancellation",
			DetectedAt: time.Now(),
			Metrics: map[string]float64{
				"peak_goroutines": float64(result.PeakGoroutines),
			},
		})
	}

	// Check for goroutine growth
	if result.GoroutineGrowth > 50 { // 50% growth
		analysis.Bottlenecks = append(analysis.Bottlenecks, Bottleneck{
			Type:        BottleneckGoroutine,
			Severity:    SeverityHigh,
			Description: "Goroutine leak suspected",
			Evidence: []string{
				fmt.Sprintf("Goroutine growth: %.2f%%", result.GoroutineGrowth),
			},
			Impact:     "Goroutines accumulating over time",
			Suggestion: "Ensure all goroutines have exit conditions, use context for cancellation",
			DetectedAt: time.Now(),
			Metrics: map[string]float64{
				"goroutine_growth_pct": result.GoroutineGrowth,
			},
		})
	}
}

// checkGCBottlenecks identifies garbage collection issues.
func (a *BottleneckAnalyzer) checkGCBottlenecks(result *SoakResult, analysis *BottleneckAnalysis) {
	// Analyze GC from samples
	if len(result.ResourceSamples) < 2 {
		return
	}

	first := result.ResourceSamples[0]
	last := result.ResourceSamples[len(result.ResourceSamples)-1]

	gcCycles := last.NumGC - first.NumGC
	duration := last.Timestamp.Sub(first.Timestamp)

	if duration.Seconds() > 0 && gcCycles > 0 {
		gcPerSecond := float64(gcCycles) / duration.Seconds()

		// More than 1 GC per second is concerning
		if gcPerSecond > 1 {
			analysis.Bottlenecks = append(analysis.Bottlenecks, Bottleneck{
				Type:        BottleneckGC,
				Severity:    SeverityMedium,
				Description: "High GC frequency",
				Evidence: []string{
					fmt.Sprintf("GC cycles: %d in %v", gcCycles, duration),
					fmt.Sprintf("GC rate: %.2f/sec", gcPerSecond),
				},
				Impact:     "GC overhead reducing throughput",
				Suggestion: "Reduce allocations, use sync.Pool, consider GOGC tuning",
				DetectedAt: time.Now(),
				Metrics: map[string]float64{
					"gc_per_second": gcPerSecond,
					"total_gc":      float64(gcCycles),
				},
			})
		}
	}
}

// checkBreakingPoint analyzes stress test breaking point.
func (a *BottleneckAnalyzer) checkBreakingPoint(result *StressResult, analysis *BottleneckAnalysis) {
	if result.BreakingPointRPS > 0 && result.MaxRPSAchieved > 0 {
		severity := SeverityInfo
		ratio := float64(result.BreakingPointRPS) / float64(result.MaxRPSAchieved)
		if ratio < 0.5 {
			severity = SeverityMedium
		}
		if ratio < 0.25 {
			severity = SeverityHigh
		}

		analysis.Bottlenecks = append(analysis.Bottlenecks, Bottleneck{
			Type:        BottleneckConcurrency,
			Severity:    severity,
			Description: "System breaking point identified",
			Evidence: []string{
				fmt.Sprintf("Breaking point: %d RPS", result.BreakingPointRPS),
				fmt.Sprintf("Max RPS achieved: %d", result.MaxRPSAchieved),
				fmt.Sprintf("Stop reason: %s", result.StopReason),
			},
			Impact:     "System cannot sustain target load",
			Suggestion: "Scale resources, optimize hotspots, implement backpressure",
			DetectedAt: time.Now(),
			Metrics: map[string]float64{
				"breaking_point_rps": float64(result.BreakingPointRPS),
				"max_rps_achieved":   float64(result.MaxRPSAchieved),
			},
		})
	}
}

// calculateHealthScore computes overall system health from bottlenecks.
func (a *BottleneckAnalyzer) calculateHealthScore(analysis *BottleneckAnalysis) {
	score := 100.0

	for _, b := range analysis.Bottlenecks {
		switch b.Severity {
		case SeverityCritical:
			score -= 30
		case SeverityHigh:
			score -= 20
		case SeverityMedium:
			score -= 10
		case SeverityLow:
			score -= 5
		case SeverityInfo:
			score -= 2
		}
	}

	if score < 0 {
		score = 0
	}

	analysis.HealthScore = score

	// Identify top issue
	if len(analysis.Bottlenecks) > 0 {
		// Sort by severity
		sorted := make([]Bottleneck, len(analysis.Bottlenecks))
		copy(sorted, analysis.Bottlenecks)
		sort.Slice(sorted, func(i, j int) bool {
			return severityRank(sorted[i].Severity) > severityRank(sorted[j].Severity)
		})
		analysis.TopIssue = &sorted[0]
	}
}

// severityRank returns numeric rank for sorting.
func severityRank(s Severity) int {
	switch s {
	case SeverityCritical:
		return 5
	case SeverityHigh:
		return 4
	case SeverityMedium:
		return 3
	case SeverityLow:
		return 2
	case SeverityInfo:
		return 1
	default:
		return 0
	}
}

// generateSummary creates a human-readable summary.
func (a *BottleneckAnalyzer) generateSummary(analysis *BottleneckAnalysis) {
	if len(analysis.Bottlenecks) == 0 {
		analysis.Summary = "No significant bottlenecks detected. System health is good."
		return
	}

	// Count by severity
	counts := make(map[Severity]int)
	for _, b := range analysis.Bottlenecks {
		counts[b.Severity]++
	}

	analysis.Summary = fmt.Sprintf("Detected %d bottleneck(s): ", len(analysis.Bottlenecks))

	if counts[SeverityCritical] > 0 {
		analysis.Summary += fmt.Sprintf("%d critical, ", counts[SeverityCritical])
	}
	if counts[SeverityHigh] > 0 {
		analysis.Summary += fmt.Sprintf("%d high, ", counts[SeverityHigh])
	}
	if counts[SeverityMedium] > 0 {
		analysis.Summary += fmt.Sprintf("%d medium, ", counts[SeverityMedium])
	}
	if counts[SeverityLow] > 0 {
		analysis.Summary += fmt.Sprintf("%d low, ", counts[SeverityLow])
	}

	// Remove trailing comma and space
	if len(analysis.Summary) > 2 {
		analysis.Summary = analysis.Summary[:len(analysis.Summary)-2]
	}

	analysis.Summary += fmt.Sprintf(". Health score: %.0f/100", analysis.HealthScore)
}

// GenerateReport creates a detailed report of the analysis.
func (analysis *BottleneckAnalysis) GenerateReport() string {
	report := "Bottleneck Analysis Report\n"
	report += "==========================\n\n"
	report += fmt.Sprintf("Test: %s\n", analysis.TestName)
	report += fmt.Sprintf("Analyzed: %s\n", analysis.Timestamp.Format(time.RFC3339))
	report += fmt.Sprintf("Health Score: %.0f/100\n\n", analysis.HealthScore)
	report += fmt.Sprintf("Summary: %s\n\n", analysis.Summary)

	if len(analysis.Bottlenecks) == 0 {
		report += "No bottlenecks detected.\n"
		return report
	}

	report += "Identified Bottlenecks:\n"
	report += "-----------------------\n"

	for i, b := range analysis.Bottlenecks {
		report += fmt.Sprintf("\n%d. [%s] %s - %s\n", i+1, b.Severity, b.Type, b.Description)
		report += "   Evidence:\n"
		for _, e := range b.Evidence {
			report += fmt.Sprintf("   - %s\n", e)
		}
		report += fmt.Sprintf("   Impact: %s\n", b.Impact)
		report += fmt.Sprintf("   Suggestion: %s\n", b.Suggestion)
	}

	return report
}
