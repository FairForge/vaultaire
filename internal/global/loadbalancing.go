// internal/global/loadbalancing.go
package global

import (
	"context"
	"fmt"
	"math/rand"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// LoadBalancerAlgorithm defines load balancing algorithms
type LoadBalancerAlgorithm string

const (
	AlgorithmRoundRobin         LoadBalancerAlgorithm = "round_robin"
	AlgorithmWeightedRoundRobin LoadBalancerAlgorithm = "weighted_round_robin"
	AlgorithmLeastConnections   LoadBalancerAlgorithm = "least_connections"
	AlgorithmLeastResponseTime  LoadBalancerAlgorithm = "least_response_time"
	AlgorithmIPHash             LoadBalancerAlgorithm = "ip_hash"
	AlgorithmGeoProximity       LoadBalancerAlgorithm = "geo_proximity"
	AlgorithmRandom             LoadBalancerAlgorithm = "random"
)

// BackendState represents the health state of a backend
type BackendState string

const (
	BackendStateHealthy   BackendState = "healthy"
	BackendStateUnhealthy BackendState = "unhealthy"
	BackendStateDraining  BackendState = "draining"
	BackendStateDisabled  BackendState = "disabled"
)

// Backend represents a load balancer backend
type Backend struct {
	ID              string
	Address         string
	Port            int
	Weight          int
	Region          string
	LocationID      string
	State           BackendState
	ActiveConns     int64
	TotalConns      int64
	TotalRequests   int64
	FailedRequests  int64
	AvgResponseTime time.Duration
	LastHealthCheck time.Time
	Metadata        map[string]string
}

// GlobalLoadBalancer manages global traffic distribution
type GlobalLoadBalancer struct {
	mu          sync.RWMutex
	backends    map[string]*Backend
	algorithm   LoadBalancerAlgorithm
	edgeManager *EdgeManager
	config      *LoadBalancerConfig
	rrIndex     uint64
	healthStop  chan struct{}
	healthWg    sync.WaitGroup
}

// LoadBalancerConfig configures the load balancer
type LoadBalancerConfig struct {
	Algorithm                LoadBalancerAlgorithm
	HealthCheckInterval      time.Duration
	HealthCheckTimeout       time.Duration
	HealthCheckPath          string
	HealthyThreshold         int
	UnhealthyThreshold       int
	ConnectionDrainTime      time.Duration
	StickySessionTTL         time.Duration
	EnableHealthChecks       bool
	MaxConnectionsPerBackend int64
}

// DefaultLoadBalancerConfig returns default configuration
func DefaultLoadBalancerConfig() *LoadBalancerConfig {
	return &LoadBalancerConfig{
		Algorithm:                AlgorithmRoundRobin,
		HealthCheckInterval:      30 * time.Second,
		HealthCheckTimeout:       5 * time.Second,
		HealthCheckPath:          "/health",
		HealthyThreshold:         2,
		UnhealthyThreshold:       3,
		ConnectionDrainTime:      30 * time.Second,
		StickySessionTTL:         time.Hour,
		EnableHealthChecks:       true,
		MaxConnectionsPerBackend: 10000,
	}
}

// NewGlobalLoadBalancer creates a new global load balancer
func NewGlobalLoadBalancer(edgeManager *EdgeManager, config *LoadBalancerConfig) *GlobalLoadBalancer {
	if config == nil {
		config = DefaultLoadBalancerConfig()
	}

	return &GlobalLoadBalancer{
		backends:    make(map[string]*Backend),
		algorithm:   config.Algorithm,
		edgeManager: edgeManager,
		config:      config,
		healthStop:  make(chan struct{}),
	}
}

// AddBackend adds a backend to the load balancer
func (lb *GlobalLoadBalancer) AddBackend(backend *Backend) error {
	if backend.ID == "" {
		return fmt.Errorf("backend ID required")
	}

	lb.mu.Lock()
	defer lb.mu.Unlock()

	if backend.Weight == 0 {
		backend.Weight = 1
	}
	if backend.State == "" {
		backend.State = BackendStateHealthy
	}
	if backend.Metadata == nil {
		backend.Metadata = make(map[string]string)
	}

	lb.backends[backend.ID] = backend
	return nil
}

// RemoveBackend removes a backend
func (lb *GlobalLoadBalancer) RemoveBackend(id string) bool {
	lb.mu.Lock()
	defer lb.mu.Unlock()

	if _, ok := lb.backends[id]; !ok {
		return false
	}
	delete(lb.backends, id)
	return true
}

// GetBackend returns a backend by ID
func (lb *GlobalLoadBalancer) GetBackend(id string) (*Backend, bool) {
	lb.mu.RLock()
	defer lb.mu.RUnlock()
	backend, ok := lb.backends[id]
	return backend, ok
}

// GetBackends returns all backends
func (lb *GlobalLoadBalancer) GetBackends() []*Backend {
	lb.mu.RLock()
	defer lb.mu.RUnlock()

	backends := make([]*Backend, 0, len(lb.backends))
	for _, b := range lb.backends {
		backends = append(backends, b)
	}
	return backends
}

// GetHealthyBackends returns only healthy backends
func (lb *GlobalLoadBalancer) GetHealthyBackends() []*Backend {
	lb.mu.RLock()
	defer lb.mu.RUnlock()

	var healthy []*Backend
	for _, b := range lb.backends {
		if b.State == BackendStateHealthy {
			healthy = append(healthy, b)
		}
	}
	// Sort by ID for consistent ordering
	sort.Slice(healthy, func(i, j int) bool {
		return healthy[i].ID < healthy[j].ID
	})
	return healthy
}

// SetBackendState sets the state of a backend
func (lb *GlobalLoadBalancer) SetBackendState(id string, state BackendState) error {
	lb.mu.Lock()
	defer lb.mu.Unlock()

	backend, ok := lb.backends[id]
	if !ok {
		return fmt.Errorf("backend not found: %s", id)
	}
	backend.State = state
	return nil
}

// SetAlgorithm changes the load balancing algorithm
func (lb *GlobalLoadBalancer) SetAlgorithm(algorithm LoadBalancerAlgorithm) {
	lb.mu.Lock()
	defer lb.mu.Unlock()
	lb.algorithm = algorithm
}

// GetAlgorithm returns the current algorithm
func (lb *GlobalLoadBalancer) GetAlgorithm() LoadBalancerAlgorithm {
	lb.mu.RLock()
	defer lb.mu.RUnlock()
	return lb.algorithm
}

// RequestContext contains context for routing decisions
type RequestContext struct {
	ClientIP  string
	Latitude  float64
	Longitude float64
	SessionID string
	Headers   map[string]string
}

// SelectBackend selects a backend based on the configured algorithm
func (lb *GlobalLoadBalancer) SelectBackend(ctx context.Context, reqCtx *RequestContext) (*Backend, error) {
	lb.mu.RLock()
	defer lb.mu.RUnlock()

	healthy := lb.getHealthyBackendsLocked()
	if len(healthy) == 0 {
		return nil, fmt.Errorf("no healthy backends available")
	}

	var selected *Backend

	switch lb.algorithm {
	case AlgorithmRoundRobin:
		selected = lb.roundRobin(healthy)
	case AlgorithmWeightedRoundRobin:
		selected = lb.weightedRoundRobin(healthy)
	case AlgorithmLeastConnections:
		selected = lb.leastConnections(healthy)
	case AlgorithmLeastResponseTime:
		selected = lb.leastResponseTime(healthy)
	case AlgorithmIPHash:
		selected = lb.ipHash(healthy, reqCtx.ClientIP)
	case AlgorithmGeoProximity:
		selected = lb.geoProximity(healthy, reqCtx.Latitude, reqCtx.Longitude)
	case AlgorithmRandom:
		selected = lb.randomSelect(healthy)
	default:
		selected = lb.roundRobin(healthy)
	}

	return selected, nil
}

func (lb *GlobalLoadBalancer) getHealthyBackendsLocked() []*Backend {
	var healthy []*Backend
	for _, b := range lb.backends {
		if b.State == BackendStateHealthy {
			healthy = append(healthy, b)
		}
	}
	// Sort by ID for consistent ordering
	sort.Slice(healthy, func(i, j int) bool {
		return healthy[i].ID < healthy[j].ID
	})
	return healthy
}

func (lb *GlobalLoadBalancer) roundRobin(backends []*Backend) *Backend {
	idx := atomic.AddUint64(&lb.rrIndex, 1) - 1
	return backends[idx%uint64(len(backends))]
}

func (lb *GlobalLoadBalancer) weightedRoundRobin(backends []*Backend) *Backend {
	totalWeight := 0
	for _, b := range backends {
		totalWeight += b.Weight
	}

	if totalWeight == 0 {
		return backends[0]
	}

	idx := atomic.AddUint64(&lb.rrIndex, 1) - 1
	target := int(idx % uint64(totalWeight))

	for _, b := range backends {
		target -= b.Weight
		if target < 0 {
			return b
		}
	}
	return backends[0]
}

func (lb *GlobalLoadBalancer) leastConnections(backends []*Backend) *Backend {
	var selected *Backend
	minConns := int64(^uint64(0) >> 1) // Max int64

	for _, b := range backends {
		conns := atomic.LoadInt64(&b.ActiveConns)
		if conns < minConns {
			minConns = conns
			selected = b
		}
	}
	return selected
}

func (lb *GlobalLoadBalancer) leastResponseTime(backends []*Backend) *Backend {
	var selected *Backend
	minTime := time.Duration(^uint64(0) >> 1) // Max duration

	for _, b := range backends {
		if b.AvgResponseTime < minTime {
			minTime = b.AvgResponseTime
			selected = b
		}
	}

	// Fallback to least connections if no response time data
	if selected == nil || minTime == 0 {
		return lb.leastConnections(backends)
	}
	return selected
}

func (lb *GlobalLoadBalancer) ipHash(backends []*Backend, clientIP string) *Backend {
	if clientIP == "" {
		return lb.roundRobin(backends)
	}

	hash := uint64(0)
	for _, c := range clientIP {
		hash = hash*31 + uint64(c)
	}
	return backends[hash%uint64(len(backends))]
}

func (lb *GlobalLoadBalancer) geoProximity(backends []*Backend, lat, lon float64) *Backend {
	if lat == 0 && lon == 0 {
		return lb.roundRobin(backends)
	}

	var nearest *Backend
	minDist := float64(1e18)

	for _, b := range backends {
		loc, ok := lb.edgeManager.GetLocation(b.LocationID)
		if !ok {
			continue
		}
		dist := haversineDistance(lat, lon, loc.Latitude, loc.Longitude)
		if dist < minDist {
			minDist = dist
			nearest = b
		}
	}

	if nearest == nil {
		return lb.roundRobin(backends)
	}
	return nearest
}

func (lb *GlobalLoadBalancer) randomSelect(backends []*Backend) *Backend {
	return backends[rand.Intn(len(backends))]
}

// RecordRequest records a request to a backend
func (lb *GlobalLoadBalancer) RecordRequest(backendID string, responseTime time.Duration, success bool) {
	lb.mu.Lock()
	defer lb.mu.Unlock()

	backend, ok := lb.backends[backendID]
	if !ok {
		return
	}

	atomic.AddInt64(&backend.TotalRequests, 1)
	if !success {
		atomic.AddInt64(&backend.FailedRequests, 1)
	}

	// Update average response time (exponential moving average)
	if backend.AvgResponseTime == 0 {
		backend.AvgResponseTime = responseTime
	} else {
		alpha := 0.1
		backend.AvgResponseTime = time.Duration(
			float64(backend.AvgResponseTime)*(1-alpha) + float64(responseTime)*alpha,
		)
	}
}

// IncrementConnections increments active connections for a backend
func (lb *GlobalLoadBalancer) IncrementConnections(backendID string) {
	lb.mu.RLock()
	defer lb.mu.RUnlock()

	if backend, ok := lb.backends[backendID]; ok {
		atomic.AddInt64(&backend.ActiveConns, 1)
		atomic.AddInt64(&backend.TotalConns, 1)
	}
}

// DecrementConnections decrements active connections for a backend
func (lb *GlobalLoadBalancer) DecrementConnections(backendID string) {
	lb.mu.RLock()
	defer lb.mu.RUnlock()

	if backend, ok := lb.backends[backendID]; ok {
		atomic.AddInt64(&backend.ActiveConns, -1)
	}
}

// HealthChecker defines a function to check backend health
type HealthChecker func(ctx context.Context, backend *Backend) bool

// StartHealthChecks starts background health checking
func (lb *GlobalLoadBalancer) StartHealthChecks(checker HealthChecker) {
	if !lb.config.EnableHealthChecks {
		return
	}

	lb.healthWg.Add(1)
	go lb.healthCheckLoop(checker)
}

// StopHealthChecks stops health checking
func (lb *GlobalLoadBalancer) StopHealthChecks() {
	close(lb.healthStop)
	lb.healthWg.Wait()
}

func (lb *GlobalLoadBalancer) healthCheckLoop(checker HealthChecker) {
	defer lb.healthWg.Done()

	ticker := time.NewTicker(lb.config.HealthCheckInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			lb.checkAllBackends(checker)
		case <-lb.healthStop:
			return
		}
	}
}

