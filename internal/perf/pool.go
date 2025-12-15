// internal/perf/pool.go
package perf

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"
)

var (
	ErrPoolClosed    = errors.New("pool is closed")
	ErrPoolExhausted = errors.New("pool exhausted")
	ErrConnTimeout   = errors.New("connection timeout")
)

// Poolable represents a poolable resource
type Poolable interface {
	Close() error
	IsHealthy() bool
	Reset() error
}

// PoolFactory creates new poolable resources
type PoolFactory[T Poolable] func(ctx context.Context) (T, error)

// Pool is a generic connection pool
type Pool[T Poolable] struct {
	mu          sync.Mutex
	factory     PoolFactory[T]
	connections chan T
	config      *PoolConfig
	stats       *PoolStats
	closed      atomic.Bool
}

// PoolConfig configures the connection pool
type PoolConfig struct {
	MinSize             int
	MaxSize             int
	MaxIdleTime         time.Duration
	MaxLifetime         time.Duration
	AcquireTimeout      time.Duration
	HealthCheckInterval time.Duration
	EnableHealthCheck   bool
}

// DefaultPoolConfig returns default pool configuration
func DefaultPoolConfig() *PoolConfig {
	return &PoolConfig{
		MinSize:             5,
		MaxSize:             25,
		MaxIdleTime:         5 * time.Minute,
		MaxLifetime:         30 * time.Minute,
		AcquireTimeout:      30 * time.Second,
		HealthCheckInterval: 30 * time.Second,
		EnableHealthCheck:   true,
	}
}

// PoolStats tracks pool statistics
type PoolStats struct {
	TotalCreated  int64
	TotalClosed   int64
	TotalAcquired int64
	TotalReleased int64
	TotalTimeouts int64
	TotalErrors   int64
	CurrentSize   int64
	CurrentInUse  int64
	CurrentIdle   int64
	WaitCount     int64
	WaitDuration  int64 // nanoseconds
}

// NewPool creates a new connection pool
func NewPool[T Poolable](factory PoolFactory[T], config *PoolConfig) (*Pool[T], error) {
	if config == nil {
		config = DefaultPoolConfig()
	}

	if config.MaxSize < config.MinSize {
		config.MaxSize = config.MinSize
	}

	p := &Pool[T]{
		factory:     factory,
		connections: make(chan T, config.MaxSize),
		config:      config,
		stats:       &PoolStats{},
	}

	// Pre-warm pool with minimum connections
	ctx := context.Background()
	for i := 0; i < config.MinSize; i++ {
		conn, err := factory(ctx)
		if err != nil {
			// Close any created connections on error
			close(p.connections)
			return nil, err
		}
		p.connections <- conn
		atomic.AddInt64(&p.stats.TotalCreated, 1)
		atomic.AddInt64(&p.stats.CurrentSize, 1)
		atomic.AddInt64(&p.stats.CurrentIdle, 1)
	}

	return p, nil
}

