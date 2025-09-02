package drivers

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"

	"go.uber.org/zap"
)

// Test write buffering reduces syscalls
func TestLocalDriver_BufferedWrites(t *testing.T) {
	driver := NewLocalDriver(t.TempDir(), zap.NewNop())
	ctx := context.Background()

	// Small writes that should be buffered
	data := []byte("small write ")

	writer, err := driver.PutBuffered(ctx, "buffer-test", "output.txt")
	if err != nil {
		t.Fatal(err)
	}

	// Write small chunks
	for i := 0; i < 100; i++ {
		n, err := writer.Write(data)
		if err != nil {
			t.Fatal(err)
		}
		if n != len(data) {
			t.Errorf("wrote %d bytes, expected %d", n, len(data))
		}
	}

	// Must flush to persist
	err = writer.Close()
	if err != nil {
		t.Fatal(err)
	}

	// Verify data was written
	reader, err := driver.Get(ctx, "buffer-test", "output.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Close() }()

	content, _ := io.ReadAll(reader)
	expected := bytes.Repeat(data, 100)

	if !bytes.Equal(content, expected) {
		t.Errorf("content mismatch: got %d bytes, expected %d", len(content), len(expected))
	}
}

// Test concurrent buffered writes
func TestLocalDriver_ConcurrentBufferedWrites(t *testing.T) {
	driver := NewLocalDriver(t.TempDir(), zap.NewNop())
	ctx := context.Background()

	var wg sync.WaitGroup
	concurrency := 50

	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()

			filename := fmt.Sprintf("file-%d.txt", id)
			writer, err := driver.PutBuffered(ctx, "concurrent", filename)
			if err != nil {
				t.Errorf("worker %d: %v", id, err)
				return
			}

			// Write incrementally
			for j := 0; j < 10; j++ {
				data := fmt.Sprintf("Worker %d line %d\n", id, j)
				_, _ = writer.Write([]byte(data))
			}

			// Close to flush
			if err := writer.Close(); err != nil {
				t.Errorf("worker %d close: %v", id, err)
			}
		}(i)
	}

	wg.Wait()

	// Verify all files exist
	for i := 0; i < concurrency; i++ {
		filename := fmt.Sprintf("file-%d.txt", i)
		reader, err := driver.Get(ctx, "concurrent", filename)
		if err != nil {
			t.Errorf("file %s missing: %v", filename, err)
			continue
		}
		_ = reader.Close()
	}
}

// Test auto-flush on buffer size
func TestLocalDriver_BufferAutoFlush(t *testing.T) {
	driver := NewLocalDriver(t.TempDir(), zap.NewNop())
	ctx := context.Background()

	writer, err := driver.PutBuffered(ctx, "autoflush", "large.txt")
	if err != nil {
		t.Fatal(err)
	}

	// Write more than buffer size (assuming 64KB buffer)
	largeData := bytes.Repeat([]byte("A"), 70*1024) // 70KB

	n, err := writer.Write(largeData)
	if err != nil {
		t.Fatal(err)
	}
	if n != len(largeData) {
		t.Errorf("wrote %d bytes, expected %d", n, len(largeData))
	}

	// Should auto-flush when buffer fills
	// Close to ensure final flush
	_ = writer.Close()

	// Verify
	reader, err := driver.Get(ctx, "autoflush", "large.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Close() }()

	content, _ := io.ReadAll(reader)
	if len(content) != len(largeData) {
		t.Errorf("size mismatch: got %d, expected %d", len(content), len(largeData))
	}
}

// Benchmark buffered vs unbuffered writes
func BenchmarkLocalDriver_Writes(b *testing.B) {
	driver := NewLocalDriver(b.TempDir(), zap.NewNop())
	ctx := context.Background()

	smallData := []byte(strings.Repeat("test data ", 10)) // 100 bytes

	b.Run("Unbuffered", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			filename := fmt.Sprintf("unbuf-%d.txt", i)
			err := driver.Put(ctx, "bench", filename, bytes.NewReader(smallData))
			if err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("Buffered", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			filename := fmt.Sprintf("buf-%d.txt", i)
			writer, _ := driver.PutBuffered(ctx, "bench", filename)
			_, _ = writer.Write(smallData)
			_ = writer.Close()
		}
	})
}
