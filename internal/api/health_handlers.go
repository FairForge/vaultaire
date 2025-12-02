// internal/api/health_handlers.go
// Step 361: Enhanced Health Monitoring HTTP Handlers
package api

import (
	"encoding/json"
	"net/http"
	"runtime"
	"sync"
	"time"

	"github.com/FairForge/vaultaire/internal/engine"
)

// BackendHealthChecker manages backend health state
type BackendHealthChecker struct {
	mu            sync.RWMutex
	backends      map[string]*BackendHealthState
	scorer        *engine.HealthScorer
	checkInterval time.Duration
}

// BackendHealthState tracks individual backend health
type BackendHealthState struct {
	ID        string
	Healthy   bool
	Score     float64
	Latency   time.Duration
	LastCheck time.Time
	LastError string
}

// NewBackendHealthChecker creates a health checker
func NewBackendHealthChecker() *BackendHealthChecker {
	return &BackendHealthChecker{
		backends:      make(map[string]*BackendHealthState),
		scorer:        engine.NewHealthScorer(),
		checkInterval: 30 * time.Second,
	}
}

// RegisterBackend adds a backend to monitor
func (b *BackendHealthChecker) RegisterBackend(id string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.backends[id] = &BackendHealthState{
		ID:        id,
		Healthy:   true,
		Score:     100.0,
		LastCheck: time.Now(),
	}
}

// UpdateHealth updates backend health status
func (b *BackendHealthChecker) UpdateHealth(id string, healthy bool, latency time.Duration, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	state, exists := b.backends[id]
	if !exists {
		state = &BackendHealthState{ID: id}
		b.backends[id] = state
	}

	state.Healthy = healthy
	state.Latency = latency
	state.LastCheck = time.Now()
	if err != nil {
		state.LastError = err.Error()
	} else {
		state.LastError = ""
	}

	// Calculate score using engine's HealthScorer
	errorRate := 0.0
	if !healthy {
		errorRate = 1.0
	}
	metrics := engine.HealthMetrics{
		BackendID:   id,
		Timestamp:   time.Now(),
		Latency:     latency,
		ErrorRate:   errorRate,
		Uptime:      1.0,
		Throughput:  50 * 1024 * 1024, // Assume 50MB/s baseline
		LastSuccess: time.Now(),
	}
	if !healthy {
		metrics.Uptime = 0.0
	}
	state.Score = b.scorer.CalculateScore(metrics)
}

// GetOverallStatus returns aggregated health status
func (b *BackendHealthChecker) GetOverallStatus() (status string, healthy int, total int) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	total = len(b.backends)
	for _, state := range b.backends {
		if state.Healthy {
			healthy++
		}
	}

	switch {
	case total == 0:
		return "unknown", 0, 0
	case healthy == total:
		return "healthy", healthy, total
	case healthy == 0:
		return "unhealthy", 0, total
	default:
		return "degraded", healthy, total
	}
}

// GetBackendStates returns all backend states
func (b *BackendHealthChecker) GetBackendStates() map[string]*BackendHealthState {
	b.mu.RLock()
	defer b.mu.RUnlock()

	result := make(map[string]*BackendHealthState, len(b.backends))
	for k, v := range b.backends {
		// Copy to avoid race
		copy := *v
		result[k] = &copy
	}
	return result
}

// IsReady returns true if at least one backend is healthy
func (b *BackendHealthChecker) IsReady() bool {
	b.mu.RLock()
	defer b.mu.RUnlock()

	for _, state := range b.backends {
		if state.Healthy {
			return true
		}
	}
	// If no backends registered, consider ready (startup phase)
	return len(b.backends) == 0
}

// handleHealthEnhanced replaces the basic health handler
func (s *Server) handleHealthEnhanced(w http.ResponseWriter, r *http.Request) {
	status, healthy, total := s.healthChecker.GetOverallStatus()

	resp := map[string]interface{}{
		"status":           status,
		"version":          "0.1.0",
		"uptime":           time.Since(s.startTime).Seconds(),
		"backends_healthy": healthy,
		"backends_total":   total,
	}

	// Add backend details if requested
	if r.URL.Query().Get("details") == "true" {
		backends := make(map[string]interface{})
		for id, state := range s.healthChecker.GetBackendStates() {
			backends[id] = map[string]interface{}{
				"status":     boolToStatus(state.Healthy),
				"score":      state.Score,
				"latency_ms": state.Latency.Milliseconds(),
				"last_check": state.LastCheck.Format(time.RFC3339),
			}
		}
		resp["backends"] = backends
	}

	w.Header().Set("Content-Type", "application/json")

	// Set appropriate status code
	httpStatus := http.StatusOK
	if status == "unhealthy" {
		httpStatus = http.StatusServiceUnavailable
	}
	w.WriteHeader(httpStatus)

	_ = json.NewEncoder(w).Encode(resp)
}

// handleLiveness is a simple liveness probe (process alive?)
func (s *Server) handleLiveness(w http.ResponseWriter, r *http.Request) {
	resp := map[string]interface{}{
		"status":    "alive",
		"timestamp": time.Now().Format(time.RFC3339),
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}

// handleReadiness checks if server can accept traffic
func (s *Server) handleReadiness(w http.ResponseWriter, r *http.Request) {
	ready := s.healthChecker.IsReady()

	resp := map[string]interface{}{
		"ready":     ready,
		"timestamp": time.Now().Format(time.RFC3339),
		"memory_mb": getMemoryUsageMB(),
	}

	w.Header().Set("Content-Type", "application/json")

	if ready {
		w.WriteHeader(http.StatusOK)
	} else {
		w.WriteHeader(http.StatusServiceUnavailable)
	}

	_ = json.NewEncoder(w).Encode(resp)
}

// handleBackendsHealth returns detailed backend health
func (s *Server) handleBackendsHealth(w http.ResponseWriter, r *http.Request) {
	backends := make(map[string]map[string]interface{})

	for id, state := range s.healthChecker.GetBackendStates() {
		backends[id] = map[string]interface{}{
			"status":     boolToStatus(state.Healthy),
			"score":      state.Score,
			"latency_ms": state.Latency.Milliseconds(),
			"last_check": state.LastCheck.Format(time.RFC3339),
			"last_error": state.LastError,
		}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(backends)
}

// Helper functions
func boolToStatus(healthy bool) string {
	if healthy {
		return "healthy"
	}
	return "unhealthy"
}

func getMemoryUsageMB() float64 {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return float64(m.Alloc) / 1024 / 1024
}
