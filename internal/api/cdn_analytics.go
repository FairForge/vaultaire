package api

import (
	"context"
	"database/sql"
	"sync"
	"time"

	"go.uber.org/zap"
)

type cdnAccessEvent struct {
	tenantID  string
	bucket    string
	objectKey string
	bytesSent int64
	country   string
	referer   string
}

// CDNAnalyticsTracker buffers CDN access events and flushes them to the
// cdn_access_log table periodically. Follows the same pattern as BandwidthTracker.
type CDNAnalyticsTracker struct {
	db     *sql.DB
	mu     sync.Mutex
	buffer []cdnAccessEvent
	logger *zap.Logger
}

func NewCDNAnalyticsTracker(db *sql.DB) *CDNAnalyticsTracker {
	return &CDNAnalyticsTracker{
		db:     db,
		buffer: make([]cdnAccessEvent, 0, 128),
	}
}

func (ct *CDNAnalyticsTracker) SetLogger(logger *zap.Logger) {
	ct.logger = logger
}

// Record appends a CDN access event to the buffer. Auto-flushes at 100 events.
func (ct *CDNAnalyticsTracker) Record(_ context.Context, tenantID, bucket, objectKey string, bytesSent int64, country, referer string) {
	if tenantID == "" || bucket == "" {
		return
	}

	ct.mu.Lock()
	ct.buffer = append(ct.buffer, cdnAccessEvent{
		tenantID:  tenantID,
		bucket:    bucket,
		objectKey: objectKey,
		bytesSent: bytesSent,
		country:   country,
		referer:   referer,
	})
	needsFlush := len(ct.buffer) >= 100
	ct.mu.Unlock()

	if needsFlush {
		ct.Flush()
	}
}

// StartFlusher runs a background goroutine that flushes the buffer every interval.
func (ct *CDNAnalyticsTracker) StartFlusher(ctx context.Context, interval time.Duration) {
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				ct.Flush()
				return
			case <-ticker.C:
				ct.Flush()
			}
		}
	}()
}

// Flush writes all buffered events to the cdn_access_log table.
func (ct *CDNAnalyticsTracker) Flush() {
	ct.mu.Lock()
	if len(ct.buffer) == 0 {
		ct.mu.Unlock()
		return
	}
	events := ct.buffer
	ct.buffer = make([]cdnAccessEvent, 0, 128)
	ct.mu.Unlock()

	if ct.db == nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	for _, e := range events {
		_, err := ct.db.ExecContext(ctx, `
			INSERT INTO cdn_access_log (tenant_id, bucket, object_key, bytes_sent, country, referer)
			VALUES ($1, $2, $3, $4, $5, $6)
		`, e.tenantID, e.bucket, e.objectKey, e.bytesSent, e.country, e.referer)
		if err != nil && ct.logger != nil {
			ct.logger.Error("flush cdn access log",
				zap.String("tenant_id", e.tenantID), zap.Error(err))
		}
	}
}

// StartRollup runs a background goroutine that aggregates cdn_access_log into
// cdn_stats_daily once per hour.
func (ct *CDNAnalyticsTracker) StartRollup(ctx context.Context) {
	if ct.db == nil {
		return
	}
	go func() {
		ct.runRollup()
		ticker := time.NewTicker(1 * time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				ct.runRollup()
			}
		}
	}()
}

func (ct *CDNAnalyticsTracker) runRollup() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	_, err := ct.db.ExecContext(ctx, `
		INSERT INTO cdn_stats_daily (tenant_id, bucket, date, requests, bytes_sent, unique_objects)
		SELECT tenant_id, bucket, DATE(accessed_at), COUNT(*), SUM(bytes_sent), COUNT(DISTINCT object_key)
		FROM cdn_access_log
		WHERE accessed_at >= CURRENT_DATE
		GROUP BY tenant_id, bucket, DATE(accessed_at)
		ON CONFLICT (tenant_id, bucket, date)
		DO UPDATE SET requests = EXCLUDED.requests,
		             bytes_sent = EXCLUDED.bytes_sent,
		             unique_objects = EXCLUDED.unique_objects
	`)
	if err != nil && ct.logger != nil {
		ct.logger.Error("cdn stats rollup", zap.Error(err))
	}
}

// CheckBudget returns the bandwidth used this month, the budget limit, and
// whether the budget has been exceeded. A budget of 0 means unlimited.
func (ct *CDNAnalyticsTracker) CheckBudget(ctx context.Context, tenantID, bucket string) (used, limit int64, exceeded bool) {
	if ct.db == nil {
		return 0, 0, false
	}

	var budgetBytes int64
	err := ct.db.QueryRowContext(ctx,
		`SELECT bandwidth_budget_bytes FROM buckets WHERE tenant_id = $1 AND name = $2`,
		tenantID, bucket).Scan(&budgetBytes)
	if err != nil || budgetBytes <= 0 {
		return 0, 0, false
	}

	var monthBytes int64
	err = ct.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(bytes_sent), 0) FROM cdn_stats_daily
		WHERE tenant_id = $1 AND bucket = $2
		  AND date >= date_trunc('month', CURRENT_DATE)
	`, tenantID, bucket).Scan(&monthBytes)
	if err != nil {
		return 0, budgetBytes, false
	}

	// Also sum in-memory buffer for tighter enforcement.
	ct.mu.Lock()
	for _, e := range ct.buffer {
		if e.tenantID == tenantID && e.bucket == bucket {
			monthBytes += e.bytesSent
		}
	}
	ct.mu.Unlock()

	return monthBytes, budgetBytes, monthBytes >= budgetBytes
}
