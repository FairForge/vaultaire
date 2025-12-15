// internal/perf/database.go
package perf

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// DBConfig configures database optimization
type DBConfig struct {
	MaxOpenConns        int
	MaxIdleConns        int
	ConnMaxLifetime     time.Duration
	ConnMaxIdleTime     time.Duration
	StatementCacheSize  int
	EnableQueryLogging  bool
	SlowQueryThreshold  time.Duration
	EnablePreparedStmts bool
}

// DefaultDBConfig returns sensible defaults
func DefaultDBConfig() *DBConfig {
	return &DBConfig{
		MaxOpenConns:        25,
		MaxIdleConns:        10,
		ConnMaxLifetime:     5 * time.Minute,
		ConnMaxIdleTime:     1 * time.Minute,
		StatementCacheSize:  100,
		EnableQueryLogging:  false,
		SlowQueryThreshold:  100 * time.Millisecond,
		EnablePreparedStmts: true,
	}
}

// DBOptimizer optimizes database operations
type DBOptimizer struct {
	mu             sync.RWMutex
	db             *sql.DB
	config         *DBConfig
	stmtCache      map[string]*sql.Stmt
	stats          *DBStats
	slowQueryLog   []SlowQuery
	maxSlowQueries int
}

// DBStats tracks database statistics
type DBStats struct {
	TotalQueries     int64
	TotalDuration    int64 // nanoseconds
	SlowQueries      int64
	Errors           int64
	PreparedHits     int64
	PreparedMisses   int64
	ConnectionsOpen  int
	ConnectionsInUse int
	ConnectionsIdle  int
}

// SlowQuery represents a slow query record
type SlowQuery struct {
	Query     string
	Duration  time.Duration
	Timestamp time.Time
	Args      []interface{}
}

// NewDBOptimizer creates a new database optimizer
func NewDBOptimizer(db *sql.DB, config *DBConfig) *DBOptimizer {
	if config == nil {
		config = DefaultDBConfig()
	}

	// Apply connection pool settings
	db.SetMaxOpenConns(config.MaxOpenConns)
	db.SetMaxIdleConns(config.MaxIdleConns)
	db.SetConnMaxLifetime(config.ConnMaxLifetime)
	db.SetConnMaxIdleTime(config.ConnMaxIdleTime)

	return &DBOptimizer{
		db:             db,
		config:         config,
		stmtCache:      make(map[string]*sql.Stmt),
		stats:          &DBStats{},
		slowQueryLog:   make([]SlowQuery, 0),
		maxSlowQueries: 100,
	}
}

// GetStats returns current database stats
func (o *DBOptimizer) GetStats() *DBStats {
	dbStats := o.db.Stats()

	return &DBStats{
		TotalQueries:     atomic.LoadInt64(&o.stats.TotalQueries),
		TotalDuration:    atomic.LoadInt64(&o.stats.TotalDuration),
		SlowQueries:      atomic.LoadInt64(&o.stats.SlowQueries),
		Errors:           atomic.LoadInt64(&o.stats.Errors),
		PreparedHits:     atomic.LoadInt64(&o.stats.PreparedHits),
		PreparedMisses:   atomic.LoadInt64(&o.stats.PreparedMisses),
		ConnectionsOpen:  dbStats.OpenConnections,
		ConnectionsInUse: dbStats.InUse,
		ConnectionsIdle:  dbStats.Idle,
	}
}

// PrepareStmt prepares and caches a statement
func (o *DBOptimizer) PrepareStmt(ctx context.Context, query string) (*sql.Stmt, error) {
	o.mu.RLock()
	stmt, ok := o.stmtCache[query]
	o.mu.RUnlock()

	if ok {
		atomic.AddInt64(&o.stats.PreparedHits, 1)
		return stmt, nil
	}

	atomic.AddInt64(&o.stats.PreparedMisses, 1)

	o.mu.Lock()
	defer o.mu.Unlock()

	// Double check after acquiring write lock
	if stmt, ok := o.stmtCache[query]; ok {
		return stmt, nil
	}

	// Evict if cache is full
	if len(o.stmtCache) >= o.config.StatementCacheSize {
		o.evictOldestStmt()
	}

	stmt, err := o.db.PrepareContext(ctx, query)
	if err != nil {
		return nil, err
	}

	o.stmtCache[query] = stmt
	return stmt, nil
}

func (o *DBOptimizer) evictOldestStmt() {
	// Simple eviction: remove first entry found
	for k, stmt := range o.stmtCache {
		_ = stmt.Close()
		delete(o.stmtCache, k)
		break
	}
}

// Query executes a query with optimization
func (o *DBOptimizer) Query(ctx context.Context, query string, args ...interface{}) (*sql.Rows, error) {
	start := time.Now()
	defer o.recordQuery(query, start, args)

	if o.config.EnablePreparedStmts {
		stmt, err := o.PrepareStmt(ctx, query)
		if err != nil {
			atomic.AddInt64(&o.stats.Errors, 1)
			return nil, err
		}
		return stmt.QueryContext(ctx, args...)
	}

	return o.db.QueryContext(ctx, query, args...)
}

