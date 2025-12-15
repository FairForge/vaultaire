// internal/perf/batch_test.go
package perf

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestDefaultBatchConfig(t *testing.T) {
	config := DefaultBatchConfig()

	if config.MaxBatchSize != 100 {
		t.Errorf("expected 100, got %d", config.MaxBatchSize)
	}
	if config.MaxWaitTime != 100*time.Millisecond {
		t.Error("unexpected max wait")
	}
	if config.MaxConcurrent != 10 {
		t.Errorf("expected 10, got %d", config.MaxConcurrent)
	}
}

func TestNewBatchProcessor(t *testing.T) {
	processor := func(items []int) ([]int, error) {
		return items, nil
	}

	bp := NewBatchProcessor[int, int](nil, processor)
	defer func() { _ = bp.Close() }()

	if bp == nil {
		t.Fatal("expected non-nil")
	}
}

func TestBatchProcessorAdd(t *testing.T) {
	processor := func(items []int) ([]int, error) {
		return items, nil
	}

	config := &BatchConfig{MaxBatchSize: 10, MaxWaitTime: time.Hour}
	bp := NewBatchProcessor[int, int](config, processor)
	defer func() { _ = bp.Close() }()

	bp.Add(1)
	bp.Add(2)

	if bp.Pending() != 2 {
		t.Errorf("expected 2 pending, got %d", bp.Pending())
	}
}

func TestBatchProcessorAutoFlush(t *testing.T) {
	var processed int64
	processor := func(items []int) ([]int, error) {
		atomic.AddInt64(&processed, int64(len(items)))
		return items, nil
	}

	config := &BatchConfig{MaxBatchSize: 5, MaxWaitTime: time.Hour}
	bp := NewBatchProcessor[int, int](config, processor)
	defer func() { _ = bp.Close() }()

	for i := 0; i < 5; i++ {
		bp.Add(i)
	}

	time.Sleep(50 * time.Millisecond)

	if atomic.LoadInt64(&processed) != 5 {
		t.Errorf("expected 5 processed, got %d", processed)
	}
}

func TestBatchProcessorFlush(t *testing.T) {
	processor := func(items []int) ([]int, error) {
		results := make([]int, len(items))
		for i, v := range items {
			results[i] = v * 2
		}
		return results, nil
	}

	config := &BatchConfig{MaxBatchSize: 100, MaxWaitTime: time.Hour}
	bp := NewBatchProcessor[int, int](config, processor)
	defer func() { _ = bp.Close() }()

	bp.Add(1)
	bp.Add(2)
	bp.Add(3)

	results, err := bp.Flush()
	if err != nil {
		t.Fatalf("flush failed: %v", err)
	}

	if len(results) != 3 {
		t.Errorf("expected 3 results, got %d", len(results))
	}
	if results[0] != 2 || results[1] != 4 || results[2] != 6 {
		t.Error("unexpected results")
	}
}

func TestBatchProcessorAddBatch(t *testing.T) {
	processor := func(items []int) ([]int, error) {
		return items, nil
	}

	config := &BatchConfig{MaxBatchSize: 100, MaxWaitTime: time.Hour}
	bp := NewBatchProcessor[int, int](config, processor)
	defer func() { _ = bp.Close() }()

	bp.AddBatch([]int{1, 2, 3, 4, 5})

	if bp.Pending() != 5 {
		t.Errorf("expected 5 pending, got %d", bp.Pending())
	}
}

func TestBatchProcessorStats(t *testing.T) {
	processor := func(items []int) ([]int, error) {
		return items, nil
	}

	config := &BatchConfig{MaxBatchSize: 100, MaxWaitTime: time.Hour}
	bp := NewBatchProcessor[int, int](config, processor)
	defer func() { _ = bp.Close() }()

	bp.AddBatch([]int{1, 2, 3})
	_, _ = bp.Flush()

	stats := bp.Stats()
	if stats.BatchesProcessed != 1 {
		t.Errorf("expected 1 batch, got %d", stats.BatchesProcessed)
	}
	if stats.ItemsProcessed != 3 {
		t.Errorf("expected 3 items, got %d", stats.ItemsProcessed)
	}
}