func (lb *GlobalLoadBalancer) checkAllBackends(checker HealthChecker) {
	backends := lb.GetBackends()

	var wg sync.WaitGroup
	for _, backend := range backends {
		if backend.State == BackendStateDisabled {
			continue
		}

		wg.Add(1)
		go func(b *Backend) {
			defer wg.Done()

			ctx, cancel := context.WithTimeout(context.Background(), lb.config.HealthCheckTimeout)
			defer cancel()

			healthy := checker(ctx, b)

			lb.mu.Lock()
			b.LastHealthCheck = time.Now()
			if healthy {
				if b.State == BackendStateUnhealthy {
					b.State = BackendStateHealthy
				}
			} else {
				if b.State == BackendStateHealthy {
					b.State = BackendStateUnhealthy
				}
			}
			lb.mu.Unlock()
		}(backend)
	}
	wg.Wait()
}

// DrainBackend starts draining a backend
func (lb *GlobalLoadBalancer) DrainBackend(id string) error {
	lb.mu.Lock()
	defer lb.mu.Unlock()

	backend, ok := lb.backends[id]
	if !ok {
		return fmt.Errorf("backend not found: %s", id)
	}

	backend.State = BackendStateDraining
	return nil
}

// LoadBalancerStats contains load balancer statistics
type LoadBalancerStats struct {
	TotalBackends     int
	HealthyBackends   int
	UnhealthyBackends int
	DrainingBackends  int
	DisabledBackends  int
	TotalConnections  int64
	ActiveConnections int64
	TotalRequests     int64
	FailedRequests    int64
	AvgResponseTime   time.Duration
	BackendsByRegion  map[string]int
}

