package loadtest

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestDefaultChaosConfig(t *testing.T) {
	config := DefaultChaosConfig("test")

	if config.Name != "test" {
		t.Errorf("expected name 'test', got %q", config.Name)
	}
	if len(config.ChaosTypes) == 0 {
		t.Error("expected default ChaosTypes")
	}
	if config.ChaosProbability <= 0 || config.ChaosProbability > 1 {
		t.Error("expected ChaosProbability between 0 and 1")
	}
}

func TestChaosTester_New(t *testing.T) {
	worker := func(ctx context.Context, id int) Result {
		return Result{}
	}

	// With nil config
	ct := NewChaosTester(nil, worker)
	if ct.config.Name != "default-chaos" {
		t.Error("expected default config when nil provided")
	}

	// With custom config
	config := &ChaosConfig{
		Name:       "custom",
		ChaosTypes: []ChaosType{ChaosLatency},
	}
	ct = NewChaosTester(config, worker)
	if ct.config.Name != "custom" {
		t.Error("expected custom config to be used")
	}
}

func TestChaosTester_Run_LatencyChaos(t *testing.T) {
	worker := func(ctx context.Context, id int) Result {
		return Result{
			StartTime:  time.Now(),
			Duration:   time.Millisecond,
			StatusCode: 200,
		}
	}

	config := &ChaosConfig{
		Name:             "latency-chaos",
		Duration:         500 * time.Millisecond,
		TargetRPS:        50,
		MaxConcurrency:   20,
		Timeout:          time.Second,
		ChaosTypes:       []ChaosType{ChaosLatency},
		ChaosProbability: 0.5,
		ChaosInterval:    100 * time.Millisecond,
		ChaosDuration:    150 * time.Millisecond,
		LatencyMin:       10 * time.Millisecond,
		LatencyMax:       50 * time.Millisecond,
		RecoveryPeriod:   100 * time.Millisecond,
	}

	ct := NewChaosTester(config, worker)
	result, err := ct.Run(context.Background())

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == nil {
		t.Fatal("expected result, got nil")
	}
	if result.TotalRequests == 0 {
		t.Error("expected some requests")
	}

	t.Logf("Latency chaos test: requests=%d, events=%d, score=%.1f",
		result.TotalRequests, len(result.ChaosEvents), result.ResilienceScore)
}

func TestChaosTester_Run_ErrorChaos(t *testing.T) {
	worker := func(ctx context.Context, id int) Result {
		return Result{
			StartTime:  time.Now(),
			Duration:   time.Millisecond,
			StatusCode: 200,
		}
	}

	config := &ChaosConfig{
		Name:             "error-chaos",
		Duration:         400 * time.Millisecond,
		TargetRPS:        50,
		MaxConcurrency:   20,
		Timeout:          time.Second,
		ChaosTypes:       []ChaosType{ChaosError},
		ChaosProbability: 0.3,
		ChaosInterval:    100 * time.Millisecond,
		ChaosDuration:    100 * time.Millisecond,
		ErrorTypes:       []error{fmt.Errorf("test error 1"), fmt.Errorf("test error 2")},
		RecoveryPeriod:   100 * time.Millisecond,
	}

	ct := NewChaosTester(config, worker)
	result, err := ct.Run(context.Background())

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should have some failures from injected errors
	if result.FailureCount == 0 {
		t.Log("No failures recorded - chaos may not have triggered")
	}

	t.Logf("Error chaos test: requests=%d, failures=%d, errorRate=%.2f%%",
		result.TotalRequests, result.FailureCount, result.ErrorRate*100)
}

