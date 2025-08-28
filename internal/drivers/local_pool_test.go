package drivers

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
)

// Helper function to setup test driver
func setupTestDriver(t testing.TB) *LocalDriver {
	t.Helper()
	tempDir := t.TempDir()

	// Create a test logger
	logger, _ := zap.NewDevelopment()
	driver := NewLocalDriver(tempDir, logger)

	// Create test container
	containerPath := filepath.Join(tempDir, "test-container")
	err := os.MkdirAll(containerPath, 0755)
	if err != nil {
		t.Fatal(err)
	}

	// Create pool-test container
	poolPath := filepath.Join(tempDir, "pool-test")
	err = os.MkdirAll(poolPath, 0755)
	if err != nil {
		t.Fatal(err)
	}

	// Create bench container
	benchPath := filepath.Join(tempDir, "bench")
	err = os.MkdirAll(benchPath, 0755)
	if err != nil {
		t.Fatal(err)
	}

	return driver
}

// Test that proves we need pooling (it will show the problem)
func TestLocalDriver_ParallelReads_ShowsProblem(t *testing.T) {
	driver := setupTestDriver(t)
	ctx := context.Background()

	// Create a test file
	testData := bytes.Repeat([]byte("test"), 1024) // 4KB
	err := driver.Put(ctx, "test-container", "pooltest.txt", bytes.NewReader(testData))
	if err != nil {
		t.Fatal(err)
	}

	// Track open file descriptors
	startFDs := countOpenFDs(t)

	// Parallel reads WITHOUT pooling (current implementation)
	var wg sync.WaitGroup
	concurrency := 100

	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()

			reader, err := driver.Get(ctx, "test-container", "pooltest.txt")
			if err != nil {
				t.Errorf("Worker %d failed: %v", id, err)
				return
			}
			defer reader.Close()

			// Simulate some work
			io.Copy(io.Discard, reader)
			time.Sleep(10 * time.Millisecond)
		}(i)
	}

	wg.Wait()

	endFDs := countOpenFDs(t)
	fdIncrease := endFDs - startFDs

	t.Logf("FD increase: %d (started: %d, ended: %d)", fdIncrease, startFDs, endFDs)

	// This test shows the problem - too many FDs!
	if fdIncrease > 20 {
		t.Logf("WARNING: FD leak detected! Increased by %d", fdIncrease)
	}
}

// Test the pooled implementation
func TestLocalDriver_PooledReads(t *testing.T) {
	driver := setupTestDriver(t)
	ctx := context.Background()

	// Create test files
	for i := 0; i < 10; i++ {
		data := []byte(fmt.Sprintf("file-%d-content", i))
		err := driver.Put(ctx, "pool-test", fmt.Sprintf("file-%d.txt", i), bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
	}

	// Test GetPooled method (to be implemented)
	reader, err := driver.GetPooled(ctx, "pool-test", "file-5.txt")
	if err != nil {
		t.Fatal("GetPooled failed:", err)
	}
	defer driver.ReturnPooledReader(reader) // Return to pool

	content, _ := io.ReadAll(reader)
	if !bytes.Equal(content, []byte("file-5-content")) {
		t.Error("Content mismatch")
	}
}

// Test concurrent pooled reads
func TestLocalDriver_ConcurrentPooledReads(t *testing.T) {
	driver := setupTestDriver(t)
	ctx := context.Background()

	// Create test file
	testData := []byte("concurrent-test-data")
	err := driver.Put(ctx, "pool-test", "concurrent.txt", bytes.NewReader(testData))
	if err != nil {
		t.Fatal(err)
	}

	// Track FDs with pooling
	startFDs := countOpenFDs(t)

	var wg sync.WaitGroup
	concurrency := 100
	errors := make(chan error, concurrency)

	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()

			reader, err := driver.GetPooled(ctx, "pool-test", "concurrent.txt")
			if err != nil {
				errors <- fmt.Errorf("worker %d: %v", id, err)
				return
			}

			// Read content
			content, err := io.ReadAll(reader)
			if err != nil {
				errors <- fmt.Errorf("worker %d read: %v", id, err)
				driver.ReturnPooledReader(reader)
				return
			}

			// Verify content
			if !bytes.Equal(content, testData) {
				errors <- fmt.Errorf("worker %d: content mismatch", id)
			}

			// Return to pool
			driver.ReturnPooledReader(reader)
		}(i)
	}

	wg.Wait()
	close(errors)

	// Check for errors
	for err := range errors {
		t.Error(err)
	}

	endFDs := countOpenFDs(t)
	fdIncrease := endFDs - startFDs

	t.Logf("Pooled FD increase: %d (should be minimal)", fdIncrease)

	// With pooling, FD increase should be minimal
	if fdIncrease > 10 {
		t.Errorf("Too many FDs with pooling: increased by %d", fdIncrease)
	}
}

// Benchmark to show improvement
func BenchmarkLocalDriver_ConcurrentReads(b *testing.B) {
	driver := setupTestDriver(b)
	ctx := context.Background()

	// Setup test data
	testData := bytes.Repeat([]byte("benchmark"), 1024*10) // 80KB
	driver.Put(ctx, "bench", "test.dat", bytes.NewReader(testData))

	b.Run("WithoutPool", func(b *testing.B) {
		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				reader, _ := driver.Get(ctx, "bench", "test.dat")
				io.Copy(io.Discard, reader)
				reader.Close()
			}
		})
	})

	b.Run("WithPool", func(b *testing.B) {
		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				reader, _ := driver.GetPooled(ctx, "bench", "test.dat")
				io.Copy(io.Discard, reader)
				driver.ReturnPooledReader(reader)
			}
		})
	})
}

// Helper to count open FDs (Linux/Mac)
func countOpenFDs(t testing.TB) int {
	pid := os.Getpid()
	fdPath := fmt.Sprintf("/proc/%d/fd", pid)

	// Try Linux first
	entries, err := os.ReadDir(fdPath)
	if err == nil {
		return len(entries)
	}

	// Mac fallback - use lsof command
	// On Mac, /dev/fd shows system-wide, not process-specific
	return 0 // Return 0 on Mac for now, as accurate counting is complex
}
