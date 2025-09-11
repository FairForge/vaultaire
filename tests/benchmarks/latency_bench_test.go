package benchmarks

import (
	"bytes"
	"fmt"
	"net/http"
	"sort"
	"testing"
	"time"
)

func BenchmarkS3Latency(b *testing.B) {
	sizes := []int{1024, 100 * 1024, 1024 * 1024}

	for _, size := range sizes {
		b.Run(fmt.Sprintf("%dKB", size/1024), func(b *testing.B) {
			data := make([]byte, size)
			latencies := make([]time.Duration, 0, b.N)

			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				start := time.Now()
				req, _ := http.NewRequest("PUT",
					fmt.Sprintf("http://localhost:8000/bench-bucket/latency-%d.bin", i),
					bytes.NewReader(data))
				client := &http.Client{Timeout: 10 * time.Second}
				resp, err := client.Do(req)
				if err == nil {
					_ = resp.Body.Close()
					latencies = append(latencies, time.Since(start))
				}
			}

			if len(latencies) > 0 {
				sort.Slice(latencies, func(i, j int) bool {
					return latencies[i] < latencies[j]
				})

				p50 := latencies[len(latencies)*50/100]
				p95 := latencies[len(latencies)*95/100]
				p99 := latencies[len(latencies)*99/100]

				b.Logf("Size: %dKB - P50: %v, P95: %v, P99: %v",
					size/1024, p50, p95, p99)
			}
		})
	}
}
