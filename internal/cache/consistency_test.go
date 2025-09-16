package cache

import (
	"github.com/stretchr/testify/assert"
	"testing"
	"time"
)

func TestConsistencyManager_StrongConsistency(t *testing.T) {
	manager := NewConsistencyManager(StrongConsistency)

	// Set initial version
	manager.UpdateVersion("key1", 1, "checksum1")

	// Same version should be valid
	assert.True(t, manager.ValidateConsistency("key1", 1))

	// Different version should be invalid
	assert.False(t, manager.ValidateConsistency("key1", 2))

	// After invalidation, old version invalid
	manager.Invalidate("key1")
	assert.False(t, manager.ValidateConsistency("key1", 1))
}

func TestConsistencyManager_BoundedStaleness(t *testing.T) {
	manager := NewConsistencyManager(BoundedStaleness)
	manager.maxStaleness = 100 * time.Millisecond

	manager.UpdateVersion("key1", 1, "checksum1")

	// Fresh data is valid
	assert.True(t, manager.ValidateConsistency("key1", 1))

	// Wait for staleness
	time.Sleep(150 * time.Millisecond)

	// Stale data is invalid
	assert.False(t, manager.ValidateConsistency("key1", 1))
}

func TestConsistencyManager_EventualConsistency(t *testing.T) {
	manager := NewConsistencyManager(EventualConsistency)

	manager.UpdateVersion("key1", 1, "checksum1")

	// Any version is valid with eventual consistency
	assert.True(t, manager.ValidateConsistency("key1", 1))
	assert.True(t, manager.ValidateConsistency("key1", 999))

	// Even non-existent keys return true (will sync eventually)
	assert.True(t, manager.ValidateConsistency("nonexistent", 0))
}

func TestConsistencyManager_Invalidation(t *testing.T) {
	manager := NewConsistencyManager(StrongConsistency)

	manager.UpdateVersion("key1", 1, "checksum1")
	initialVersion := manager.versions["key1"].Version

	// Invalidate increases version
	manager.Invalidate("key1")
	newVersion := manager.versions["key1"].Version

	assert.Greater(t, newVersion, initialVersion)

	// Invalidation updates timestamp
	assert.WithinDuration(t, time.Now(), manager.versions["key1"].Timestamp, time.Second)
}
