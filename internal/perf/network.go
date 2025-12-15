// internal/perf/network.go
package perf

import (
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// NetworkConfig configures network optimizations
type NetworkConfig struct {
	// TCP settings
	KeepAlive       bool
	KeepAlivePeriod time.Duration
	NoDelay         bool // TCP_NODELAY
	ReadBufferSize  int
	WriteBufferSize int

	// Timeouts
	ConnectTimeout time.Duration
	ReadTimeout    time.Duration
	WriteTimeout   time.Duration
	IdleTimeout    time.Duration

	// Connection limits
	MaxConnections  int
	MaxIdleConns    int
	MaxConnsPerHost int
}

// DefaultNetworkConfig returns optimized defaults
func DefaultNetworkConfig() *NetworkConfig {
	return &NetworkConfig{
		KeepAlive:       true,
		KeepAlivePeriod: 30 * time.Second,
		NoDelay:         true,
		ReadBufferSize:  64 * 1024, // 64KB
		WriteBufferSize: 64 * 1024, // 64KB
		ConnectTimeout:  10 * time.Second,
		ReadTimeout:     30 * time.Second,
		WriteTimeout:    30 * time.Second,
		IdleTimeout:     90 * time.Second,
		MaxConnections:  1000,
		MaxIdleConns:    100,
		MaxConnsPerHost: 100,
	}
}

// NetworkStats tracks network statistics
type NetworkStats struct {
	BytesSent        int64
	BytesReceived    int64
	ConnectionsOpen  int64
	ConnectionsTotal int64
	Errors           int64
	Latency          int64 // nanoseconds average
	latencyCount     int64
}

// NetworkOptimizer optimizes network operations
type NetworkOptimizer struct {
	mu     sync.RWMutex
	config *NetworkConfig
	stats  *NetworkStats
	dialer *net.Dialer
}

// NewNetworkOptimizer creates a new network optimizer
func NewNetworkOptimizer(config *NetworkConfig) *NetworkOptimizer {
	if config == nil {
		config = DefaultNetworkConfig()
	}

	dialer := &net.Dialer{
		Timeout:   config.ConnectTimeout,
		KeepAlive: config.KeepAlivePeriod,
	}

	return &NetworkOptimizer{
		config: config,
		stats:  &NetworkStats{},
		dialer: dialer,
	}
}

// Dial creates an optimized connection
func (o *NetworkOptimizer) Dial(network, address string) (net.Conn, error) {
	start := time.Now()

	conn, err := o.dialer.Dial(network, address)
	if err != nil {
		atomic.AddInt64(&o.stats.Errors, 1)
		return nil, err
	}

	// Apply TCP optimizations
	if tcpConn, ok := conn.(*net.TCPConn); ok {
		o.optimizeTCP(tcpConn)
	}

	atomic.AddInt64(&o.stats.ConnectionsOpen, 1)
	atomic.AddInt64(&o.stats.ConnectionsTotal, 1)

	latency := time.Since(start).Nanoseconds()
	o.recordLatency(latency)

	return &trackedConn{
		Conn:      conn,
		optimizer: o,
	}, nil
}

func (o *NetworkOptimizer) optimizeTCP(conn *net.TCPConn) {
	if o.config.NoDelay {
		_ = conn.SetNoDelay(true)
	}
	if o.config.KeepAlive {
		_ = conn.SetKeepAlive(true)
		_ = conn.SetKeepAlivePeriod(o.config.KeepAlivePeriod)
	}
	if o.config.ReadBufferSize > 0 {
		_ = conn.SetReadBuffer(o.config.ReadBufferSize)
	}
	if o.config.WriteBufferSize > 0 {
		_ = conn.SetWriteBuffer(o.config.WriteBufferSize)
	}
}

func (o *NetworkOptimizer) recordLatency(ns int64) {
	atomic.AddInt64(&o.stats.latencyCount, 1)
	// Simple moving average
	count := atomic.LoadInt64(&o.stats.latencyCount)
	current := atomic.LoadInt64(&o.stats.Latency)
	newAvg := current + (ns-current)/count
	atomic.StoreInt64(&o.stats.Latency, newAvg)
}

// Stats returns network statistics
func (o *NetworkOptimizer) Stats() *NetworkStats {
	return &NetworkStats{
		BytesSent:        atomic.LoadInt64(&o.stats.BytesSent),
		BytesReceived:    atomic.LoadInt64(&o.stats.BytesReceived),
		ConnectionsOpen:  atomic.LoadInt64(&o.stats.ConnectionsOpen),
		ConnectionsTotal: atomic.LoadInt64(&o.stats.ConnectionsTotal),
		Errors:           atomic.LoadInt64(&o.stats.Errors),
		Latency:          atomic.LoadInt64(&o.stats.Latency),
	}
}

// Config returns current configuration
func (o *NetworkOptimizer) Config() *NetworkConfig {
	o.mu.RLock()
	defer o.mu.RUnlock()
	return o.config
}

// trackedConn wraps a connection to track stats
type trackedConn struct {
	net.Conn
	optimizer *NetworkOptimizer
	closed    bool
}

func (c *trackedConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	if n > 0 {
		atomic.AddInt64(&c.optimizer.stats.BytesReceived, int64(n))
	}
	if err != nil {
		atomic.AddInt64(&c.optimizer.stats.Errors, 1)
	}
	return n, err
}

func (c *trackedConn) Write(b []byte) (int, error) {
	n, err := c.Conn.Write(b)
	if n > 0 {
		atomic.AddInt64(&c.optimizer.stats.BytesSent, int64(n))
	}
	if err != nil {
		atomic.AddInt64(&c.optimizer.stats.Errors, 1)
	}
	return n, err
}

func (c *trackedConn) Close() error {
	if !c.closed {
		c.closed = true
		atomic.AddInt64(&c.optimizer.stats.ConnectionsOpen, -1)
	}
	return c.Conn.Close()
}

// Bandwidth represents bandwidth measurement
type Bandwidth struct {
	BytesPerSecond float64
	MBPerSecond    float64
	GBPerSecond    float64
}

// BandwidthTracker tracks bandwidth usage
type BandwidthTracker struct {
	mu         sync.Mutex
	samples    []bandwidthSample
	maxSamples int
	windowSize time.Duration
}

type bandwidthSample struct {
	bytes     int64
	timestamp time.Time
}

// NewBandwidthTracker creates a new bandwidth tracker
func NewBandwidthTracker(windowSize time.Duration) *BandwidthTracker {
	return &BandwidthTracker{
		samples:    make([]bandwidthSample, 0),
		maxSamples: 1000,
		windowSize: windowSize,
	}
}

// Record records bytes transferred
func (t *BandwidthTracker) Record(bytes int64) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.samples = append(t.samples, bandwidthSample{
		bytes:     bytes,
		timestamp: time.Now(),
	})

	// Trim old samples
	if len(t.samples) > t.maxSamples {
		t.samples = t.samples[len(t.samples)-t.maxSamples:]
	}
}

