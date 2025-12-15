// internal/perf/pool_test.go
package perf

import (
	"context"
	"sync"
	"testing"
	"time"
)

func createTestFactory(counter *int64) PoolFactory[*SimpleConn] {
	var mu sync.Mutex
	return func(ctx context.Context) (*SimpleConn, error) {
		mu.Lock()
		*counter++
		id := int(*counter)
		mu.Unlock()
		return &SimpleConn{ID: id, healthy: true}, nil
	}
}

func TestDefaultPoolConfig(t *testing.T) {
	config := DefaultPoolConfig()

	if config.MinSize != 5 {
		t.Errorf("expected min 5, got %d", config.MinSize)
	}
	if config.MaxSize != 25 {
		t.Errorf("expected max 25, got %d", config.MaxSize)
	}
	if config.AcquireTimeout != 30*time.Second {
		t.Error("unexpected acquire timeout")
	}
}

func TestNewPool(t *testing.T) {
	var counter int64
	factory := createTestFactory(&counter)

	config := &PoolConfig{
		MinSize: 3,
		MaxSize: 10,
	}

	pool, err := NewPool(factory, config)
	if err != nil {
		t.Fatalf("failed to create pool: %v", err)
	}
	defer func() { _ = pool.Close() }()

	if counter != 3 {
		t.Errorf("expected 3 pre-warmed connections, got %d", counter)
	}

	if pool.Size() != 3 {
		t.Errorf("expected size 3, got %d", pool.Size())
	}
}

func TestPoolAcquireRelease(t *testing.T) {
	var counter int64
	factory := createTestFactory(&counter)

	config := &PoolConfig{MinSize: 2, MaxSize: 5}
	pool, _ := NewPool(factory, config)
	defer func() { _ = pool.Close() }()

	ctx := context.Background()
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire failed: %v", err)
	}

	if pool.InUse() != 1 {
		t.Errorf("expected 1 in use, got %d", pool.InUse())
	}

	err = pool.Release(conn)
	if err != nil {
		t.Fatalf("release failed: %v", err)
	}

	if pool.InUse() != 0 {
		t.Errorf("expected 0 in use after release, got %d", pool.InUse())
	}
}

func TestPoolGrowsOnDemand(t *testing.T) {
	var counter int64
	factory := createTestFactory(&counter)

	config := &PoolConfig{MinSize: 1, MaxSize: 5}
	pool, _ := NewPool(factory, config)
	defer func() { _ = pool.Close() }()

	ctx := context.Background()

	conn1, _ := pool.Acquire(ctx)
	conn2, _ := pool.Acquire(ctx)

	if counter != 2 {
		t.Errorf("expected 2 connections created, got %d", counter)
	}

	_ = pool.Release(conn1)
	_ = pool.Release(conn2)
}

func TestPoolMaxSize(t *testing.T) {
	var counter int64
	factory := createTestFactory(&counter)

	config := &PoolConfig{
		MinSize:        1,
		MaxSize:        2,
		AcquireTimeout: 50 * time.Millisecond,
	}
	pool, _ := NewPool(factory, config)
	defer func() { _ = pool.Close() }()

	ctx := context.Background()

	conn1, _ := pool.Acquire(ctx)
	conn2, _ := pool.Acquire(ctx)

	_, err := pool.Acquire(ctx)
	if err != ErrConnTimeout {
		t.Errorf("expected timeout error, got %v", err)
	}

	_ = pool.Release(conn1)
	_ = pool.Release(conn2)
}

func TestPoolClose(t *testing.T) {
	var counter int64
	factory := createTestFactory(&counter)

	config := &PoolConfig{MinSize: 2, MaxSize: 5}
	pool, _ := NewPool(factory, config)

	err := pool.Close()
	if err != nil {
		t.Fatalf("close failed: %v", err)
	}

	ctx := context.Background()
	_, err = pool.Acquire(ctx)
	if err != ErrPoolClosed {
		t.Errorf("expected pool closed error, got %v", err)
	}
}

func TestPoolStats(t *testing.T) {
	var counter int64
	factory := createTestFactory(&counter)

	config := &PoolConfig{MinSize: 2, MaxSize: 5}
	pool, _ := NewPool(factory, config)
	defer func() { _ = pool.Close() }()

	ctx := context.Background()
	conn, _ := pool.Acquire(ctx)
	_ = pool.Release(conn)

	stats := pool.Stats()

	if stats.TotalCreated != 2 {
		t.Errorf("expected 2 created, got %d", stats.TotalCreated)
	}
	if stats.TotalAcquired != 1 {
		t.Errorf("expected 1 acquired, got %d", stats.TotalAcquired)
	}
	if stats.TotalReleased != 1 {
		t.Errorf("expected 1 released, got %d", stats.TotalReleased)
	}
}

func TestPoolConcurrentAccess(t *testing.T) {
	var counter int64
	factory := createTestFactory(&counter)

	config := &PoolConfig{MinSize: 5, MaxSize: 10}
	pool, _ := NewPool(factory, config)
	defer func() { _ = pool.Close() }()

	var wg sync.WaitGroup
	ctx := context.Background()

	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			conn, err := pool.Acquire(ctx)
			if err != nil {
				return
			}
			time.Sleep(10 * time.Millisecond)
			_ = pool.Release(conn)
		}()
	}

	wg.Wait()

	stats := pool.Stats()
	if stats.TotalAcquired != stats.TotalReleased {
		t.Errorf("acquire/release mismatch: %d/%d", stats.TotalAcquired, stats.TotalReleased)
	}
}

