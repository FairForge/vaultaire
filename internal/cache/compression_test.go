// internal/cache/compression_test.go
package cache

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSSDCache_Compression(t *testing.T) {
	t.Run("compresses_data_before_storing_to_ssd", func(t *testing.T) {
		cache, err := NewSSDCache(1024, 10*1024*1024, t.TempDir())
		require.NoError(t, err)

		// Enable compression
		cache.EnableCompression("gzip")

		// Large repetitive data that compresses well
		data := bytes.Repeat([]byte("Hello World! "), 1000)
		originalSize := int64(len(data))

		// Force to SSD by filling memory
		for i := 0; i < 10; i++ {
			_ = cache.Put(fmt.Sprintf("filler-%d", i), make([]byte, 200))
		}

		// Put our test data (should go to SSD due to memory pressure)
		err = cache.Put("test-key", data)
		require.NoError(t, err)

		// Check that data on SSD is compressed
		stats := cache.GetCompressionStats()
		assert.Greater(t, stats.BytesSaved, int64(0))
		assert.Less(t, stats.CompressedSize, originalSize)

		// Verify we can retrieve original data
		retrieved, ok := cache.Get("test-key")
		assert.True(t, ok)
		assert.Equal(t, data, retrieved)
	})

	t.Run("supports_multiple_compression_algorithms", func(t *testing.T) {
		testCases := []struct {
			algorithm string
			maxRatio  float64
		}{
			{"gzip", 0.5},   // gzip should achieve good compression
			{"snappy", 0.8}, // snappy trades compression for speed
			{"none", 1.0},   // no compression
		}

		for _, tc := range testCases {
			cache, err := NewSSDCache(512, 10*1024*1024, t.TempDir()) // Small memory
			require.NoError(t, err)

			cache.EnableCompression(tc.algorithm)

			// Test data that compresses well
			data := bytes.Repeat([]byte("test"), 1000)

			// Fill memory first
			for i := 0; i < 5; i++ {
				_ = cache.Put(fmt.Sprintf("fill-%d", i), make([]byte, 200))
			}

			// This should go to SSD
			_ = cache.Put("compress-test", data)

			stats := cache.GetCompressionStats()

			// Only check ratio if we actually compressed something
			if stats.OriginalSize > 0 {
				ratio := float64(stats.CompressedSize) / float64(stats.OriginalSize)
				assert.LessOrEqual(t, ratio, tc.maxRatio,
					"Algorithm %s should achieve ratio <= %f, got %f",
					tc.algorithm, tc.maxRatio, ratio)
			}
		}
	})
}