// Calculate returns current bandwidth
func (t *BandwidthTracker) Calculate() Bandwidth {
	t.mu.Lock()
	defer t.mu.Unlock()

	if len(t.samples) == 0 {
		return Bandwidth{}
	}

	cutoff := time.Now().Add(-t.windowSize)
	var totalBytes int64
	var validSamples int

	for i := len(t.samples) - 1; i >= 0; i-- {
		if t.samples[i].timestamp.Before(cutoff) {
			break
		}
		totalBytes += t.samples[i].bytes
		validSamples++
	}

	if validSamples == 0 {
		return Bandwidth{}
	}

	seconds := t.windowSize.Seconds()
	bps := float64(totalBytes) / seconds

	return Bandwidth{
		BytesPerSecond: bps,
		MBPerSecond:    bps / (1024 * 1024),
		GBPerSecond:    bps / (1024 * 1024 * 1024),
	}
}

// LatencyTracker tracks connection latency
type LatencyTracker struct {
	mu         sync.Mutex
	samples    []time.Duration
	maxSamples int
}

// NewLatencyTracker creates a new latency tracker
func NewLatencyTracker(maxSamples int) *LatencyTracker {
	return &LatencyTracker{
		samples:    make([]time.Duration, 0),
		maxSamples: maxSamples,
	}
}

