// internal/drivers/resumable_test.go
package drivers

import (
	"bytes"
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestResumableUpload(t *testing.T) {
	t.Run("resumes interrupted upload", func(t *testing.T) {
		// Arrange
		tempDir := t.TempDir()
		driver := NewLocalDriver(tempDir, zap.NewNop())
		resumable := NewResumableUpload(driver, zap.NewNop())
		ctx := context.Background()

		// Create 10KB test data
		fullData := make([]byte, 10*1024)
		for i := range fullData {
			fullData[i] = byte(i % 256)
		}

		// Simulate partial upload (first 6KB)
		uploadID := "test-upload-123"
		err := resumable.StartUpload(ctx, uploadID, "test", "file.bin", int64(len(fullData)))
		require.NoError(t, err)

		// Upload first 6KB
		err = resumable.UploadChunk(ctx, uploadID, 0, bytes.NewReader(fullData[:6*1024]))
		require.NoError(t, err)

		// Act - Resume upload from offset 6KB
		offset, err := resumable.GetUploadOffset(ctx, uploadID)
		require.NoError(t, err)
		assert.Equal(t, int64(6*1024), offset)

		// Upload remaining 4KB
		err = resumable.UploadChunk(ctx, uploadID, offset, bytes.NewReader(fullData[6*1024:]))
		require.NoError(t, err)

		// Complete upload
		err = resumable.CompleteUpload(ctx, uploadID)
		require.NoError(t, err)

		// Assert - Verify complete file
		reader, err := driver.Get(ctx, "test", "file.bin")
		require.NoError(t, err)
		defer func() { _ = reader.Close() }()

		result := new(bytes.Buffer)
		_, err = result.ReadFrom(reader)
		require.NoError(t, err)

		assert.Equal(t, fullData, result.Bytes())
	})
}
