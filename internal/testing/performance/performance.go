// Package performance provides utilities for performance testing.
package performance

import (
	"context"
	"fmt"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Benchmark represents a performance benchmark.
type Benchmark struct {
	Name        string
	Iterations  int
	Concurrency int
	WarmupRuns  int
	Timeout     time.Duration
	Setup       func() error
	Teardown    func() error
}

// Result contains benchmark results.
type Result struct {
	Name          string
	Iterations    int
	TotalDuration time.Duration
	MinDuration   time.Duration
	MaxDuration   time.Duration
	AvgDuration   time.Duration
	P50Duration   time.Duration
	P95Duration   time.Duration
	P99Duration   time.Duration
	OpsPerSecond  float64
	Allocations   int64
	AllocBytes    int64
	Errors        int
}

// Runner executes performance benchmarks.
type Runner struct {
	results []Result
	mu      sync.Mutex
}

// NewRunner creates a performance test runner.
func NewRunner() *Runner {
	return &Runner{
		results: make([]Result, 0),
	}
}

// Run executes a benchmark.
func (r *Runner) Run(b *testing.B, bench Benchmark, fn func() error) Result {
	if bench.Iterations == 0 {
		bench.Iterations = b.N
	}
	if bench.Concurrency == 0 {
		bench.Concurrency = 1
	}

	// Setup
	if bench.Setup != nil {
		if err := bench.Setup(); err != nil {
			b.Fatalf("Setup failed: %v", err)
		}
	}

	// Teardown
	defer func() {
		if bench.Teardown != nil {
			if err := bench.Teardown(); err != nil {
				b.Logf("Teardown failed: %v", err)
			}
		}
	}()

	// Warmup
	for i := 0; i < bench.WarmupRuns; i++ {
		_ = fn()
	}

	// Run benchmark
	durations := make([]time.Duration, 0, bench.Iterations)
	var errors int64
	var mu sync.Mutex

	var memStatsBefore, memStatsAfter runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&memStatsBefore)

	b.ResetTimer()
	start := time.Now()

	if bench.Concurrency == 1 {
		// Sequential execution
		for i := 0; i < bench.Iterations; i++ {
			iterStart := time.Now()
			if err := fn(); err != nil {
				atomic.AddInt64(&errors, 1)
			}
			mu.Lock()
			durations = append(durations, time.Since(iterStart))
			mu.Unlock()
		}
	} else {
		// Concurrent execution
		var wg sync.WaitGroup
		sem := make(chan struct{}, bench.Concurrency)
		iterCount := int64(0)

		for i := 0; i < bench.Iterations; i++ {
			wg.Add(1)
			sem <- struct{}{}

			go func() {
				defer wg.Done()
				defer func() { <-sem }()

				iterStart := time.Now()
				if err := fn(); err != nil {
					atomic.AddInt64(&errors, 1)
				}
				d := time.Since(iterStart)

				mu.Lock()
				durations = append(durations, d)
				mu.Unlock()

				atomic.AddInt64(&iterCount, 1)
			}()
		}
		wg.Wait()
	}

	totalDuration := time.Since(start)
	b.StopTimer()

	runtime.ReadMemStats(&memStatsAfter)

	// Calculate statistics
	result := r.calculateStats(bench.Name, durations, totalDuration, int(errors))
	result.Allocations = int64(memStatsAfter.Mallocs - memStatsBefore.Mallocs)
	result.AllocBytes = int64(memStatsAfter.TotalAlloc - memStatsBefore.TotalAlloc)

	r.mu.Lock()
	r.results = append(r.results, result)
	r.mu.Unlock()

	return result
}

// RunFunc executes a simple benchmark function.
func (r *Runner) RunFunc(t *testing.T, name string, iterations int, fn func() error) Result {
	durations := make([]time.Duration, 0, iterations)
	var errors int

	var memStatsBefore, memStatsAfter runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&memStatsBefore)

	start := time.Now()

	for i := 0; i < iterations; i++ {
		iterStart := time.Now()
		if err := fn(); err != nil {
			errors++
		}
		durations = append(durations, time.Since(iterStart))
	}

	totalDuration := time.Since(start)

	runtime.ReadMemStats(&memStatsAfter)

	result := r.calculateStats(name, durations, totalDuration, errors)
	result.Allocations = int64(memStatsAfter.Mallocs - memStatsBefore.Mallocs)
	result.AllocBytes = int64(memStatsAfter.TotalAlloc - memStatsBefore.TotalAlloc)

	r.mu.Lock()
	r.results = append(r.results, result)
	r.mu.Unlock()

	t.Logf("%s: %d iterations, avg=%v, p99=%v, ops/s=%.2f",
		name, iterations, result.AvgDuration, result.P99Duration, result.OpsPerSecond)

	return result
}

