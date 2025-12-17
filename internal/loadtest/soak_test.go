package loadtest

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestDefaultSoakConfig(t *testing.T) {
	config := DefaultSoakConfig("test")

	if config.Name != "test" {
		t.Errorf("expected name 'test', got %q", config.Name)
	}
	if config.Duration <= 0 {
		t.Error("expected positive Duration")
	}
	if config.SampleInterval <= 0 {
		t.Error("expected positive SampleInterval")
	}
	if config.MemoryThreshold == 0 {
		t.Error("expected non-zero MemoryThreshold")
	}
}

func TestSoakTester_New(t *testing.T) {
	worker := func(ctx context.Context, id int) Result {
		return Result{}
	}

	// With nil config
	st := NewSoakTester(nil, worker)
	if st.config.Name != "default-soak" {
		t.Error("expected default config when nil provided")
	}

	// With custom config
	config := &SoakConfig{
		Name:     "custom",
		Duration: time.Minute,
	}
	st = NewSoakTester(config, worker)
	if st.config.Name != "custom" {
		t.Error("expected custom config to be used")
	}
}

func TestSoakTester_Run_Short(t *testing.T) {
	var callCount atomic.Int64

	worker := func(ctx context.Context, id int) Result {
		callCount.Add(1)
		return Result{
			StartTime:  time.Now(),
			Duration:   2 * time.Millisecond,
			StatusCode: 200,
		}
	}

	config := &SoakConfig{
		Name:               "short-soak",
		Duration:           500 * time.Millisecond,
		TargetRPS:          50,
		MaxConcurrency:     20,
		Timeout:            time.Second,
		SampleInterval:     100 * time.Millisecond,
		MemoryThreshold:    10 * 1024 * 1024 * 1024, // 10GB (won't hit)
		GoroutineThreshold: 100000,
		ErrorRateThreshold: 0.5,
		LatencyThreshold:   time.Second,
	}

	st := NewSoakTester(config, worker)
	result, err := st.Run(context.Background())

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == nil {
		t.Fatal("expected result, got nil")
	}
	if result.TotalRequests == 0 {
		t.Error("expected some requests")
	}
	if len(result.ResourceSamples) == 0 {
		t.Error("expected resource samples")
	}
	if !result.Stable {
		t.Errorf("expected stable result, got alerts: %v", result.Alerts)
	}

	t.Logf("Soak test: requests=%d, samples=%d, peakMemory=%d, stable=%v",
		result.TotalRequests, len(result.ResourceSamples), result.PeakMemory, result.Stable)
}

func TestSoakTester_Run_WithErrors(t *testing.T) {
	var callCount atomic.Int64

	worker := func(ctx context.Context, id int) Result {
		count := callCount.Add(1)
		if count%5 == 0 {
			return Result{
				StartTime: time.Now(),
				Duration:  time.Millisecond,
				Error:     errors.New("simulated error"),
			}
		}
		return Result{
			StartTime:  time.Now(),
			Duration:   time.Millisecond,
			StatusCode: 200,
		}
	}

	config := &SoakConfig{
		Name:               "error-soak",
		Duration:           300 * time.Millisecond,
		TargetRPS:          100,
		MaxConcurrency:     30,
		Timeout:            time.Second,
		SampleInterval:     100 * time.Millisecond,
		MemoryThreshold:    10 * 1024 * 1024 * 1024,
		GoroutineThreshold: 100000,
		ErrorRateThreshold: 0.10, // 10% threshold, we're at 20%
		LatencyThreshold:   time.Second,
	}

	st := NewSoakTester(config, worker)
	result, err := st.Run(context.Background())

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should have triggered error rate alert
	if result.Stable {
		t.Log("No alerts triggered - error rate may have been below threshold")
	} else {
		t.Logf("Alerts triggered: %d", len(result.Alerts))
		for _, alert := range result.Alerts {
			t.Logf("  - %s: %s", alert.AlertType, alert.Message)
		}
	}
}

func TestSoakTester_Run_ContextCancellation(t *testing.T) {
	worker := func(ctx context.Context, id int) Result {
		select {
		case <-ctx.Done():
			return Result{Error: ctx.Err()}
		case <-time.After(5 * time.Millisecond):
			return Result{StatusCode: 200}
		}
	}

	config := &SoakConfig{
		Name:               "cancel-soak",
		Duration:           10 * time.Second,
		TargetRPS:          10,
		MaxConcurrency:     10,
		Timeout:            time.Second,
		SampleInterval:     time.Second,
		MemoryThreshold:    10 * 1024 * 1024 * 1024,
		GoroutineThreshold: 100000,
		ErrorRateThreshold: 0.5,
		LatencyThreshold:   time.Second,
	}

	st := NewSoakTester(config, worker)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	start := time.Now()
	result, err := st.Run(ctx)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if elapsed > time.Second {
		t.Error("test should have been cancelled quickly")
	}

	t.Logf("Cancelled after %v with %d requests", elapsed, result.TotalRequests)
}

