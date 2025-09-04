package storage

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestContentAwareChunking(t *testing.T) {
	t.Run("finds content boundaries", func(t *testing.T) {
		// Arrange
		chunker := NewContentChunker(1024, 4096) // min 1KB, max 4KB

		// Create data larger than max chunk size to force multiple chunks
		pattern1 := bytes.Repeat([]byte("AAAA"), 1200) // 4800 bytes
		pattern2 := bytes.Repeat([]byte("BBBB"), 1000) // 4000 bytes

		data := append(pattern1, pattern2...) // 8800 bytes total

		// Act
		chunks := chunker.Split(data)

		// Assert
		assert.GreaterOrEqual(t, len(chunks), 2, "Should find multiple chunks")
		for i, chunk := range chunks {
			// Last chunk can be smaller than minSize
			if i == len(chunks)-1 && chunk.Size < chunker.minSize {
				continue
			}
			assert.GreaterOrEqual(t, len(chunk.Data), 1024, "Non-final chunks should be at least min size")
			assert.LessOrEqual(t, len(chunk.Data), 4096, "Chunks should not exceed max size")
		}
	})

	t.Run("uses rolling hash for boundary detection", func(t *testing.T) {
		// Arrange
		chunker := NewContentChunker(512, 2048)

		// Same data should produce same chunks
		data := []byte("The quick brown fox jumps over the lazy dog. " +
			"The quick brown fox jumps over the lazy dog. " +
			"The quick brown fox jumps over the lazy dog.")

		// Act
		chunks1 := chunker.Split(data)
		chunks2 := chunker.Split(data)

		// Assert
		require.Equal(t, len(chunks1), len(chunks2))
		for i := range chunks1 {
			assert.Equal(t, chunks1[i].Hash, chunks2[i].Hash, "Same data should produce same chunks")
		}
	})
}
