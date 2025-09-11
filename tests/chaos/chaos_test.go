package chaos

import (
	"bytes"
	"context"
	"fmt"
	"math/rand"
	"net/http"
	"sync"
	"testing"
	"time"
)

// TestRandomFailures simulates random operation failures
func TestRandomFailures(t *testing.T) {
	client := &http.Client{Timeout: 5 * time.Second}

	// Randomly fail some percentage of requests
	for i := 0; i < 100; i++ {
		if rand.Float32() < 0.1 { // 10% failure rate
			// Simulate network failure by using wrong port
			req, _ := http.NewRequest("PUT",
				"http://localhost:9999/chaos/fail.txt",
				bytes.NewReader([]byte("test")))
			_, _ = client.Do(req)
		} else {
			req, _ := http.NewRequest("PUT",
				fmt.Sprintf("http://localhost:8000/chaos/test-%d.txt", i),
				bytes.NewReader([]byte("test")))
			resp, err := client.Do(req)
			if err == nil {
				_ = resp.Body.Close()
			}
		}
	}
}

// TestServerRestart simulates server restart during operations
func TestServerRestart(t *testing.T) {
	// This would need server control, skipping for now
	t.Skip("Requires server restart capability")
}

// TestMemoryPressure creates memory pressure
func TestMemoryPressure(t *testing.T) {
	var wg sync.WaitGroup

	// Create many concurrent operations with large payloads
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()

			// 1MB payload
			data := make([]byte, 1024*1024)
			for j := 0; j < 10; j++ {
				req, _ := http.NewRequest("PUT",
					fmt.Sprintf("http://localhost:8000/chaos/mem-%d-%d.bin", id, j),
					bytes.NewReader(data))

				client := &http.Client{Timeout: 30 * time.Second}
				resp, err := client.Do(req)
				if err == nil {
					_ = resp.Body.Close()
				}
			}
		}(i)
	}

	wg.Wait()
}

// TestTimeoutResilience tests handling of slow operations
func TestTimeoutResilience(t *testing.T) {
	// Use very short timeout
	client := &http.Client{Timeout: 100 * time.Millisecond}

	failures := 0
	for i := 0; i < 50; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		req, _ := http.NewRequestWithContext(ctx,
			"PUT",
			fmt.Sprintf("http://localhost:8000/chaos/timeout-%d.txt", i),
			bytes.NewReader([]byte("timeout test")))

		_, err := client.Do(req)
		if err != nil {
			failures++
		}
		cancel()
	}

	t.Logf("Timeout failures: %d/50 (expected some failures)", failures)
}
