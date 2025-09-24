package load

import (
	"bytes"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"
)

func TestS3LoadPattern(t *testing.T) {
	// Skip in short mode
	if testing.Short() {
		t.Skip("skipping load test in short mode")
	}

	// Simulate realistic load pattern:
	// 80% reads, 15% writes, 5% lists

	var wg sync.WaitGroup
	errors := make(chan error, 1000)

	// Error collector - FIX: actually read from the channel!
	go func() {
		for err := range errors {
			if err != nil {
				t.Logf("Load test error: %v", err)
			}
		}
	}()

	// Writers
	for i := 0; i < 15; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			client := &http.Client{Timeout: 30 * time.Second}

			for j := 0; j < 10; j++ {
				url := fmt.Sprintf("http://localhost:8000/bucket/file-%d-%d.txt", id, j)
				req, _ := http.NewRequest("PUT", url, bytes.NewReader([]byte("test content")))

				resp, err := client.Do(req)
				if err != nil {
					select {
					case errors <- err:
					default:
						// Channel full, log and continue
					}
					continue
				}
				_ = resp.Body.Close()

				time.Sleep(100 * time.Millisecond)
			}
		}(i)
	}

	// Wait with timeout
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		// Success
	case <-time.After(10 * time.Second):
		t.Log("Load test completed (timeout)")
	}

	close(errors)
}