// Acquire gets a connection from the pool
func (p *Pool[T]) Acquire(ctx context.Context) (T, error) {
	var zero T

	if p.closed.Load() {
		return zero, ErrPoolClosed
	}

	atomic.AddInt64(&p.stats.WaitCount, 1)
	waitStart := time.Now()

	// Try to get an existing connection
	select {
	case conn := <-p.connections:
		atomic.AddInt64(&p.stats.WaitDuration, int64(time.Since(waitStart)))
		atomic.AddInt64(&p.stats.CurrentIdle, -1)
		atomic.AddInt64(&p.stats.CurrentInUse, 1)
		atomic.AddInt64(&p.stats.TotalAcquired, 1)

		// Health check
		if p.config.EnableHealthCheck && !conn.IsHealthy() {
			_ = conn.Close()
			atomic.AddInt64(&p.stats.TotalClosed, 1)
			atomic.AddInt64(&p.stats.CurrentSize, -1)
			atomic.AddInt64(&p.stats.CurrentInUse, -1)
			// Create a new one
			return p.createNew(ctx)
		}

		return conn, nil

	case <-ctx.Done():
		atomic.AddInt64(&p.stats.WaitDuration, int64(time.Since(waitStart)))
		atomic.AddInt64(&p.stats.TotalTimeouts, 1)
		return zero, ctx.Err()

	default:
		// No connection available, try to create new
		currentSize := atomic.LoadInt64(&p.stats.CurrentSize)
		if int(currentSize) < p.config.MaxSize {
			return p.createNew(ctx)
		}
	}

	// Pool at max, wait for one
	timeout := p.config.AcquireTimeout
	if deadline, ok := ctx.Deadline(); ok {
		if d := time.Until(deadline); d < timeout {
			timeout = d
		}
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case conn := <-p.connections:
		atomic.AddInt64(&p.stats.WaitDuration, int64(time.Since(waitStart)))
		atomic.AddInt64(&p.stats.CurrentIdle, -1)
		atomic.AddInt64(&p.stats.CurrentInUse, 1)
		atomic.AddInt64(&p.stats.TotalAcquired, 1)
		return conn, nil

	case <-timer.C:
		atomic.AddInt64(&p.stats.WaitDuration, int64(time.Since(waitStart)))
		atomic.AddInt64(&p.stats.TotalTimeouts, 1)
		return zero, ErrConnTimeout

	case <-ctx.Done():
		atomic.AddInt64(&p.stats.WaitDuration, int64(time.Since(waitStart)))
		atomic.AddInt64(&p.stats.TotalTimeouts, 1)
		return zero, ctx.Err()
	}
}

func (p *Pool[T]) createNew(ctx context.Context) (T, error) {
	var zero T

	conn, err := p.factory(ctx)
	if err != nil {
		atomic.AddInt64(&p.stats.TotalErrors, 1)
		return zero, err
	}

	atomic.AddInt64(&p.stats.TotalCreated, 1)
	atomic.AddInt64(&p.stats.CurrentSize, 1)
	atomic.AddInt64(&p.stats.CurrentInUse, 1)
	atomic.AddInt64(&p.stats.TotalAcquired, 1)

	return conn, nil
}

// Release returns a connection to the pool
func (p *Pool[T]) Release(conn T) error {
	if p.closed.Load() {
		_ = conn.Close()
		atomic.AddInt64(&p.stats.TotalClosed, 1)
		return ErrPoolClosed
	}

	// Reset connection state
	if err := conn.Reset(); err != nil {
		_ = conn.Close()
		atomic.AddInt64(&p.stats.TotalClosed, 1)
		atomic.AddInt64(&p.stats.CurrentSize, -1)
		atomic.AddInt64(&p.stats.CurrentInUse, -1)
		return err
	}

	atomic.AddInt64(&p.stats.CurrentInUse, -1)
	atomic.AddInt64(&p.stats.TotalReleased, 1)

	select {
	case p.connections <- conn:
		atomic.AddInt64(&p.stats.CurrentIdle, 1)
		return nil
	default:
		// Pool is full, close this connection
		_ = conn.Close()
		atomic.AddInt64(&p.stats.TotalClosed, 1)
		atomic.AddInt64(&p.stats.CurrentSize, -1)
		return nil
	}
}

