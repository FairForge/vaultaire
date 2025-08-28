package drivers

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"

	"go.uber.org/zap"
)

// Benchmark multiple small writes (where buffering helps)
func BenchmarkLocalDriver_MultipleWrites(b *testing.B) {
	driver := NewLocalDriver(b.TempDir(), zap.NewNop())
	ctx := context.Background()

	smallData := []byte(strings.Repeat("x", 100)) // 100 bytes

	b.Run("Unbuffered_Many", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			b.StopTimer()
			filename := fmt.Sprintf("unbuf-%d.txt", i)
			var buf bytes.Buffer
			b.StartTimer()

			// Write 100 small chunks
			for j := 0; j < 100; j++ {
				buf.Write(smallData)
			}

			// Single write to disk
			driver.Put(ctx, "bench", filename, &buf)
		}
	})

	b.Run("Buffered_Many", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			b.StopTimer()
			filename := fmt.Sprintf("buf-%d.txt", i)
			b.StartTimer()

			writer, _ := driver.PutBuffered(ctx, "bench", filename)
			// Write 100 small chunks - buffering reduces syscalls
			for j := 0; j < 100; j++ {
				mustWrite(writer, smallData)
			}
			mustClose(writer)
		}
	})
}