func TestChaosTester_Run_MultipleChaosTypes(t *testing.T) {
	worker := func(ctx context.Context, id int) Result {
		return Result{
			StartTime:  time.Now(),
			Duration:   time.Millisecond,
			StatusCode: 200,
		}
	}

	config := &ChaosConfig{
		Name:             "multi-chaos",
		Duration:         600 * time.Millisecond,
		TargetRPS:        30,
		MaxConcurrency:   15,
		Timeout:          time.Second,
		ChaosTypes:       []ChaosType{ChaosLatency, ChaosError, ChaosPartition},
		ChaosProbability: 0.2,
		ChaosInterval:    100 * time.Millisecond,
		ChaosDuration:    100 * time.Millisecond,
		LatencyMin:       5 * time.Millisecond,
		LatencyMax:       20 * time.Millisecond,
		ErrorTypes:       []error{fmt.Errorf("chaos error")},
		RecoveryPeriod:   100 * time.Millisecond,
	}

	ct := NewChaosTester(config, worker)
	result, err := ct.Run(context.Background())

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	t.Logf("Multi-chaos test: requests=%d, events=%d, score=%.1f",
		result.TotalRequests, len(result.ChaosEvents), result.ResilienceScore)

	for i, event := range result.ChaosEvents {
		t.Logf("  Event %d: type=%s, duration=%v, affected=%d",
			i+1, event.Type, event.Duration, event.Affected)
	}
}

func TestChaosTester_Run_ContextCancellation(t *testing.T) {
	worker := func(ctx context.Context, id int) Result {
		return Result{StatusCode: 200, Duration: time.Millisecond}
	}

	config := &ChaosConfig{
		Name:             "cancel-chaos",
		Duration:         10 * time.Second,
		TargetRPS:        10,
		MaxConcurrency:   5,
		Timeout:          time.Second,
		ChaosTypes:       []ChaosType{ChaosLatency},
		ChaosProbability: 0.1,
		ChaosInterval:    time.Second,
		ChaosDuration:    time.Second,
		LatencyMin:       10 * time.Millisecond,
		LatencyMax:       50 * time.Millisecond,
		RecoveryPeriod:   time.Second,
	}

	ct := NewChaosTester(config, worker)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	start := time.Now()
	result, err := ct.Run(ctx)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if elapsed > time.Second {
		t.Error("test should have been cancelled quickly")
	}

	t.Logf("Cancelled after %v with %d requests", elapsed, result.TotalRequests)
}

func TestChaosTester_IsChaosActive(t *testing.T) {
	worker := func(ctx context.Context, id int) Result {
		time.Sleep(5 * time.Millisecond)
		return Result{StatusCode: 200}
	}

	config := &ChaosConfig{
		Name:             "active-test",
		Duration:         500 * time.Millisecond,
		TargetRPS:        20,
		MaxConcurrency:   10,
		Timeout:          time.Second,
		ChaosTypes:       []ChaosType{ChaosLatency},
		ChaosProbability: 0.5,
		ChaosInterval:    50 * time.Millisecond,
		ChaosDuration:    100 * time.Millisecond,
		LatencyMin:       5 * time.Millisecond,
		LatencyMax:       10 * time.Millisecond,
		RecoveryPeriod:   50 * time.Millisecond,
	}

	ct := NewChaosTester(config, worker)

	// Not active before run
	if ct.IsChaosActive() {
		t.Error("should not be active before run")
	}

	done := make(chan struct{})
	go func() {
		_, _ = ct.Run(context.Background())
		close(done)
	}()

	// Wait for chaos to potentially activate
	time.Sleep(150 * time.Millisecond)

	// Check if chaos was ever active (may vary due to timing)
	wasActive := ct.IsChaosActive()
	t.Logf("Chaos active during test: %v", wasActive)

	<-done
}

func TestChaosTester_Events(t *testing.T) {
	worker := func(ctx context.Context, id int) Result {
		return Result{StatusCode: 200, Duration: time.Millisecond}
	}

	config := &ChaosConfig{
		Name:             "events-test",
		Duration:         400 * time.Millisecond,
		TargetRPS:        20,
		MaxConcurrency:   10,
		Timeout:          time.Second,
		ChaosTypes:       []ChaosType{ChaosError},
		ChaosProbability: 0.5,
		ChaosInterval:    100 * time.Millisecond,
		ChaosDuration:    80 * time.Millisecond,
		ErrorTypes:       []error{fmt.Errorf("test error")},
		RecoveryPeriod:   50 * time.Millisecond,
	}

	ct := NewChaosTester(config, worker)
	result, _ := ct.Run(context.Background())

	events := ct.Events()
	t.Logf("Events recorded: %d", len(events))

	// Result should have same events
	if len(result.ChaosEvents) != len(events) {
		t.Errorf("event count mismatch: result=%d, api=%d",
			len(result.ChaosEvents), len(events))
	}
}

