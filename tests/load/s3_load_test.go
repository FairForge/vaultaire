package load

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestS3LoadPattern(t *testing.T) {
	// Skip if no server running
	resp, err := http.Get("http://localhost:8000/health")
	if err != nil {
		t.Skip("S3 server not running, skipping load test")
	}
	_ = resp.Body.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	errors := make(chan error, 100)

	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				select {
				case <-ctx.Done():
					return
				default:
				}

				url := fmt.Sprintf("http://localhost:8000/bucket/file-%d-%d.txt", workerID, j)
				req, _ := http.NewRequestWithContext(ctx, "PUT", url, strings.NewReader("test data"))

				client := &http.Client{Timeout: 2 * time.Second}
				resp, err := client.Do(req)
				if err != nil {
					select {
					case errors <- err:
					default:
					}
					return
				}
				_ = resp.Body.Close()
			}
		}(i)
	}

	wg.Wait()
	close(errors)

	for err := range errors {
		t.Logf("Load test error: %v", err)
	}
}