// calculateStats computes statistics from durations.
func (r *Runner) calculateStats(name string, durations []time.Duration, total time.Duration, errors int) Result {
	if len(durations) == 0 {
		return Result{Name: name}
	}

	// Sort for percentiles
	sorted := make([]time.Duration, len(durations))
	copy(sorted, durations)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i] < sorted[j]
	})

	var sum time.Duration
	for _, d := range sorted {
		sum += d
	}

	result := Result{
		Name:          name,
		Iterations:    len(durations),
		TotalDuration: total,
		MinDuration:   sorted[0],
		MaxDuration:   sorted[len(sorted)-1],
		AvgDuration:   sum / time.Duration(len(sorted)),
		P50Duration:   percentile(sorted, 50),
		P95Duration:   percentile(sorted, 95),
		P99Duration:   percentile(sorted, 99),
		Errors:        errors,
	}

	if total > 0 {
		result.OpsPerSecond = float64(len(durations)) / total.Seconds()
	}

	return result
}

// percentile calculates the nth percentile.
func percentile(sorted []time.Duration, n int) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	idx := (n * len(sorted)) / 100
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

// Results returns all benchmark results.
func (r *Runner) Results() []Result {
	r.mu.Lock()
	defer r.mu.Unlock()

	results := make([]Result, len(r.results))
	copy(results, r.results)
	return results
}

// GenerateReport creates a formatted performance report.
func (r *Runner) GenerateReport() string {
	r.mu.Lock()
	defer r.mu.Unlock()

	report := "Performance Test Report\n"
	report += "=======================\n\n"

	for _, result := range r.results {
		report += fmt.Sprintf("Benchmark: %s\n", result.Name)
		report += fmt.Sprintf("  Iterations:   %d\n", result.Iterations)
		report += fmt.Sprintf("  Total Time:   %v\n", result.TotalDuration)
		report += fmt.Sprintf("  Ops/Second:   %.2f\n", result.OpsPerSecond)
		report += "  Latency:\n"
		report += fmt.Sprintf("    Min:  %v\n", result.MinDuration)
		report += fmt.Sprintf("    Avg:  %v\n", result.AvgDuration)
		report += fmt.Sprintf("    P50:  %v\n", result.P50Duration)
		report += fmt.Sprintf("    P95:  %v\n", result.P95Duration)
		report += fmt.Sprintf("    P99:  %v\n", result.P99Duration)
		report += fmt.Sprintf("    Max:  %v\n", result.MaxDuration)
		report += "  Memory:\n"
		report += fmt.Sprintf("    Allocations: %d\n", result.Allocations)
		report += fmt.Sprintf("    Bytes:       %d\n", result.AllocBytes)
		if result.Errors > 0 {
			report += fmt.Sprintf("  Errors: %d\n", result.Errors)
		}
		report += "\n"
	}

	return report
}

// Profiler tracks performance metrics during execution.
type Profiler struct {
	name      string
	startTime time.Time
	samples   []Sample
	mu        sync.Mutex
}

// Sample represents a point-in-time measurement.
type Sample struct {
	Timestamp   time.Time
	HeapAlloc   uint64
	HeapObjects uint64
	Goroutines  int
	Custom      map[string]float64
}

// NewProfiler creates a new profiler.
func NewProfiler(name string) *Profiler {
	return &Profiler{
		name:    name,
		samples: make([]Sample, 0),
	}
}

// Start begins profiling.
func (p *Profiler) Start() {
	p.startTime = time.Now()
	p.takeSample()
}

// TakeSample records current metrics.
func (p *Profiler) TakeSample() {
	p.takeSample()
}

func (p *Profiler) takeSample() {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)

	sample := Sample{
		Timestamp:   time.Now(),
		HeapAlloc:   m.HeapAlloc,
		HeapObjects: m.HeapObjects,
		Goroutines:  runtime.NumGoroutine(),
		Custom:      make(map[string]float64),
	}

	p.mu.Lock()
	p.samples = append(p.samples, sample)
	p.mu.Unlock()
}

// Stop ends profiling and returns summary.
func (p *Profiler) Stop() ProfileSummary {
	p.takeSample()

	p.mu.Lock()
	defer p.mu.Unlock()

	summary := ProfileSummary{
		Name:       p.name,
		Duration:   time.Since(p.startTime),
		NumSamples: len(p.samples),
	}

	if len(p.samples) > 0 {
		first := p.samples[0]
		last := p.samples[len(p.samples)-1]

		summary.StartHeap = first.HeapAlloc
		summary.EndHeap = last.HeapAlloc
		summary.HeapGrowth = int64(last.HeapAlloc) - int64(first.HeapAlloc)

		summary.StartGoroutines = first.Goroutines
		summary.EndGoroutines = last.Goroutines

		// Find peaks
		for _, s := range p.samples {
			if s.HeapAlloc > summary.PeakHeap {
				summary.PeakHeap = s.HeapAlloc
			}
			if s.Goroutines > summary.PeakGoroutines {
				summary.PeakGoroutines = s.Goroutines
			}
		}
	}

	return summary
}

