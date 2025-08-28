package drivers

import (
	"bytes"
	"context"
	"fmt"
	"go.uber.org/zap"
	"testing"
)

func BenchmarkParallelWorkers(b *testing.B) {
	driver := NewLocalDriver(b.TempDir(), zap.NewNop())
	ctx := context.Background()

	// Smaller files, more of them - better for parallelism
	fileCount := 1000
	fileSize := 10 * 1024 // 10KB each

	operations := make([]PutOperation, fileCount)
	for i := 0; i < fileCount; i++ {
		data := make([]byte, fileSize)
		operations[i] = PutOperation{
			Container: "bench",
			Artifact:  fmt.Sprintf("file-%d.dat", i),
			Data:      bytes.NewReader(data),
		}
	}

	for _, workers := range []int{1, 2, 4, 8, 16, 32} {
		b.Run(fmt.Sprintf("Workers-%d", workers), func(b *testing.B) {
			parallel := NewParallelDriver(driver, workers, zap.NewNop())
			b.ResetTimer()
			for n := 0; n < b.N; n++ {
				parallel.ParallelPut(ctx, operations)
			}
			b.SetBytes(int64(fileCount * fileSize))
		})
	}
}
