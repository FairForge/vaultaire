// internal/api/health_handlers.go
// Step 361: Enhanced Health Monitoring HTTP Handlers
package api

import (
	"encoding/json"
	"net"
	"net/http"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/FairForge/vaultaire/internal/clientip"

	dashhandlers "github.com/FairForge/vaultaire/internal/dashboard/handlers"
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
	// Failures counts failed probes since process start (monotonic; exported
	// as vaultaire_backend_probe_failures_total).
	Failures int64
	// ConsecutiveFailures counts failed probes since the last success:
	// Healthy turns false at probeFailuresToUnhealthy (WP-R6-3).
	ConsecutiveFailures int
	// LastSuccess is the last successful probe; zero = none yet, and then
	// the first failure is enough (RP9-9).
	LastSuccess time.Time
}

// probeFailuresToUnhealthy is how many probes in a row must fail before a
// backend is unhealthy — on /health (status, reason), on
// vaultaire_backend_health and on the dashboard (WP-R6-3). One success
// makes it healthy again. The probe runs every 30 s with a 15 s deadline,
// and ONE failure used to flip it: Wasabi's edge lost ~2.5 % of flows a
// minute on 2026-10-17, and 199 single failures in 2.7 days each read as
// an outage. Two in a row is ≥ 30 s of silence across two probes. Every
// failure is still counted and its error kept (LastError).
//
// The two-in-a-row rule is for a backend that has answered at least once: one
// that never has (LastSuccess zero) is unhealthy at its first failure (RP9-9,
// Prompt 10 Part 0.3). RegisterBackend seeds Healthy, so after every slot
// switch /health said "healthy" for ~30 s with eleven dead iDrive drivers.
const probeFailuresToUnhealthy = 2

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

	state.Latency = latency
	state.LastCheck = time.Now()
	if healthy {
		state.ConsecutiveFailures = 0
		state.LastSuccess = state.LastCheck
	} else {
		state.Failures++
		state.ConsecutiveFailures++
	}
	healthy = state.ConsecutiveFailures < probeFailuresToUnhealthy &&
		(state.ConsecutiveFailures == 0 || !state.LastSuccess.IsZero())
	state.Healthy = healthy
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

// failingBackends names the backends unhealthy by the probe (two failed
// probes in a row), sorted.
func (b *BackendHealthChecker) failingBackends() []string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	var out []string
	for id, st := range b.backends {
		if !st.Healthy {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

// backendProvenUp is one backend's probe verdict for the region gate: up =
// healthy AND at least one probe has succeeded (RegisterBackend seeds
// Healthy before the first probe runs). known is false when no probe state
// exists for it at all (never registered: tests, a dev box without probes).
func (b *BackendHealthChecker) backendProvenUp(id string) (up, known bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	st, ok := b.backends[id]
	if !ok {
		return false, false
	}
	return st.Healthy && !st.LastSuccess.IsZero(), true
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
		"version":          BuildSHA,
		"build":            BuildDate,
		"uptime":           time.Since(s.startTime).Seconds(),
		"backends_healthy": healthy,
		"backends_total":   total,
	}

	var reasons []string
	if failing := s.healthChecker.failingBackends(); len(failing) > 0 {
		reasons = append(reasons, "probe failing on "+strings.Join(failing, ", "))
	}
	if s.engine != nil {
		breakers := s.engine.GetFailoverStatus()
		resp["circuit_breakers"] = breakers
		if reason := primaryBreakerReason(s.engine.GetPrimary(), breakers); reason != "" {
			status, healthy, total = withPrimaryBreakerOpen(s.healthChecker, s.engine.GetPrimary(), healthy, total)
			resp["status"], resp["backends_healthy"], resp["backends_total"] = status, healthy, total
			reasons = append(reasons, reason)
		}
	}
	if len(reasons) > 0 {
		resp["reason"] = strings.Join(reasons, "; ")
	}
	// The slot's own port (vaultaire-switch polls it before stopping the old
	// slot): how many detached long operations it would cut. Not through
	// HAProxy — the peer it appends is never loopback.
	if ip := net.ParseIP(clientip.FromRequest(r)); ip != nil && ip.IsLoopback() {
		resp["long_ops_in_flight"] = s.longOps().count()
		resp["chunk_puts_in_flight"] = s.chunkPuts().count()
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

	// Always 200 while this instance is serving, even when every backend probe
	// is failing. HAProxy health-checks this endpoint, so a 503 evicts the
	// instance from rotation — which turns one unreachable third-party backend
	// into a total outage. That happened in production on 2026-07-30: Lyve was
	// the only registered backend, its DNS lookups intermittently timed out,
	// and stored.ge served "503 No server is available" from HAProxy each time.
	//
	// The instance can still serve auth, listings, cached reads, and every
	// other backend, so it belongs in rotation. Backend trouble is reported in
	// the body (status + backends_healthy/backends_total) for operators and
	// dashboards; it must not be signalled by failing the load-balancer check.
	// Process-level liveness is /health/live, which the deploy gate uses.
	w.WriteHeader(http.StatusOK)

	_ = json.NewEncoder(w).Encode(resp)
}

// primaryBreakerReason is the /health reason when the primary's breaker is
// not closed: every write is then falling over (or 503ing) and every read of
// the primary's objects 503s, whatever its probe says (Prompt 2b.5 F2.2 — on
// 2026-10-11 the breaker was open 8.7 h while /health said "healthy").
// Half-open counts (Prompt 2b.6 G2): the breaker reports it 30 s after
// opening and keeps it until a request charges or clears it, so on a dead
// primary with sparse traffic matching only "open" said healthy most of the
// time while vaultaire_backend_circuit_open was 1. No breaker for the
// primary yet is nothing to report.
func primaryBreakerReason(primary string, breakers map[string]string) string {
	state := breakers[primary]
	if primary == "" || state == "" || state == engine.StateClosed.String() {
		return ""
	}
	return "circuit breaker " + state + " on the primary backend " + primary
}

// withPrimaryBreakerOpen counts the primary as one unhealthy backend: a
// healthy probe of it is taken back, an unprobed primary is added.
func withPrimaryBreakerOpen(hc *BackendHealthChecker, primary string, healthy, total int) (string, int, int) {
	if st, ok := hc.GetBackendStates()[primary]; ok {
		if st.Healthy {
			healthy--
		}
	} else {
		total++
	}
	if healthy <= 0 {
		return "unhealthy", 0, total
	}
	return "degraded", healthy, total
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

// healthCheckerAdapter adapts BackendHealthChecker to the dashboard
// handlers.HealthChecker interface (different BackendState types).
type healthCheckerAdapter struct {
	inner *BackendHealthChecker
}

func (a *healthCheckerAdapter) GetBackendStates() map[string]*dashhandlers.BackendState {
	raw := a.inner.GetBackendStates()
	out := make(map[string]*dashhandlers.BackendState, len(raw))
	for k, v := range raw {
		out[k] = &dashhandlers.BackendState{
			Healthy:   v.Healthy,
			Score:     v.Score,
			Latency:   v.Latency,
			LastCheck: v.LastCheck,
			LastError: v.LastError,
		}
	}
	return out
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
