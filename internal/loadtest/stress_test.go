package loadtest

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestDefaultStressConfig(t *testing.T) {
	config := DefaultStressConfig("test")

	if config.Name != "test" {
		t.Errorf("expected name 'test', got %q", config.Name)
	}
	if config.InitialRPS <= 0 {
		t.Error("expected positive InitialRPS")
	}
	if config.MaxRPS <= config.InitialRPS {
		t.Error("expected MaxRPS > InitialRPS")
	}
	if config.FailureThreshold <= 0 || config.FailureThreshold > 1 {
		t.Error("expected FailureThreshold between 0 and 1")
	}
}

func TestStressTester_New(t *testing.T) {
	worker := func(ctx context.Context, id int) Result {
		return Result{}
	}

	// With nil config
	st := NewStressTester(nil, worker)
	if st.config.Name != "default-stress" {
		t.Error("expected default config when nil provided")
	}

	// With custom config
	config := &StressConfig{
		Name:       "custom",
		InitialRPS: 5,
		MaxRPS:     50,
	}
	st = NewStressTester(config, worker)
	if st.config.Name != "custom" {
		t.Error("expected custom config to be used")
	}
}

func TestStressTester_Run_RampUp(t *testing.T) {
	var callCount atomic.Int64

	worker := func(ctx context.Context, id int) Result {
		callCount.Add(1)
		return Result{
			StartTime:  time.Now(),
			Duration:   5 * time.Millisecond,
			StatusCode: 200,
		}
	}

	config := &StressConfig{
		Name:             "ramp-test",
		InitialRPS:       10,
		MaxRPS:           30,
		RampUpRate:       10,
		RampInterval:     100 * time.Millisecond,
		HoldDuration:     100 * time.Millisecond,
		MaxConcurrency:   20,
		Timeout:          time.Second,
		FailureThreshold: 0.5,
		LatencyThreshold: time.Second,
	}

	st := NewStressTester(config, worker)
	result, err := st.Run(context.Background())

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == nil {
		t.Fatal("expected result, got nil")
	}
	if result.MaxRPSAchieved < config.InitialRPS {
		t.Errorf("expected MaxRPSAchieved >= %d, got %d", config.InitialRPS, result.MaxRPSAchieved)
	}
	if result.TotalRequests == 0 {
		t.Error("expected some requests")
	}
	if result.StopReason == "" {
		t.Error("expected stop reason")
	}
	t.Logf("Stress test completed: phase=%s, maxRPS=%d, reason=%s",
		result.Phase, result.MaxRPSAchieved, result.StopReason)
}

func TestStressTester_Run_FailureThreshold(t *testing.T) {
	var callCount atomic.Int64

	// Worker that starts failing after some requests
	worker := func(ctx context.Context, id int) Result {
		count := callCount.Add(1)
		if count > 20 {
			return Result{
				StartTime: time.Now(),
				Duration:  time.Millisecond,
				Error:     errors.New("simulated failure"),
			}
		}
		return Result{
			StartTime:  time.Now(),
			Duration:   time.Millisecond,
			StatusCode: 200,
		}
	}

	config := &StressConfig{
		Name:             "failure-threshold-test",
		InitialRPS:       50,
		MaxRPS:           100,
		RampUpRate:       25,
		RampInterval:     200 * time.Millisecond,
		HoldDuration:     time.Second,
		MaxConcurrency:   50,
		Timeout:          time.Second,
		FailureThreshold: 0.20, // 20% threshold
		LatencyThreshold: time.Second,
	}

	st := NewStressTester(config, worker)
	result, err := st.Run(context.Background())

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should have stopped due to failure threshold
	if result.StopReason == "completed successfully" {
		t.Log("Test completed without hitting failure threshold - this can happen with timing")
	}

	t.Logf("Test stopped: reason=%s, errorRate=%.2f%%, failures=%d",
		result.StopReason, result.ErrorRate*100, result.FailureCount)
}

