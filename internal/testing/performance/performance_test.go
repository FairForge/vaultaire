package performance

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestNewRunner(t *testing.T) {
	runner := NewRunner()
	if runner.results == nil {
		t.Error("expected results to be initialized")
	}
	if len(runner.results) != 0 {
		t.Error("expected empty results")
	}
}

func TestRunner_RunFunc(t *testing.T) {
	runner := NewRunner()

	result := runner.RunFunc(t, "simple-test", 100, func() error {
		time.Sleep(time.Microsecond)
		return nil
	})

	if result.Iterations != 100 {
		t.Errorf("expected 100 iterations, got %d", result.Iterations)
	}
	if result.OpsPerSecond <= 0 {
		t.Error("expected positive ops/sec")
	}
	if result.AvgDuration <= 0 {
		t.Error("expected positive avg duration")
	}
}

func TestRunner_RunFunc_WithErrors(t *testing.T) {
	runner := NewRunner()

	callCount := 0
	result := runner.RunFunc(t, "error-test", 10, func() error {
		callCount++
		if callCount%2 == 0 {
			return errors.New("test error")
		}
		return nil
	})

	if result.Errors != 5 {
		t.Errorf("expected 5 errors, got %d", result.Errors)
	}
}

func TestRunner_Results(t *testing.T) {
	runner := NewRunner()

	runner.RunFunc(t, "test-1", 10, func() error { return nil })
	runner.RunFunc(t, "test-2", 10, func() error { return nil })

	results := runner.Results()
	if len(results) != 2 {
		t.Errorf("expected 2 results, got %d", len(results))
	}
}

func TestRunner_GenerateReport(t *testing.T) {
	runner := NewRunner()

	runner.RunFunc(t, "report-test", 50, func() error {
		time.Sleep(10 * time.Microsecond)
		return nil
	})

	report := runner.GenerateReport()
	if report == "" {
		t.Error("expected non-empty report")
	}
	if len(report) < 100 {
		t.Error("report seems too short")
	}

	t.Logf("Report:\n%s", report)
}

func TestPercentile(t *testing.T) {
	durations := []time.Duration{
		1 * time.Millisecond,
		2 * time.Millisecond,
		3 * time.Millisecond,
		4 * time.Millisecond,
		5 * time.Millisecond,
		6 * time.Millisecond,
		7 * time.Millisecond,
		8 * time.Millisecond,
		9 * time.Millisecond,
		10 * time.Millisecond,
	}

	p50 := percentile(durations, 50)
	// 50% of 10 = index 5 = 6ms
	if p50 != 6*time.Millisecond {
		t.Errorf("expected p50=6ms, got %v", p50)
	}

	p99 := percentile(durations, 99)
	if p99 != 10*time.Millisecond {
		t.Errorf("expected p99=10ms, got %v", p99)
	}
}

func TestProfiler(t *testing.T) {
	profiler := NewProfiler("test-profile")

	profiler.Start()

	// Do some work
	data := make([]byte, 1024*1024) // 1MB allocation
	_ = data

	profiler.TakeSample()
	time.Sleep(10 * time.Millisecond)
	profiler.TakeSample()

	summary := profiler.Stop()

	if summary.Name != "test-profile" {
		t.Errorf("expected name 'test-profile', got %q", summary.Name)
	}
	if summary.NumSamples < 3 {
		t.Errorf("expected at least 3 samples, got %d", summary.NumSamples)
	}
	if summary.Duration < 10*time.Millisecond {
		t.Errorf("expected duration >= 10ms, got %v", summary.Duration)
	}

	t.Logf("Profile: duration=%v, samples=%d, peakHeap=%d",
		summary.Duration, summary.NumSamples, summary.PeakHeap)
}

func TestProfiler_Samples(t *testing.T) {
	profiler := NewProfiler("samples-test")
	profiler.Start()
	profiler.TakeSample()
	profiler.TakeSample()

	samples := profiler.Samples()
	if len(samples) < 3 {
		t.Errorf("expected at least 3 samples, got %d", len(samples))
	}

	for _, s := range samples {
		if s.Timestamp.IsZero() {
			t.Error("expected non-zero timestamp")
		}
		if s.Goroutines <= 0 {
			t.Error("expected positive goroutine count")
		}
	}
}

func TestTimer(t *testing.T) {
	timer := NewTimer()

	time.Sleep(10 * time.Millisecond)
	lap1 := timer.Lap()

	time.Sleep(10 * time.Millisecond)
	lap2 := timer.Lap()

	if lap1 < 10*time.Millisecond {
		t.Errorf("expected lap1 >= 10ms, got %v", lap1)
	}
	if lap2 < 20*time.Millisecond {
		t.Errorf("expected lap2 >= 20ms, got %v", lap2)
	}

	laps := timer.Laps()
	if len(laps) != 2 {
		t.Errorf("expected 2 laps, got %d", len(laps))
	}

	elapsed := timer.Elapsed()
	if elapsed < 20*time.Millisecond {
		t.Errorf("expected elapsed >= 20ms, got %v", elapsed)
	}
}

func TestTimer_Reset(t *testing.T) {
	timer := NewTimer()
	timer.Lap()
	timer.Reset()

	if len(timer.Laps()) != 0 {
		t.Error("expected empty laps after reset")
	}
	if timer.Elapsed() > time.Millisecond {
		t.Error("expected minimal elapsed time after reset")
	}
}

func TestMeasure(t *testing.T) {
	duration := Measure(func() {
		time.Sleep(10 * time.Millisecond)
	})

	if duration < 10*time.Millisecond {
		t.Errorf("expected duration >= 10ms, got %v", duration)
	}
}

