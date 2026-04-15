package drivers

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// TestLocalDriver_HealthCheck tests health check functionality
func TestLocalDriver_HealthCheck(t *testing.T) {
	ctx := context.Background()

	t.Run("HealthyDriver", func(t *testing.T) {
		tmpDir := t.TempDir()
		driver := NewLocalDriver(tmpDir, zap.NewNop())

		err := driver.HealthCheck(ctx)
		assert.NoError(t, err, "Health check should pass for valid path")
	})

	t.Run("UnhealthyDriver", func(t *testing.T) {
		// Use non-existent path
		driver := NewLocalDriver("/nonexistent/path/12345", zap.NewNop())

		err := driver.HealthCheck(ctx)
		assert.Error(t, err, "Health check should fail for invalid path")
		assert.Contains(t, err.Error(), "health check failed")
	})
}

// TestLocalDriver_Put_AtomicWithConcurrentReader verifies that writing to a key
// while a reader is mid-stream does not corrupt the in-flight read. Direct
// os.Create truncates the destination file in-place, which corrupts any open
// reader against the same inode. The write-to-temp-then-rename pattern keeps
// the original inode intact for existing FDs.
func TestLocalDriver_Put_AtomicWithConcurrentReader(t *testing.T) {
	ctx := context.Background()
	driver := NewLocalDriver(t.TempDir(), zap.NewNop())

	original := strings.Repeat("ORIGINAL", 1024) // 8 KiB
	require.NoError(t, driver.Put(ctx, "c", "k", strings.NewReader(original)))

	// Open a reader against the original content but do not consume it yet.
	reader, err := driver.Get(ctx, "c", "k")
	require.NoError(t, err)
	defer func() { _ = reader.Close() }()

	// Overwrite the key with new content while the reader is held open.
	require.NoError(t, driver.Put(ctx, "c", "k", strings.NewReader("X")))

	// Now drain the reader. With the atomic Put it must still see the
	// original bytes; with truncate-in-place the read returns 0 bytes.
	got, err := io.ReadAll(reader)
	require.NoError(t, err)
	assert.Equal(t, original, string(got),
		"reader must see the original content; got %d bytes", len(got))

	// Confirm the new content is what a fresh Get returns.
	fresh, err := driver.Get(ctx, "c", "k")
	require.NoError(t, err)
	defer func() { _ = fresh.Close() }()
	freshBytes, err := io.ReadAll(fresh)
	require.NoError(t, err)
	assert.Equal(t, "X", string(freshBytes))
}

// TestLocalDriver_Put_OverwritesExisting confirms basic overwrite still works
// — a regression check for the atomic Put refactor.
func TestLocalDriver_Put_OverwritesExisting(t *testing.T) {
	ctx := context.Background()
	driver := NewLocalDriver(t.TempDir(), zap.NewNop())

	require.NoError(t, driver.Put(ctx, "c", "k", bytes.NewReader([]byte("first"))))
	require.NoError(t, driver.Put(ctx, "c", "k", bytes.NewReader([]byte("second"))))

	r, err := driver.Get(ctx, "c", "k")
	require.NoError(t, err)
	defer func() { _ = r.Close() }()
	got, err := io.ReadAll(r)
	require.NoError(t, err)
	assert.Equal(t, "second", string(got))
}
