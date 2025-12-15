// internal/perf/async.go
package perf

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// AsyncConfig configures async processing
type AsyncConfig struct {
	WorkerCount     int
	QueueSize       int
	ShutdownTimeout time.Duration
}

// DefaultAsyncConfig returns default configuration
func DefaultAsyncConfig() *AsyncConfig {
	return &AsyncConfig{
		WorkerCount:     10,
		QueueSize:       1000,
		ShutdownTimeout: 30 * time.Second,
	}
}

// AsyncProcessor processes tasks asynchronously
type AsyncProcessor[T any, R any] struct {
	config  *AsyncConfig
	process func(T) (R, error)
	tasks   chan *asyncTask[T, R]
	stats   *AsyncStats
	wg      sync.WaitGroup
	closed  atomic.Bool
}

type asyncTask[T any, R any] struct {
	input  T
	result chan asyncResult[R]
}

type asyncResult[R any] struct {
	value R
	err   error
}

// AsyncStats tracks async processing statistics
type AsyncStats struct {
	TasksSubmitted int64
	TasksCompleted int64
	TasksFailed    int64
	TasksDropped   int64
	QueueDepth     int64
	AvgProcessTime int64
	totalTime      int64
}

// NewAsyncProcessor creates a new async processor
func NewAsyncProcessor[T any, R any](config *AsyncConfig, process func(T) (R, error)) *AsyncProcessor[T, R] {
	if config == nil {
		config = DefaultAsyncConfig()
	}

	ap := &AsyncProcessor[T, R]{
		config:  config,
		process: process,
		tasks:   make(chan *asyncTask[T, R], config.QueueSize),
		stats:   &AsyncStats{},
	}

	for i := 0; i < config.WorkerCount; i++ {
		ap.wg.Add(1)
		go ap.worker()
	}

	return ap
}

func (ap *AsyncProcessor[T, R]) worker() {
	defer ap.wg.Done()

	for task := range ap.tasks {
		atomic.AddInt64(&ap.stats.QueueDepth, -1)

		start := time.Now()
		result, err := ap.process(task.input)
		elapsed := time.Since(start).Nanoseconds()

		atomic.AddInt64(&ap.stats.totalTime, elapsed)
		atomic.AddInt64(&ap.stats.TasksCompleted, 1)

		if err != nil {
			atomic.AddInt64(&ap.stats.TasksFailed, 1)
		}

		// Update average
		completed := atomic.LoadInt64(&ap.stats.TasksCompleted)
		total := atomic.LoadInt64(&ap.stats.totalTime)
		if completed > 0 {
			atomic.StoreInt64(&ap.stats.AvgProcessTime, total/completed)
		}

		task.result <- asyncResult[R]{value: result, err: err}
		close(task.result)
	}
}

// Submit submits a task and returns a future
func (ap *AsyncProcessor[T, R]) Submit(input T) *Future[R] {
	if ap.closed.Load() {
		f := &Future[R]{done: make(chan struct{})}
		close(f.done)
		f.err = ErrProcessorClosed
		return f
	}

	task := &asyncTask[T, R]{
		input:  input,
		result: make(chan asyncResult[R], 1),
	}

	select {
	case ap.tasks <- task:
		atomic.AddInt64(&ap.stats.TasksSubmitted, 1)
		atomic.AddInt64(&ap.stats.QueueDepth, 1)
		return newFuture(task.result)
	default:
		atomic.AddInt64(&ap.stats.TasksDropped, 1)
		f := &Future[R]{done: make(chan struct{})}
		close(f.done)
		f.err = ErrQueueFull
		return f
	}
}

// SubmitWait submits and waits for result
func (ap *AsyncProcessor[T, R]) SubmitWait(ctx context.Context, input T) (R, error) {
	future := ap.Submit(input)
	return future.Get(ctx)
}

// Stats returns processing statistics
func (ap *AsyncProcessor[T, R]) Stats() *AsyncStats {
	return &AsyncStats{
		TasksSubmitted: atomic.LoadInt64(&ap.stats.TasksSubmitted),
		TasksCompleted: atomic.LoadInt64(&ap.stats.TasksCompleted),
		TasksFailed:    atomic.LoadInt64(&ap.stats.TasksFailed),
		TasksDropped:   atomic.LoadInt64(&ap.stats.TasksDropped),
		QueueDepth:     atomic.LoadInt64(&ap.stats.QueueDepth),
		AvgProcessTime: atomic.LoadInt64(&ap.stats.AvgProcessTime),
	}
}

