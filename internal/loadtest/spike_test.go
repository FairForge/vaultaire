package loadtest

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestDefaultSpikeConfig(t *testing.T) {
	config := DefaultSpikeConfig("test")

	if config.Name != "test" {
		t.Errorf("expected name 'test', got %q", config.Name)
	}
	if config.SpikeRPS <= config.BaselineRPS {
		t.Error("expected SpikeRPS > BaselineRPS")
	}
	if config.NumSpikes <= 0 {
		t.Error("expected positive NumSpikes")
	}
}

func TestSpikeTester_New(t *testing.T) {
	worker := func(ctx context.Context, id int) Result {
		return Result{}
	}

	// With nil config
	st := NewSpikeTester(nil, worker)
	if st.config.Name != "default-spike" {
		t.Error("expected default config when nil provided")
	}

	// With custom config
	config := &SpikeConfig{
		Name:        "custom",
		BaselineRPS: 10,
		SpikeRPS:    100,
	}
	st = NewSpikeTester(config, worker)
	if st.config.Name != "custom" {
		t.Error("expected custom config to be used")
	}
}

func TestSpikeTester_Run_SingleSpike(t *testing.T) {
	var callCount atomic.Int64

	worker := func(ctx context.Context, id int) Result {
		callCount.Add(1)
		return Result{
			StartTime:  time.Now(),
			Duration:   5 * time.Millisecond,
			StatusCode: 200,
		}
	}

	config := &SpikeConfig{
		Name:           "single-spike-test",
		BaselineRPS:    20,
		SpikeRPS:       100,
		BaselinePeriod: 100 * time.Millisecond,
		SpikeDuration:  100 * time.Millisecond,
		RecoveryPeriod: 100 * time.Millisecond,
		NumSpikes:      1,
		MaxConcurrency: 50,
		Timeout:        time.Second,
	}

	st := NewSpikeTester(config, worker)
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
	if len(result.SpikeMetrics) == 0 {
		t.Error("expected spike metrics")
	}

	// Should have baseline, spike, and recovery phases
	hasSpike := false
	for _, m := range result.SpikeMetrics {
		if m.State == SpikeStateSpike {
			hasSpike = true
			t.Logf("Spike phase: requests=%d, actualRPS=%.2f, errorRate=%.2f%%",
				m.Requests, m.ActualRPS, m.ErrorRate*100)
		}
	}
	if !hasSpike {
		t.Error("expected at least one spike phase in metrics")
	}

	t.Logf("Test completed: totalRequests=%d, phases=%d",
		result.TotalRequests, len(result.SpikeMetrics))
}

func TestSpikeTester_Run_MultipleSpikes(t *testing.T) {
	worker := func(ctx context.Context, id int) Result {
		return Result{
			StartTime:  time.Now(),
			Duration:   2 * time.Millisecond,
			StatusCode: 200,
		}
	}

	config := &SpikeConfig{
		Name:           "multi-spike-test",
		BaselineRPS:    20,
		SpikeRPS:       80,
		BaselinePeriod: 50 * time.Millisecond,
		SpikeDuration:  50 * time.Millisecond,
		RecoveryPeriod: 50 * time.Millisecond,
		NumSpikes:      2,
		MaxConcurrency: 30,
		Timeout:        time.Second,
	}

	st := NewSpikeTester(config, worker)
	result, err := st.Run(context.Background())

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Count spike phases
	spikeCount := 0
	for _, m := range result.SpikeMetrics {
		if m.State == SpikeStateSpike {
			spikeCount++
		}
	}

	if spikeCount < config.NumSpikes {
		t.Errorf("expected at least %d spikes, got %d", config.NumSpikes, spikeCount)
	}

	t.Logf("Multiple spikes completed: totalRequests=%d, spikePhases=%d, recoveryTimes=%d",
		result.TotalRequests, spikeCount, len(result.RecoveryTimes))
}

