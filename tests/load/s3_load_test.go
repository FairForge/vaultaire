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
	// Simulate realistic load pattern:
	// 80% reads, 15% writes, 5% lists

	var wg sync.WaitGroup
	errors := make(chan error, 1000)

	// Writers
	for i := 0; i < 15; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			client := &http.Client{Timeout: 30 * time.Second}
			data := make([]byte, 50*1024) // 50KB average file

			for j := 0; j < 100; j++ {
				req, _ := http.NewRequest("PUT",
					fmt.Sprintf("http://localhost:8000/load-test/file-%d-%d.dat", id, j),
					bytes.NewReader(data))
				resp, err := client.Do(req)
				if err != nil {
					errors <- err
					continue
				}
				_ = resp.Body.Close()
				time.Sleep(100 * time.Millisecond)
			}
		}(i)
	}

	// Readers (placeholder for now)
	for i := 0; i < 80; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			// Add read operations
		}(i)
	}

	wg.Wait()
	close(errors)

	errorCount := 0
	for err := range errors {
		t.Logf("Error: %v", err)
		errorCount++
	}

	if errorCount > 10 {
		t.Fatalf("Too many errors: %d", errorCount)
	}
}