// Close shuts down the processor
func (ap *AsyncProcessor[T, R]) Close() error {
	if ap.closed.Swap(true) {
		return ErrProcessorClosed
	}
	close(ap.tasks)
	ap.wg.Wait()
	return nil
}

// QueueDepth returns current queue depth
func (ap *AsyncProcessor[T, R]) QueueDepth() int {
	return int(atomic.LoadInt64(&ap.stats.QueueDepth))
}

// ErrProcessorClosed indicates processor is closed
var ErrProcessorClosed = &AsyncError{msg: "processor closed"}

// ErrQueueFull indicates queue is full
var ErrQueueFull = &AsyncError{msg: "queue full"}

// AsyncError represents an async error
type AsyncError struct {
	msg string
}

func (e *AsyncError) Error() string {
	return e.msg
}

// Future represents a future result
type Future[R any] struct {
	result R
	err    error
	done   chan struct{}
}

func newFuture[R any](resultCh chan asyncResult[R]) *Future[R] {
	f := &Future[R]{
		done: make(chan struct{}),
	}

	go func() {
		result := <-resultCh
		f.result = result.value
		f.err = result.err
		close(f.done)
	}()

	return f
}

// Get waits for and returns the result
func (f *Future[R]) Get(ctx context.Context) (R, error) {
	select {
	case <-f.done:
		return f.result, f.err
	case <-ctx.Done():
		var zero R
		return zero, ctx.Err()
	}
}

// Done returns a channel that closes when result is ready
func (f *Future[R]) Done() <-chan struct{} {
	return f.done
}

// IsReady returns whether result is ready
func (f *Future[R]) IsReady() bool {
	select {
	case <-f.done:
		return true
	default:
		return false
	}
}

// WorkerPool manages a pool of workers
type WorkerPool struct {
	workers int
	tasks   chan func()
	wg      sync.WaitGroup
	closed  atomic.Bool
}

// NewWorkerPool creates a new worker pool
func NewWorkerPool(workers, queueSize int) *WorkerPool {
	if workers <= 0 {
		workers = 1
	}

	wp := &WorkerPool{
		workers: workers,
		tasks:   make(chan func(), queueSize),
	}

	for i := 0; i < workers; i++ {
		wp.wg.Add(1)
		go wp.worker()
	}

	return wp
}

func (wp *WorkerPool) worker() {
	defer wp.wg.Done()
	for task := range wp.tasks {
		task()
	}
}

// Submit submits a task
func (wp *WorkerPool) Submit(task func()) bool {
	if wp.closed.Load() {
		return false
	}

	select {
	case wp.tasks <- task:
		return true
	default:
		return false
	}
}

// Close closes the pool
func (wp *WorkerPool) Close() {
	if wp.closed.Swap(true) {
		return
	}
	close(wp.tasks)
	wp.wg.Wait()
}

// Workers returns worker count
func (wp *WorkerPool) Workers() int {
	return wp.workers
}

// Debouncer debounces function calls
type Debouncer struct {
	mu       sync.Mutex
	timer    *time.Timer
	duration time.Duration
}

// NewDebouncer creates a new debouncer
func NewDebouncer(duration time.Duration) *Debouncer {
	return &Debouncer{duration: duration}
}

// Debounce debounces a function call
func (d *Debouncer) Debounce(fn func()) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.timer != nil {
		d.timer.Stop()
	}

	d.timer = time.AfterFunc(d.duration, fn)
}

// Cancel cancels pending debounced call
func (d *Debouncer) Cancel() {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.timer != nil {
		d.timer.Stop()
		d.timer = nil
	}
}

// Throttler throttles function calls
type Throttler struct {
	mu       sync.Mutex
	lastCall time.Time
	interval time.Duration
}

// NewThrottler creates a new throttler
func NewThrottler(interval time.Duration) *Throttler {
	return &Throttler{interval: interval}
}

// Allow returns whether call is allowed
func (t *Throttler) Allow() bool {
	t.mu.Lock()
	defer t.mu.Unlock()

	now := time.Now()
	if now.Sub(t.lastCall) >= t.interval {
		t.lastCall = now
		return true
	}
	return false
}

// Throttle executes function if allowed
func (t *Throttler) Throttle(fn func()) bool {
	if t.Allow() {
		fn()
		return true
	}
	return false
}
