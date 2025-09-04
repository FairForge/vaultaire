// internal/storage/garbage_collector_test.go
package storage

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestGarbageCollection(t *testing.T) {
	t.Run("identifies orphaned blocks", func(t *testing.T) {
		// Arrange
		gc := NewGarbageCollector()

		// Create blocks and references
		gc.AddBlock("block1", 1024)
		gc.AddBlock("block2", 2048)
		gc.AddBlock("block3", 512) // This will be orphaned

		gc.AddReference("file1", "block1")
		gc.AddReference("file1", "block2")
		// block3 has no references

		// Act
		orphaned := gc.FindOrphaned()

		// Assert
		assert.Len(t, orphaned, 1)
		assert.Contains(t, orphaned, "block3")
	})

	t.Run("cleans up expired data", func(t *testing.T) {
		// Arrange
		gc := NewGarbageCollector()
		gc.SetTTL(24 * time.Hour) // 24 hour TTL

		// Add blocks with different ages
		gc.AddBlockWithTime("recent", 1024, time.Now())
		gc.AddBlockWithTime("old", 2048, time.Now().Add(-48*time.Hour))

		// Act
		expired := gc.FindExpired()
		reclaimed := gc.Cleanup()

		// Assert
		assert.Len(t, expired, 1)
		assert.Contains(t, expired, "old")
		assert.Equal(t, int64(2048), reclaimed, "Should reclaim old block's space")
	})
}
