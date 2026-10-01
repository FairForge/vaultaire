package api

import (
	"context"
	"database/sql"
	"net/http"
	"sync"
	"time"

	"github.com/FairForge/vaultaire/internal/usage"
	"go.uber.org/zap"
)

// countingResponseWriter wraps http.ResponseWriter to count bytes written
// and capture the HTTP status code for access logging.
type countingResponseWriter struct {
	http.ResponseWriter
	bytesWritten int64
	statusCode   int
	wroteHeader  bool

	// head: the request is a HEAD — net/http accepts and discards a body
	// written to it, so nothing was sent and nothing is counted.
	head bool
	// span, when set, adds every byte to the tenant's month counter as it is
	// written, so a response still in flight counts (WP-R10-9).
	span *egressSpan
}

func (cw *countingResponseWriter) WriteHeader(code int) {
	if !cw.wroteHeader {
		cw.statusCode = code
		cw.wroteHeader = true
	}
	cw.ResponseWriter.WriteHeader(code)
}

func (cw *countingResponseWriter) Write(b []byte) (int, error) {
	if !cw.wroteHeader {
		cw.statusCode = http.StatusOK
		cw.wroteHeader = true
	}
	n, err := cw.ResponseWriter.Write(b)
	if n > 0 && !cw.head {
		cw.bytesWritten += int64(n)
		if cw.span != nil {
			cw.span.count(int64(n))
		}
	}
	return n, err
}

// Flush passes through to the underlying writer when it supports it, so
// wrapping a streaming handler does not silently disable flushing.
func (cw *countingResponseWriter) Flush() {
	if f, ok := cw.ResponseWriter.(http.Flusher); ok {
		if !cw.wroteHeader {
			cw.statusCode = http.StatusOK
			cw.wroteHeader = true
		}
		f.Flush()
	}
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (cw *countingResponseWriter) Unwrap() http.ResponseWriter { return cw.ResponseWriter }

// bandwidthEvent represents a single ingress/egress event for a tenant.
// backend is the storage backend that served the bytes ("" when the request
// never touched one — errors, listings, cache hits without attribution).
type bandwidthEvent struct {
	tenantID string
	backend  string
	day      string // UTC date the bytes are recorded on, "2006-01-02"
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

// Record adds a bandwidth event to the buffer with no backend attribution.
func (bt *BandwidthTracker) Record(ctx context.Context, tenantID string, ingress, egress int64) {
	bt.RecordWithBackend(ctx, tenantID, "", ingress, egress)
}

// RecordWithBackend adds a bandwidth event attributed to a storage backend,
// on today's UTC date.
func (bt *BandwidthTracker) RecordWithBackend(_ context.Context, tenantID, backend string, ingress, egress int64) {
	bt.recordOn(time.Now(), tenantID, backend, ingress, egress)
}

// bandwidthDay is the date an event lands on: the UTC day, so the rows, the
// live counter and every reader share one month boundary (WP-R10-9; the SQL
// used CURRENT_DATE, which follows the session time zone).
func bandwidthDay(t time.Time) string { return t.UTC().Format("2006-01-02") }

// recordOn adds a bandwidth event on the UTC date of `at`.
func (bt *BandwidthTracker) recordOn(at time.Time, tenantID, backend string, ingress, egress int64) {
	if tenantID == "" || (ingress == 0 && egress == 0) {
		return
	}

	bt.mu.Lock()
	bt.buffer = append(bt.buffer, bandwidthEvent{
		tenantID: tenantID,
		backend:  backend,
		day:      bandwidthDay(at),
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

// recordResponse records one finished response: ingress and the bytes the
// counting writer sent. With a live span the egress recorded is what the
// tenant's counter still holds as unrecorded (a shutdown drain may already
// have written this response's bytes), and a response that was open across
// the UTC month boundary is split, so the bytes written before midnight stay
// in the month the live counter put them in.
func (bt *BandwidthTracker) recordResponse(tenantID, backend string, ingress int64, cw *countingResponseWriter) {
	now := time.Now()
	egress := cw.bytesWritten
	if cw.span != nil {
		egress = cw.span.t.takeUnrecorded(egress)
		if carried := min(cw.span.carried, egress); carried > 0 {
			lastMonth := usage.EgressMonthStart(now).AddDate(0, 0, -1)
			bt.recordOn(lastMonth, tenantID, backend, 0, carried)
			egress -= carried
		}
	}
	bt.recordOn(now, tenantID, backend, ingress, egress)
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

	// Aggregate by (tenant, UTC day) and (backend, UTC day).
	type aggKey struct{ name, day string }
	type agg struct {
		ingress  int64
		egress   int64
		requests int
	}
	totals := make(map[aggKey]*agg)
	byBackend := make(map[aggKey]*agg)
	for _, e := range events {
		day := e.day
		if day == "" {
			day = bandwidthDay(time.Now())
		}
		a, ok := totals[aggKey{e.tenantID, day}]
		if !ok {
			a = &agg{}
			totals[aggKey{e.tenantID, day}] = a
		}
		a.ingress += e.ingress
		a.egress += e.egress
		a.requests++

		if e.backend != "" {
			b, ok := byBackend[aggKey{e.backend, day}]
			if !ok {
				b = &agg{}
				byBackend[aggKey{e.backend, day}] = b
			}
			b.ingress += e.ingress
			b.egress += e.egress
			b.requests++
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	for k, a := range totals {
		_, err := bt.db.ExecContext(ctx, `
			INSERT INTO bandwidth_usage_daily (tenant_id, date, ingress_bytes, egress_bytes, requests_count)
			VALUES ($1, $2::date, $3, $4, $5)
			ON CONFLICT (tenant_id, date)
			DO UPDATE SET
				ingress_bytes = bandwidth_usage_daily.ingress_bytes + EXCLUDED.ingress_bytes,
				egress_bytes = bandwidth_usage_daily.egress_bytes + EXCLUDED.egress_bytes,
				requests_count = bandwidth_usage_daily.requests_count + EXCLUDED.requests_count
		`, k.name, k.day, a.ingress, a.egress, a.requests)
		if err != nil && bt.logger != nil {
			bt.logger.Error("flush bandwidth",
				zap.String("tenant_id", k.name), zap.Error(err))
		}
	}

	for k, a := range byBackend {
		_, err := bt.db.ExecContext(ctx, `
			INSERT INTO backend_bandwidth_daily (backend_name, date, ingress_bytes, egress_bytes, requests_count)
			VALUES ($1, $2::date, $3, $4, $5)
			ON CONFLICT (backend_name, date)
			DO UPDATE SET
				ingress_bytes = backend_bandwidth_daily.ingress_bytes + EXCLUDED.ingress_bytes,
				egress_bytes = backend_bandwidth_daily.egress_bytes + EXCLUDED.egress_bytes,
				requests_count = backend_bandwidth_daily.requests_count + EXCLUDED.requests_count
		`, k.name, k.day, a.ingress, a.egress, a.requests)
		if err != nil && bt.logger != nil {
			bt.logger.Error("flush backend bandwidth",
				zap.String("backend", k.name), zap.Error(err))
		}
	}
}
