// internal/streaming/stream_test.go
package streaming

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStreamConfig_Validate(t *testing.T) {
	t.Run("valid config passes validation", func(t *testing.T) {
		config := &StreamConfig{
			Name:            "object-events",
			RetentionPeriod: 24 * time.Hour,
			MaxSize:         1024 * 1024 * 100, // 100MB
			Partitions:      4,
		}
		err := config.Validate()
		assert.NoError(t, err)
	})

	t.Run("rejects empty name", func(t *testing.T) {
		config := &StreamConfig{
			RetentionPeriod: 24 * time.Hour,
		}
		err := config.Validate()
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "name")
	})

	t.Run("defaults retention to 7 days", func(t *testing.T) {
		config := &StreamConfig{Name: "test-stream"}
		config.ApplyDefaults()
		assert.Equal(t, 7*24*time.Hour, config.RetentionPeriod)
	})

	t.Run("defaults partitions to 1", func(t *testing.T) {
		config := &StreamConfig{Name: "test-stream"}
		config.ApplyDefaults()
		assert.Equal(t, 1, config.Partitions)
	})
}

func TestNewStreamManager(t *testing.T) {
	t.Run("creates manager", func(t *testing.T) {
		manager := NewStreamManager(nil)
		assert.NotNil(t, manager)
	})
}

func TestStreamManager_CreateStream(t *testing.T) {
	manager := NewStreamManager(nil)

	t.Run("creates stream", func(t *testing.T) {
		config := &StreamConfig{Name: "test-stream", Partitions: 2}
		stream, err := manager.CreateStream(config)
		require.NoError(t, err)
		assert.Equal(t, "test-stream", stream.Name)
		assert.Equal(t, 2, stream.Partitions)
	})

	t.Run("rejects duplicate stream", func(t *testing.T) {
		config := &StreamConfig{Name: "duplicate-stream"}
		_, _ = manager.CreateStream(config)

		_, err := manager.CreateStream(config)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "exists")
	})
}

func TestStreamManager_DeleteStream(t *testing.T) {
	manager := NewStreamManager(nil)

	t.Run("deletes stream", func(t *testing.T) {
		config := &StreamConfig{Name: "to-delete"}
		_, _ = manager.CreateStream(config)

		err := manager.DeleteStream("to-delete")
		assert.NoError(t, err)
		assert.Nil(t, manager.GetStream("to-delete"))
	})

	t.Run("returns error for unknown stream", func(t *testing.T) {
		err := manager.DeleteStream("unknown")
		assert.Error(t, err)
	})
}

func TestStreamManager_Publish(t *testing.T) {
	manager := NewStreamManager(nil)
	config := &StreamConfig{Name: "pub-stream", Partitions: 2}
	_, _ = manager.CreateStream(config)

	t.Run("publishes message", func(t *testing.T) {
		msg := &Message{
			Key:   "user-123",
			Value: []byte(`{"action":"created"}`),
			Headers: map[string]string{
				"content-type": "application/json",
			},
		}

		offset, err := manager.Publish(context.Background(), "pub-stream", msg)
		require.NoError(t, err)
		assert.GreaterOrEqual(t, offset, int64(0))
	})

	t.Run("assigns partition by key", func(t *testing.T) {
		msg1 := &Message{Key: "same-key", Value: []byte("data1")}
		msg2 := &Message{Key: "same-key", Value: []byte("data2")}

		offset1, _ := manager.Publish(context.Background(), "pub-stream", msg1)
		offset2, _ := manager.Publish(context.Background(), "pub-stream", msg2)

		// Same key should go to same partition
		p1 := manager.GetPartitionForOffset("pub-stream", offset1)
		p2 := manager.GetPartitionForOffset("pub-stream", offset2)
		assert.Equal(t, p1, p2)
	})

	t.Run("returns error for unknown stream", func(t *testing.T) {
		msg := &Message{Value: []byte("test")}
		_, err := manager.Publish(context.Background(), "unknown-stream", msg)
		assert.Error(t, err)
	})
}

func TestStreamManager_Subscribe(t *testing.T) {
	manager := NewStreamManager(nil)
	config := &StreamConfig{Name: "sub-stream", Partitions: 1}
	_, _ = manager.CreateStream(config)

	t.Run("subscribes to stream", func(t *testing.T) {
		sub, err := manager.Subscribe("sub-stream", "consumer-group-1", SubscribeOptions{})
		require.NoError(t, err)
		assert.NotNil(t, sub)
		defer func() { _ = sub.Close() }()
	})

	t.Run("receives published messages", func(t *testing.T) {
		sub, _ := manager.Subscribe("sub-stream", "consumer-group-2", SubscribeOptions{
			StartOffset: OffsetEarliest,
		})
		defer func() { _ = sub.Close() }()

		// Publish a message
		msg := &Message{Key: "test", Value: []byte("hello")}
		_, _ = manager.Publish(context.Background(), "sub-stream", msg)

		// Receive it
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()

		received, err := sub.Receive(ctx)
		require.NoError(t, err)
		assert.Equal(t, []byte("hello"), received.Value)
	})
}

