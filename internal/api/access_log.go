package api

import (
	"bufio"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/lib/pq"
	"go.uber.org/zap"
)

type s3AccessEvent struct {
	tenantID      string
	bucket        string
	objectKey     string
	operation     string
	statusCode    int
	bytesSent     int64
	bytesReceived int64
	sourceIP      string
	userAgent     string
	requestID     string
	errorCode     string
	loggedAt      time.Time
}

// S3AccessLogTracker buffers S3 access events and flushes them to the
// s3_access_log table periodically. A separate delivery goroutine writes
// accumulated log records as objects to the configured target bucket.
type S3AccessLogTracker struct {
	db     *sql.DB
	mu     sync.Mutex
	buffer []s3AccessEvent
	logger *zap.Logger

	// writer delivers log objects through the customer write path (Review
	// R13-02). nil = delivery disabled (no DB / no engine).
	writer *generatedObjectWriter

	// enabled is the set of "<tenant>/<bucket>" with logging_enabled, loaded
	// when delivery starts and refreshed every pass; PutBucketLogging writes
	// through (SetLoggingEnabled). Success rows are recorded only for these
	// buckets (Review R13-14 / R9-04: every request from every tenant used to
	// insert a row that only logging-enabled buckets ever consumed). Error
	// rows (status >= 400) are recorded for every bucket — the admin support
	// page reads them — and the retention job bounds both at 30 days.
	enabledMu     sync.RWMutex
	enabled       map[string]bool
	enabledLoaded bool
}

func NewS3AccessLogTracker(db *sql.DB) *S3AccessLogTracker {
	return &S3AccessLogTracker{
		db:      db,
		buffer:  make([]s3AccessEvent, 0, 128),
		enabled: map[string]bool{},
	}
}

func (at *S3AccessLogTracker) SetLogger(logger *zap.Logger) {
	at.logger = logger
}

// SetWriter wires the delivery writer (nil disables delivery).
func (at *S3AccessLogTracker) SetWriter(w *generatedObjectWriter) { at.writer = w }

// SetLoggingEnabled is the write-through from PutBucketLogging so a newly
// enabled bucket's requests are recorded before the next refresh.
func (at *S3AccessLogTracker) SetLoggingEnabled(tenantID, bucket string, on bool) {
	at.enabledMu.Lock()
	defer at.enabledMu.Unlock()
	if on {
		at.enabled[tenantID+"/"+bucket] = true
	} else {
		delete(at.enabled, tenantID+"/"+bucket)
	}
}

// refreshLoggingEnabled reloads the enabled set from the buckets table.
func (at *S3AccessLogTracker) refreshLoggingEnabled(ctx context.Context) {
	if at.db == nil {
		return
	}
	rows, err := at.db.QueryContext(ctx,
		`SELECT tenant_id, name FROM buckets WHERE logging_enabled = TRUE AND logging_target_bucket IS NOT NULL`)
	if err != nil {
		if at.logger != nil {
			at.logger.Warn("refresh logging-enabled buckets", zap.Error(err))
		}
		return
	}
	defer func() { _ = rows.Close() }()
	next := map[string]bool{}
	for rows.Next() {
		var tenantID, bucket string
		if err := rows.Scan(&tenantID, &bucket); err == nil {
			next[tenantID+"/"+bucket] = true
		}
	}
	if rows.Err() != nil {
		return
	}
	at.enabledMu.Lock()
	at.enabled, at.enabledLoaded = next, true
	at.enabledMu.Unlock()
}

// shouldRecord applies the logging_enabled gate. Until the set has been
// loaded (boot, or no DB) every event is recorded, as before.
func (at *S3AccessLogTracker) shouldRecord(event s3AccessEvent) bool {
	if event.statusCode >= 400 {
		return true
	}
	at.enabledMu.RLock()
	defer at.enabledMu.RUnlock()
	if !at.enabledLoaded {
		return true
	}
	return at.enabled[event.tenantID+"/"+event.bucket]
}

// Record appends an S3 access event to the buffer. Auto-flushes at 100 events.
func (at *S3AccessLogTracker) Record(_ context.Context, event s3AccessEvent) {
	if event.tenantID == "" || event.bucket == "" {
		return
	}
	if !at.shouldRecord(event) {
		return
	}
	if event.loggedAt.IsZero() {
		event.loggedAt = time.Now()
	}

	at.mu.Lock()
	at.buffer = append(at.buffer, event)
	needsFlush := len(at.buffer) >= 100
	at.mu.Unlock()

	if needsFlush {
		at.Flush()
	}
}

