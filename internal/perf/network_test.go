// internal/perf/network_test.go
package perf

import (
	"net"
	"testing"
	"time"
)

func TestDefaultNetworkConfig(t *testing.T) {
	config := DefaultNetworkConfig()

	if !config.KeepAlive {
		t.Error("expected keep alive enabled")
	}
	if !config.NoDelay {
		t.Error("expected no delay enabled")
	}
	if config.ReadBufferSize != 64*1024 {
		t.Errorf("expected 64KB read buffer, got %d", config.ReadBufferSize)
	}
	if config.MaxConnections != 1000 {
		t.Errorf("expected 1000 max connections, got %d", config.MaxConnections)
	}
}

func TestNewNetworkOptimizer(t *testing.T) {
	opt := NewNetworkOptimizer(nil)

	if opt == nil {
		t.Fatal("expected non-nil optimizer")
	}
	if opt.config == nil {
		t.Error("expected default config")
	}
	if opt.dialer == nil {
		t.Error("expected dialer")
	}
}

func TestNetworkOptimizerConfig(t *testing.T) {
	config := &NetworkConfig{
		MaxConnections: 500,
		KeepAlive:      true,
	}
	opt := NewNetworkOptimizer(config)

	retrieved := opt.Config()
	if retrieved.MaxConnections != 500 {
		t.Errorf("expected 500, got %d", retrieved.MaxConnections)
	}
}

func TestNetworkOptimizerStats(t *testing.T) {
	opt := NewNetworkOptimizer(nil)

	stats := opt.Stats()

	if stats.BytesSent != 0 {
		t.Error("expected 0 bytes sent initially")
	}
	if stats.ConnectionsOpen != 0 {
		t.Error("expected 0 connections initially")
	}
}

func TestNetworkOptimizerGenerateReport(t *testing.T) {
	opt := NewNetworkOptimizer(nil)

	report := opt.GenerateReport()

	if report.GeneratedAt.IsZero() {
		t.Error("expected generated time")
	}
}

func TestBandwidthTracker(t *testing.T) {
	tracker := NewBandwidthTracker(time.Second)

	tracker.Record(1024)
	tracker.Record(2048)

	bandwidth := tracker.Calculate()

	if bandwidth.BytesPerSecond == 0 {
		t.Error("expected non-zero bandwidth")
	}
}

func TestBandwidthTrackerEmpty(t *testing.T) {
	tracker := NewBandwidthTracker(time.Second)

	bandwidth := tracker.Calculate()

	if bandwidth.BytesPerSecond != 0 {
		t.Error("expected zero bandwidth for empty tracker")
	}
}

func TestLatencyTracker(t *testing.T) {
	tracker := NewLatencyTracker(100)

	tracker.Record(10 * time.Millisecond)
	tracker.Record(20 * time.Millisecond)
	tracker.Record(30 * time.Millisecond)

	stats := tracker.Stats()

	if stats.Min != 10*time.Millisecond {
		t.Errorf("expected min 10ms, got %v", stats.Min)
	}
	if stats.Max != 30*time.Millisecond {
		t.Errorf("expected max 30ms, got %v", stats.Max)
	}
	if stats.Avg != 20*time.Millisecond {
		t.Errorf("expected avg 20ms, got %v", stats.Avg)
	}
	if stats.Samples != 3 {
		t.Errorf("expected 3 samples, got %d", stats.Samples)
	}
}

func TestLatencyTrackerEmpty(t *testing.T) {
	tracker := NewLatencyTracker(100)

	stats := tracker.Stats()

	if stats.Samples != 0 {
		t.Error("expected 0 samples")
	}
}

func TestLatencyTrackerP95(t *testing.T) {
	tracker := NewLatencyTracker(100)

	// Add 100 samples
	for i := 1; i <= 100; i++ {
		tracker.Record(time.Duration(i) * time.Millisecond)
	}

	stats := tracker.Stats()

	// P95 should be around 95ms
	if stats.P95 < 90*time.Millisecond || stats.P95 > 100*time.Millisecond {
		t.Errorf("expected P95 around 95ms, got %v", stats.P95)
	}
}