func TestChaosTester_RecoveryMetrics(t *testing.T) {
	worker := func(ctx context.Context, id int) Result {
		return Result{StatusCode: 200, Duration: time.Millisecond}
	}

	config := &ChaosConfig{
		Name:             "recovery-test",
		Duration:         400 * time.Millisecond,
		TargetRPS:        30,
		MaxConcurrency:   15,
		Timeout:          time.Second,
		ChaosTypes:       []ChaosType{ChaosError},
		ChaosProbability: 0.3,
		ChaosInterval:    100 * time.Millisecond,
		ChaosDuration:    100 * time.Millisecond,
		ErrorTypes:       []error{fmt.Errorf("test error")},
		RecoveryPeriod:   100 * time.Millisecond,
	}

	ct := NewChaosTester(config, worker)
	result, err := ct.Run(context.Background())

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	metrics := result.RecoveryMetrics
	t.Logf("Recovery metrics:")
	t.Logf("  Pre-chaos error rate: %.2f%%", metrics.PreChaosErrorRate*100)
	t.Logf("  During-chaos error rate: %.2f%%", metrics.DuringChaosErrorRate*100)
	t.Logf("  Post-chaos error rate: %.2f%%", metrics.PostChaosErrorRate*100)
	t.Logf("  Fully recovered: %v", metrics.FullyRecovered)
}

func TestChaosTester_ResilienceScore(t *testing.T) {
	worker := func(ctx context.Context, id int) Result {
		return Result{StatusCode: 200, Duration: time.Millisecond}
	}

	config := &ChaosConfig{
		Name:             "score-test",
		Duration:         300 * time.Millisecond,
		TargetRPS:        30,
		MaxConcurrency:   15,
		Timeout:          time.Second,
		ChaosTypes:       []ChaosType{ChaosLatency},
		ChaosProbability: 0.1, // Low chaos for high score
		ChaosInterval:    100 * time.Millisecond,
		ChaosDuration:    50 * time.Millisecond,
		LatencyMin:       5 * time.Millisecond,
		LatencyMax:       10 * time.Millisecond,
		RecoveryPeriod:   50 * time.Millisecond,
	}

	ct := NewChaosTester(config, worker)
	result, err := ct.Run(context.Background())

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result.ResilienceScore < 0 || result.ResilienceScore > 100 {
		t.Errorf("resilience score out of range: %.1f", result.ResilienceScore)
	}

	t.Logf("Resilience score: %.1f/100", result.ResilienceScore)
}

func TestChaosEvent(t *testing.T) {
	event := ChaosEvent{
		Type:      ChaosLatency,
		StartTime: time.Now(),
		EndTime:   time.Now().Add(10 * time.Second),
		Duration:  10 * time.Second,
		Affected:  150,
	}

	if event.Type != ChaosLatency {
		t.Error("Type not set correctly")
	}
	if event.Affected != 150 {
		t.Error("Affected not set correctly")
	}
}

func TestRecoveryMetrics(t *testing.T) {
	metrics := RecoveryMetrics{
		PreChaosRPS:          100,
		DuringChaosRPS:       80,
		PostChaosRPS:         95,
		PreChaosErrorRate:    0.01,
		DuringChaosErrorRate: 0.15,
		PostChaosErrorRate:   0.02,
		RecoveryTime:         30 * time.Second,
		FullyRecovered:       true,
	}

	if !metrics.FullyRecovered {
		t.Error("FullyRecovered not set correctly")
	}
	if metrics.DuringChaosErrorRate <= metrics.PreChaosErrorRate {
		t.Error("During-chaos should have higher error rate")
	}
}