// StartFlusher runs a background goroutine that flushes the buffer every interval.
func (at *S3AccessLogTracker) StartFlusher(ctx context.Context, interval time.Duration) {
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				at.Flush()
				return
			case <-ticker.C:
				at.Flush()
			}
		}
	}()
}

// Flush writes all buffered events to the s3_access_log table.
func (at *S3AccessLogTracker) Flush() {
	at.mu.Lock()
	if len(at.buffer) == 0 {
		at.mu.Unlock()
		return
	}
	events := at.buffer
	at.buffer = make([]s3AccessEvent, 0, 128)
	at.mu.Unlock()

	if at.db == nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	for _, e := range events {
		_, err := at.db.ExecContext(ctx, `
			INSERT INTO s3_access_log (tenant_id, bucket, object_key, operation, status_code,
				bytes_sent, bytes_received, source_ip, user_agent, request_id, error_code, logged_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
		`, e.tenantID, e.bucket, e.objectKey, e.operation, e.statusCode,
			e.bytesSent, e.bytesReceived, e.sourceIP, e.userAgent, e.requestID, e.errorCode, e.loggedAt)
		if err != nil && at.logger != nil {
			at.logger.Error("flush s3 access log",
				zap.String("tenant_id", e.tenantID), zap.Error(err))
		}
	}
}

// formatAccessLogLine formats a single access log record in S3 server access log format.
func formatAccessLogLine(e s3AccessEvent) string {
	ts := e.loggedAt.UTC().Format("02/Jan/2006:15:04:05 -0700")
	key := e.objectKey
	if key == "" {
		key = "-"
	}
	errCode := e.errorCode
	if errCode == "" {
		errCode = "-"
	}
	ua := e.userAgent
	if ua == "" {
		ua = "-"
	}
	return fmt.Sprintf("%s %s [%s] %s %s %s %d %s %d %d %s \"%s\"",
		e.tenantID, e.bucket, ts, e.sourceIP, e.operation, key,
		e.statusCode, errCode, e.bytesSent, e.bytesReceived, e.requestID, ua)
}

