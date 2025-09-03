// internal/drivers/chunked_transfer_test.go
package drivers

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestChunkedTransfer_ChunkedWrite(t *testing.T) {
	t.Run("writes data in chunks", func(t *testing.T) {
		// Arrange
		chunker := NewChunkedTransfer(1024, zap.NewNop()) // 1KB chunks

		// Create 5KB test data
		data := make([]byte, 5*1024)
		for i := range data {
			data[i] = byte(i % 256)
		}

		var output bytes.Buffer

		// Act
		written, err := chunker.ChunkedWrite(&output, bytes.NewReader(data))

		// Assert
		require.NoError(t, err)
		assert.Equal(t, int64(len(data)), written)
		assert.Equal(t, data, output.Bytes())
	})
}
