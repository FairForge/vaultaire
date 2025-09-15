// internal/cache/encryption_test.go
package cache

import (
	"bytes"
	"container/list"
	"fmt"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSSDCache_Encryption(t *testing.T) {
	t.Run("encrypts_data_on_ssd", func(t *testing.T) {
		cache, err := NewSSDCache(100, 10*1024*1024, t.TempDir())
		require.NoError(t, err)

		// Enable encryption with a test key
		testKey := []byte("test-encryption-key-32-bytes-long!")[:32]
		err = cache.EnableEncryption(testKey)
		require.NoError(t, err)

		// Store sensitive data
		sensitiveData := []byte("This is sensitive information that must be encrypted")

		// Put data first
		err = cache.Put("secret.txt", sensitiveData)
		require.NoError(t, err)

		// Force to SSD by filling memory
		for i := 0; i < 5; i++ {
			_ = cache.Put(fmt.Sprintf("filler-%d", i), make([]byte, 50))
		}

		// Verify it's on SSD
		cache.mu.RLock()
		entry, exists := cache.index["secret.txt"]
		cache.mu.RUnlock()
		require.True(t, exists, "Data should be on SSD")

		// Read raw data from disk
		rawData, err := os.ReadFile(entry.Path)
		require.NoError(t, err)

		// Raw data should NOT match original (it's encrypted)
		assert.NotEqual(t, sensitiveData, rawData, "Data on disk should be encrypted")
		assert.Greater(t, len(rawData), len(sensitiveData), "Encrypted data should be larger (has metadata)")

		// Clear memory to force read from SSD
		cache.memMu.Lock()
		cache.memLRU = list.New()
		cache.memItems = make(map[string]*list.Element)
		cache.memCurrBytes = 0
		cache.memMu.Unlock()

		// Now Get should read from SSD and decrypt
		retrieved, ok := cache.Get("secret.txt")
		assert.True(t, ok)
		assert.Equal(t, sensitiveData, retrieved, "Retrieved data should be decrypted")
	})

	t.Run("encryption_with_compression", func(t *testing.T) {
		cache, err := NewSSDCache(100, 10*1024*1024, t.TempDir())
		require.NoError(t, err)

		// Enable both compression and encryption
		cache.EnableCompression("gzip")
		testKey := []byte("test-encryption-key-32-bytes-long!")[:32]
		_ = cache.EnableEncryption(testKey)

		// Large repetitive data
		data := bytes.Repeat([]byte("REPEAT"), 100)

		// Force to SSD
		_ = cache.Put("filler", make([]byte, 90))
		_ = cache.Put("data.txt", data)

		// Should compress then encrypt
		stats := cache.GetCompressionStats()
		assert.Greater(t, stats.BytesSaved, int64(0), "Should still compress")

		// Should decrypt then decompress
		retrieved, ok := cache.Get("data.txt")
		assert.True(t, ok)
		assert.Equal(t, data, retrieved)
	})

	t.Run("key_rotation", func(t *testing.T) {
		cache, err := NewSSDCache(100, 10*1024*1024, t.TempDir())
		require.NoError(t, err)

		// Start with one key
		oldKey := []byte("old-encryption-key-32-bytes-long!")[:32]
		_ = cache.EnableEncryption(oldKey)

		data := []byte("data encrypted with old key")

		// Force to SSD
		_ = cache.Put("filler", make([]byte, 90))
		_ = cache.Put("file.txt", data)

		// Rotate to new key
		newKey := []byte("new-encryption-key-32-bytes-long!")[:32]
		err = cache.RotateEncryptionKey(newKey)
		require.NoError(t, err)

		// Should still be able to read old data
		retrieved, ok := cache.Get("file.txt")
		assert.True(t, ok)
		assert.Equal(t, data, retrieved, "Should decrypt with appropriate key")

		// New data should use new key
		newData := []byte("data encrypted with new key")
		_ = cache.Put("filler2", make([]byte, 90))
		_ = cache.Put("new.txt", newData)

		retrieved2, ok := cache.Get("new.txt")
		assert.True(t, ok)
		assert.Equal(t, newData, retrieved2)
	})
}

func TestSSDCache_EncryptionValidation(t *testing.T) {
	t.Run("rejects_invalid_key_size", func(t *testing.T) {
		cache, err := NewSSDCache(1024, 10*1024*1024, t.TempDir())
		require.NoError(t, err)

		// Too short
		shortKey := []byte("short")
		err = cache.EnableEncryption(shortKey)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "32 bytes") // Updated check

		// Too long
		longKey := make([]byte, 64)
		err = cache.EnableEncryption(longKey)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "32 bytes")
	})
}
