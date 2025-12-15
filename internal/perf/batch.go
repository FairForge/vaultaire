// internal/perf/batch.go
package perf

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// BatchConfig configures batch processing
type BatchConfig struct {
	MaxBatchSize  int
	MaxWaitTime   time.Duration
	MaxConcurrent int
	RetryAttempts int
	RetryDelay    time.Duration
}

// DefaultBatchConfig returns default configuration
func DefaultBatchConfig() *BatchConfig {
	return &BatchConfig{
		MaxBatchSize:  100,
		MaxWaitTime:   100 * time.Millisecond,
		MaxConcurrent: 10,
		RetryAttempts: 3,
		RetryDelay:    100 * time.Millisecond,
	}
}

// BatchProcessor processes items in batches
type BatchProcessor[T any, R any] struct {
	config    *BatchConfig
	processor func([]T) ([]R, error)
	pending   []T
	mu        sync.Mutex
	stats     *BatchStats
	flushCh   chan struct{}
	closed    atomic.Bool
}

// BatchStats tracks batch statistics
type BatchStats struct {
	BatchesProcessed int64
	ItemsProcessed   int64
	Errors           int64
	AvgBatchSize     float64
	TotalWaitTime    int64
}

// NewBatchProcessor creates a new batch processor
func NewBatchProcessor[T any, R any](config *BatchConfig, processor func([]T) ([]R, error)) *BatchProcessor[T, R] {
	if config == nil {
		config = DefaultBatchConfig()
	}

	bp := &BatchProcessor[T, R]{
		config:    config,
		processor: processor,
		pending:   make([]T, 0, config.MaxBatchSize),
		stats:     &BatchStats{},
		flushCh:   make(chan struct{}, 1),
	}

	go bp.flushLoop()

	return bp
}

func (bp *BatchProcessor[T, R]) flushLoop() {
	ticker := time.NewTicker(bp.config.MaxWaitTime)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			_, _ = bp.Flush()
		case <-bp.flushCh:
			if bp.closed.Load() {
				return
			}
		}
	}
}

// Add adds an item to the batch
func (bp *BatchProcessor[T, R]) Add(item T) {
	bp.mu.Lock()
	bp.pending = append(bp.pending, item)
	shouldFlush := len(bp.pending) >= bp.config.MaxBatchSize
	bp.mu.Unlock()

	if shouldFlush {
		_, _ = bp.Flush()
	}
}

// AddBatch adds multiple items
func (bp *BatchProcessor[T, R]) AddBatch(items []T) {
	bp.mu.Lock()
	bp.pending = append(bp.pending, items...)
	shouldFlush := len(bp.pending) >= bp.config.MaxBatchSize
	bp.mu.Unlock()

	if shouldFlush {
		_, _ = bp.Flush()
	}
}

// Flush processes pending items
func (bp *BatchProcessor[T, R]) Flush() ([]R, error) {
	bp.mu.Lock()
	if len(bp.pending) == 0 {
		bp.mu.Unlock()
		return nil, nil
	}

	batch := bp.pending
	bp.pending = make([]T, 0, bp.config.MaxBatchSize)
	bp.mu.Unlock()

	return bp.processBatch(batch)
}

func (bp *BatchProcessor[T, R]) processBatch(batch []T) ([]R, error) {
	var results []R
	var err error

	for attempt := 0; attempt <= bp.config.RetryAttempts; attempt++ {
		results, err = bp.processor(batch)
		if err == nil {
			break
		}
		if attempt < bp.config.RetryAttempts {
			time.Sleep(bp.config.RetryDelay)
		}
	}

	atomic.AddInt64(&bp.stats.BatchesProcessed, 1)
	atomic.AddInt64(&bp.stats.ItemsProcessed, int64(len(batch)))

	if err != nil {
		atomic.AddInt64(&bp.stats.Errors, 1)
	}

	// Update average batch size
	batches := atomic.LoadInt64(&bp.stats.BatchesProcessed)
	items := atomic.LoadInt64(&bp.stats.ItemsProcessed)
	if batches > 0 {
		bp.stats.AvgBatchSize = float64(items) / float64(batches)
	}

	return results, err
}

