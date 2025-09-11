// internal/engine/load_balancer.go
package engine

import (
	"context"
	"sync"
	"sync/atomic"
)

// LoadBalancingStrategy defines how to distribute load
type LoadBalancingStrategy int

const (
	RoundRobin LoadBalancingStrategy = iota
	LeastConnections
	WeightedRandom
	Adaptive // Based on response times
)

// LoadBalancer distributes requests across backends
type LoadBalancer struct {
	mu       sync.RWMutex
	strategy LoadBalancingStrategy
	backends []BackendWeight
	current  uint64 // For round-robin

	// Track active connections per backend
	connections map[string]*int64
}

// BackendWeight represents a weighted backend
type BackendWeight struct {
	ID     string
	Weight float64
}

// NewLoadBalancer creates a load balancer
func NewLoadBalancer(strategy LoadBalancingStrategy) *LoadBalancer {
	return &LoadBalancer{
		strategy:    strategy,
		backends:    make([]BackendWeight, 0),
		connections: make(map[string]*int64),
	}
}

// AddBackend adds a backend with weight
func (lb *LoadBalancer) AddBackend(id string, weight float64) {
	lb.mu.Lock()
	defer lb.mu.Unlock()

	lb.backends = append(lb.backends, BackendWeight{
		ID:     id,
		Weight: weight,
	})

	// Initialize connection counter
	var counter int64
	lb.connections[id] = &counter
}

// NextBackend selects the next backend based on strategy
func (lb *LoadBalancer) NextBackend(ctx context.Context) string {
	lb.mu.RLock()
	defer lb.mu.RUnlock()

	if len(lb.backends) == 0 {
		return ""
	}

	switch lb.strategy {
	case RoundRobin:
		return lb.roundRobin()
	case LeastConnections:
		return lb.leastConnections()
	case WeightedRandom:
		return lb.weightedRandom()
	case Adaptive:
		return lb.adaptive()
	default:
		return lb.roundRobin()
	}
}

func (lb *LoadBalancer) roundRobin() string {
	n := atomic.AddUint64(&lb.current, 1)
	idx := (n - 1) % uint64(len(lb.backends))
	return lb.backends[idx].ID
}

func (lb *LoadBalancer) leastConnections() string {
	minConns := int64(^uint64(0) >> 1) // Max int64
	selected := ""

	for _, backend := range lb.backends {
		conns := atomic.LoadInt64(lb.connections[backend.ID])
		if conns < minConns {
			minConns = conns
			selected = backend.ID
		}
	}

	return selected
}

func (lb *LoadBalancer) weightedRandom() string {
	// Simple weighted selection
	// TODO: Implement proper weighted random
	return lb.backends[0].ID
}

func (lb *LoadBalancer) adaptive() string {
	// TODO: Select based on response times
	return lb.roundRobin()
}

// StartRequest increments connection count
func (lb *LoadBalancer) StartRequest(backend string) {
	if counter, ok := lb.connections[backend]; ok {
		atomic.AddInt64(counter, 1)
	}
}

// EndRequest decrements connection count
func (lb *LoadBalancer) EndRequest(backend string) {
	if counter, ok := lb.connections[backend]; ok {
		atomic.AddInt64(counter, -1)
	}
}

// GetActiveConnections returns active connection count
func (lb *LoadBalancer) GetActiveConnections(backend string) int {
	if counter, ok := lb.connections[backend]; ok {
		return int(atomic.LoadInt64(counter))
	}
	return 0
}
