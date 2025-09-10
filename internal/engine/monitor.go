package engine

import (
	"context"
	"database/sql"
	"time"

	"go.uber.org/zap"
)

// BackendMonitor tracks backend health
type BackendMonitor struct {
	scorer   *HealthScorer
	db       *sql.DB
	logger   *zap.Logger
	interval time.Duration
	stop     chan struct{}

	// Track error rates
	errorCounts   map[string]int
	successCounts map[string]int
}

// NewBackendMonitor creates a monitor
func NewBackendMonitor(db *sql.DB, logger *zap.Logger) *BackendMonitor {
	return &BackendMonitor{
		scorer:        NewHealthScorer(),
		db:            db,
		logger:        logger,
		interval:      30 * time.Second,
		stop:          make(chan struct{}),
		errorCounts:   make(map[string]int),
		successCounts: make(map[string]int),
	}
}

// Start begins monitoring
func (m *BackendMonitor) Start(ctx context.Context, drivers map[string]Driver) {
	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			m.checkAllDrivers(ctx, drivers)
		case <-m.stop:
			return
		case <-ctx.Done():
			return
		}
	}
}

func (m *BackendMonitor) checkAllDrivers(ctx context.Context, drivers map[string]Driver) {
	for id, driver := range drivers {
		metrics := m.probeDriver(ctx, id, driver)
		score := m.scorer.CalculateScore(metrics)

		// Store in database if available
		if m.db != nil {
			m.storeMetrics(ctx, metrics, score)
		}

		m.logger.Info("backend health",
			zap.String("backend", id),
			zap.Float64("score", score),
			zap.Duration("latency", metrics.Latency),
			zap.Float64("error_rate", metrics.ErrorRate))
	}
}

func (m *BackendMonitor) probeDriver(ctx context.Context, id string, driver Driver) HealthMetrics {
	start := time.Now()

	// Use HealthCheck method that exists on Driver
	err := driver.HealthCheck(ctx)
	latency := time.Since(start)

	metrics := HealthMetrics{
		BackendID:  id,
		Timestamp:  time.Now(),
		Latency:    latency,
		Uptime:     1.0,              // Assume up if we can probe it
		Throughput: 10 * 1024 * 1024, // 10MB/s default for now
	}

	// Update error counts
	if err != nil {
		m.errorCounts[id]++
		metrics.LastError = err
	} else {
		m.successCounts[id]++
		metrics.LastSuccess = time.Now()
	}

	// Calculate error rate
	metrics.ErrorRate = m.calculateErrorRate(id)

	return metrics
}

func (m *BackendMonitor) calculateErrorRate(id string) float64 {
	errors := m.errorCounts[id]
	successes := m.successCounts[id]
	total := errors + successes

	if total == 0 {
		return 0
	}

	return float64(errors) / float64(total)
}

func (m *BackendMonitor) storeMetrics(ctx context.Context, metrics HealthMetrics, score float64) {
	if m.db == nil {
		return
	}

	query := `
        INSERT INTO backend_health
        (backend_id, score, latency_ms, error_rate, uptime, throughput_bps, last_error)
        VALUES ($1, $2, $3, $4, $5, $6, $7)
    `

	var lastError *string
	if metrics.LastError != nil {
		errStr := metrics.LastError.Error()
		lastError = &errStr
	}

	_, err := m.db.ExecContext(ctx, query,
		metrics.BackendID,
		score,
		metrics.Latency.Milliseconds(),
		metrics.ErrorRate,
		metrics.Uptime,
		metrics.Throughput,
		lastError,
	)

	if err != nil {
		m.logger.Error("failed to store metrics", zap.Error(err))
	}
}

// Stop stops the monitor
func (m *BackendMonitor) Stop() {
	close(m.stop)
}