func TestMeasureWithResult(t *testing.T) {
	result, duration := MeasureWithResult(func() int {
		time.Sleep(10 * time.Millisecond)
		return 42
	})

	if result != 42 {
		t.Errorf("expected result 42, got %d", result)
	}
	if duration < 10*time.Millisecond {
		t.Errorf("expected duration >= 10ms, got %v", duration)
	}
}

func TestThroughput(t *testing.T) {
	ops := Throughput(1000, time.Second)
	if ops != 1000 {
		t.Errorf("expected 1000 ops/s, got %.2f", ops)
	}

	ops = Throughput(100, 100*time.Millisecond)
	if ops != 1000 {
		t.Errorf("expected 1000 ops/s, got %.2f", ops)
	}

	ops = Throughput(100, 0)
	if ops != 0 {
		t.Error("expected 0 for zero duration")
	}
}

func TestMemoryUsage(t *testing.T) {
	heapAlloc, heapSys := MemoryUsage()

	if heapAlloc == 0 {
		t.Error("expected non-zero heap alloc")
	}
	if heapSys == 0 {
		t.Error("expected non-zero heap sys")
	}

	t.Logf("Memory: alloc=%d, sys=%d", heapAlloc, heapSys)
}

func TestAssertPerformance(t *testing.T) {
	result := Result{
		Name:         "test",
		P99Duration:  50 * time.Millisecond,
		OpsPerSecond: 1000,
	}

	// Should pass
	AssertPerformance(t, result, 100*time.Millisecond, 500)
}

func TestCompareResults(t *testing.T) {
	baseline := Result{
		Name:         "baseline",
		AvgDuration:  100 * time.Millisecond,
		P99Duration:  200 * time.Millisecond,
		OpsPerSecond: 100,
	}

	// Improved result
	improved := Result{
		Name:         "improved",
		AvgDuration:  80 * time.Millisecond,
		P99Duration:  150 * time.Millisecond,
		OpsPerSecond: 120,
	}

	comp := CompareResults(baseline, improved)

	if !comp.Improved {
		t.Error("expected improved=true")
	}
	if comp.Regressed {
		t.Error("expected regressed=false")
	}
	if comp.AvgLatencyChange >= 0 {
		t.Error("expected negative latency change (improvement)")
	}
	if comp.ThroughputChange <= 0 {
		t.Error("expected positive throughput change")
	}

	t.Logf("Comparison: latency=%.1f%%, throughput=%.1f%%",
		comp.AvgLatencyChange, comp.ThroughputChange)
}

func TestCompareResults_Regression(t *testing.T) {
	baseline := Result{
		Name:         "baseline",
		AvgDuration:  100 * time.Millisecond,
		OpsPerSecond: 100,
	}

	regressed := Result{
		Name:         "regressed",
		AvgDuration:  150 * time.Millisecond,
		OpsPerSecond: 70,
	}

	comp := CompareResults(baseline, regressed)

	if comp.Improved {
		t.Error("expected improved=false")
	}
	if !comp.Regressed {
		t.Error("expected regressed=true")
	}
}

func TestRunWithTimeout(t *testing.T) {
	ctx := context.Background()

	// Should complete
	err := RunWithTimeout(ctx, time.Second, func() error {
		time.Sleep(10 * time.Millisecond)
		return nil
	})
	if err != nil {
		t.Errorf("expected no error, got %v", err)
	}

	// Should timeout
	err = RunWithTimeout(ctx, 10*time.Millisecond, func() error {
		time.Sleep(100 * time.Millisecond)
		return nil
	})
	if err == nil {
		t.Error("expected timeout error")
	}
}

func TestResult(t *testing.T) {
	result := Result{
		Name:          "test",
		Iterations:    1000,
		TotalDuration: time.Second,
		MinDuration:   time.Millisecond,
		MaxDuration:   100 * time.Millisecond,
		AvgDuration:   10 * time.Millisecond,
		P50Duration:   8 * time.Millisecond,
		P95Duration:   50 * time.Millisecond,
		P99Duration:   90 * time.Millisecond,
		OpsPerSecond:  1000,
		Allocations:   5000,
		AllocBytes:    1024 * 1024,
		Errors:        0,
	}

	if result.Name != "test" {
		t.Error("Name not set")
	}
	if result.OpsPerSecond != 1000 {
		t.Error("OpsPerSecond not set")
	}
}

func TestProfileSummary(t *testing.T) {
	summary := ProfileSummary{
		Name:            "test",
		Duration:        time.Second,
		NumSamples:      10,
		StartHeap:       1000,
		EndHeap:         2000,
		PeakHeap:        2500,
		HeapGrowth:      1000,
		StartGoroutines: 5,
		EndGoroutines:   6,
		PeakGoroutines:  8,
	}

	if summary.HeapGrowth != 1000 {
		t.Error("HeapGrowth not set")
	}
	if summary.PeakGoroutines != 8 {
		t.Error("PeakGoroutines not set")
	}
}

func TestSample(t *testing.T) {
	sample := Sample{
		Timestamp:   time.Now(),
		HeapAlloc:   1024,
		HeapObjects: 100,
		Goroutines:  5,
		Custom:      map[string]float64{"metric": 1.5},
	}

	if sample.HeapAlloc != 1024 {
		t.Error("HeapAlloc not set")
	}
	if sample.Custom["metric"] != 1.5 {
		t.Error("Custom metric not set")
	}
}

func TestComparison(t *testing.T) {
	comp := Comparison{
		BaselineName:     "v1",
		CurrentName:      "v2",
		AvgLatencyChange: -15.0,
		P99LatencyChange: -10.0,
		ThroughputChange: 20.0,
		Improved:         true,
		Regressed:        false,
	}

	if comp.BaselineName != "v1" {
		t.Error("BaselineName not set")
	}
	if !comp.Improved {
		t.Error("Improved not set")
	}
}
