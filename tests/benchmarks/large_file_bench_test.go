package benchmarks

import (
	"bytes"
	"fmt"
	"net/http"
	"testing"
	"time"
)

func BenchmarkLargeFileUpload(b *testing.B) {
	sizes := []struct {
		name string
		size int
	}{
		{"10MB", 10 * 1024 * 1024},
		{"50MB", 50 * 1024 * 1024},
		{"100MB", 100 * 1024 * 1024},
	}

	for _, tc := range sizes {
		b.Run(tc.name, func(b *testing.B) {
			data := make([]byte, tc.size)
			b.ResetTimer()

			for i := 0; i < b.N; i++ {
				req, _ := http.NewRequest("PUT",
					fmt.Sprintf("http://localhost:8000/bench-bucket/large-%d.bin", i),
					bytes.NewReader(data))

				client := &http.Client{Timeout: 5 * time.Minute}
				resp, err := client.Do(req)
				if err != nil {
					b.Fatal(err)
				}
				_ = resp.Body.Close()

				if resp.StatusCode != 200 {
					b.Fatalf("upload failed: %d", resp.StatusCode)
				}
			}

			b.SetBytes(int64(tc.size))
		})
	}
}