func TestSpikeTester_Run_ContextCancellation(t *testing.T) {
	worker := func(ctx context.Context, id int) Result {
		select {
		case <-ctx.Done():
			return Result{Error: ctx.Err()}
		case <-time.After(5 * time.Millisecond):
			return Result{StatusCode: 200}
		}
	}

	config := &SpikeConfig{
		Name:           "cancel-test",
		BaselineRPS:    10,
		SpikeRPS:       50,
		BaselinePeriod: 5 * time.Second,
		SpikeDuration:  5 * time.Second,
		RecoveryPeriod: 5 * time.Second,
		NumSpikes:      5,
		MaxConcurrency: 20,
		Timeout:        time.Second,
	}

	st := NewSpikeTester(config, worker)

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

func TestSpikeTester_State(t *testing.T) {
	worker := func(ctx context.Context, id int) Result {
		time.Sleep(5 * time.Millisecond)
		return Result{StatusCode: 200}
	}

	config := &SpikeConfig{
		Name:           "state-test",
		BaselineRPS:    10,
		SpikeRPS:       30,
		BaselinePeriod: 100 * time.Millisecond,
		SpikeDuration:  100 * time.Millisecond,
		RecoveryPeriod: 100 * time.Millisecond,
		NumSpikes:      1,
		MaxConcurrency: 20,
		Timeout:        time.Second,
	}

	st := NewSpikeTester(config, worker)

	// Initial state
	if st.State() != SpikeStateBaseline {
		t.Errorf("expected initial state %v, got %v", SpikeStateBaseline, st.State())
	}

	done := make(chan struct{})
	go func() {
		_, _ = st.Run(context.Background())
		close(done)
	}()

	// Monitor state changes
	states := make(map[SpikeState]bool)
	timeout := time.After(2 * time.Second)

	for {
		select {
		case <-done:
			t.Logf("Observed states: %v", states)
			return
		case <-timeout:
			t.Fatal("test timed out")
		default:
			states[st.State()] = true
			time.Sleep(10 * time.Millisecond)
		}
	}
}

func TestSpikeTester_CurrentRPS(t *testing.T) {
	worker := func(ctx context.Context, id int) Result {
		time.Sleep(2 * time.Millisecond)
		return Result{StatusCode: 200}
	}

	config := &SpikeConfig{
		Name:           "rps-test",
		BaselineRPS:    10,
		SpikeRPS:       50,
		BaselinePeriod: 100 * time.Millisecond,
		SpikeDuration:  100 * time.Millisecond,
		RecoveryPeriod: 100 * time.Millisecond,
		NumSpikes:      1,
		MaxConcurrency: 30,
		Timeout:        time.Second,
	}

	st := NewSpikeTester(config, worker)

	go func() {
		_, _ = st.Run(context.Background())
	}()

	// Wait for spike phase and check RPS
	time.Sleep(150 * time.Millisecond)

	rps := st.CurrentRPS()
	// During spike, should be at SpikeRPS
	if rps != config.BaselineRPS && rps != config.SpikeRPS {
		t.Errorf("expected RPS to be %d or %d, got %d",
			config.BaselineRPS, config.SpikeRPS, rps)
	}

	t.Logf("Current RPS: %d", rps)
}

func TestSpikeTester_SystemRecovery(t *testing.T) {
	worker := func(ctx context.Context, id int) Result {
		return Result{
			StartTime:  time.Now(),
			Duration:   time.Millisecond,
			StatusCode: 200,
		}
	}

	config := &SpikeConfig{
		Name:           "recovery-test",
		BaselineRPS:    10,
		SpikeRPS:       30,
		BaselinePeriod: 50 * time.Millisecond,
		SpikeDuration:  50 * time.Millisecond,
		RecoveryPeriod: 50 * time.Millisecond,
		NumSpikes:      1,
		MaxConcurrency: 20,
		Timeout:        time.Second,
	}

	st := NewSpikeTester(config, worker)
	result, err := st.Run(context.Background())

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// With no errors, system should recover
	if !result.SystemRecovered {
		t.Error("expected system to recover with no errors")
	}

	t.Logf("System recovered: %v, max error rate: %.2f%%",
		result.SystemRecovered, result.MaxErrorRate*100)
}

func TestSpikeMetrics(t *testing.T) {
	metrics := SpikeMetrics{
		State:       SpikeStateSpike,
		SpikeNumber: 1,
		StartTime:   time.Now(),
		EndTime:     time.Now().Add(5 * time.Second),
		Duration:    5 * time.Second,
		TargetRPS:   100,
		ActualRPS:   95.5,
		Requests:    478,
		Successes:   475,
		Failures:    3,
		ErrorRate:   0.0063,
		AvgLatency:  50 * time.Millisecond,
		P95Latency:  100 * time.Millisecond,
		P99Latency:  150 * time.Millisecond,
		MaxLatency:  200 * time.Millisecond,
	}

	if metrics.State != SpikeStateSpike {
		t.Error("state not set correctly")
	}
	if metrics.SpikeNumber != 1 {
		t.Error("spike number not set correctly")
	}
	if metrics.ErrorRate > 0.01 {
		t.Error("error rate seems too high for test data")
	}
}

func TestPeriodCollector(t *testing.T) {
	pc := newPeriodCollector()

	// Record some results
	for i := 0; i < 10; i++ {
		result := Result{
			Duration:   time.Duration(i) * time.Millisecond,
			StatusCode: 200,
		}
		if i%3 == 0 {
			result.Error = context.Canceled
		}
		pc.record(result)
	}

	requests, successes, failures, latencies := pc.snapshot()

	if requests != 10 {
		t.Errorf("expected 10 requests, got %d", requests)
	}
	if successes+failures != requests {
		t.Error("successes + failures should equal requests")
	}
	if len(latencies) != 10 {
		t.Errorf("expected 10 latencies, got %d", len(latencies))
	}

	// Test reset
	pc.reset()
	requests, successes, failures, latencies = pc.snapshot()
	if requests != 0 || successes != 0 || failures != 0 || len(latencies) != 0 {
		t.Error("reset should clear all values")
	}
}
