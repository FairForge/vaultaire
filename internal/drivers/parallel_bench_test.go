package drivers

import (
	"bytes"
	"context"
	"fmt"
	"testing"
)

func BenchmarkParallelVsSequential(b *testing.B) {
	sizes := []int{
		1 * 1024 * 1024,   // 1MB
		10 * 1024 * 1024,  // 10MB
		100 * 1024 * 1024, // 100MB
	}

	for _, size := range sizes {
		data := bytes.Repeat([]byte("x"), size)
		reader := bytes.NewReader(data)

		b.Run(fmt.Sprintf("Sequential_%dMB", size/1024/1024), func(b *testing.B) {
			b.SetBytes(int64(size))
			for i := 0; i < b.N; i++ {
				buf := make([]byte, size)
				_, _ = reader.ReadAt(buf, 0)
			}
		})

		b.Run(fmt.Sprintf("Parallel_%dMB", size/1024/1024), func(b *testing.B) {
			b.SetBytes(int64(size))
			for i := 0; i < b.N; i++ {
				cr := NewChunkReader(reader, int64(size))
				_, _ = cr.ReadParallel(context.Background())
			}
		})
	}
}
