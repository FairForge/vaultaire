package queue

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestQueueConfig_Validate(t *testing.T) {
	t.Run("valid config passes validation", func(t *testing.T) {
		config := &QueueConfig{
			Name:              "tasks",
			MaxRetries:        3,
			VisibilityTimeout: 30 * time.Second,
		}
		err := config.Validate()
		assert.NoError(t, err)
	})

	t.Run("rejects empty name", func(t *testing.T) {
		config := &QueueConfig{}
		err := config.Validate()
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "name")
	})

	t.Run("applies defaults", func(t *testing.T) {
		config := &QueueConfig{Name: "test"}
		config.ApplyDefaults()
		assert.Equal(t, 3, config.MaxRetries)
		assert.Equal(t, 30*time.Second, config.VisibilityTimeout)
	})
}

func TestNewQueueManager(t *testing.T) {
	t.Run("creates manager", func(t *testing.T) {
		manager := NewQueueManager(nil)
		assert.NotNil(t, manager)
	})
}

func TestQueueManager_CreateQueue(t *testing.T) {
	manager := NewQueueManager(nil)

	t.Run("creates queue", func(t *testing.T) {
		config := &QueueConfig{Name: "test-queue"}
		queue, err := manager.CreateQueue(config)
		require.NoError(t, err)
		assert.Equal(t, "test-queue", queue.Name)
	})

	t.Run("rejects duplicate queue", func(t *testing.T) {
		config := &QueueConfig{Name: "dup-queue"}
		_, _ = manager.CreateQueue(config)
		_, err := manager.CreateQueue(config)
		assert.Error(t, err)
	})
}

func TestQueueManager_DeleteQueue(t *testing.T) {
	manager := NewQueueManager(nil)

	t.Run("deletes queue", func(t *testing.T) {
		config := &QueueConfig{Name: "to-delete"}
		_, _ = manager.CreateQueue(config)
		err := manager.DeleteQueue("to-delete")
		assert.NoError(t, err)
	})

	t.Run("returns error for unknown queue", func(t *testing.T) {
		err := manager.DeleteQueue("unknown")
		assert.Error(t, err)
	})
}

func TestQueueManager_Enqueue(t *testing.T) {
	manager := NewQueueManager(nil)
	_, _ = manager.CreateQueue(&QueueConfig{Name: "enqueue-test"})

	t.Run("enqueues message", func(t *testing.T) {
		msg := &Message{
			Body: []byte(`{"task":"process"}`),
			Metadata: map[string]string{
				"type": "processing",
			},
		}
		id, err := manager.Enqueue(context.Background(), "enqueue-test", msg)
		require.NoError(t, err)
		assert.NotEmpty(t, id)
	})

	t.Run("returns error for unknown queue", func(t *testing.T) {
		msg := &Message{Body: []byte("test")}
		_, err := manager.Enqueue(context.Background(), "unknown", msg)
		assert.Error(t, err)
	})
}

func TestQueueManager_Dequeue(t *testing.T) {
	manager := NewQueueManager(nil)
	_, _ = manager.CreateQueue(&QueueConfig{
		Name:              "dequeue-test",
		VisibilityTimeout: 100 * time.Millisecond,
	})

	t.Run("dequeues message", func(t *testing.T) {
		msg := &Message{Body: []byte("task data")}
		_, _ = manager.Enqueue(context.Background(), "dequeue-test", msg)

		received, err := manager.Dequeue(context.Background(), "dequeue-test")
		require.NoError(t, err)
		assert.Equal(t, []byte("task data"), received.Body)
		assert.NotEmpty(t, received.ReceiptHandle)
	})

	t.Run("returns nil when queue empty", func(t *testing.T) {
		_, _ = manager.CreateQueue(&QueueConfig{Name: "empty-queue"})
		msg, err := manager.Dequeue(context.Background(), "empty-queue")
		assert.NoError(t, err)
		assert.Nil(t, msg)
	})

	t.Run("message invisible after dequeue", func(t *testing.T) {
		_, _ = manager.CreateQueue(&QueueConfig{
			Name:              "visibility-test",
			VisibilityTimeout: 200 * time.Millisecond,
		})
		_, _ = manager.Enqueue(context.Background(), "visibility-test", &Message{Body: []byte("x")})

		// First dequeue succeeds
		msg1, _ := manager.Dequeue(context.Background(), "visibility-test")
		assert.NotNil(t, msg1)

		// Second dequeue returns nil (message invisible)
		msg2, _ := manager.Dequeue(context.Background(), "visibility-test")
		assert.Nil(t, msg2)
	})
}

