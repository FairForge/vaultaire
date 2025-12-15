// internal/perf/async_test.go
package perf

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestDefaultAsyncConfig(t *testing.T) {
	config := DefaultAsyncConfig()

	if config.WorkerCount != 10 {
		t.Errorf("expected 10 workers, got %d", config.WorkerCount)
	}
	if config.QueueSize != 1000 {
		t.Errorf("expected 1000 queue, got %d", config.QueueSize)
	}
}

func TestNewAsyncProcessor(t *testing.T) {
	process := func(n int) (int, error) { return n * 2, nil }
	ap := NewAsyncProcessor[int, int](nil, process)
	defer func() { _ = ap.Close() }()

	if ap == nil {
		t.Fatal("expected non-nil")
	}
}

func TestAsyncProcessorSubmit(t *testing.T) {
	process := func(n int) (int, error) { return n * 2, nil }
	ap := NewAsyncProcessor[int, int](nil, process)
	defer func() { _ = ap.Close() }()

	future := ap.Submit(5)

	ctx := context.Background()
	result, err := future.Get(ctx)
	if err != nil {
		t.Fatalf("get failed: %v", err)
	}
	if result != 10 {
		t.Errorf("expected 10, got %d", result)
	}
}

func TestAsyncProcessorSubmitWait(t *testing.T) {
	process := func(n int) (int, error) { return n + 1, nil }
	ap := NewAsyncProcessor[int, int](nil, process)
	defer func() { _ = ap.Close() }()

	ctx := context.Background()
	result, err := ap.SubmitWait(ctx, 5)
	if err != nil {
		t.Fatalf("submit wait failed: %v", err)
	}
	if result != 6 {
		t.Errorf("expected 6, got %d", result)
	}
}

func TestAsyncProcessorStats(t *testing.T) {
	process := func(n int) (int, error) { return n, nil }
	ap := NewAsyncProcessor[int, int](nil, process)
	defer func() { _ = ap.Close() }()

	ctx := context.Background()
	_, _ = ap.SubmitWait(ctx, 1)
	_, _ = ap.SubmitWait(ctx, 2)

	stats := ap.Stats()
	if stats.TasksSubmitted != 2 {
		t.Errorf("expected 2 submitted, got %d", stats.TasksSubmitted)
	}
	if stats.TasksCompleted != 2 {
		t.Errorf("expected 2 completed, got %d", stats.TasksCompleted)
	}
}

func TestAsyncProcessorClose(t *testing.T) {
	process := func(n int) (int, error) { return n, nil }
	ap := NewAsyncProcessor[int, int](nil, process)

	err := ap.Close()
	if err != nil {
		t.Fatalf("close failed: %v", err)
	}

	// Submit after close should fail
	future := ap.Submit(1)
	_, err = future.Get(context.Background())
	if err != ErrProcessorClosed {
		t.Errorf("expected closed error, got %v", err)
	}
}

func TestAsyncProcessorQueueFull(t *testing.T) {
	process := func(n int) (int, error) {
		time.Sleep(100 * time.Millisecond)
		return n, nil
	}

	config := &AsyncConfig{WorkerCount: 1, QueueSize: 1}
	ap := NewAsyncProcessor[int, int](config, process)
	defer func() { _ = ap.Close() }()

	// Fill queue
	ap.Submit(1)
	ap.Submit(2)

	// This should be dropped
	future := ap.Submit(3)
	_, err := future.Get(context.Background())
	if err != ErrQueueFull {
		t.Errorf("expected queue full, got %v", err)
	}
}

func TestFutureIsReady(t *testing.T) {
	process := func(n int) (int, error) { return n, nil }
	ap := NewAsyncProcessor[int, int](nil, process)
	defer func() { _ = ap.Close() }()

	future := ap.Submit(1)

	// Wait for completion
	<-future.Done()

	if !future.IsReady() {
		t.Error("expected ready")
	}
}

func TestFutureContextCancel(t *testing.T) {
	process := func(n int) (int, error) {
		time.Sleep(time.Second)
		return n, nil
	}
	ap := NewAsyncProcessor[int, int](nil, process)
	defer func() { _ = ap.Close() }()

	future := ap.Submit(1)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := future.Get(ctx)
	if err != context.Canceled {
		t.Errorf("expected canceled, got %v", err)
	}
}

func TestWorkerPool(t *testing.T) {
	wp := NewWorkerPool(4, 100)
	defer wp.Close()

	if wp.Workers() != 4 {
		t.Errorf("expected 4 workers, got %d", wp.Workers())
	}
}

func TestWorkerPoolSubmit(t *testing.T) {
	wp := NewWorkerPool(4, 100)
	defer wp.Close()

	var count int64
	done := make(chan struct{})

	ok := wp.Submit(func() {
		atomic.AddInt64(&count, 1)
		close(done)
	})

	if !ok {
		t.Error("expected submit success")
	}

	<-done
	if atomic.LoadInt64(&count) != 1 {
		t.Error("task not executed")
	}
}

func TestWorkerPoolClose(t *testing.T) {
	wp := NewWorkerPool(2, 10)
	wp.Close()

	ok := wp.Submit(func() {})
	if ok {
		t.Error("expected submit to fail after close")
	}
}

func TestDebouncer(t *testing.T) {
	d := NewDebouncer(50 * time.Millisecond)

	var count int64

	// Rapid calls
	for i := 0; i < 5; i++ {
		d.Debounce(func() {
			atomic.AddInt64(&count, 1)
		})
	}

	time.Sleep(100 * time.Millisecond)

	if atomic.LoadInt64(&count) != 1 {
		t.Errorf("expected 1 call, got %d", count)
	}
}

func TestDebouncerCancel(t *testing.T) {
	d := NewDebouncer(50 * time.Millisecond)

	var count int64

	d.Debounce(func() {
		atomic.AddInt64(&count, 1)
	})

	d.Cancel()
	time.Sleep(100 * time.Millisecond)

	if atomic.LoadInt64(&count) != 0 {
		t.Error("expected no calls after cancel")
	}
}

func TestThrottler(t *testing.T) {
	th := NewThrottler(50 * time.Millisecond)

	if !th.Allow() {
		t.Error("first call should be allowed")
	}

	if th.Allow() {
		t.Error("immediate second call should not be allowed")
	}

	time.Sleep(60 * time.Millisecond)

	if !th.Allow() {
		t.Error("call after interval should be allowed")
	}
}

func TestThrottlerThrottle(t *testing.T) {
	th := NewThrottler(50 * time.Millisecond)

	var count int64

	for i := 0; i < 5; i++ {
		th.Throttle(func() {
			atomic.AddInt64(&count, 1)
		})
	}

	if atomic.LoadInt64(&count) != 1 {
		t.Errorf("expected 1 call, got %d", count)
	}
}

func TestAsyncError(t *testing.T) {
	err := &AsyncError{msg: "test error"}
	if err.Error() != "test error" {
		t.Error("error message mismatch")
	}
}

func TestAsyncStatsFields(t *testing.T) {
	stats := &AsyncStats{
		TasksSubmitted: 100,
		TasksCompleted: 95,
		TasksFailed:    5,
		TasksDropped:   2,
		QueueDepth:     10,
		AvgProcessTime: 1000000,
	}

	if stats.TasksSubmitted != 100 {
		t.Error("unexpected submitted")
	}
}