// Pending returns number of pending items
func (bp *BatchProcessor[T, R]) Pending() int {
	bp.mu.Lock()
	defer bp.mu.Unlock()
	return len(bp.pending)
}

// Stats returns batch statistics
func (bp *BatchProcessor[T, R]) Stats() *BatchStats {
	return &BatchStats{
		BatchesProcessed: atomic.LoadInt64(&bp.stats.BatchesProcessed),
		ItemsProcessed:   atomic.LoadInt64(&bp.stats.ItemsProcessed),
		Errors:           atomic.LoadInt64(&bp.stats.Errors),
		AvgBatchSize:     bp.stats.AvgBatchSize,
	}
}

// Close closes the processor
func (bp *BatchProcessor[T, R]) Close() error {
	bp.closed.Store(true)
	close(bp.flushCh)
	_, err := bp.Flush()
	return err
}

// BatchCollector collects items into batches
type BatchCollector[T any] struct {
	batches   [][]T
	current   []T
	batchSize int
	mu        sync.Mutex
}

// NewBatchCollector creates a new collector
func NewBatchCollector[T any](batchSize int) *BatchCollector[T] {
	return &BatchCollector[T]{
		batches:   make([][]T, 0),
		current:   make([]T, 0, batchSize),
		batchSize: batchSize,
	}
}

// Add adds an item
func (c *BatchCollector[T]) Add(item T) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.current = append(c.current, item)
	if len(c.current) >= c.batchSize {
		c.batches = append(c.batches, c.current)
		c.current = make([]T, 0, c.batchSize)
	}
}

// Batches returns all complete batches
func (c *BatchCollector[T]) Batches() [][]T {
	c.mu.Lock()
	defer c.mu.Unlock()

	result := make([][]T, len(c.batches))
	copy(result, c.batches)
	return result
}

// Remaining returns items not yet in a batch
func (c *BatchCollector[T]) Remaining() []T {
	c.mu.Lock()
	defer c.mu.Unlock()

	result := make([]T, len(c.current))
	copy(result, c.current)
	return result
}

// All returns all batches including partial
func (c *BatchCollector[T]) All() [][]T {
	c.mu.Lock()
	defer c.mu.Unlock()

	result := make([][]T, len(c.batches))
	copy(result, c.batches)
	if len(c.current) > 0 {
		result = append(result, c.current)
	}
	return result
}

// Reset clears the collector
func (c *BatchCollector[T]) Reset() {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.batches = make([][]T, 0)
	c.current = make([]T, 0, c.batchSize)
}

// ParallelBatchExecutor executes batches in parallel
type ParallelBatchExecutor[T any, R any] struct {
	workers int
	process func(T) (R, error)
}

// NewParallelBatchExecutor creates a new executor
func NewParallelBatchExecutor[T any, R any](workers int, process func(T) (R, error)) *ParallelBatchExecutor[T, R] {
	if workers <= 0 {
		workers = 1
	}
	return &ParallelBatchExecutor[T, R]{
		workers: workers,
		process: process,
	}
}

// Execute processes items in parallel
func (e *ParallelBatchExecutor[T, R]) Execute(ctx context.Context, items []T) ([]R, []error) {
	results := make([]R, len(items))
	errors := make([]error, len(items))

	sem := make(chan struct{}, e.workers)
	var wg sync.WaitGroup

	for i, item := range items {
		select {
		case <-ctx.Done():
			errors[i] = ctx.Err()
			continue
		case sem <- struct{}{}:
		}

		wg.Add(1)
		go func(idx int, it T) {
			defer wg.Done()
			defer func() { <-sem }()

			result, err := e.process(it)
			results[idx] = result
			errors[idx] = err
		}(i, item)
	}

	wg.Wait()
	return results, errors
}

// Workers returns worker count
func (e *ParallelBatchExecutor[T, R]) Workers() int {
	return e.workers
}