func TestQueueManager_Acknowledge(t *testing.T) {
	manager := NewQueueManager(nil)
	_, _ = manager.CreateQueue(&QueueConfig{Name: "ack-test"})

	t.Run("acknowledges message", func(t *testing.T) {
		_, _ = manager.Enqueue(context.Background(), "ack-test", &Message{Body: []byte("ack me")})
		msg, _ := manager.Dequeue(context.Background(), "ack-test")

		err := manager.Acknowledge(context.Background(), "ack-test", msg.ReceiptHandle)
		assert.NoError(t, err)

		// Message should be gone
		stats := manager.GetQueueStats("ack-test")
		assert.Equal(t, int64(0), stats.Messages)
	})

	t.Run("returns error for invalid receipt", func(t *testing.T) {
		err := manager.Acknowledge(context.Background(), "ack-test", "invalid-receipt")
		assert.Error(t, err)
	})
}

func TestQueueManager_Nack(t *testing.T) {
	manager := NewQueueManager(nil)
	_, _ = manager.CreateQueue(&QueueConfig{
		Name:              "nack-test",
		VisibilityTimeout: 50 * time.Millisecond,
	})

	t.Run("nack makes message visible immediately", func(t *testing.T) {
		_, _ = manager.Enqueue(context.Background(), "nack-test", &Message{Body: []byte("retry me")})
		msg, _ := manager.Dequeue(context.Background(), "nack-test")

		err := manager.Nack(context.Background(), "nack-test", msg.ReceiptHandle)
		assert.NoError(t, err)

		// Message should be immediately available
		msg2, _ := manager.Dequeue(context.Background(), "nack-test")
		assert.NotNil(t, msg2)
	})
}

func TestQueueManager_Retry(t *testing.T) {
	manager := NewQueueManager(nil)
	_, _ = manager.CreateQueue(&QueueConfig{
		Name:              "retry-test",
		MaxRetries:        3,
		VisibilityTimeout: 50 * time.Millisecond,
	})

	t.Run("tracks retry count", func(t *testing.T) {
		_, _ = manager.Enqueue(context.Background(), "retry-test", &Message{Body: []byte("retry")})

		// Dequeue and nack multiple times
		for i := 0; i < 3; i++ {
			msg, _ := manager.Dequeue(context.Background(), "retry-test")
			if msg != nil {
				assert.Equal(t, i, msg.RetryCount)
				_ = manager.Nack(context.Background(), "retry-test", msg.ReceiptHandle)
			}
			time.Sleep(10 * time.Millisecond)
		}
	})
}

func TestQueueManager_DeadLetterQueue(t *testing.T) {
	manager := NewQueueManager(nil)
	_, _ = manager.CreateQueue(&QueueConfig{Name: "dlq"})
	_, _ = manager.CreateQueue(&QueueConfig{
		Name:              "main-queue",
		MaxRetries:        2,
		DeadLetterQueue:   "dlq",
		VisibilityTimeout: 20 * time.Millisecond,
	})

	t.Run("moves to DLQ after max retries", func(t *testing.T) {
		_, _ = manager.Enqueue(context.Background(), "main-queue", &Message{Body: []byte("will fail")})

		// Exhaust retries
		for i := 0; i < 3; i++ {
			msg, _ := manager.Dequeue(context.Background(), "main-queue")
			if msg != nil {
				_ = manager.Nack(context.Background(), "main-queue", msg.ReceiptHandle)
			}
			time.Sleep(25 * time.Millisecond)
		}

		// Should be in DLQ
		dlqStats := manager.GetQueueStats("dlq")
		assert.Equal(t, int64(1), dlqStats.Messages)
	})
}

func TestQueueManager_DelayedMessages(t *testing.T) {
	manager := NewQueueManager(nil)
	_, _ = manager.CreateQueue(&QueueConfig{Name: "delayed-test"})

	t.Run("delays message delivery", func(t *testing.T) {
		msg := &Message{
			Body:  []byte("delayed"),
			Delay: 100 * time.Millisecond,
		}
		_, _ = manager.Enqueue(context.Background(), "delayed-test", msg)

		// Should not be available immediately
		immediate, _ := manager.Dequeue(context.Background(), "delayed-test")
		assert.Nil(t, immediate)

		// Wait for delay
		time.Sleep(150 * time.Millisecond)

		delayed, _ := manager.Dequeue(context.Background(), "delayed-test")
		assert.NotNil(t, delayed)
	})
}

