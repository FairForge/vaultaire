// internal/drivers/queue.go - Proper queue implementation
package drivers

import (
	"context"
	"errors"
	"sync"

	"go.uber.org/zap"
)

// ErrQueueFull is returned when the queue is at capacity
var ErrQueueFull = errors.New("request queue is full")

// ErrQueueClosed is returned when submitting to a closed queue
var ErrQueueClosed = errors.New("request queue is closed")

// job represents a queued work item
type job struct {
	fn     func() error
	result chan error
}

// RequestQueue manages request processing with a worker pool
type RequestQueue struct {
	jobs   chan *job
	closed chan struct{}
	wg     sync.WaitGroup
	logger *zap.Logger
}

// NewRequestQueue creates a new request queue
func NewRequestQueue(capacity, workers int, logger *zap.Logger) *RequestQueue {
	q := &RequestQueue{
		jobs:   make(chan *job, capacity),
		closed: make(chan struct{}),
		logger: logger,
	}

	// Start workers
	for i := 0; i < workers; i++ {
		q.wg.Add(1)
		go q.worker(i)
	}

	return q
}

// worker processes jobs from the queue
func (q *RequestQueue) worker(id int) {
	defer q.wg.Done()

	for {
		select {
		case job, ok := <-q.jobs:
			if !ok {
				return // Channel closed
			}

			// Process the job
			err := job.fn()

			// Send result back (non-blocking in case submitter gave up)
			select {
			case job.result <- err:
			default:
			}

			q.logger.Debug("job processed",
				zap.Int("worker", id),
				zap.Error(err))

		case <-q.closed:
			return
		}
	}
}

// Submit adds a job to the queue
func (q *RequestQueue) Submit(ctx context.Context, priority int, fn func() error) error {
	// Priority is ignored in this simple implementation
	// Could be added with a heap-based priority queue

	job := &job{
		fn:     fn,
		result: make(chan error, 1),
	}

	// Try to queue the job
	select {
	case q.jobs <- job:
		// Job queued successfully
	case <-ctx.Done():
		return ctx.Err()
	case <-q.closed:
		return ErrQueueClosed
	default:
		// Queue is full
		return ErrQueueFull
	}

	// Wait for result
	select {
	case err := <-job.result:
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-q.closed:
		return ErrQueueClosed
	}
}

// Close shuts down the queue gracefully
func (q *RequestQueue) Close() {
	close(q.closed)
	close(q.jobs)
	q.wg.Wait()
}