func TestBatchProcessorRetry(t *testing.T) {
	attempts := 0
	processor := func(items []int) ([]int, error) {
		attempts++
		if attempts < 3 {
			return nil, errors.New("temporary error")
		}
		return items, nil
	}

	config := &BatchConfig{
		MaxBatchSize:  100,
		MaxWaitTime:   time.Hour,
		RetryAttempts: 3,
		RetryDelay:    10 * time.Millisecond,
	}
	bp := NewBatchProcessor[int, int](config, processor)
	defer func() { _ = bp.Close() }()

	bp.Add(1)
	results, err := bp.Flush()

	if err != nil {
		t.Errorf("expected success after retry, got %v", err)
	}
	if len(results) != 1 {
		t.Error("expected results")
	}
}

func TestBatchCollector(t *testing.T) {
	c := NewBatchCollector[int](3)

	c.Add(1)
	c.Add(2)
	c.Add(3)
	c.Add(4)

	batches := c.Batches()
	if len(batches) != 1 {
		t.Errorf("expected 1 complete batch, got %d", len(batches))
	}

	remaining := c.Remaining()
	if len(remaining) != 1 {
		t.Errorf("expected 1 remaining, got %d", len(remaining))
	}
}

func TestBatchCollectorAll(t *testing.T) {
	c := NewBatchCollector[int](3)

	c.Add(1)
	c.Add(2)
	c.Add(3)
	c.Add(4)

	all := c.All()
	if len(all) != 2 {
		t.Errorf("expected 2 batches, got %d", len(all))
	}
}

func TestBatchCollectorReset(t *testing.T) {
	c := NewBatchCollector[int](3)

	c.Add(1)
	c.Add(2)
	c.Reset()

	if len(c.Batches()) != 0 {
		t.Error("expected empty after reset")
	}
	if len(c.Remaining()) != 0 {
		t.Error("expected no remaining after reset")
	}
}

func TestParallelBatchExecutor(t *testing.T) {
	process := func(n int) (int, error) {
		return n * 2, nil
	}

	exec := NewParallelBatchExecutor[int, int](4, process)

	ctx := context.Background()
	results, errs := exec.Execute(ctx, []int{1, 2, 3, 4, 5})

	for _, err := range errs {
		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	}

	expected := []int{2, 4, 6, 8, 10}
	for i, r := range results {
		if r != expected[i] {
			t.Errorf("result[%d] = %d, want %d", i, r, expected[i])
		}
	}
}

func TestParallelBatchExecutorWorkers(t *testing.T) {
	exec := NewParallelBatchExecutor[int, int](8, func(n int) (int, error) {
		return n, nil
	})

	if exec.Workers() != 8 {
		t.Errorf("expected 8 workers, got %d", exec.Workers())
	}
}

func TestParallelBatchExecutorDefaultWorkers(t *testing.T) {
	exec := NewParallelBatchExecutor[int, int](0, func(n int) (int, error) {
		return n, nil
	})

	if exec.Workers() != 1 {
		t.Errorf("expected 1 worker, got %d", exec.Workers())
	}
}

func TestParallelBatchExecutorContextCancel(t *testing.T) {
	process := func(n int) (int, error) {
		time.Sleep(100 * time.Millisecond)
		return n, nil
	}

	exec := NewParallelBatchExecutor[int, int](2, process)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, errs := exec.Execute(ctx, []int{1, 2, 3})

	hasCancel := false
	for _, err := range errs {
		if err == context.Canceled {
			hasCancel = true
		}
	}
	if !hasCancel {
		t.Error("expected context canceled error")
	}
}