func TestStreamManager_ConsumerGroups(t *testing.T) {
	manager := NewStreamManager(nil)
	config := &StreamConfig{Name: "cg-stream", Partitions: 4}
	_, _ = manager.CreateStream(config)

	// Publish messages
	for i := 0; i < 10; i++ {
		msg := &Message{Key: string(rune('a' + i)), Value: []byte("data")}
		_, _ = manager.Publish(context.Background(), "cg-stream", msg)
	}

	t.Run("distributes partitions among consumers", func(t *testing.T) {
		sub1, _ := manager.Subscribe("cg-stream", "shared-group", SubscribeOptions{StartOffset: OffsetEarliest})
		sub2, _ := manager.Subscribe("cg-stream", "shared-group", SubscribeOptions{StartOffset: OffsetEarliest})
		defer func() { _ = sub1.Close() }()
		defer func() { _ = sub2.Close() }()

		// Each should get assigned partitions
		assert.NotEmpty(t, sub1.AssignedPartitions())
		assert.NotEmpty(t, sub2.AssignedPartitions())
	})

	t.Run("tracks consumer group offsets", func(t *testing.T) {
		sub, _ := manager.Subscribe("cg-stream", "offset-group", SubscribeOptions{StartOffset: OffsetEarliest})
		defer func() { _ = sub.Close() }()

		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()

		msg, _ := sub.Receive(ctx)
		err := sub.Commit(msg)
		assert.NoError(t, err)

		offset := manager.GetCommittedOffset("cg-stream", "offset-group", msg.Partition)
		assert.Equal(t, msg.Offset, offset)
	})
}

func TestStreamManager_Replay(t *testing.T) {
	manager := NewStreamManager(nil)
	config := &StreamConfig{Name: "replay-stream", Partitions: 1}
	_, _ = manager.CreateStream(config)

	// Publish messages
	for i := 0; i < 5; i++ {
		msg := &Message{Value: []byte{byte(i)}}
		_, _ = manager.Publish(context.Background(), "replay-stream", msg)
	}

	t.Run("replays from specific offset", func(t *testing.T) {
		sub, _ := manager.Subscribe("replay-stream", "replay-group", SubscribeOptions{
			StartOffset: 2, // Start from offset 2
		})
		defer func() { _ = sub.Close() }()

		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()

		msg, err := sub.Receive(ctx)
		require.NoError(t, err)
		assert.Equal(t, int64(2), msg.Offset)
	})

	t.Run("replays from earliest", func(t *testing.T) {
		sub, _ := manager.Subscribe("replay-stream", "earliest-group", SubscribeOptions{
			StartOffset: OffsetEarliest,
		})
		defer func() { _ = sub.Close() }()

		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()

		msg, _ := sub.Receive(ctx)
		assert.Equal(t, int64(0), msg.Offset)
	})

	t.Run("starts from latest", func(t *testing.T) {
		sub, _ := manager.Subscribe("replay-stream", "latest-group", SubscribeOptions{
			StartOffset: OffsetLatest,
		})
		defer func() { _ = sub.Close() }()

		// Publish new message
		newMsg := &Message{Value: []byte("new")}
		_, _ = manager.Publish(context.Background(), "replay-stream", newMsg)

		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()

		msg, _ := sub.Receive(ctx)
		assert.Equal(t, []byte("new"), msg.Value)
	})
}

func TestStreamManager_Retention(t *testing.T) {
	manager := NewStreamManager(nil)

	t.Run("enforces retention by size", func(t *testing.T) {
		config := &StreamConfig{
			Name:    "size-retention",
			MaxSize: 100, // Very small for testing
		}
		_, _ = manager.CreateStream(config)

		// Publish until exceeds size
		for i := 0; i < 20; i++ {
			msg := &Message{Value: []byte("some data that takes space")}
			_, _ = manager.Publish(context.Background(), "size-retention", msg)
		}

		stats := manager.GetStreamStats("size-retention")
		assert.LessOrEqual(t, stats.CurrentSize, int64(200)) // Some buffer
	})
}

func TestMessage(t *testing.T) {
	t.Run("has timestamp", func(t *testing.T) {
		msg := NewMessage("key", []byte("value"))
		assert.False(t, msg.Timestamp.IsZero())
	})

	t.Run("supports headers", func(t *testing.T) {
		msg := NewMessage("key", []byte("value"))
		msg.Headers["trace-id"] = "abc123"
		assert.Equal(t, "abc123", msg.Headers["trace-id"])
	})
}

func TestSubscription_Concurrent(t *testing.T) {
	manager := NewStreamManager(nil)
	config := &StreamConfig{Name: "concurrent-stream", Partitions: 1}
	_, _ = manager.CreateStream(config)

	t.Run("handles concurrent publishes", func(t *testing.T) {
		var wg sync.WaitGroup
		for i := 0; i < 100; i++ {
			wg.Add(1)
			go func(n int) {
				defer wg.Done()
				msg := &Message{Value: []byte{byte(n)}}
				_, _ = manager.Publish(context.Background(), "concurrent-stream", msg)
			}(i)
		}
		wg.Wait()

		stats := manager.GetStreamStats("concurrent-stream")
		assert.Equal(t, int64(100), stats.MessageCount)
	})
}

func TestStreamStats(t *testing.T) {
	manager := NewStreamManager(nil)
	config := &StreamConfig{Name: "stats-stream", Partitions: 2}
	_, _ = manager.CreateStream(config)

	for i := 0; i < 10; i++ {
		msg := &Message{Value: []byte("test")}
		_, _ = manager.Publish(context.Background(), "stats-stream", msg)
	}

	t.Run("returns stream statistics", func(t *testing.T) {
		stats := manager.GetStreamStats("stats-stream")
		assert.Equal(t, int64(10), stats.MessageCount)
		assert.Equal(t, 2, stats.Partitions)
		assert.Greater(t, stats.CurrentSize, int64(0))
	})
}

func TestDefaultStreamConfig(t *testing.T) {
	t.Run("provides sensible defaults", func(t *testing.T) {
		config := DefaultStreamConfig()
		assert.Equal(t, 7*24*time.Hour, config.RetentionPeriod)
		assert.Equal(t, int64(1024*1024*1024), config.MaxSize) // 1GB
		assert.Equal(t, 1, config.Partitions)
	})
}