// Record records a latency sample
func (t *LatencyTracker) Record(d time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.samples = append(t.samples, d)
	if len(t.samples) > t.maxSamples {
		t.samples = t.samples[1:]
	}
}

// Stats returns latency statistics
func (t *LatencyTracker) Stats() LatencyStats {
	t.mu.Lock()
	defer t.mu.Unlock()

	if len(t.samples) == 0 {
		return LatencyStats{}
	}

	var total time.Duration
	min := t.samples[0]
	max := t.samples[0]

	for _, d := range t.samples {
		total += d
		if d < min {
			min = d
		}
		if d > max {
			max = d
		}
	}

	avg := total / time.Duration(len(t.samples))

	// Calculate P95
	sorted := make([]time.Duration, len(t.samples))
	copy(sorted, t.samples)
	for i := 0; i < len(sorted)-1; i++ {
		for j := i + 1; j < len(sorted); j++ {
			if sorted[j] < sorted[i] {
				sorted[i], sorted[j] = sorted[j], sorted[i]
			}
		}
	}
	p95Idx := int(float64(len(sorted)) * 0.95)
	if p95Idx >= len(sorted) {
		p95Idx = len(sorted) - 1
	}

	return LatencyStats{
		Min:     min,
		Max:     max,
		Avg:     avg,
		P95:     sorted[p95Idx],
		Samples: len(t.samples),
	}
}

// LatencyStats contains latency statistics
type LatencyStats struct {
	Min     time.Duration
	Max     time.Duration
	Avg     time.Duration
	P95     time.Duration
	Samples int
}

// ConnectionLimiter limits concurrent connections
type ConnectionLimiter struct {
	sem     chan struct{}
	max     int
	current int64
}

// NewConnectionLimiter creates a new connection limiter
func NewConnectionLimiter(max int) *ConnectionLimiter {
	return &ConnectionLimiter{
		sem: make(chan struct{}, max),
		max: max,
	}
}

// Acquire acquires a connection slot
func (l *ConnectionLimiter) Acquire() {
	l.sem <- struct{}{}
	atomic.AddInt64(&l.current, 1)
}

// TryAcquire tries to acquire without blocking
func (l *ConnectionLimiter) TryAcquire() bool {
	select {
	case l.sem <- struct{}{}:
		atomic.AddInt64(&l.current, 1)
		return true
	default:
		return false
	}
}

// Release releases a connection slot
func (l *ConnectionLimiter) Release() {
	<-l.sem
	atomic.AddInt64(&l.current, -1)
}

// Current returns current connections
func (l *ConnectionLimiter) Current() int {
	return int(atomic.LoadInt64(&l.current))
}

// Max returns maximum connections
func (l *ConnectionLimiter) Max() int {
	return l.max
}

// NetworkReport generates a network performance report
type NetworkReport struct {
	GeneratedAt      time.Time
	BytesSent        int64
	BytesReceived    int64
	ConnectionsOpen  int64
	ConnectionsTotal int64
	ErrorRate        float64
	AvgLatency       time.Duration
	Bandwidth        Bandwidth
}

// GenerateReport generates a network report
func (o *NetworkOptimizer) GenerateReport() *NetworkReport {
	stats := o.Stats()

	report := &NetworkReport{
		GeneratedAt:      time.Now(),
		BytesSent:        stats.BytesSent,
		BytesReceived:    stats.BytesReceived,
		ConnectionsOpen:  stats.ConnectionsOpen,
		ConnectionsTotal: stats.ConnectionsTotal,
		AvgLatency:       time.Duration(stats.Latency),
	}

	if stats.ConnectionsTotal > 0 {
		report.ErrorRate = float64(stats.Errors) / float64(stats.ConnectionsTotal) * 100
	}

	return report
}
