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
		tmpDir := t.TempDir()
		logger := zap.NewNop()
		driver := NewLocalDriver(tmpDir, logger)

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		events, errors, err := driver.Watch(ctx, "")
		require.NoError(t, err)

		// Create file after watching starts
		testFile := filepath.Join(tmpDir, "test.txt")
		time.Sleep(100 * time.Millisecond)

		err = os.WriteFile(testFile, []byte("hello"), 0644)
		require.NoError(t, err)

		// Should receive create event
		select {
		case event := <-events:
			// On macOS, could be Create or Modify
			assert.Contains(t, []WatchEventType{WatchEventCreate, WatchEventModify}, event.Type)
			assert.Equal(t, "test.txt", event.Path)
		case err := <-errors:
			t.Fatalf("unexpected error: %v", err)
		case <-time.After(2 * time.Second):
			t.Fatal("timeout waiting for create event")
		}
	})

	t.Run("handles context cancellation", func(t *testing.T) {
		tmpDir := t.TempDir()
		logger := zap.NewNop()
		driver := NewLocalDriver(tmpDir, logger)

		ctx, cancel := context.WithCancel(context.Background())

		events, errors, err := driver.Watch(ctx, "")
		require.NoError(t, err)

		// Cancel context
		time.Sleep(100 * time.Millisecond)
		cancel()

		// Channels should close
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

func TestLocalDriver_WatchExtended(t *testing.T) {
	t.Run("detects file changes", func(t *testing.T) {
		tmpDir := t.TempDir()
		driver := NewLocalDriver(tmpDir, zap.NewNop())

		testFile := filepath.Join(tmpDir, "modify.txt")
		require.NoError(t, os.WriteFile(testFile, []byte("initial"), 0644))

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		events, errors, err := driver.Watch(ctx, "")
		require.NoError(t, err)

		time.Sleep(100 * time.Millisecond)
		require.NoError(t, os.WriteFile(testFile, []byte("modified"), 0644))

		select {
		case event := <-events:
			// macOS may report as Create or Modify
			assert.Contains(t, []WatchEventType{WatchEventCreate, WatchEventModify}, event.Type)
			assert.Equal(t, "modify.txt", event.Path)
		case err := <-errors:
			t.Fatalf("unexpected error: %v", err)
		case <-time.After(2 * time.Second):
			t.Fatal("timeout waiting for modify event")
		}
	})

	t.Run("detects file deletion", func(t *testing.T) {
		tmpDir := t.TempDir()
		driver := NewLocalDriver(tmpDir, zap.NewNop())

		testFile := filepath.Join(tmpDir, "delete.txt")
		require.NoError(t, os.WriteFile(testFile, []byte("will delete"), 0644))

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		events, errors, err := driver.Watch(ctx, "")
		require.NoError(t, err)

		time.Sleep(100 * time.Millisecond)
		require.NoError(t, os.Remove(testFile))

		select {
		case event := <-events:
			// Should be delete or rename (macOS sometimes reports as rename)
			assert.Contains(t, []WatchEventType{WatchEventDelete, WatchEventRename}, event.Type)
			assert.Equal(t, "delete.txt", event.Path)
		case err := <-errors:
			t.Fatalf("unexpected error: %v", err)
		case <-time.After(2 * time.Second):
			t.Fatal("timeout waiting for delete event")
		}
	})
}
