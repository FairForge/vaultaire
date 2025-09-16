package cache

import (
	"fmt"
	"testing"
)

func BenchmarkSimpleCache_Put(b *testing.B) {
	cache, _ := NewSSDCache(100*1024*1024, 1024*1024*1024, b.TempDir())
	data := []byte("test data")

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = cache.Put(fmt.Sprintf("key-%d", i), data)
	}
}

func BenchmarkSimpleCache_Get(b *testing.B) {
	cache, _ := NewSSDCache(100*1024*1024, 1024*1024*1024, b.TempDir())

	// Pre-populate with 100 items
	for i := 0; i < 100; i++ {
		_ = cache.Put(fmt.Sprintf("key-%d", i), []byte("test data"))
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = cache.Get(fmt.Sprintf("key-%d", i%100))
	}
}
