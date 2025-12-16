// Package loadtest provides infrastructure for load, stress, and chaos testing.
package loadtest

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Baseline represents a recorded performance baseline.
type Baseline struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	CreatedAt   time.Time       `json:"created_at"`
	Environment string          `json:"environment"`
	Version     string          `json:"version"`
	Metrics     BaselineMetrics `json:"metrics"`
}

// BaselineMetrics captures the key performance indicators.
type BaselineMetrics struct {
	// Throughput
	RequestsPerSec float64 `json:"requests_per_sec"`
	BytesPerSec    float64 `json:"bytes_per_sec"`

	// Latency (in milliseconds for JSON readability)
	AvgLatencyMs float64 `json:"avg_latency_ms"`
	P50LatencyMs float64 `json:"p50_latency_ms"`
	P95LatencyMs float64 `json:"p95_latency_ms"`
	P99LatencyMs float64 `json:"p99_latency_ms"`
	MaxLatencyMs float64 `json:"max_latency_ms"`

	// Reliability
	ErrorRate   float64 `json:"error_rate"`
	SuccessRate float64 `json:"success_rate"`

	// Resource usage
	PeakMemoryMB  float64 `json:"peak_memory_mb"`
	AvgGoroutines float64 `json:"avg_goroutines"`

	// Test parameters
	Duration    time.Duration `json:"duration"`
	TargetRPS   int           `json:"target_rps"`
	Concurrency int           `json:"concurrency"`
}

// Comparison represents a comparison between current and baseline metrics.
type Comparison struct {
	Baseline      *Baseline
	Current       BaselineMetrics
	Differences   map[string]Difference
	OverallStatus ComparisonStatus
	Regressions   []string
	Improvements  []string
}

// Difference captures the delta between baseline and current values.
type Difference struct {
	Metric      string
	BaselineVal float64
	CurrentVal  float64
	DeltaAbs    float64
	DeltaPct    float64
	Status      ComparisonStatus
	Threshold   float64 // Acceptable deviation percentage
}

// ComparisonStatus indicates whether performance is acceptable.
type ComparisonStatus string

const (
	StatusPass       ComparisonStatus = "pass"
	StatusRegression ComparisonStatus = "regression"
	StatusImproved   ComparisonStatus = "improved"
	StatusUnknown    ComparisonStatus = "unknown"
)

// DefaultThresholds for baseline comparisons (percentage deviation allowed).
var DefaultThresholds = map[string]float64{
	"requests_per_sec": 10.0, // Allow 10% RPS decrease
	"avg_latency_ms":   15.0, // Allow 15% latency increase
	"p95_latency_ms":   20.0, // Allow 20% P95 increase
	"p99_latency_ms":   25.0, // Allow 25% P99 increase
	"error_rate":       50.0, // Allow 50% error rate increase (relative)
	"peak_memory_mb":   20.0, // Allow 20% memory increase
}

// BaselineManager handles baseline storage and comparison.
type BaselineManager struct {
	mu         sync.RWMutex
	baselines  map[string]*Baseline
	storePath  string
	thresholds map[string]float64
}

// NewBaselineManager creates a new baseline manager.
func NewBaselineManager(storePath string) *BaselineManager {
	bm := &BaselineManager{
		baselines:  make(map[string]*Baseline),
		storePath:  storePath,
		thresholds: make(map[string]float64),
	}

	// Copy default thresholds
	for k, v := range DefaultThresholds {
		bm.thresholds[k] = v
	}

	return bm
}

// SetThreshold sets a custom threshold for a metric.
func (bm *BaselineManager) SetThreshold(metric string, threshold float64) {
	bm.mu.Lock()
	defer bm.mu.Unlock()
	bm.thresholds[metric] = threshold
}

// CreateBaseline creates a new baseline from a test summary.
func (bm *BaselineManager) CreateBaseline(name, description, env, version string, summary *Summary) *Baseline {
	metrics := BaselineMetrics{
		RequestsPerSec: summary.RequestsPerSec,
		AvgLatencyMs:   float64(summary.AvgLatency.Milliseconds()),
		P50LatencyMs:   float64(summary.P50Latency.Milliseconds()),
		P95LatencyMs:   float64(summary.P95Latency.Milliseconds()),
		P99LatencyMs:   float64(summary.P99Latency.Milliseconds()),
		MaxLatencyMs:   float64(summary.MaxLatency.Milliseconds()),
		ErrorRate:      summary.ErrorRate,
		SuccessRate:    1 - summary.ErrorRate,
		Duration:       summary.EndTime.Sub(summary.StartTime),
	}

	if summary.TotalBytes > 0 && metrics.Duration.Seconds() > 0 {
		metrics.BytesPerSec = float64(summary.TotalBytes) / metrics.Duration.Seconds()
	}

	baseline := &Baseline{
		Name:        name,
		Description: description,
		CreatedAt:   time.Now(),
		Environment: env,
		Version:     version,
		Metrics:     metrics,
	}

	bm.mu.Lock()
	bm.baselines[name] = baseline
	bm.mu.Unlock()

	return baseline
}