// QueryRow executes a query returning a single row
func (o *DBOptimizer) QueryRow(ctx context.Context, query string, args ...interface{}) *sql.Row {
	start := time.Now()
	defer o.recordQuery(query, start, args)

	if o.config.EnablePreparedStmts {
		stmt, err := o.PrepareStmt(ctx, query)
		if err != nil {
			atomic.AddInt64(&o.stats.Errors, 1)
			return nil
		}
		return stmt.QueryRowContext(ctx, args...)
	}

	return o.db.QueryRowContext(ctx, query, args...)
}

// Exec executes a statement
func (o *DBOptimizer) Exec(ctx context.Context, query string, args ...interface{}) (sql.Result, error) {
	start := time.Now()
	defer o.recordQuery(query, start, args)

	if o.config.EnablePreparedStmts {
		stmt, err := o.PrepareStmt(ctx, query)
		if err != nil {
			atomic.AddInt64(&o.stats.Errors, 1)
			return nil, err
		}
		return stmt.ExecContext(ctx, args...)
	}

	return o.db.ExecContext(ctx, query, args...)
}

func (o *DBOptimizer) recordQuery(query string, start time.Time, args []interface{}) {
	duration := time.Since(start)
	atomic.AddInt64(&o.stats.TotalQueries, 1)
	atomic.AddInt64(&o.stats.TotalDuration, int64(duration))

	if duration > o.config.SlowQueryThreshold {
		atomic.AddInt64(&o.stats.SlowQueries, 1)
		o.logSlowQuery(query, duration, args)
	}
}

func (o *DBOptimizer) logSlowQuery(query string, duration time.Duration, args []interface{}) {
	o.mu.Lock()
	defer o.mu.Unlock()

	o.slowQueryLog = append(o.slowQueryLog, SlowQuery{
		Query:     query,
		Duration:  duration,
		Timestamp: time.Now(),
		Args:      args,
	})

	if len(o.slowQueryLog) > o.maxSlowQueries {
		o.slowQueryLog = o.slowQueryLog[1:]
	}
}

// GetSlowQueries returns recent slow queries
func (o *DBOptimizer) GetSlowQueries() []SlowQuery {
	o.mu.RLock()
	defer o.mu.RUnlock()

	result := make([]SlowQuery, len(o.slowQueryLog))
	copy(result, o.slowQueryLog)
	return result
}

// ClearSlowQueries clears the slow query log
func (o *DBOptimizer) ClearSlowQueries() {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.slowQueryLog = o.slowQueryLog[:0]
}

// Close closes all cached statements
func (o *DBOptimizer) Close() error {
	o.mu.Lock()
	defer o.mu.Unlock()

	for _, stmt := range o.stmtCache {
		_ = stmt.Close()
	}
	o.stmtCache = make(map[string]*sql.Stmt)
	return nil
}

// Transaction wraps a transaction with optimization
type Transaction struct {
	tx        *sql.Tx
	optimizer *DBOptimizer
	stmtCache map[string]*sql.Stmt
}

// BeginTx starts an optimized transaction
func (o *DBOptimizer) BeginTx(ctx context.Context, opts *sql.TxOptions) (*Transaction, error) {
	tx, err := o.db.BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}

	return &Transaction{
		tx:        tx,
		optimizer: o,
		stmtCache: make(map[string]*sql.Stmt),
	}, nil
}

// Prepare prepares a statement in the transaction
func (t *Transaction) Prepare(ctx context.Context, query string) (*sql.Stmt, error) {
	if stmt, ok := t.stmtCache[query]; ok {
		return stmt, nil
	}

	stmt, err := t.tx.PrepareContext(ctx, query)
	if err != nil {
		return nil, err
	}

	t.stmtCache[query] = stmt
	return stmt, nil
}

// Exec executes a statement in the transaction
func (t *Transaction) Exec(ctx context.Context, query string, args ...interface{}) (sql.Result, error) {
	start := time.Now()
	defer t.optimizer.recordQuery(query, start, args)

	if t.optimizer.config.EnablePreparedStmts {
		stmt, err := t.Prepare(ctx, query)
		if err != nil {
			return nil, err
		}
		return stmt.ExecContext(ctx, args...)
	}

	return t.tx.ExecContext(ctx, query, args...)
}

// Query executes a query in the transaction
func (t *Transaction) Query(ctx context.Context, query string, args ...interface{}) (*sql.Rows, error) {
	start := time.Now()
	defer t.optimizer.recordQuery(query, start, args)

	if t.optimizer.config.EnablePreparedStmts {
		stmt, err := t.Prepare(ctx, query)
		if err != nil {
			return nil, err
		}
		return stmt.QueryContext(ctx, args...)
	}

	return t.tx.QueryContext(ctx, query, args...)
}

// Commit commits the transaction
func (t *Transaction) Commit() error {
	t.closeStmts()
	return t.tx.Commit()
}

