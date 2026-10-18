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

	// pending holds the aggregates a flush could not write, re-sent (merged
	// with newer events) by the next one (RP11-3).
	pendingTenant  map[bandwidthAggKey]*bandwidthAgg
	pendingBackend map[bandwidthAggKey]*bandwidthAgg

	// stmtTimeout bounds ONE upsert: a slow statement no longer spends the
	// deadline of the ones after it (they shared one 10 s context before
	// RP11-3, and the per-backend rows written last were dropped under load).
	stmtTimeout time.Duration
}

// bandwidthFlushMaxAttempts is how many flushes in a row an aggregate may fail
// before it is dropped — with one Warn naming its bytes, never silently.
const bandwidthFlushMaxAttempts = 5

// bandwidthAggKey is one row of a flush: (tenant or backend, UTC day).
type bandwidthAggKey struct{ name, day string }

// bandwidthAgg is a row's delta; attempts counts the flushes that failed it.
type bandwidthAgg struct {
	ingress  int64
	egress   int64
	requests int
	attempts int
}

// NewBandwidthTracker creates a new tracker. Pass nil db to buffer without flushing.
func NewBandwidthTracker(db *sql.DB) *BandwidthTracker {
	return &BandwidthTracker{
		db:     db,
		buffer: make([]bandwidthEvent, 0, 128),

		stmtTimeout: 10 * time.Second,
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

// Flush writes all buffered events to the database, aggregated by tenant+date
// and backend+date, together with what earlier flushes could not write. Each
// upsert has its own deadline; one that fails keeps its delta for the next
// flush, up to bandwidthFlushMaxAttempts flushes.
func (bt *BandwidthTracker) Flush() { bt.flush(false) }

// FinalFlush is the shutdown's flush: there is no next flush, so a delta it
// cannot write is dropped at once with a Warn naming its bytes (RP13-3) —
// never "kept for the next flush".
func (bt *BandwidthTracker) FinalFlush() { bt.flush(true) }

func (bt *BandwidthTracker) flush(final bool) {
	bt.mu.Lock()
	if len(bt.buffer) == 0 && len(bt.pendingTenant) == 0 && len(bt.pendingBackend) == 0 {
		bt.mu.Unlock()
		return
	}
	if bt.db == nil {
		bt.buffer = make([]bandwidthEvent, 0, 128)
		bt.mu.Unlock()
		return
	}
	events := bt.buffer
	bt.buffer = make([]bandwidthEvent, 0, 128)
	totals, byBackend := bt.pendingTenant, bt.pendingBackend
	bt.pendingTenant, bt.pendingBackend = nil, nil
	bt.mu.Unlock()

	if totals == nil {
		totals = make(map[bandwidthAggKey]*bandwidthAgg)
	}
	if byBackend == nil {
		byBackend = make(map[bandwidthAggKey]*bandwidthAgg)
	}
	add := func(m map[bandwidthAggKey]*bandwidthAgg, k bandwidthAggKey, e bandwidthEvent) {
		a, ok := m[k]
		if !ok {
			a = &bandwidthAgg{}
			m[k] = a
		}
		a.ingress += e.ingress
		a.egress += e.egress
		a.requests++
	}
	for _, e := range events {
		day := e.day
		if day == "" {
			day = bandwidthDay(time.Now())
		}
		add(totals, bandwidthAggKey{e.tenantID, day}, e)
		if e.backend != "" {
			add(byBackend, bandwidthAggKey{e.backend, day}, e)
		}
	}

	failedTenant := bt.upsertAll(totals, final, "tenant_id", `
			INSERT INTO bandwidth_usage_daily (tenant_id, date, ingress_bytes, egress_bytes, requests_count)
			VALUES ($1, $2::date, $3, $4, $5)
			ON CONFLICT (tenant_id, date)
			DO UPDATE SET
				ingress_bytes = bandwidth_usage_daily.ingress_bytes + EXCLUDED.ingress_bytes,
				egress_bytes = bandwidth_usage_daily.egress_bytes + EXCLUDED.egress_bytes,
				requests_count = bandwidth_usage_daily.requests_count + EXCLUDED.requests_count
		`)
	failedBackend := bt.upsertAll(byBackend, final, "backend", `
			INSERT INTO backend_bandwidth_daily (backend_name, date, ingress_bytes, egress_bytes, requests_count)
			VALUES ($1, $2::date, $3, $4, $5)
			ON CONFLICT (backend_name, date)
			DO UPDATE SET
				ingress_bytes = backend_bandwidth_daily.ingress_bytes + EXCLUDED.ingress_bytes,
				egress_bytes = backend_bandwidth_daily.egress_bytes + EXCLUDED.egress_bytes,
				requests_count = backend_bandwidth_daily.requests_count + EXCLUDED.requests_count
		`)

	if len(failedTenant) == 0 && len(failedBackend) == 0 {
		return
	}
	bt.mu.Lock()
	bt.pendingTenant = mergeBandwidthAggs(bt.pendingTenant, failedTenant)
	bt.pendingBackend = mergeBandwidthAggs(bt.pendingBackend, failedBackend)
	bt.mu.Unlock()
}

// upsertAll runs one upsert per row, each under its own deadline, and returns
// the rows to try again on the next flush. A row that has failed
// bandwidthFlushMaxAttempts flushes — or any failed row of the final flush —
// is dropped with a Warn naming its bytes.
func (bt *BandwidthTracker) upsertAll(rows map[bandwidthAggKey]*bandwidthAgg, final bool, field, query string) map[bandwidthAggKey]*bandwidthAgg {
	var failed map[bandwidthAggKey]*bandwidthAgg
	for k, a := range rows {
		ctx, cancel := context.WithTimeout(context.Background(), bt.stmtTimeout)
		_, err := bt.db.ExecContext(ctx, query, k.name, k.day, a.ingress, a.egress, a.requests)
		cancel()
		if err == nil {
			continue
		}
		a.attempts++
		if final {
			if bt.logger != nil {
				bt.logger.Warn("bandwidth delta dropped at shutdown",
					zap.String(field, k.name), zap.String("date", k.day),
					zap.Int64("ingress_bytes", a.ingress), zap.Int64("egress_bytes", a.egress),
					zap.Int("requests", a.requests), zap.Int("attempts", a.attempts), zap.Error(err))
			}
			continue
		}
		if a.attempts >= bandwidthFlushMaxAttempts {
			if bt.logger != nil {
				bt.logger.Warn("bandwidth delta dropped after repeated flush failures",
					zap.String(field, k.name), zap.String("date", k.day),
					zap.Int64("ingress_bytes", a.ingress), zap.Int64("egress_bytes", a.egress),
					zap.Int("requests", a.requests), zap.Int("attempts", a.attempts), zap.Error(err))
			}
			continue
		}
		if bt.logger != nil {
			bt.logger.Warn("bandwidth flush failed — delta kept for the next flush",
				zap.String(field, k.name), zap.String("date", k.day),
				zap.Int("attempt", a.attempts), zap.Error(err))
		}
		if failed == nil {
			failed = make(map[bandwidthAggKey]*bandwidthAgg)
		}
		failed[k] = a
	}
	return failed
}

// mergeBandwidthAggs adds src's rows into dst (a concurrent flush may have
// put rows back meanwhile) and returns dst.
func mergeBandwidthAggs(dst, src map[bandwidthAggKey]*bandwidthAgg) map[bandwidthAggKey]*bandwidthAgg {
	if len(src) == 0 {
		return dst
	}
	if dst == nil {
		return src
	}
	for k, a := range src {
		if d, ok := dst[k]; ok {
			d.ingress += a.ingress
			d.egress += a.egress
			d.requests += a.requests
			d.attempts = max(d.attempts, a.attempts)
			continue
		}
		dst[k] = a
	}
	return dst
}
