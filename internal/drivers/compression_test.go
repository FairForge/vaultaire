// internal/drivers/compression_test.go
package drivers

import (
	"bytes"
	"context"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// Adapter to make LocalDriver compatible with Driver interface
type driverAdapter struct {
	*LocalDriver
}

func (d *driverAdapter) Put(ctx context.Context, container, artifact string, data io.Reader) error {
	// Call LocalDriver.Put without options
	return d.LocalDriver.Put(ctx, container, artifact, data)
}

func TestCompressionDriver(t *testing.T) {
	t.Run("compresses and decompresses data", func(t *testing.T) {
		// Arrange
		backend := NewLocalDriver(t.TempDir(), zap.NewNop())
		compressed := NewCompressionDriver(backend, "gzip", zap.NewNop())
		ctx := context.Background()

		// Create compressible data
		data := bytes.Repeat([]byte("Hello World! "), 1000)
		originalSize := int64(len(data))

		// Act - Put compressed
		err := compressed.Put(ctx, "test", "file.txt", bytes.NewReader(data))
		require.NoError(t, err)

		// Get should decompress
		reader, err := compressed.Get(ctx, "test", "file.txt")
		require.NoError(t, err)
		defer reader.Close()

		result, err := io.ReadAll(reader)
		require.NoError(t, err)

		// Assert
		assert.Equal(t, data, result)

		// Verify compression worked
		info, err := backend.GetInfo(ctx, "test", "file.txt.gz")
		require.NoError(t, err)
		assert.Less(t, info.Size, originalSize)
	})
}
