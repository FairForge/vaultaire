// internal/engine/health.go
package engine

import (
	"sync"
	"time"
)

// HealthMetrics represents backend health data
type HealthMetrics struct {
	BackendID   string
	Timestamp   time.Time
	Latency     time.Duration
	ErrorRate   float64 // 0.0 to 1.0
	Uptime      float64 // 0.0 to 1.0
	Throughput  int64   // bytes/second
	LastError   error
	LastSuccess time.Time
}

// HealthScorer calculates health scores
type HealthScorer struct {
	mu      sync.RWMutex
	metrics map[string]*HealthMetrics
	history map[string][]HealthMetrics
	weights ScoreWeights
}

// ScoreWeights defines importance of each metric
type ScoreWeights struct {
	Latency    float64
	ErrorRate  float64
	Uptime     float64
	Throughput float64
}

// NewHealthScorer creates a scorer with default weights
func NewHealthScorer() *HealthScorer {
	return &HealthScorer{
		metrics: make(map[string]*HealthMetrics),
		history: make(map[string][]HealthMetrics),
		weights: ScoreWeights{
			Latency:    0.3,
			ErrorRate:  0.3,
			Uptime:     0.2,
			Throughput: 0.2,
		},
	}
}

// CalculateScore returns 0-100 health score
func (h *HealthScorer) CalculateScore(m HealthMetrics) float64 {
	score := 0.0

	// Latency scoring (lower is better)
	// Perfect: <50ms, Good: <200ms, Poor: >1s
	latencyScore := 100.0
	if m.Latency > 50*time.Millisecond {
		latencyScore = 100.0 - float64(m.Latency.Milliseconds()-50)*0.1
	}
	if latencyScore < 0 {
		latencyScore = 0
	}

	// Error rate scoring (0% = 100, 10% = 0)
	errorScore := (1.0 - m.ErrorRate) * 100

	// Uptime scoring (direct percentage)
	uptimeScore := m.Uptime * 100

	// Throughput scoring (>50MB/s = 100)
	throughputScore := float64(m.Throughput) / (50 * 1024 * 1024) * 100
	if throughputScore > 100 {
		throughputScore = 100
	}

	// Weighted average
	score = latencyScore*h.weights.Latency +
		errorScore*h.weights.ErrorRate +
		uptimeScore*h.weights.Uptime +
		throughputScore*h.weights.Throughput

	return score
}

// UpdateMetrics stores the latest metrics for a backend
func (h *HealthScorer) UpdateMetrics(backendID string, metrics HealthMetrics) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.metrics[backendID] = &metrics

	// Keep history (last 100 entries per backend)
	if h.history[backendID] == nil {
		h.history[backendID] = []HealthMetrics{}
	}
	h.history[backendID] = append(h.history[backendID], metrics)
	if len(h.history[backendID]) > 100 {
		h.history[backendID] = h.history[backendID][1:]
	}
}

// GetMetrics retrieves metrics for a backend
func (h *HealthScorer) GetMetrics(backendID string) *HealthMetrics {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.metrics[backendID]
}