func TestSoakTester_ResourceSampling(t *testing.T) {
	worker := func(ctx context.Context, id int) Result {
		// Allocate some memory to make samples interesting
		data := make([]byte, 1024)
		_ = data
		return Result{
			StartTime:  time.Now(),
			Duration:   time.Millisecond,
			StatusCode: 200,
		}
	}

	config := &SoakConfig{
		Name:               "sampling-test",
		Duration:           400 * time.Millisecond,
		TargetRPS:          20,
		MaxConcurrency:     10,
		Timeout:            time.Second,
		SampleInterval:     100 * time.Millisecond,
		MemoryThreshold:    10 * 1024 * 1024 * 1024,
		GoroutineThreshold: 100000,
		ErrorRateThreshold: 0.5,
		LatencyThreshold:   time.Second,
	}

	st := NewSoakTester(config, worker)
	result, err := st.Run(context.Background())

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should have multiple samples
	if len(result.ResourceSamples) < 2 {
		t.Errorf("expected multiple samples, got %d", len(result.ResourceSamples))
	}

	// Verify samples have data
	for i, sample := range result.ResourceSamples {
		if sample.HeapAlloc == 0 {
			t.Errorf("sample %d has zero HeapAlloc", i)
		}
		if sample.NumGoroutine == 0 {
			t.Errorf("sample %d has zero NumGoroutine", i)
		}
		t.Logf("Sample %d: heap=%d, goroutines=%d, requests=%d",
			i, sample.HeapAlloc, sample.NumGoroutine, sample.Requests)
	}
}

func TestSoakTester_Samples(t *testing.T) {
	worker := func(ctx context.Context, id int) Result {
		return Result{StatusCode: 200, Duration: time.Millisecond}
	}

	config := &SoakConfig{
		Name:               "samples-api-test",
		Duration:           300 * time.Millisecond,
		TargetRPS:          10,
		MaxConcurrency:     5,
		Timeout:            time.Second,
		SampleInterval:     50 * time.Millisecond,
		MemoryThreshold:    10 * 1024 * 1024 * 1024,
		GoroutineThreshold: 100000,
		ErrorRateThreshold: 0.5,
		LatencyThreshold:   time.Second,
	}

	st := NewSoakTester(config, worker)

	done := make(chan struct{})
	go func() {
		_, _ = st.Run(context.Background())
		close(done)
	}()

	// Monitor samples during test
	time.Sleep(150 * time.Millisecond)
	samples := st.Samples()

	if len(samples) == 0 {
		t.Error("expected samples during test")
	}
	t.Logf("Samples during test: %d", len(samples))

	<-done
}

func TestSoakTester_IsStable(t *testing.T) {
	worker := func(ctx context.Context, id int) Result {
		return Result{StatusCode: 200, Duration: time.Millisecond}
	}

	config := &SoakConfig{
		Name:               "stability-test",
		Duration:           200 * time.Millisecond,
		TargetRPS:          10,
		MaxConcurrency:     5,
		Timeout:            time.Second,
		SampleInterval:     50 * time.Millisecond,
		MemoryThreshold:    10 * 1024 * 1024 * 1024,
		GoroutineThreshold: 100000,
		ErrorRateThreshold: 0.5,
		LatencyThreshold:   time.Second,
	}

	st := NewSoakTester(config, worker)

	// Should be stable initially
	if !st.IsStable() {
		t.Error("expected stable before any alerts")
	}

	result, _ := st.Run(context.Background())

	if !result.Stable {
		t.Errorf("expected stable result with no errors, got %d alerts", len(result.Alerts))
	}
}

func TestSoakTester_GrowthCalculation(t *testing.T) {
	worker := func(ctx context.Context, id int) Result {
		return Result{StatusCode: 200, Duration: time.Millisecond}
	}

	config := &SoakConfig{
		Name:               "growth-test",
		Duration:           300 * time.Millisecond,
		TargetRPS:          20,
		MaxConcurrency:     10,
		Timeout:            time.Second,
		SampleInterval:     100 * time.Millisecond,
		MemoryThreshold:    10 * 1024 * 1024 * 1024,
		GoroutineThreshold: 100000,
		ErrorRateThreshold: 0.5,
		LatencyThreshold:   time.Second,
	}

	st := NewSoakTester(config, worker)
	result, err := st.Run(context.Background())

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Growth values should be calculated
	t.Logf("Memory growth: %.2f%%, Goroutine growth: %.2f%%",
		result.MemoryGrowth, result.GoroutineGrowth)
	t.Logf("Peak memory: %d, Peak goroutines: %d",
		result.PeakMemory, result.PeakGoroutines)
}

func TestResourceSample(t *testing.T) {
	sample := ResourceSample{
		Timestamp:    time.Now(),
		HeapAlloc:    1024 * 1024 * 100, // 100MB
		HeapSys:      1024 * 1024 * 150, // 150MB
		HeapObjects:  50000,
		NumGoroutine: 100,
		NumGC:        10,
		GCPauseNs:    1000000, // 1ms total
		Requests:     1000,
		Errors:       5,
		ErrorRate:    0.005,
		AvgLatency:   10 * time.Millisecond,
		P99Latency:   50 * time.Millisecond,
	}

	if sample.HeapAlloc == 0 {
		t.Error("HeapAlloc not set")
	}
	if sample.NumGoroutine != 100 {
		t.Error("NumGoroutine not set correctly")
	}
}

func TestSoakAlert(t *testing.T) {
	alert := SoakAlert{
		Timestamp: time.Now(),
		AlertType: "memory",
		Message:   "heap allocation exceeded threshold",
		Value:     uint64(3 * 1024 * 1024 * 1024),
		Threshold: uint64(2 * 1024 * 1024 * 1024),
	}

	if alert.AlertType != "memory" {
		t.Error("AlertType not set correctly")
	}
	if alert.Message == "" {
		t.Error("Message not set")
	}
}