// GetStats returns load balancer statistics
func (lb *GlobalLoadBalancer) GetStats() *LoadBalancerStats {
	lb.mu.RLock()
	defer lb.mu.RUnlock()

	stats := &LoadBalancerStats{
		BackendsByRegion: make(map[string]int),
	}

	var totalResponseTime time.Duration
	var responseTimeCount int

	for _, b := range lb.backends {
		stats.TotalBackends++
		stats.TotalConnections += b.TotalConns
		stats.ActiveConnections += b.ActiveConns
		stats.TotalRequests += b.TotalRequests
		stats.FailedRequests += b.FailedRequests

		if b.AvgResponseTime > 0 {
			totalResponseTime += b.AvgResponseTime
			responseTimeCount++
		}

		switch b.State {
		case BackendStateHealthy:
			stats.HealthyBackends++
		case BackendStateUnhealthy:
			stats.UnhealthyBackends++
		case BackendStateDraining:
			stats.DrainingBackends++
		case BackendStateDisabled:
			stats.DisabledBackends++
		}

		if b.Region != "" {
			stats.BackendsByRegion[b.Region]++
		}
	}

	if responseTimeCount > 0 {
		stats.AvgResponseTime = totalResponseTime / time.Duration(responseTimeCount)
	}

	return stats
}

