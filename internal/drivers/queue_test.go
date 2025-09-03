// internal/drivers/queue_test.go
package drivers

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestRequestQueue(t *testing.T) {
	t.Run("processes requests successfully", func(t *testing.T) {
		queue := NewRequestQueue(10, 2, zap.NewNop())
		defer queue.Close()

		var counter int32
		err := queue.Submit(context.Background(), 1, func() error {
			atomic.AddInt32(&counter, 1)
			return nil
		})

		require.NoError(t, err)
		assert.Equal(t, int32(1), counter)
	})

	t.Run("respects queue capacity", func(t *testing.T) {
		// Create queue with NO workers and capacity 2
		// This way jobs queue up but don't get processed
		queue := &RequestQueue{
			jobs:   make(chan *job, 2), // capacity 2
			closed: make(chan struct{}),
			logger: zap.NewNop(),
		}
		// Don't start workers so jobs just sit in queue

		// Submit 2 jobs to fill the queue
		job1 := &job{
			fn:     func() error { return nil },
			result: make(chan error, 1),
		}
		job2 := &job{
			fn:     func() error { return nil },
			result: make(chan error, 1),
		}

		// These should succeed (queue has capacity 2)
		queue.jobs <- job1
		queue.jobs <- job2

		// Now queue is full, try to submit another
		ctx := context.Background()
		err := queue.Submit(ctx, 1, func() error {
			return nil
		})

		// Should get ErrQueueFull
		assert.ErrorIs(t, err, ErrQueueFull)

		close(queue.closed)
		close(queue.jobs)
	})

	t.Run("handles concurrent submissions", func(t *testing.T) {
		queue := NewRequestQueue(100, 5, zap.NewNop())
		defer queue.Close()

		var processed int32
		var wg sync.WaitGroup

		for i := 0; i < 10; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_ = queue.Submit(context.Background(), 1, func() error {
					atomic.AddInt32(&processed, 1)
					return nil
				})
			}()
		}

		wg.Wait()
		assert.Equal(t, int32(10), processed)
	})

	t.Run("returns function errors", func(t *testing.T) {
		queue := NewRequestQueue(10, 2, zap.NewNop())
		defer queue.Close()

		expectedErr := errors.New("test error")
		err := queue.Submit(context.Background(), 1, func() error {
			return expectedErr
		})

		assert.ErrorIs(t, err, expectedErr)
	})

	t.Run("respects context cancellation", func(t *testing.T) {
		queue := NewRequestQueue(10, 2, zap.NewNop())
		defer queue.Close()

		ctx, cancel := context.WithCancel(context.Background())
		cancel() // Cancel immediately

		err := queue.Submit(ctx, 1, func() error {
			return nil
		})

		assert.ErrorIs(t, err, context.Canceled)
	})
}
