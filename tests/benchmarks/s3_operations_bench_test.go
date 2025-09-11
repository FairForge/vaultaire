package benchmarks

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"
)

// Benchmark GET operations
func BenchmarkS3Download(b *testing.B) {
	// First upload a test file
	data := make([]byte, 1024*1024) // 1MB
	req, _ := http.NewRequest("PUT",
		"http://localhost:8000/bench-bucket/download-test.bin",
		bytes.NewReader(data))
	client := &http.Client{Timeout: 10 * time.Second}
	resp, _ := client.Do(req)
	_ = resp.Body.Close()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		req, _ := http.NewRequest("GET",
			"http://localhost:8000/bench-bucket/download-test.bin", nil)
		resp, err := client.Do(req)
		if err != nil {
			b.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
	b.SetBytes(int64(len(data)))
}

// Benchmark LIST operations
func BenchmarkS3List(b *testing.B) {
	client := &http.Client{Timeout: 10 * time.Second}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		req, _ := http.NewRequest("GET",
			"http://localhost:8000/bench-bucket/", nil)
		resp, err := client.Do(req)
		if err != nil {
			b.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
}

// Benchmark concurrent uploads
func BenchmarkS3ConcurrentUpload(b *testing.B) {
	concurrency := []int{1, 10, 50, 100}

	for _, c := range concurrency {
		b.Run(fmt.Sprintf("concurrent-%d", c), func(b *testing.B) {
			data := make([]byte, 100*1024) // 100KB each
			b.ResetTimer()

			b.RunParallel(func(pb *testing.PB) {
				client := &http.Client{Timeout: 10 * time.Second}
				i := 0
				for pb.Next() {
					req, _ := http.NewRequest("PUT",
						fmt.Sprintf("http://localhost:8000/bench-bucket/concurrent-%d.bin", i),
						bytes.NewReader(data))
					resp, err := client.Do(req)
					if err != nil {
						b.Fatal(err)
					}
					_ = resp.Body.Close()
					i++
				}
			})
			b.SetBytes(int64(len(data)))
		})
	}
}