// Samples returns all collected samples.
func (p *Profiler) Samples() []Sample {
	p.mu.Lock()
	defer p.mu.Unlock()

	samples := make([]Sample, len(p.samples))
	copy(samples, p.samples)
	return samples
}

// ProfileSummary contains profiling results.
type ProfileSummary struct {
	Name            string
	Duration        time.Duration
	NumSamples      int
	StartHeap       uint64
	EndHeap         uint64
	PeakHeap        uint64
	HeapGrowth      int64
	StartGoroutines int
	EndGoroutines   int
	PeakGoroutines  int
}

// Timer provides simple timing utilities.
type Timer struct {
	start time.Time
	laps  []time.Duration
}

// NewTimer creates a new timer.
func NewTimer() *Timer {
	return &Timer{
		start: time.Now(),
		laps:  make([]time.Duration, 0),
	}
}

// Lap records a lap time.
func (t *Timer) Lap() time.Duration {
	lap := time.Since(t.start)
	t.laps = append(t.laps, lap)
	return lap
}

// Reset resets the timer.
func (t *Timer) Reset() {
	t.start = time.Now()
	t.laps = t.laps[:0]
}

// Elapsed returns time since start.
func (t *Timer) Elapsed() time.Duration {
	return time.Since(t.start)
}

// Laps returns all recorded laps.
func (t *Timer) Laps() []time.Duration {
	laps := make([]time.Duration, len(t.laps))
	copy(laps, t.laps)
	return laps
}

// Measure times a function execution.
func Measure(fn func()) time.Duration {
	start := time.Now()
	fn()
	return time.Since(start)
}

// MeasureWithResult times a function and returns its result.
func MeasureWithResult[T any](fn func() T) (T, time.Duration) {
	start := time.Now()
	result := fn()
	return result, time.Since(start)
}

// Throughput calculates operations per second.
func Throughput(operations int, duration time.Duration) float64 {
	if duration == 0 {
		return 0
	}
	return float64(operations) / duration.Seconds()
}

// MemoryUsage returns current memory usage.
func MemoryUsage() (heapAlloc, heapSys uint64) {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.HeapAlloc, m.HeapSys
}

// AssertPerformance fails the test if performance requirements aren't met.
func AssertPerformance(t *testing.T, result Result, maxP99 time.Duration, minOpsPerSec float64) {
	t.Helper()

	if result.P99Duration > maxP99 {
		t.Errorf("%s: P99 latency %v exceeds maximum %v", result.Name, result.P99Duration, maxP99)
	}

	if result.OpsPerSecond < minOpsPerSec {
		t.Errorf("%s: throughput %.2f ops/s below minimum %.2f", result.Name, result.OpsPerSecond, minOpsPerSec)
	}
}

// CompareResults compares two benchmark results.
func CompareResults(baseline, current Result) Comparison {
	comp := Comparison{
		BaselineName: baseline.Name,
		CurrentName:  current.Name,
	}

	if baseline.AvgDuration > 0 {
		comp.AvgLatencyChange = float64(current.AvgDuration-baseline.AvgDuration) / float64(baseline.AvgDuration) * 100
	}
	if baseline.P99Duration > 0 {
		comp.P99LatencyChange = float64(current.P99Duration-baseline.P99Duration) / float64(baseline.P99Duration) * 100
	}
	if baseline.OpsPerSecond > 0 {
		comp.ThroughputChange = (current.OpsPerSecond - baseline.OpsPerSecond) / baseline.OpsPerSecond * 100
	}

	// Negative latency change is good, positive throughput change is good
	comp.Improved = comp.AvgLatencyChange < -5 || comp.ThroughputChange > 5
	comp.Regressed = comp.AvgLatencyChange > 10 || comp.ThroughputChange < -10

	return comp
}

// Comparison holds comparison between two results.
type Comparison struct {
	BaselineName     string
	CurrentName      string
	AvgLatencyChange float64 // Percentage change
	P99LatencyChange float64
	ThroughputChange float64
	Improved         bool
	Regressed        bool
}

// RunWithTimeout executes fn with a timeout.
func RunWithTimeout(ctx context.Context, timeout time.Duration, fn func() error) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- fn()
	}()

	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}