func randomHex6() string {
	b := make([]byte, 3)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// writeAccessLogObject formats records as S3 access log lines and delivers
// them as ONE object into the tenant's target bucket through the customer
// write path (namespace, tenant ctx, quota, head row — Review R13-02).
// Returns the key written.
func (at *S3AccessLogTracker) writeAccessLogObject(ctx context.Context, tenantID, targetBucket, prefix string, records []s3AccessEvent) (string, error) {
	if len(records) == 0 {
		return "", nil
	}
	if at.writer == nil {
		return "", errors.New("access log delivery: no writer configured")
	}
	now := time.Now().UTC()
	objectKey := fmt.Sprintf("%s%s-%s", prefix, now.Format("2006-01-02-15-04-05"), randomHex6())
	_, err := at.writer.write(ctx, tenantID, targetBucket, objectKey, "text/plain", func(w io.Writer) error {
		bw := bufio.NewWriter(w)
		for _, r := range records {
			if _, err := bw.WriteString(formatAccessLogLine(r)); err != nil {
				return err
			}
			if err := bw.WriteByte('\n'); err != nil {
				return err
			}
		}
		return bw.Flush()
	})
	if err != nil {
		return "", err
	}
	return objectKey, nil
}

// PrepareLogDelivery loads the logging_enabled gate and reports whether log
// delivery can run on this process (database, engine and delivery writer
// present). The delivery itself — accumulated s3_access_log rows written to
// the configured target buckets as log objects every 5 minutes — is the
// `access_log_delivery` job of the scheduler (jobs.go).
func (at *S3AccessLogTracker) PrepareLogDelivery(ctx context.Context, eng *engine.CoreEngine) bool {
	if at.db == nil || eng == nil || at.writer == nil {
		return false
	}
	// Load the logging_enabled gate before the first request can be
	// recorded against it; every delivery pass refreshes it.
	loadCtx, cancelLoad := context.WithTimeout(ctx, 10*time.Second)
	at.refreshLoggingEnabled(loadCtx)
	cancelLoad()
	return true
}

// deliverLogs is one delivery pass over every logging-enabled bucket that
// has undelivered rows: the `access_log_delivery` job (every 5 minutes,
// jobs.go). It fails when the list of buckets could not be read; a bucket
// whose rows could not be delivered keeps them for the next pass.
func (at *S3AccessLogTracker) deliverLogs(ctx context.Context) (int, error) {
	deliverCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	at.refreshLoggingEnabled(deliverCtx)

	// Find all buckets with logging enabled.
	rows, err := at.db.QueryContext(deliverCtx, `
		SELECT DISTINCT b.tenant_id, b.name, b.logging_target_bucket, b.logging_prefix
		FROM buckets b
		INNER JOIN s3_access_log l ON l.tenant_id = b.tenant_id AND l.bucket = b.name
		WHERE b.logging_enabled = TRUE AND b.logging_target_bucket IS NOT NULL
	`)
	if err != nil {
		return 0, fmt.Errorf("query logging-enabled buckets: %w", err)
	}
	defer func() { _ = rows.Close() }()

	type bucketConfig struct {
		tenantID     string
		bucket       string
		targetBucket string
		prefix       string
	}
	var configs []bucketConfig
	for rows.Next() {
		var c bucketConfig
		if err := rows.Scan(&c.tenantID, &c.bucket, &c.targetBucket, &c.prefix); err != nil {
			if at.logger != nil {
				at.logger.Error("scan logging bucket config", zap.Error(err))
			}
			continue
		}
		configs = append(configs, c)
	}
	if err := rows.Err(); err != nil {
		at.logger.Warn("iterate rows", zap.Error(err))
	}

	delivered := 0
	for _, c := range configs {
		// A bucket busier than 1000 requests per pass used to fall behind
		// forever (Review R13-24): keep delivering until a batch comes back
		// short or the pass deadline is spent.
		for deliverCtx.Err() == nil {
			n, err := at.deliverBucketLogs(deliverCtx, c.tenantID, c.bucket, c.targetBucket, c.prefix)
			delivered += n
			if err != nil || n < accessLogDeliveryBatch {
				break
			}
		}
	}
	return delivered, nil
}

// accessLogDeliveryBatch is the number of rows one delivered object carries.
const accessLogDeliveryBatch = 1000

// deliverBucketLogs delivers up to one batch for a bucket and returns how
// many rows it delivered.
func (at *S3AccessLogTracker) deliverBucketLogs(ctx context.Context, tenantID, bucket, targetBucket, prefix string) (int, error) {
	rows, err := at.db.QueryContext(ctx, `
		SELECT id, tenant_id, bucket, object_key, operation, status_code,
			bytes_sent, bytes_received, source_ip, user_agent, request_id, error_code, logged_at
		FROM s3_access_log
		WHERE tenant_id = $1 AND bucket = $2
		ORDER BY logged_at ASC
		LIMIT $3
	`, tenantID, bucket, accessLogDeliveryBatch)
	if err != nil {
		if at.logger != nil {
			at.logger.Error("query access log records", zap.Error(err))
		}
		return 0, err
	}
	defer func() { _ = rows.Close() }()

	var ids []int64
	var records []s3AccessEvent
	for rows.Next() {
		var id int64
		var e s3AccessEvent
		if err := rows.Scan(&id, &e.tenantID, &e.bucket, &e.objectKey, &e.operation, &e.statusCode,
			&e.bytesSent, &e.bytesReceived, &e.sourceIP, &e.userAgent, &e.requestID, &e.errorCode, &e.loggedAt); err != nil {
			if at.logger != nil {
				at.logger.Error("scan access log row", zap.Error(err))
			}
			continue
		}
		ids = append(ids, id)
		records = append(records, e)
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("iterate rows: %w", err)
	}

	_ = rows.Close()
	if len(records) == 0 {
		return 0, nil
	}

	key, err := at.writeAccessLogObject(ctx, tenantID, targetBucket, prefix, records)
	if err != nil {
		if at.logger != nil {
			at.logger.Warn("access log delivery skipped — rows kept for the next pass",
				zap.String("tenant_id", tenantID),
				zap.String("bucket", bucket),
				zap.String("target_bucket", targetBucket),
				zap.Error(err))
		}
		return 0, err
	}

	// Delivered rows go in one statement (R4-18: one DELETE per row).
	if _, err := at.db.ExecContext(ctx, `DELETE FROM s3_access_log WHERE id = ANY($1)`, pq.Array(ids)); err != nil {
		if at.logger != nil {
			at.logger.Error("delete delivered access log rows (will be re-delivered)", zap.Error(err))
		}
		return len(records), err
	}

	if at.logger != nil {
		at.logger.Info("delivered access log records",
			zap.String("tenant_id", tenantID),
			zap.String("bucket", bucket),
			zap.String("target", targetBucket+"/"+key),
			zap.Int("records", len(records)))
	}
	return len(records), nil
}