// CreateBaselineFromSoak creates a baseline from soak test results.
func (bm *BaselineManager) CreateBaselineFromSoak(name, description, env, version string, result *SoakResult) *Baseline {
	baseline := bm.CreateBaseline(name, description, env, version, result.Summary)

	// Add soak-specific metrics
	baseline.Metrics.PeakMemoryMB = float64(result.PeakMemory) / (1024 * 1024)
	baseline.Metrics.AvgGoroutines = float64(result.PeakGoroutines) // Simplified

	return baseline
}

// GetBaseline retrieves a baseline by name.
func (bm *BaselineManager) GetBaseline(name string) (*Baseline, bool) {
	bm.mu.RLock()
	defer bm.mu.RUnlock()
	baseline, ok := bm.baselines[name]
	return baseline, ok
}

// ListBaselines returns all baseline names.
func (bm *BaselineManager) ListBaselines() []string {
	bm.mu.RLock()
	defer bm.mu.RUnlock()

	names := make([]string, 0, len(bm.baselines))
	for name := range bm.baselines {
		names = append(names, name)
	}
	return names
}

// Compare compares current metrics against a baseline.
func (bm *BaselineManager) Compare(baselineName string, current *Summary) (*Comparison, error) {
	baseline, ok := bm.GetBaseline(baselineName)
	if !ok {
		return nil, fmt.Errorf("baseline %q not found", baselineName)
	}

	currentMetrics := BaselineMetrics{
		RequestsPerSec: current.RequestsPerSec,
		AvgLatencyMs:   float64(current.AvgLatency.Milliseconds()),
		P50LatencyMs:   float64(current.P50Latency.Milliseconds()),
		P95LatencyMs:   float64(current.P95Latency.Milliseconds()),
		P99LatencyMs:   float64(current.P99Latency.Milliseconds()),
		MaxLatencyMs:   float64(current.MaxLatency.Milliseconds()),
		ErrorRate:      current.ErrorRate,
		SuccessRate:    1 - current.ErrorRate,
		Duration:       current.EndTime.Sub(current.StartTime),
	}

	return bm.compareMetrics(baseline, currentMetrics), nil
}

// compareMetrics performs the actual comparison.
func (bm *BaselineManager) compareMetrics(baseline *Baseline, current BaselineMetrics) *Comparison {
	bm.mu.RLock()
	thresholds := make(map[string]float64)
	for k, v := range bm.thresholds {
		thresholds[k] = v
	}
	bm.mu.RUnlock()

	comparison := &Comparison{
		Baseline:    baseline,
		Current:     current,
		Differences: make(map[string]Difference),
	}

	// Compare each metric
	// For throughput metrics, regression means decrease
	comparison.Differences["requests_per_sec"] = bm.compareThroughput(
		"requests_per_sec",
		baseline.Metrics.RequestsPerSec,
		current.RequestsPerSec,
		thresholds["requests_per_sec"],
	)

	// For latency metrics, regression means increase
	comparison.Differences["avg_latency_ms"] = bm.compareLatency(
		"avg_latency_ms",
		baseline.Metrics.AvgLatencyMs,
		current.AvgLatencyMs,
		thresholds["avg_latency_ms"],
	)

	comparison.Differences["p95_latency_ms"] = bm.compareLatency(
		"p95_latency_ms",
		baseline.Metrics.P95LatencyMs,
		current.P95LatencyMs,
		thresholds["p95_latency_ms"],
	)

	comparison.Differences["p99_latency_ms"] = bm.compareLatency(
		"p99_latency_ms",
		baseline.Metrics.P99LatencyMs,
		current.P99LatencyMs,
		thresholds["p99_latency_ms"],
	)

	// For error rate, regression means increase
	comparison.Differences["error_rate"] = bm.compareErrorRate(
		"error_rate",
		baseline.Metrics.ErrorRate,
		current.ErrorRate,
		thresholds["error_rate"],
	)

	// Determine overall status and collect regressions/improvements
	hasRegression := false
	for metric, diff := range comparison.Differences {
		switch diff.Status {
		case StatusRegression:
			hasRegression = true
			comparison.Regressions = append(comparison.Regressions, metric)
		case StatusImproved:
			comparison.Improvements = append(comparison.Improvements, metric)
		}
	}

	if hasRegression {
		comparison.OverallStatus = StatusRegression
	} else {
		comparison.OverallStatus = StatusPass
	}

	return comparison
}

// compareThroughput compares throughput metrics (higher is better).
func (bm *BaselineManager) compareThroughput(metric string, baseline, current, threshold float64) Difference {
	diff := Difference{
		Metric:      metric,
		BaselineVal: baseline,
		CurrentVal:  current,
		DeltaAbs:    current - baseline,
		Threshold:   threshold,
	}

	if baseline > 0 {
		diff.DeltaPct = (current - baseline) / baseline * 100
	}

	// For throughput, negative delta is regression
	if diff.DeltaPct < -threshold {
		diff.Status = StatusRegression
	} else if diff.DeltaPct > threshold {
		diff.Status = StatusImproved
	} else {
		diff.Status = StatusPass
	}

	return diff
}

