package api

import (
	"context"
	"database/sql"
	"net/http"
	"sync"
	"time"

	"go.uber.org/zap"
)

// countingResponseWriter wraps http.ResponseWriter to count bytes written.
type countingResponseWriter struct {
	http.ResponseWriter
	bytesWritten int64
}

func (cw *countingResponseWriter) Write(b []byte) (int, error) {
	n, err := cw.ResponseWriter.Write(b)
	cw.bytesWritten += int64(n)
	return n, err
}

// bandwidthEvent represents a single ingress/egress event for a tenant.
type bandwidthEvent struct {
	tenantID string
	ingress  int64
	egress   int64
}

// BandwidthTracker buffers bandwidth events and flushes them to the
// bandwidth_usage_daily table periodically.
type BandwidthTracker struct {
	db     *sql.DB
	mu     sync.Mutex
	buffer []bandwidthEvent
	logger *zap.Logger
}

// NewBandwidthTracker creates a new tracker. Pass nil db to buffer without flushing.
func NewBandwidthTracker(db *sql.DB) *BandwidthTracker {
	return &BandwidthTracker{
		db:     db,
		buffer: make([]bandwidthEvent, 0, 128),
	}
}

// SetLogger sets the logger for the tracker.
func (bt *BandwidthTracker) SetLogger(logger *zap.Logger) {
	bt.logger = logger
}

// Record adds a bandwidth event to the buffer.
func (bt *BandwidthTracker) Record(_ context.Context, tenantID string, ingress, egress int64) {
	if tenantID == "" || (ingress == 0 && egress == 0) {
		return
	}

	bt.mu.Lock()
	bt.buffer = append(bt.buffer, bandwidthEvent{
		tenantID: tenantID,
		ingress:  ingress,
		egress:   egress,
	})
	needsFlush := len(bt.buffer) >= 100
	bt.mu.Unlock()

	if needsFlush {
		bt.Flush()
	}
}

// StartFlusher runs a background goroutine that flushes the buffer every interval.
// It stops when ctx is cancelled.
func (bt *BandwidthTracker) StartFlusher(ctx context.Context, interval time.Duration) {
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				bt.Flush() // Final flush on shutdown.
				return
			case <-ticker.C:
				bt.Flush()
			}
		}
	}()
}

// IsOverLimit checks if a tenant has exceeded their monthly bandwidth limit.
// Returns false (allows access) if DB is nil or on any error (fail open).
func (bt *BandwidthTracker) IsOverLimit(ctx context.Context, tenantID string) bool {
	if bt.db == nil {
		return false
	}

	var usedBytes, limitBytes sql.NullInt64
	err := bt.db.QueryRowContext(ctx, `
		SELECT
			COALESCE(SUM(b.ingress_bytes + b.egress_bytes), 0),
			q.bandwidth_limit_bytes
		FROM tenant_quotas q
		LEFT JOIN bandwidth_usage_daily b
			ON b.tenant_id = q.tenant_id
			AND b.date >= date_trunc('month', CURRENT_DATE)
		WHERE q.tenant_id = $1
		GROUP BY q.bandwidth_limit_bytes
	`, tenantID).Scan(&usedBytes, &limitBytes)
	if err != nil {
		return false // fail open
	}

	// No limit set means unlimited bandwidth.
	if !limitBytes.Valid || limitBytes.Int64 <= 0 {
		return false
	}

	return usedBytes.Valid && usedBytes.Int64 >= limitBytes.Int64
}

// Flush writes all buffered events to the database, aggregated by tenant+date.
func (bt *BandwidthTracker) Flush() {
	bt.mu.Lock()
	if len(bt.buffer) == 0 {
		bt.mu.Unlock()
		return
	}
	events := bt.buffer
	bt.buffer = make([]bandwidthEvent, 0, 128)
	bt.mu.Unlock()

	if bt.db == nil {
		return
	}

	// Aggregate by tenant ID (all events are for today).
	type agg struct {
		ingress  int64
		egress   int64
		requests int
	}
	totals := make(map[string]*agg)
	for _, e := range events {
		a, ok := totals[e.tenantID]
		if !ok {
			a = &agg{}
			totals[e.tenantID] = a
		}
		a.ingress += e.ingress
		a.egress += e.egress
		a.requests++
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	for tenantID, a := range totals {
		_, err := bt.db.ExecContext(ctx, `
			INSERT INTO bandwidth_usage_daily (tenant_id, date, ingress_bytes, egress_bytes, requests_count)
			VALUES ($1, CURRENT_DATE, $2, $3, $4)
			ON CONFLICT (tenant_id, date)
			DO UPDATE SET
				ingress_bytes = bandwidth_usage_daily.ingress_bytes + EXCLUDED.ingress_bytes,
				egress_bytes = bandwidth_usage_daily.egress_bytes + EXCLUDED.egress_bytes,
				requests_count = bandwidth_usage_daily.requests_count + EXCLUDED.requests_count
		`, tenantID, a.ingress, a.egress, a.requests)
		if err != nil && bt.logger != nil {
			bt.logger.Error("flush bandwidth",
				zap.String("tenant_id", tenantID), zap.Error(err))
		}
	}
}
