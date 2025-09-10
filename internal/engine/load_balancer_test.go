// internal/engine/load_balancer_test.go
package engine

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestLoadBalancer_RoundRobin(t *testing.T) {
	lb := NewLoadBalancer(RoundRobin)

	backends := []string{"backend1", "backend2", "backend3"}
	for _, b := range backends {
		lb.AddBackend(b, 1.0) // Equal weight
	}

	// Should cycle through backends
	selections := make([]string, 6)
	for i := 0; i < 6; i++ {
		selections[i] = lb.NextBackend(context.Background())
	}

	// Each backend should be selected twice
	counts := make(map[string]int)
	for _, s := range selections {
		counts[s]++
	}

	for _, b := range backends {
		assert.Equal(t, 2, counts[b])
	}
}

func TestLoadBalancer_Concurrent(t *testing.T) {
	lb := NewLoadBalancer(LeastConnections)
	lb.AddBackend("backend1", 1.0)
	lb.AddBackend("backend2", 1.0)

	var wg sync.WaitGroup
	concurrent := 100

	for i := 0; i < concurrent; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()

			backend := lb.NextBackend(context.Background())
			lb.StartRequest(backend)
			defer lb.EndRequest(backend)

			// Simulate work
		}()
	}

	wg.Wait()

	// Both backends should have 0 active connections
	assert.Equal(t, 0, lb.GetActiveConnections("backend1"))
	assert.Equal(t, 0, lb.GetActiveConnections("backend2"))
}