func TestPoolUnhealthyConnection(t *testing.T) {
	var counter int64
	factory := func(ctx context.Context) (*SimpleConn, error) {
		counter++
		return &SimpleConn{ID: int(counter), healthy: counter > 1}, nil
	}

	config := &PoolConfig{
		MinSize:           1,
		MaxSize:           5,
		EnableHealthCheck: true,
	}
	pool, _ := NewPool(factory, config)
	defer func() { _ = pool.Close() }()

	ctx := context.Background()
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire failed: %v", err)
	}

	if counter < 2 {
		t.Error("expected unhealthy connection to be replaced")
	}

	_ = pool.Release(conn)
}

func TestPoolGenerateReport(t *testing.T) {
	var counter int64
	factory := createTestFactory(&counter)

	config := &PoolConfig{MinSize: 3, MaxSize: 10}
	pool, _ := NewPool(factory, config)
	defer func() { _ = pool.Close() }()

	ctx := context.Background()
	conn, _ := pool.Acquire(ctx)
	_ = pool.Release(conn)

	report := pool.GenerateReport()

	if report.CurrentSize != 3 {
		t.Errorf("expected size 3, got %d", report.CurrentSize)
	}
	if report.MaxSize != 10 {
		t.Errorf("expected max 10, got %d", report.MaxSize)
	}
	if report.TotalAcquired != 1 {
		t.Errorf("expected 1 acquired, got %d", report.TotalAcquired)
	}
}

func TestPoolAvailable(t *testing.T) {
	var counter int64
	factory := createTestFactory(&counter)

	config := &PoolConfig{MinSize: 3, MaxSize: 5}
	pool, _ := NewPool(factory, config)
	defer func() { _ = pool.Close() }()

	if pool.Available() != 3 {
		t.Errorf("expected 3 available, got %d", pool.Available())
	}

	ctx := context.Background()
	conn, _ := pool.Acquire(ctx)

	if pool.Available() != 2 {
		t.Errorf("expected 2 available after acquire, got %d", pool.Available())
	}

	_ = pool.Release(conn)
}

func TestPoolContextCancellation(t *testing.T) {
	var counter int64
	factory := createTestFactory(&counter)

	config := &PoolConfig{MinSize: 1, MaxSize: 1}
	pool, _ := NewPool(factory, config)
	defer func() { _ = pool.Close() }()

	ctx := context.Background()
	conn, _ := pool.Acquire(ctx)

	cancelCtx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := pool.Acquire(cancelCtx)
	if err != context.Canceled {
		t.Errorf("expected context canceled, got %v", err)
	}

	_ = pool.Release(conn)
}

func TestHTTPClientPool(t *testing.T) {
	config := &PoolConfig{MinSize: 2, MaxSize: 5}
	pool, err := NewHTTPClientPool(config)
	if err != nil {
		t.Fatalf("failed to create pool: %v", err)
	}
	defer func() { _ = pool.Close() }()

	ctx := context.Background()
	client, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire failed: %v", err)
	}

	if !client.IsHealthy() {
		t.Error("expected healthy client")
	}

	err = pool.Release(client)
	if err != nil {
		t.Fatalf("release failed: %v", err)
	}
}

func TestHTTPClientPoolStats(t *testing.T) {
	config := &PoolConfig{MinSize: 2, MaxSize: 5}
	pool, _ := NewHTTPClientPool(config)
	defer func() { _ = pool.Close() }()

	stats := pool.Stats()
	if stats.TotalCreated != 2 {
		t.Errorf("expected 2 created, got %d", stats.TotalCreated)
	}
}

func TestSimpleConnInterface(t *testing.T) {
	conn := &SimpleConn{ID: 1, healthy: true}

	if !conn.IsHealthy() {
		t.Error("expected healthy")
	}

	err := conn.Reset()
	if err != nil {
		t.Errorf("reset failed: %v", err)
	}

	err = conn.Close()
	if err != nil {
		t.Errorf("close failed: %v", err)
	}

	if conn.IsHealthy() {
		t.Error("expected unhealthy after close")
	}
}

func TestPoolReleaseAfterClose(t *testing.T) {
	var counter int64
	factory := createTestFactory(&counter)

	config := &PoolConfig{MinSize: 1, MaxSize: 5}
	pool, _ := NewPool(factory, config)

	ctx := context.Background()
	conn, _ := pool.Acquire(ctx)

	_ = pool.Close()

	err := pool.Release(conn)
	if err != ErrPoolClosed {
		t.Errorf("expected pool closed error, got %v", err)
	}
}

func TestPoolUtilization(t *testing.T) {
	var counter int64
	factory := createTestFactory(&counter)

	config := &PoolConfig{MinSize: 4, MaxSize: 10}
	pool, _ := NewPool(factory, config)
	defer func() { _ = pool.Close() }()

	ctx := context.Background()

	conn1, _ := pool.Acquire(ctx)
	conn2, _ := pool.Acquire(ctx)

	report := pool.GenerateReport()

	if report.Utilization != 50.0 {
		t.Errorf("expected 50%% utilization, got %.1f%%", report.Utilization)
	}

	_ = pool.Release(conn1)
	_ = pool.Release(conn2)
}
