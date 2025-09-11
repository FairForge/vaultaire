package chaos

import (
	"bytes"
	"fmt"
	"net"
	"net/http"
	"testing"
	"time"
)

// TestNetworkPartition simulates network failures
func TestNetworkPartition(t *testing.T) {
	// Test with custom transport that simulates network issues
	transport := &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:      1, // Force connection issues
		IdleConnTimeout:   1 * time.Second,
		DisableKeepAlives: true, // Simulate connection drops
	}

	client := &http.Client{
		Transport: transport,
		Timeout:   2 * time.Second,
	}

	failures := 0
	for i := 0; i < 20; i++ {
		// Every 3rd request, close connections abruptly
		if i%3 == 0 {
			transport.CloseIdleConnections()
		}

		req, _ := http.NewRequest("PUT",
			fmt.Sprintf("http://localhost:8000/chaos/partition-%d.txt", i),
			bytes.NewReader([]byte("partition test")))

		resp, err := client.Do(req)
		if err != nil {
			t.Logf("Request %d failed (expected): %v", i, err)
			failures++
		} else {
			_ = resp.Body.Close()
		}

		// Simulate network flapping
		if i%5 == 0 {
			time.Sleep(100 * time.Millisecond)
		}
	}

	t.Logf("Network partition test: %d/%d requests failed", failures, 20)
}
