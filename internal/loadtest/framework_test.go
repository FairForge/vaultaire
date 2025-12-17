package loadtest

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestDefaultConfig(t *testing.T) {
	config := DefaultConfig("test", TestTypeLoad)

	if config.Name != "test" {
		t.Errorf("expected name 'test', got %q", config.Name)
	}
	if config.Type != TestTypeLoad {
		t.Errorf("expected type %v, got %v", TestTypeLoad, config.Type)
	}
	if config.TargetRPS <= 0 {
		t.Error("expected positive TargetRPS")
	}
	if config.MaxConcurrency <= 0 {
		t.Error("expected positive MaxConcurrency")
	}
}

func TestFramework_New(t *testing.T) {
	worker := func(ctx context.Context, id int) Result {
		return Result{StartTime: time.Now(), Duration: time.Millisecond}
	}

	// With nil config
	f := New(nil, worker)
	if f.config.Name != "default" {
		t.Error("expected default config when nil provided")
	}

	// With custom config
	config := &Config{
		Name:           "custom",
		Type:           TestTypeStress,
		Duration:       time.Second,
		TargetRPS:      10,
		MaxConcurrency: 5,
	}
	f = New(config, worker)
	if f.config.Name != "custom" {
		t.Error("expected custom config to be used")
	}
}

func TestFramework_Run_Success(t *testing.T) {
	var callCount atomic.Int64

	worker := func(ctx context.Context, id int) Result {
		callCount.Add(1)
		return Result{
			StartTime:  time.Now(),
			Duration:   5 * time.Millisecond,
			StatusCode: 200,
			BytesSent:  100,
			BytesRecv:  200,
		}
	}

	config := &Config{
		Name:           "success-test",
		Type:           TestTypeLoad,
		Duration:       500 * time.Millisecond,
		TargetRPS:      20, // 20 RPS for 0.5s = ~10 requests
		MaxConcurrency: 10,
	}

	f := New(config, worker)
	summary, err := f.Run(context.Background())

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if summary == nil {
		t.Fatal("expected summary, got nil")
	}
	if summary.TestName != "success-test" {
		t.Errorf("expected test name 'success-test', got %q", summary.TestName)
	}
	if summary.TotalRequests == 0 {
		t.Error("expected some requests to be made")
	}
	if summary.SuccessCount == 0 {
		t.Error("expected some successful requests")
	}
	if summary.FailureCount != 0 {
		t.Errorf("expected no failures, got %d", summary.FailureCount)
	}
	if summary.ErrorRate != 0 {
		t.Errorf("expected 0 error rate, got %f", summary.ErrorRate)
	}
	if summary.RequestsPerSec <= 0 {
		t.Error("expected positive RPS")
	}
}

func TestFramework_Run_WithErrors(t *testing.T) {
	var callCount atomic.Int64
	testErr := errors.New("simulated failure")

	worker := func(ctx context.Context, id int) Result {
		count := callCount.Add(1)
		if count%2 == 0 {
			return Result{
				StartTime: time.Now(),
				Duration:  time.Millisecond,
				Error:     testErr,
			}
		}
		return Result{
			StartTime:  time.Now(),
			Duration:   time.Millisecond,
			StatusCode: 200,
		}
	}

	config := &Config{
		Name:           "error-test",
		Type:           TestTypeLoad,
		Duration:       300 * time.Millisecond,
		TargetRPS:      20,
		MaxConcurrency: 10,
	}

	f := New(config, worker)
	summary, err := f.Run(context.Background())

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if summary.FailureCount == 0 {
		t.Error("expected some failures")
	}
	if summary.ErrorRate == 0 {
		t.Error("expected non-zero error rate")
	}
	if len(summary.Errors) == 0 {
		t.Error("expected error map to be populated")
	}
}

