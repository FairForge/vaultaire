// internal/cache/deduplication_test.go
package cache

import (
	"crypto/sha256"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSSDCache_Deduplication(t *testing.T) {
	t.Run("detects_duplicate_content", func(t *testing.T) {
		// Small cache to force SSD usage
		cache, err := NewSSDCache(200, 10*1024*1024, t.TempDir())
		require.NoError(t, err)

		// Enable deduplication
		cache.EnableDeduplication()

		// Create identical data
		data := []byte("This is duplicate content that should be stored only once")
		hash := cache.computeHash(data)

		// Store the same data 3 times
		err = cache.Put("file1.txt", data)
		require.NoError(t, err)

		err = cache.Put("file2.txt", data)
		require.NoError(t, err)

		err = cache.Put("file3.txt", data)
		require.NoError(t, err)

		// Force eviction to SSD
		_ = cache.Put("evict", make([]byte, 300))

		// Check the specific hash in dedup index
		cache.dedupMu.RLock()
		block, exists := cache.dedupIndex[hash]
		cache.dedupMu.RUnlock()

		assert.True(t, exists, "Should have the hash in dedup index")
		if exists {
			assert.Equal(t, 3, block.RefCount, "Should have 3 references to the same data")
		}
	})

	t.Run("reference_counting_on_delete", func(t *testing.T) {
		cache, err := NewSSDCache(100, 10*1024*1024, t.TempDir())
		require.NoError(t, err)

		cache.EnableDeduplication()

		data := []byte("shared data")
		hash := cache.computeHash(data)

		// Add duplicate data
		_ = cache.Put("doc1", data)
		_ = cache.Put("doc2", data)
		_ = cache.Put("doc3", data)

		// Force to SSD
		_ = cache.Put("evict", make([]byte, 200))

		// Check initial ref count
		cache.dedupMu.RLock()
		if block, exists := cache.dedupIndex[hash]; exists {
			assert.Equal(t, 3, block.RefCount, "Should start with 3 refs")
		}
		cache.dedupMu.RUnlock()

		// Delete one
		_ = cache.Delete("doc1")

		// Check ref count decreased
		cache.dedupMu.RLock()
		if block, exists := cache.dedupIndex[hash]; exists {
			assert.Equal(t, 2, block.RefCount, "Should have 2 refs after delete")
		}
		cache.dedupMu.RUnlock()

		// Data should still be retrievable
		retrieved, ok := cache.Get("doc2")
		assert.True(t, ok)
		assert.Equal(t, data, retrieved)
	})

	t.Run("removes_block_when_last_ref_deleted", func(t *testing.T) {
		cache, err := NewSSDCache(100, 10*1024*1024, t.TempDir())
		require.NoError(t, err)

		cache.EnableDeduplication()

		data := []byte("unique data")
		hash := cache.computeHash(data)

		// Add single item
		_ = cache.Put("solo", data)

		// Force to SSD
		_ = cache.Put("evict", make([]byte, 200))

		// Verify it's in dedup index
		cache.dedupMu.RLock()
		_, exists := cache.dedupIndex[hash]
		cache.dedupMu.RUnlock()
		assert.True(t, exists, "Should be in dedup index")

		// Delete it
		_ = cache.Delete("solo")

		// Should be removed from dedup index
		cache.dedupMu.RLock()
		_, exists = cache.dedupIndex[hash]
		cache.dedupMu.RUnlock()
		assert.False(t, exists, "Should be removed from dedup index")
	})
}

func TestSSDCache_ContentHashing(t *testing.T) {
	t.Run("generates_consistent_hashes", func(t *testing.T) {
		cache, err := NewSSDCache(1024, 10*1024*1024, t.TempDir())
		require.NoError(t, err)

		data := []byte("test data")
		hash1 := cache.computeHash(data)
		hash2 := cache.computeHash(data)

		assert.Equal(t, hash1, hash2)

		// Should be SHA256
		expectedHash := fmt.Sprintf("%x", sha256.Sum256(data))
		assert.Equal(t, expectedHash, hash1)
	})
}
