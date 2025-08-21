package api

import (
	"fmt"
	"net/http"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics holds all Prometheus metrics for the API
type Metrics struct {
	RequestCounter   *prometheus.CounterVec
	LatencyHistogram *prometheus.HistogramVec
	RateLimitHits    *prometheus.CounterVec
	registry         *prometheus.Registry
}

var (
	metricsInstance *Metrics
	metricsOnce     sync.Once
)

// NewMetrics creates and registers all metrics (singleton pattern for tests)
func NewMetrics() *Metrics {
	// Use singleton pattern to avoid duplicate registration
	metricsOnce.Do(func() {
		registry := prometheus.NewRegistry()
		
		m := &Metrics{
			RequestCounter: prometheus.NewCounterVec(
				prometheus.CounterOpts{
					Name: "vaultaire_requests_total",
					Help: "Total number of HTTP requests",
				},
				[]string{"tenant", "method", "path", "status"},
			),
			LatencyHistogram: prometheus.NewHistogramVec(
				prometheus.HistogramOpts{
					Name: "vaultaire_request_duration_seconds",
					Help: "HTTP request latency in seconds",
					Buckets: prometheus.DefBuckets,
				},
				[]string{"tenant", "method", "path"},
			),
			RateLimitHits: prometheus.NewCounterVec(
				prometheus.CounterOpts{
					Name: "vaultaire_rate_limit_hits_total",
					Help: "Total number of rate limit hits",
				},
				[]string{"tenant"},
			),
			registry: registry,
		}
		
		// Register metrics with custom registry
		registry.MustRegister(m.RequestCounter)
		registry.MustRegister(m.LatencyHistogram)
		registry.MustRegister(m.RateLimitHits)
		
		metricsInstance = m
	})
	
	return metricsInstance
}

// IncrementRequest increments the request counter
func (m *Metrics) IncrementRequest(tenant, method, path string, status int) {
	m.RequestCounter.WithLabelValues(tenant, method, path, fmt.Sprintf("%d", status)).Inc()
}

// RecordLatency records request latency
func (m *Metrics) RecordLatency(tenant, method, path string, seconds float64) {
	m.LatencyHistogram.WithLabelValues(tenant, method, path).Observe(seconds)
}

// IncrementRateLimitHit increments rate limit hit counter
func (m *Metrics) IncrementRateLimitHit(tenant string) {
	m.RateLimitHits.WithLabelValues(tenant).Inc()
}

// Handler returns the Prometheus metrics handler
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
}

// ResetForTesting resets the singleton for testing
func ResetMetricsForTesting() {
	metricsInstance = nil
	metricsOnce = sync.Once{}
}
