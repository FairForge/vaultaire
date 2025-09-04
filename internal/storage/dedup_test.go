// internal/storage/dedup_test.go
package storage

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDeduplication(t *testing.T) {
	t.Run("detects duplicate blocks", func(t *testing.T) {
		// Arrange
		dedup := NewDeduplicator(1024) // 1KB blocks

		data1 := bytes.Repeat([]byte("hello world "), 100) // repeated pattern
		data2 := bytes.Repeat([]byte("hello world "), 100) // same data

		// Act
		hash1, isNew1 := dedup.CheckBlock(data1)
		hash2, isNew2 := dedup.CheckBlock(data2)

		// Assert
		assert.True(t, isNew1, "First block should be new")
		assert.False(t, isNew2, "Second identical block should be duplicate")
		assert.Equal(t, hash1, hash2, "Same data should have same hash")
	})

	t.Run("stores blocks only once", func(t *testing.T) {
		// Arrange
		store := NewDedupStore("test-dedup")

		// Write same data multiple times
		data := []byte("test data for deduplication")

		// Act
		ref1, err := store.Store("file1.txt", data)
		require.NoError(t, err)

		ref2, err := store.Store("file2.txt", data)
		require.NoError(t, err)

		// Assert
		assert.Equal(t, ref1.BlockHash, ref2.BlockHash)
		assert.Equal(t, 1, store.UniqueBlocks(), "Should only store one unique block")
	})
}