func TestQueueManager_Priority(t *testing.T) {
	manager := NewQueueManager(nil)
	_, _ = manager.CreateQueue(&QueueConfig{
		Name:           "priority-test",
		PriorityLevels: 3,
	})

	t.Run("processes high priority first", func(t *testing.T) {
		// Enqueue low priority first
		_, _ = manager.Enqueue(context.Background(), "priority-test", &Message{Body: []byte("low"), Priority: 0})
		_, _ = manager.Enqueue(context.Background(), "priority-test", &Message{Body: []byte("high"), Priority: 2})
		_, _ = manager.Enqueue(context.Background(), "priority-test", &Message{Body: []byte("medium"), Priority: 1})

		// Should get high priority first
		msg1, _ := manager.Dequeue(context.Background(), "priority-test")
		assert.Equal(t, []byte("high"), msg1.Body)

		msg2, _ := manager.Dequeue(context.Background(), "priority-test")
		assert.Equal(t, []byte("medium"), msg2.Body)

		msg3, _ := manager.Dequeue(context.Background(), "priority-test")
		assert.Equal(t, []byte("low"), msg3.Body)
	})
}

func TestQueueManager_BatchOperations(t *testing.T) {
	manager := NewQueueManager(nil)
	_, _ = manager.CreateQueue(&QueueConfig{Name: "batch-test"})

	t.Run("batch enqueue", func(t *testing.T) {
		messages := []*Message{
			{Body: []byte("msg1")},
			{Body: []byte("msg2")},
			{Body: []byte("msg3")},
		}
		ids, err := manager.EnqueueBatch(context.Background(), "batch-test", messages)
		require.NoError(t, err)
		assert.Len(t, ids, 3)
	})

	t.Run("batch dequeue", func(t *testing.T) {
		messages, err := manager.DequeueBatch(context.Background(), "batch-test", 5)
		require.NoError(t, err)
		assert.Len(t, messages, 3)
	})
}

func TestQueueManager_Concurrent(t *testing.T) {
	manager := NewQueueManager(nil)
	_, _ = manager.CreateQueue(&QueueConfig{Name: "concurrent-test"})

	t.Run("handles concurrent operations", func(t *testing.T) {
		var wg sync.WaitGroup
		var enqueued int64
		var dequeued int64

		// Concurrent enqueuers
		for i := 0; i < 10; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for j := 0; j < 100; j++ {
					_, err := manager.Enqueue(context.Background(), "concurrent-test", &Message{Body: []byte("x")})
					if err == nil {
						atomic.AddInt64(&enqueued, 1)
					}
				}
			}()
		}

		// Concurrent dequeuers
		for i := 0; i < 5; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for j := 0; j < 200; j++ {
					msg, _ := manager.Dequeue(context.Background(), "concurrent-test")
					if msg != nil {
						_ = manager.Acknowledge(context.Background(), "concurrent-test", msg.ReceiptHandle)
						atomic.AddInt64(&dequeued, 1)
					}
				}
			}()
		}

		wg.Wait()
		assert.Equal(t, int64(1000), enqueued)
	})
}

func TestQueueStats(t *testing.T) {
	manager := NewQueueManager(nil)
	_, _ = manager.CreateQueue(&QueueConfig{Name: "stats-test"})

	for i := 0; i < 5; i++ {
		_, _ = manager.Enqueue(context.Background(), "stats-test", &Message{Body: []byte("x")})
	}

	t.Run("returns queue statistics", func(t *testing.T) {
		stats := manager.GetQueueStats("stats-test")
		assert.Equal(t, int64(5), stats.Messages)
		assert.Equal(t, int64(0), stats.InFlight)
	})
}

func TestMessage(t *testing.T) {
	t.Run("has ID and timestamp", func(t *testing.T) {
		msg := NewMessage([]byte("test"))
		assert.NotEmpty(t, msg.ID)
		assert.False(t, msg.Timestamp.IsZero())
	})
}

func TestDefaultQueueConfig(t *testing.T) {
	t.Run("provides sensible defaults", func(t *testing.T) {
		config := DefaultQueueConfig()
		assert.Equal(t, 3, config.MaxRetries)
		assert.Equal(t, 30*time.Second, config.VisibilityTimeout)
		assert.Equal(t, 1, config.PriorityLevels)
	})
}
