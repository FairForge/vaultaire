package drivers

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestLocalDriver_WatchForChanges(t *testing.T) {
	t.Run("detects file creation", func(t *testing.T) {
		// Arrange
		tmpDir := t.TempDir()
		logger := zap.NewNop()
		driver := NewLocalDriver(tmpDir, logger)

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		// Start watching
		events, errors, err := driver.Watch(ctx, "")
		require.NoError(t, err)

		// Act - create a file after watching starts
		testFile := filepath.Join(tmpDir, "test.txt")
		time.Sleep(100 * time.Millisecond)

		err = os.WriteFile(testFile, []byte("hello"), 0644)
		require.NoError(t, err)

		// Assert - should receive create event
		select {
		case event := <-events:
			assert.Equal(t, WatchEventCreate, event.Type)
			assert.Equal(t, "test.txt", event.Path)
		case err := <-errors:
			t.Fatalf("unexpected error: %v", err)
		case <-time.After(2 * time.Second):
			t.Fatal("timeout waiting for create event")
		}
	})

	t.Run("handles context cancellation", func(t *testing.T) {
		// Arrange
		tmpDir := t.TempDir()
		logger := zap.NewNop()
		driver := NewLocalDriver(tmpDir, logger)

		ctx, cancel := context.WithCancel(context.Background())

		// Start watching
		events, errors, err := driver.Watch(ctx, "")
		require.NoError(t, err)

		// Act - cancel context
		time.Sleep(100 * time.Millisecond)
		cancel()

		// Assert - channels should close
		select {
		case _, ok := <-events:
			assert.False(t, ok, "events channel should be closed")
		case _, ok := <-errors:
			assert.False(t, ok, "errors channel should be closed")
		case <-time.After(2 * time.Second):
			t.Fatal("timeout waiting for channels to close")
		}
	})
}
