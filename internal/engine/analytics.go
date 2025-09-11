// internal/engine/analytics.go
package engine

import (
	"sort"
	"sync"
	"time"
)

// Analytics tracks backend performance metrics
type Analytics struct {
	mu      sync.RWMutex
	metrics map[string]*BackendMetrics
}

// BackendMetrics holds metrics for a backend
type BackendMetrics struct {
	TotalOperations int
	TotalBytes      int64
	ErrorCount      int
	Latencies       []time.Duration
	Operations      map[string]int // Count by operation type
	LastUpdated     time.Time
}

// BackendStats represents calculated statistics
type BackendStats struct {
	TotalOperations int
	TotalBytes      int64
	ErrorRate       float64
	P50Latency      time.Duration
	P95Latency      time.Duration
	P99Latency      time.Duration
	MeanLatency     time.Duration
	Throughput      float64 // Bytes per second
}

// NewAnalytics creates analytics tracker
func NewAnalytics() *Analytics {
	return &Analytics{
		metrics: make(map[string]*BackendMetrics),
	}
}

// RecordOperation records a backend operation
func (a *Analytics) RecordOperation(backend, operation string,
	latency time.Duration, bytes int64, err error) {

	a.mu.Lock()
	defer a.mu.Unlock()

	if _, ok := a.metrics[backend]; !ok {
		a.metrics[backend] = &BackendMetrics{
			Operations: make(map[string]int),
		}
	}

	m := a.metrics[backend]
	m.TotalOperations++
	m.TotalBytes += bytes
	m.Latencies = append(m.Latencies, latency)
	m.Operations[operation]++
	m.LastUpdated = time.Now()

	if err != nil {
		m.ErrorCount++
	}

	// Keep only last 10000 latencies
	if len(m.Latencies) > 10000 {
		m.Latencies = m.Latencies[1:]
	}
}

// GetStats calculates statistics for a backend
func (a *Analytics) GetStats(backend string) BackendStats {
	a.mu.RLock()
	defer a.mu.RUnlock()

	m, ok := a.metrics[backend]
	if !ok {
		return BackendStats{}
	}

	stats := BackendStats{
		TotalOperations: m.TotalOperations,
		TotalBytes:      m.TotalBytes,
	}

	if m.TotalOperations > 0 {
		stats.ErrorRate = float64(m.ErrorCount) / float64(m.TotalOperations)
	}

	if len(m.Latencies) > 0 {
		stats.P50Latency = percentile(m.Latencies, 50)
		stats.P95Latency = percentile(m.Latencies, 95)
		stats.P99Latency = percentile(m.Latencies, 99)
		stats.MeanLatency = mean(m.Latencies)
	}

	return stats
}

func percentile(latencies []time.Duration, p float64) time.Duration {
	if len(latencies) == 0 {
		return 0
	}

	sorted := make([]time.Duration, len(latencies))
	copy(sorted, latencies)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i] < sorted[j]
	})

	idx := int(float64(len(sorted)-1) * p / 100)
	return sorted[idx]
}

func mean(latencies []time.Duration) time.Duration {
	if len(latencies) == 0 {
		return 0
	}

	var sum time.Duration
	for _, l := range latencies {
		sum += l
	}
	return sum / time.Duration(len(latencies))
}

// GetComparison compares multiple backends
func (a *Analytics) GetComparison(backends []string) map[string]BackendStats {
	comparison := make(map[string]BackendStats)
	for _, backend := range backends {
		comparison[backend] = a.GetStats(backend)
	}
	return comparison
}