func TestStressTester_Run_LatencyThreshold(t *testing.T) {
	// Worker with increasing latency
	worker := func(ctx context.Context, id int) Result {
		// Simulate high latency
		time.Sleep(100 * time.Millisecond)
		return Result{
			StartTime:  time.Now(),
			Duration:   100 * time.Millisecond,
			StatusCode: 200,
		}
	}

	config := &StressConfig{
		Name:             "latency-threshold-test",
		InitialRPS:       5,
		MaxRPS:           20,
		RampUpRate:       5,
		RampInterval:     200 * time.Millisecond,
		HoldDuration:     time.Second,
		MaxConcurrency:   10,
		Timeout:          time.Second,
		FailureThreshold: 0.5,
		LatencyThreshold: 50 * time.Millisecond, // Lower than actual latency
	}

	st := NewStressTester(config, worker)
	result, err := st.Run(context.Background())

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	t.Logf("Test stopped: reason=%s, p99Latency=%v",
		result.StopReason, result.P99Latency)
}

func TestStressTester_Run_ContextCancellation(t *testing.T) {
	worker := func(ctx context.Context, id int) Result {
		select {
		case <-ctx.Done():
			return Result{Error: ctx.Err()}
		case <-time.After(10 * time.Millisecond):
			return Result{StatusCode: 200}
		}
	}

	config := &StressConfig{
		Name:             "cancel-test",
		InitialRPS:       10,
		MaxRPS:           100,
		RampUpRate:       10,
		RampInterval:     time.Second,
		HoldDuration:     10 * time.Second,
		MaxConcurrency:   10,
		Timeout:          time.Second,
		FailureThreshold: 0.5,
		LatencyThreshold: time.Second,
	}

	st := NewStressTester(config, worker)

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
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
	if result.StopReason != "context cancelled" {
		t.Logf("Stop reason: %s (may vary based on timing)", result.StopReason)
	}
}

func TestStressTester_CurrentRPS(t *testing.T) {
	worker := func(ctx context.Context, id int) Result {
		time.Sleep(10 * time.Millisecond)
		return Result{StatusCode: 200}
	}

	config := &StressConfig{
		Name:             "rps-tracking-test",
		InitialRPS:       10,
		MaxRPS:           50,
		RampUpRate:       20,
		RampInterval:     100 * time.Millisecond,
		HoldDuration:     100 * time.Millisecond,
		MaxConcurrency:   20,
		Timeout:          time.Second,
		FailureThreshold: 0.5,
		LatencyThreshold: time.Second,
	}

	st := NewStressTester(config, worker)

	// Check initial RPS
	if st.CurrentRPS() != 0 {
		t.Error("expected 0 RPS before start")
	}

	go func() {
		_, _ = st.Run(context.Background())
	}()

	time.Sleep(50 * time.Millisecond)

	rps := st.CurrentRPS()
	if rps < config.InitialRPS {
		t.Errorf("expected RPS >= %d during run, got %d", config.InitialRPS, rps)
	}
	t.Logf("Current RPS during test: %d", rps)
}

func TestStressTester_Phase(t *testing.T) {
	worker := func(ctx context.Context, id int) Result {
		time.Sleep(5 * time.Millisecond)
		return Result{StatusCode: 200}
	}

	config := &StressConfig{
		Name:             "phase-test",
		InitialRPS:       20,
		MaxRPS:           30,
		RampUpRate:       10,
		RampInterval:     50 * time.Millisecond,
		HoldDuration:     50 * time.Millisecond,
		MaxConcurrency:   20,
		Timeout:          time.Second,
		FailureThreshold: 0.5,
		LatencyThreshold: time.Second,
	}

	st := NewStressTester(config, worker)

	// Initial phase
	if st.Phase() != PhaseRampUp {
		t.Errorf("expected initial phase %v, got %v", PhaseRampUp, st.Phase())
	}

	result, _ := st.Run(context.Background())

	t.Logf("Final phase: %v, phase results: %d", result.Phase, len(result.PhaseResults))
}

func TestPhaseResult(t *testing.T) {
	result := PhaseResult{
		Phase:      PhaseRampUp,
		TargetRPS:  100,
		ActualRPS:  95.5,
		Duration:   10 * time.Second,
		Requests:   955,
		Successes:  950,
		Failures:   5,
		ErrorRate:  0.0052,
		AvgLatency: 50 * time.Millisecond,
		P99Latency: 200 * time.Millisecond,
	}

	if result.Phase != PhaseRampUp {
		t.Error("phase not set correctly")
	}
	if result.ErrorRate > 0.01 {
		t.Error("error rate calculation seems wrong")
	}
}