func TestFramework_Run_ContextCancellation(t *testing.T) {
	worker := func(ctx context.Context, id int) Result {
		select {
		case <-ctx.Done():
			return Result{Error: ctx.Err()}
		case <-time.After(10 * time.Millisecond):
			return Result{StatusCode: 200}
		}
	}

	config := &Config{
		Name:           "cancel-test",
		Type:           TestTypeLoad,
		Duration:       10 * time.Second, // Long duration
		TargetRPS:      10,
		MaxConcurrency: 5,
	}

	f := New(config, worker)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := f.Run(ctx)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if elapsed > time.Second {
		t.Error("test should have been cancelled quickly")
	}
}

func TestFramework_IsRunning(t *testing.T) {
	started := make(chan struct{})
	done := make(chan struct{})

	worker := func(ctx context.Context, id int) Result {
		select {
		case <-started:
		default:
			close(started)
		}
		<-done
		return Result{}
	}

	config := &Config{
		Name:           "running-test",
		Duration:       5 * time.Second,
		TargetRPS:      1,
		MaxConcurrency: 1,
	}

	f := New(config, worker)

	if f.IsRunning() {
		t.Error("should not be running before Run()")
	}

	go func() {
		_, _ = f.Run(context.Background()) // Fixed: explicitly ignore return values
	}()

	<-started // Wait for worker to start
	time.Sleep(50 * time.Millisecond)

	if !f.IsRunning() {
		t.Error("should be running during Run()")
	}

	close(done) // Let worker finish
}

func TestFramework_CurrentStats(t *testing.T) {
	var requests atomic.Int64

	worker := func(ctx context.Context, id int) Result {
		requests.Add(1)
		time.Sleep(10 * time.Millisecond)
		return Result{StatusCode: 200}
	}

	config := &Config{
		Name:           "stats-test",
		Duration:       500 * time.Millisecond,
		TargetRPS:      50,
		MaxConcurrency: 20,
	}

	f := New(config, worker)

	go func() {
		_, _ = f.Run(context.Background()) // Fixed: explicitly ignore return values
	}()

	time.Sleep(200 * time.Millisecond)

	total, success, failure, rps := f.CurrentStats()

	if total == 0 {
		t.Error("expected some requests in progress")
	}
	if rps <= 0 {
		t.Error("expected positive RPS")
	}
	t.Logf("Stats during run: total=%d, success=%d, failure=%d, rps=%.2f",
		total, success, failure, rps)
}

func TestCalculatePercentiles(t *testing.T) {
	latencies := []time.Duration{
		10 * time.Millisecond,
		20 * time.Millisecond,
		30 * time.Millisecond,
		40 * time.Millisecond,
		50 * time.Millisecond,
		60 * time.Millisecond,
		70 * time.Millisecond,
		80 * time.Millisecond,
		90 * time.Millisecond,
		100 * time.Millisecond,
	}

	min, max, avg, p50, p95, p99 := calculatePercentiles(latencies)

	if min != 10*time.Millisecond {
		t.Errorf("expected min 10ms, got %v", min)
	}
	if max != 100*time.Millisecond {
		t.Errorf("expected max 100ms, got %v", max)
	}
	if avg != 55*time.Millisecond {
		t.Errorf("expected avg 55ms, got %v", avg)
	}
	if p50 < 40*time.Millisecond || p50 > 60*time.Millisecond {
		t.Errorf("expected p50 around 50ms, got %v", p50)
	}
	if p95 < 90*time.Millisecond {
		t.Errorf("expected p95 >= 90ms, got %v", p95)
	}
	if p99 < 90*time.Millisecond {
		t.Errorf("expected p99 >= 90ms, got %v", p99)
	}
}

func TestCalculatePercentiles_Empty(t *testing.T) {
	min, max, avg, p50, p95, p99 := calculatePercentiles(nil)

	if min != 0 || max != 0 || avg != 0 || p50 != 0 || p95 != 0 || p99 != 0 {
		t.Error("expected all zeros for empty input")
	}
}