// Rollback rolls back the transaction
func (t *Transaction) Rollback() error {
	t.closeStmts()
	return t.tx.Rollback()
}

func (t *Transaction) closeStmts() {
	for _, stmt := range t.stmtCache {
		_ = stmt.Close()
	}
}

// DBHealthCheck performs a database health check
type DBHealthCheck struct {
	db       *sql.DB
	timeout  time.Duration
	interval time.Duration
}

// NewDBHealthCheck creates a health checker
func NewDBHealthCheck(db *sql.DB, timeout time.Duration) *DBHealthCheck {
	return &DBHealthCheck{
		db:       db,
		timeout:  timeout,
		interval: 30 * time.Second,
	}
}

// Check performs the health check
func (h *DBHealthCheck) Check(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, h.timeout)
	defer cancel()
	return h.db.PingContext(ctx)
}

// DBReport generates a database performance report
type DBReport struct {
	GeneratedAt      time.Time
	TotalQueries     int64
	QueriesPerSecond float64
	AvgQueryDuration time.Duration
	SlowQueryCount   int64
	SlowQueryPercent float64
	ErrorCount       int64
	ErrorPercent     float64
	CacheHitRatio    float64
	ConnectionsOpen  int
	ConnectionsInUse int
	ConnectionsIdle  int
	ConnectionUtil   float64
}

// GenerateReport generates a performance report
func (o *DBOptimizer) GenerateReport(duration time.Duration) *DBReport {
	stats := o.GetStats()

	report := &DBReport{
		GeneratedAt:      time.Now(),
		TotalQueries:     stats.TotalQueries,
		SlowQueryCount:   stats.SlowQueries,
		ErrorCount:       stats.Errors,
		ConnectionsOpen:  stats.ConnectionsOpen,
		ConnectionsInUse: stats.ConnectionsInUse,
		ConnectionsIdle:  stats.ConnectionsIdle,
	}

	if duration.Seconds() > 0 {
		report.QueriesPerSecond = float64(stats.TotalQueries) / duration.Seconds()
	}

	if stats.TotalQueries > 0 {
		report.AvgQueryDuration = time.Duration(stats.TotalDuration / stats.TotalQueries)
		report.SlowQueryPercent = float64(stats.SlowQueries) / float64(stats.TotalQueries) * 100
		report.ErrorPercent = float64(stats.Errors) / float64(stats.TotalQueries) * 100
	}

	totalCacheAccess := stats.PreparedHits + stats.PreparedMisses
	if totalCacheAccess > 0 {
		report.CacheHitRatio = float64(stats.PreparedHits) / float64(totalCacheAccess) * 100
	}

	if stats.ConnectionsOpen > 0 {
		report.ConnectionUtil = float64(stats.ConnectionsInUse) / float64(stats.ConnectionsOpen) * 100
	}

	return report
}

// OptimizationHint represents a database optimization suggestion
type OptimizationHint struct {
	Category    string
	Severity    string
	Description string
	Suggestion  string
}

// Analyze analyzes database performance and returns hints
func (o *DBOptimizer) Analyze() []OptimizationHint {
	stats := o.GetStats()
	hints := make([]OptimizationHint, 0)

	// Check slow query percentage
	if stats.TotalQueries > 100 {
		slowPct := float64(stats.SlowQueries) / float64(stats.TotalQueries) * 100
		if slowPct > 10 {
			hints = append(hints, OptimizationHint{
				Category:    "Query Performance",
				Severity:    "high",
				Description: fmt.Sprintf("%.1f%% of queries are slow", slowPct),
				Suggestion:  "Review slow query log and add appropriate indexes",
			})
		}
	}

	// Check error rate
	if stats.TotalQueries > 100 {
		errorPct := float64(stats.Errors) / float64(stats.TotalQueries) * 100
		if errorPct > 1 {
			hints = append(hints, OptimizationHint{
				Category:    "Error Rate",
				Severity:    "high",
				Description: fmt.Sprintf("%.1f%% error rate detected", errorPct),
				Suggestion:  "Investigate query errors and connection issues",
			})
		}
	}

	// Check cache hit ratio
	totalCache := stats.PreparedHits + stats.PreparedMisses
	if totalCache > 100 {
		hitRatio := float64(stats.PreparedHits) / float64(totalCache) * 100
		if hitRatio < 80 {
			hints = append(hints, OptimizationHint{
				Category:    "Statement Cache",
				Severity:    "medium",
				Description: fmt.Sprintf("Cache hit ratio is only %.1f%%", hitRatio),
				Suggestion:  "Increase statement cache size or normalize queries",
			})
		}
	}

	// Check connection utilization
	if stats.ConnectionsOpen > 0 {
		util := float64(stats.ConnectionsInUse) / float64(stats.ConnectionsOpen) * 100
		if util > 90 {
			hints = append(hints, OptimizationHint{
				Category:    "Connection Pool",
				Severity:    "high",
				Description: fmt.Sprintf("Connection pool is %.1f%% utilized", util),
				Suggestion:  "Increase MaxOpenConns or optimize query duration",
			})
		}
	}

	return hints
}