func TestConnectionLimiter(t *testing.T) {
	limiter := NewConnectionLimiter(3)

	if limiter.Max() != 3 {
		t.Errorf("expected max 3, got %d", limiter.Max())
	}

	limiter.Acquire()
	limiter.Acquire()

	if limiter.Current() != 2 {
		t.Errorf("expected 2 current, got %d", limiter.Current())
	}

	limiter.Release()

	if limiter.Current() != 1 {
		t.Errorf("expected 1 current after release, got %d", limiter.Current())
	}
}

func TestConnectionLimiterTryAcquire(t *testing.T) {
	limiter := NewConnectionLimiter(2)

	ok := limiter.TryAcquire()
	if !ok {
		t.Error("expected successful acquire")
	}

	ok = limiter.TryAcquire()
	if !ok {
		t.Error("expected successful acquire")
	}

	// Should fail - at capacity
	ok = limiter.TryAcquire()
	if ok {
		t.Error("expected failed acquire at capacity")
	}

	limiter.Release()

	ok = limiter.TryAcquire()
	if !ok {
		t.Error("expected successful acquire after release")
	}
}

func TestTrackedConn(t *testing.T) {
	opt := NewNetworkOptimizer(nil)

	// Create a mock connection using pipe
	server, client := net.Pipe()
	defer func() { _ = server.Close() }()

	tracked := &trackedConn{
		Conn:      client,
		optimizer: opt,
	}

	// Write data synchronously first
	done := make(chan struct{})
	go func() {
		buf := make([]byte, 10)
		_, _ = server.Read(buf)
		close(done)
	}()

	n, err := tracked.Write([]byte("hello"))
	if err != nil {
		t.Fatalf("write failed: %v", err)
	}
	if n != 5 {
		t.Errorf("expected 5 bytes written, got %d", n)
	}

	<-done // Wait for read to complete

	stats := opt.Stats()
	if stats.BytesSent != 5 {
		t.Errorf("expected 5 bytes sent, got %d", stats.BytesSent)
	}

	_ = tracked.Close()
}

func TestNetworkConfigFields(t *testing.T) {
	config := &NetworkConfig{
		KeepAlive:       true,
		KeepAlivePeriod: 30 * time.Second,
		NoDelay:         true,
		ReadBufferSize:  32 * 1024,
		WriteBufferSize: 32 * 1024,
		ConnectTimeout:  5 * time.Second,
		ReadTimeout:     10 * time.Second,
		WriteTimeout:    10 * time.Second,
		IdleTimeout:     60 * time.Second,
		MaxConnections:  500,
		MaxIdleConns:    50,
		MaxConnsPerHost: 25,
	}

	if config.MaxConnsPerHost != 25 {
		t.Error("unexpected max conns per host")
	}
}

func TestNetworkStatsFields(t *testing.T) {
	stats := &NetworkStats{
		BytesSent:        1024,
		BytesReceived:    2048,
		ConnectionsOpen:  5,
		ConnectionsTotal: 100,
		Errors:           2,
		Latency:          1000000, // 1ms in nanoseconds
	}

	if stats.BytesSent != 1024 {
		t.Error("unexpected bytes sent")
	}
	if stats.Latency != 1000000 {
		t.Error("unexpected latency")
	}
}

func TestBandwidthFields(t *testing.T) {
	bw := Bandwidth{
		BytesPerSecond: 1024 * 1024,
		MBPerSecond:    1.0,
		GBPerSecond:    0.001,
	}

	if bw.MBPerSecond != 1.0 {
		t.Error("unexpected MB/s")
	}
}

func TestLatencyStatsFields(t *testing.T) {
	stats := LatencyStats{
		Min:     5 * time.Millisecond,
		Max:     100 * time.Millisecond,
		Avg:     25 * time.Millisecond,
		P95:     90 * time.Millisecond,
		Samples: 1000,
	}

	if stats.Samples != 1000 {
		t.Error("unexpected sample count")
	}
}

func TestNetworkReportFields(t *testing.T) {
	report := &NetworkReport{
		GeneratedAt:      time.Now(),
		BytesSent:        1000,
		BytesReceived:    2000,
		ConnectionsOpen:  10,
		ConnectionsTotal: 500,
		ErrorRate:        0.5,
		AvgLatency:       10 * time.Millisecond,
	}

	if report.ErrorRate != 0.5 {
		t.Error("unexpected error rate")
	}
}
