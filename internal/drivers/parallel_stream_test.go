// internal/drivers/parallel_stream_test.go
package drivers

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestParallelStreamManager(t *testing.T) {
	t.Run("manages concurrent streams", func(t *testing.T) {
		// Arrange
		manager := NewStreamManager(3, zap.NewNop()) // Max 3 concurrent streams
		ctx := context.Background()

		// Act - Try to acquire 4 streams (should block on 4th)
		s1, err1 := manager.AcquireStream(ctx)
		s2, err2 := manager.AcquireStream(ctx)
		s3, err3 := manager.AcquireStream(ctx)

		// This should block until we release one
		acquired := make(chan bool)
		go func() {
			s4, err := manager.AcquireStream(ctx)
			require.NoError(t, err)
			defer s4.Release()
			acquired <- true
		}()

		// Assert - First 3 should succeed
		require.NoError(t, err1)
		require.NoError(t, err2)
		require.NoError(t, err3)

		// Channel shouldn't receive yet
		select {
		case <-acquired:
			t.Fatal("Should not acquire 4th stream yet")
		default:
			// Expected
		}

		// Release one stream
		s1.Release()

		// Now 4th should acquire
		select {
		case <-acquired:
			// Success
		case <-time.After(time.Second):
			t.Fatal("4th stream should have acquired")
		}

		// Cleanup
		s2.Release()
		s3.Release()
	})
}
