// internal/cache/benchmark_test.go
package cache

import (
	"crypto/rand"
	"fmt"
	"testing"
)

func generateData(size int) []byte {
	data := make([]byte, size)
	_, _ = rand.Read(data) // Explicitly ignore error for benchmark
	return data
}

func BenchmarkSSDCache_Put(b *testing.B) {
	// Use larger cache sizes to avoid filling up
	cache, err := NewSSDCache(100*1024*1024, 10*1024*1024*1024, b.TempDir()) // 100MB memory, 10GB SSD
	if err != nil {
		b.Fatal(err)
	}

	data := generateData(1024) // 1KB

	b.ResetTimer()
	b.SetBytes(1024)

	// Reuse keys to avoid exhausting space
	for i := 0; i < b.N; i++ {
		key := fmt.Sprintf("key-%d", i%10000) // Cycle through 10k keys
		err := cache.Put(key, data)
		if err != nil {
			b.Fatalf("Put failed at iteration %d: %v", i, err)
		}
	}
}

func BenchmarkSSDCache_Get(b *testing.B) {
	cache, err := NewSSDCache(10*1024*1024, 100*1024*1024, b.TempDir())
	if err != nil {
		b.Fatal(err)
	}

	data := generateData(1024)

	// Pre-populate
	for i := 0; i < 100; i++ {
		err := cache.Put(fmt.Sprintf("key-%d", i), data)
		if err != nil {
			b.Fatal(err)
		}
	}

	b.ResetTimer()
	b.SetBytes(1024)

	for i := 0; i < b.N; i++ {
		_, ok := cache.Get(fmt.Sprintf("key-%d", i%100))
		if !ok && i < 100 { // Should exist
			b.Fatal("key not found")
		}
	}
}
