package benchmarks

import (
	"bytes"
	"crypto/rand"
	"testing"
)

func BenchmarkPutObject(b *testing.B) {
	data := make([]byte, 1024*1024) // 1MB
	_, err := rand.Read(data)
	if err != nil {
		b.Fatal(err)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = bytes.NewReader(data)
	}
}

func BenchmarkGetObject(b *testing.B) {
	data := make([]byte, 1024*1024)
	_, err := rand.Read(data)
	if err != nil {
		b.Fatal(err)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = bytes.NewReader(data)
	}
}