// compareLatency compares latency metrics (lower is better).
func (bm *BaselineManager) compareLatency(metric string, baseline, current, threshold float64) Difference {
	diff := Difference{
		Metric:      metric,
		BaselineVal: baseline,
		CurrentVal:  current,
		DeltaAbs:    current - baseline,
		Threshold:   threshold,
	}

	if baseline > 0 {
		diff.DeltaPct = (current - baseline) / baseline * 100
	}

	// For latency, positive delta is regression
	if diff.DeltaPct > threshold {
		diff.Status = StatusRegression
	} else if diff.DeltaPct < -threshold {
		diff.Status = StatusImproved
	} else {
		diff.Status = StatusPass
	}

	return diff
}

// compareErrorRate compares error rates (lower is better).
func (bm *BaselineManager) compareErrorRate(metric string, baseline, current, threshold float64) Difference {
	diff := Difference{
		Metric:      metric,
		BaselineVal: baseline,
		CurrentVal:  current,
		DeltaAbs:    current - baseline,
		Threshold:   threshold,
	}

	// Handle zero baseline specially
	if baseline == 0 {
		if current == 0 {
			diff.Status = StatusPass
		} else {
			diff.Status = StatusRegression
		}
		diff.DeltaPct = 0
		return diff
	}

	diff.DeltaPct = (current - baseline) / baseline * 100

	// For error rate, positive delta is regression
	if diff.DeltaPct > threshold {
		diff.Status = StatusRegression
	} else if diff.DeltaPct < -threshold {
		diff.Status = StatusImproved
	} else {
		diff.Status = StatusPass
	}

	return diff
}

// SaveToFile persists a baseline to disk.
func (bm *BaselineManager) SaveToFile(name string) error {
	baseline, ok := bm.GetBaseline(name)
	if !ok {
		return fmt.Errorf("baseline %q not found", name)
	}

	if bm.storePath == "" {
		return fmt.Errorf("no store path configured")
	}

	// Ensure directory exists
	if err := os.MkdirAll(bm.storePath, 0755); err != nil {
		return fmt.Errorf("failed to create directory: %w", err)
	}

	filename := filepath.Join(bm.storePath, name+".json")
	data, err := json.MarshalIndent(baseline, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal baseline: %w", err)
	}

	if err := os.WriteFile(filename, data, 0644); err != nil {
		return fmt.Errorf("failed to write file: %w", err)
	}

	return nil
}

// LoadFromFile loads a baseline from disk.
func (bm *BaselineManager) LoadFromFile(name string) error {
	if bm.storePath == "" {
		return fmt.Errorf("no store path configured")
	}

	filename := filepath.Join(bm.storePath, name+".json")
	data, err := os.ReadFile(filename)
	if err != nil {
		return fmt.Errorf("failed to read file: %w", err)
	}

	var baseline Baseline
	if err := json.Unmarshal(data, &baseline); err != nil {
		return fmt.Errorf("failed to unmarshal baseline: %w", err)
	}

	bm.mu.Lock()
	bm.baselines[name] = &baseline
	bm.mu.Unlock()

	return nil
}

// DeleteBaseline removes a baseline.
func (bm *BaselineManager) DeleteBaseline(name string) bool {
	bm.mu.Lock()
	defer bm.mu.Unlock()

	if _, ok := bm.baselines[name]; ok {
		delete(bm.baselines, name)
		return true
	}
	return false
}

// GenerateReport creates a human-readable comparison report.
func (c *Comparison) GenerateReport() string {
	var report string

	report += "Performance Comparison Report\n"
	report += "=============================\n\n"
	report += fmt.Sprintf("Baseline: %s\n", c.Baseline.Name)
	report += fmt.Sprintf("Created: %s\n", c.Baseline.CreatedAt.Format(time.RFC3339))
	report += fmt.Sprintf("Environment: %s\n", c.Baseline.Environment)
	report += fmt.Sprintf("Version: %s\n\n", c.Baseline.Version)

	report += fmt.Sprintf("Overall Status: %s\n\n", c.OverallStatus)

	report += "Metric Comparison:\n"
	report += "-----------------\n"

	for metric, diff := range c.Differences {
		statusIcon := "✓"
		switch diff.Status {
		case StatusRegression:
			statusIcon = "✗"
		case StatusImproved:
			statusIcon = "↑"
		}

		report += fmt.Sprintf("%s %s: %.2f → %.2f (%.1f%%) [threshold: %.1f%%]\n",
			statusIcon, metric, diff.BaselineVal, diff.CurrentVal, diff.DeltaPct, diff.Threshold)
	}

	if len(c.Regressions) > 0 {
		report += fmt.Sprintf("\nRegressions: %v\n", c.Regressions)
	}

	if len(c.Improvements) > 0 {
		report += fmt.Sprintf("Improvements: %v\n", c.Improvements)
	}

	return report
}
