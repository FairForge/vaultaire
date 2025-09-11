package benchmarks

import (
	"bytes"
	"fmt"
	"net/http"
	"testing"
	"time"
)

func BenchmarkS3Upload(b *testing.B) {
	sizes := []int{
		1024,        // 1KB
		1024 * 100,  // 100KB
		1024 * 1024, // 1MB
	}

	for _, size := range sizes {
		b.Run(fmt.Sprintf("%dKB", size/1024), func(b *testing.B) {
			data := make([]byte, size)
			b.ResetTimer()

			for i := 0; i < b.N; i++ {
				req, _ := http.NewRequest("PUT",
					fmt.Sprintf("http://localhost:8000/bench-bucket/file-%d.txt", i),
					bytes.NewReader(data))
				req.Header.Set("x-amz-acl", "private")

				client := &http.Client{Timeout: 10 * time.Second}
				resp, err := client.Do(req)
				if err != nil {
					b.Fatal(err)
				}
				_ = resp.Body.Close()

				if resp.StatusCode != 200 {
					b.Fatalf("upload failed: %d", resp.StatusCode)
				}
			}

			b.SetBytes(int64(size))
		})
	}
}