// Close closes the pool and all connections
func (p *Pool[T]) Close() error {
	if p.closed.Swap(true) {
		return ErrPoolClosed
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	close(p.connections)
	for conn := range p.connections {
		_ = conn.Close()
		atomic.AddInt64(&p.stats.TotalClosed, 1)
	}

	return nil
}

// Stats returns current pool statistics
func (p *Pool[T]) Stats() *PoolStats {
	return &PoolStats{
		TotalCreated:  atomic.LoadInt64(&p.stats.TotalCreated),
		TotalClosed:   atomic.LoadInt64(&p.stats.TotalClosed),
		TotalAcquired: atomic.LoadInt64(&p.stats.TotalAcquired),
		TotalReleased: atomic.LoadInt64(&p.stats.TotalReleased),
		TotalTimeouts: atomic.LoadInt64(&p.stats.TotalTimeouts),
		TotalErrors:   atomic.LoadInt64(&p.stats.TotalErrors),
		CurrentSize:   atomic.LoadInt64(&p.stats.CurrentSize),
		CurrentInUse:  atomic.LoadInt64(&p.stats.CurrentInUse),
		CurrentIdle:   atomic.LoadInt64(&p.stats.CurrentIdle),
		WaitCount:     atomic.LoadInt64(&p.stats.WaitCount),
		WaitDuration:  atomic.LoadInt64(&p.stats.WaitDuration),
	}
}

// Size returns current pool size
func (p *Pool[T]) Size() int {
	return int(atomic.LoadInt64(&p.stats.CurrentSize))
}

// Available returns number of available connections
func (p *Pool[T]) Available() int {
	return len(p.connections)
}

// InUse returns number of connections in use
func (p *Pool[T]) InUse() int {
	return int(atomic.LoadInt64(&p.stats.CurrentInUse))
}

// PoolReport generates a pool status report
type PoolReport struct {
	GeneratedAt     time.Time
	CurrentSize     int
	CurrentInUse    int
	CurrentIdle     int
	MaxSize         int
	MinSize         int
	Utilization     float64
	TotalAcquired   int64
	TotalReleased   int64
	TotalTimeouts   int64
	AvgWaitDuration time.Duration
}

// GenerateReport generates a pool status report
func (p *Pool[T]) GenerateReport() *PoolReport {
	stats := p.Stats()

	report := &PoolReport{
		GeneratedAt:   time.Now(),
		CurrentSize:   int(stats.CurrentSize),
		CurrentInUse:  int(stats.CurrentInUse),
		CurrentIdle:   int(stats.CurrentIdle),
		MaxSize:       p.config.MaxSize,
		MinSize:       p.config.MinSize,
		TotalAcquired: stats.TotalAcquired,
		TotalReleased: stats.TotalReleased,
		TotalTimeouts: stats.TotalTimeouts,
	}

	if stats.CurrentSize > 0 {
		report.Utilization = float64(stats.CurrentInUse) / float64(stats.CurrentSize) * 100
	}

	if stats.WaitCount > 0 {
		report.AvgWaitDuration = time.Duration(stats.WaitDuration / stats.WaitCount)
	}

	return report
}

// SimpleConn is a simple poolable connection for testing
type SimpleConn struct {
	ID      int
	healthy bool
	closed  bool
}

// Close closes the connection
func (c *SimpleConn) Close() error {
	c.closed = true
	return nil
}

// IsHealthy returns if connection is healthy
func (c *SimpleConn) IsHealthy() bool {
	return c.healthy && !c.closed
}

// Reset resets connection state
func (c *SimpleConn) Reset() error {
	return nil
}

// HTTPClientPool manages pooled HTTP clients
type HTTPClientPool struct {
	pool *Pool[*PooledHTTPClient]
}

// PooledHTTPClient wraps an HTTP client for pooling
type PooledHTTPClient struct {
	healthy bool
	closed  bool
}

// Close closes the client
func (c *PooledHTTPClient) Close() error {
	c.closed = true
	return nil
}

// IsHealthy returns if client is healthy
func (c *PooledHTTPClient) IsHealthy() bool {
	return c.healthy && !c.closed
}

// Reset resets client state
func (c *PooledHTTPClient) Reset() error {
	return nil
}

// NewHTTPClientPool creates a new HTTP client pool
func NewHTTPClientPool(config *PoolConfig) (*HTTPClientPool, error) {
	factory := func(ctx context.Context) (*PooledHTTPClient, error) {
		return &PooledHTTPClient{healthy: true}, nil
	}

	pool, err := NewPool(factory, config)
	if err != nil {
		return nil, err
	}

	return &HTTPClientPool{pool: pool}, nil
}

// Acquire gets an HTTP client from the pool
func (p *HTTPClientPool) Acquire(ctx context.Context) (*PooledHTTPClient, error) {
	return p.pool.Acquire(ctx)
}

// Release returns an HTTP client to the pool
func (p *HTTPClientPool) Release(client *PooledHTTPClient) error {
	return p.pool.Release(client)
}

// Close closes the pool
func (p *HTTPClientPool) Close() error {
	return p.pool.Close()
}

// Stats returns pool statistics
func (p *HTTPClientPool) Stats() *PoolStats {
	return p.pool.Stats()
}
