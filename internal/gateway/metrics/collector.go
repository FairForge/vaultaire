// internal/gateway/metrics/collector.go
package metrics

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	// Request metrics
	requestsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "vaultaire_gateway_requests_total",
			Help: "Total number of requests processed",
		},
		[]string{"method", "endpoint", "status"},
	)

	requestDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "vaultaire_gateway_request_duration_seconds",
			Help:    "Request duration in seconds",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"method", "endpoint"},
	)

	requestSize = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "vaultaire_gateway_request_size_bytes",
			Help:    "Request size in bytes",
			Buckets: prometheus.ExponentialBuckets(100, 10, 8),
		},
		[]string{"method", "endpoint"},
	)

	responseSize = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "vaultaire_gateway_response_size_bytes",
			Help:    "Response size in bytes",
			Buckets: prometheus.ExponentialBuckets(100, 10, 8),
		},
		[]string{"method", "endpoint"},
	)

	// Active connections
	activeConnections = promauto.NewGauge(
		prometheus.GaugeOpts{
			Name: "vaultaire_gateway_connections_active",
			Help: "Number of active connections",
		},
	)

	// Error metrics
	errorsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "vaultaire_gateway_errors_total",
			Help: "Total number of errors",
		},
		[]string{"type", "endpoint"},
	)

	// Cache metrics
	cacheHits = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "vaultaire_gateway_cache_hits_total",
			Help: "Total number of cache hits",
		},
	)

	cacheMisses = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "vaultaire_gateway_cache_misses_total",
			Help: "Total number of cache misses",
		},
	)
)

// Collector manages metrics collection
type Collector struct {
	startTime time.Time
}

// NewCollector creates a metrics collector
func NewCollector() *Collector {
	return &Collector{
		startTime: time.Now(),
	}
}

// RecordRequest records metrics for a request
func (c *Collector) RecordRequest(method, endpoint string, status int, duration time.Duration, reqSize, respSize int64) {
	statusStr := statusClass(status)

	requestsTotal.WithLabelValues(method, endpoint, statusStr).Inc()
	requestDuration.WithLabelValues(method, endpoint).Observe(duration.Seconds())

	if reqSize > 0 {
		requestSize.WithLabelValues(method, endpoint).Observe(float64(reqSize))
	}

	if respSize > 0 {
		responseSize.WithLabelValues(method, endpoint).Observe(float64(respSize))
	}
}

// RecordError records an error
func (c *Collector) RecordError(errorType, endpoint string) {
	errorsTotal.WithLabelValues(errorType, endpoint).Inc()
}

// RecordCacheHit records a cache hit
func (c *Collector) RecordCacheHit() {
	cacheHits.Inc()
}

// RecordCacheMiss records a cache miss
func (c *Collector) RecordCacheMiss() {
	cacheMisses.Inc()
}

// IncrementConnections increments active connections
func (c *Collector) IncrementConnections() {
	activeConnections.Inc()
}

// DecrementConnections decrements active connections
func (c *Collector) DecrementConnections() {
	activeConnections.Dec()
}

// Uptime returns the uptime duration
func (c *Collector) Uptime() time.Duration {
	return time.Since(c.startTime)
}

func statusClass(status int) string {
	switch {
	case status >= 200 && status < 300:
		return "2xx"
	case status >= 300 && status < 400:
		return "3xx"
	case status >= 400 && status < 500:
		return "4xx"
	case status >= 500:
		return "5xx"
	default:
		return "unknown"
	}
}