// GetBackendsByRegion returns backends in a specific region
func (lb *GlobalLoadBalancer) GetBackendsByRegion(region string) []*Backend {
	lb.mu.RLock()
	defer lb.mu.RUnlock()

	var result []*Backend
	for _, b := range lb.backends {
		if b.Region == region {
			result = append(result, b)
		}
	}
	return result
}

// SelectBackendInRegion selects a backend within a specific region
func (lb *GlobalLoadBalancer) SelectBackendInRegion(ctx context.Context, region string, reqCtx *RequestContext) (*Backend, error) {
	lb.mu.RLock()
	defer lb.mu.RUnlock()

	var healthy []*Backend
	for _, b := range lb.backends {
		if b.Region == region && b.State == BackendStateHealthy {
			healthy = append(healthy, b)
		}
	}

	if len(healthy) == 0 {
		return nil, fmt.Errorf("no healthy backends in region: %s", region)
	}

	// Sort by ID for consistent ordering
	sort.Slice(healthy, func(i, j int) bool {
		return healthy[i].ID < healthy[j].ID
	})

	// Use the configured algorithm on regional backends
	switch lb.algorithm {
	case AlgorithmLeastConnections:
		return lb.leastConnections(healthy), nil
	case AlgorithmLeastResponseTime:
		return lb.leastResponseTime(healthy), nil
	default:
		return lb.roundRobin(healthy), nil
	}
}

// BackendPool represents a pool of backends for a service
type BackendPool struct {
	ID          string
	Name        string
	Backends    []string
	Algorithm   LoadBalancerAlgorithm
	HealthCheck *HealthCheckConfig
}

// HealthCheckConfig configures health checking for a pool
type HealthCheckConfig struct {
	Enabled            bool
	Path               string
	Interval           time.Duration
	Timeout            time.Duration
	HealthyThreshold   int
	UnhealthyThreshold int
}

// DefaultHealthCheckConfig returns default health check configuration
func DefaultHealthCheckConfig() *HealthCheckConfig {
	return &HealthCheckConfig{
		Enabled:            true,
		Path:               "/health",
		Interval:           30 * time.Second,
		Timeout:            5 * time.Second,
		HealthyThreshold:   2,
		UnhealthyThreshold: 3,
	}
}
