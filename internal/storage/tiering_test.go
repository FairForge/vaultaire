// internal/storage/tiering_test.go
package storage

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStorageTiering(t *testing.T) {
	t.Run("classifies data by access pattern", func(t *testing.T) {
		// Arrange
		tiering := NewTieringEngine()

		// Act - simulate access patterns
		tiering.RecordAccess("hot-file.txt", time.Now())
		tiering.RecordAccess("hot-file.txt", time.Now())
		tiering.RecordAccess("hot-file.txt", time.Now())

		tiering.RecordAccess("cold-file.txt", time.Now().Add(-30*24*time.Hour))

		// Assert
		assert.Equal(t, HotTier, tiering.GetTier("hot-file.txt"))
		assert.Equal(t, ColdTier, tiering.GetTier("cold-file.txt"))
	})

	t.Run("moves data between tiers automatically", func(t *testing.T) {
		// Arrange
		manager := NewTierManager()
		manager.AddTier("hot", 100*1024*1024)   // 100MB hot tier
		manager.AddTier("warm", 1024*1024*1024) // 1GB warm tier
		manager.AddTier("cold", 0)              // Unlimited cold tier

		// Act
		file1 := []byte("frequently accessed data")
		file2 := []byte("rarely accessed data")

		id1, err := manager.Store("file1.txt", file1)
		require.NoError(t, err)

		id2, err := manager.Store("file2.txt", file2)
		require.NoError(t, err)

		// Simulate time passing and access patterns
		manager.RecordAccess(id1) // Keep file1 hot
		manager.SimulateAging(24 * time.Hour)

		// Assert
		tier1 := manager.GetFileTier(id1)
		tier2 := manager.GetFileTier(id2)

		assert.Equal(t, "hot", tier1, "Frequently accessed should stay hot")
		assert.NotEqual(t, "hot", tier2, "Unaccessed should move to lower tier")
	})
}
