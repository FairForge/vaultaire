package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/FairForge/vaultaire/internal/engine"
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
}

func NewS3AccessLogTracker(db *sql.DB) *S3AccessLogTracker {
	return &S3AccessLogTracker{
		db:     db,
		buffer: make([]s3AccessEvent, 0, 128),
	}
}

func (at *S3AccessLogTracker) SetLogger(logger *zap.Logger) {
	at.logger = logger
}

// Record appends an S3 access event to the buffer. Auto-flushes at 100 events.
func (at *S3AccessLogTracker) Record(_ context.Context, event s3AccessEvent) {
	if event.tenantID == "" || event.bucket == "" {
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

// writeAccessLogObject formats records as S3 access log lines and writes them
// as an object to the target bucket via engine.Put.
func (at *S3AccessLogTracker) writeAccessLogObject(ctx context.Context, eng *engine.CoreEngine, tenantID, targetBucket, prefix string, records []s3AccessEvent) error {
	if eng == nil || len(records) == 0 {
		return nil
	}

	var buf bytes.Buffer
	for _, r := range records {
		buf.WriteString(formatAccessLogLine(r))
		buf.WriteByte('\n')
	}

	now := time.Now().UTC()
	objectKey := fmt.Sprintf("%s%s-%s", prefix, now.Format("2006-01-02-15-04-05"), randomHex6())

	container := fmt.Sprintf("tenant/%s/%s", tenantID, targetBucket)
	_, err := eng.Put(ctx, container, objectKey, strings.NewReader(buf.String()))
	return err
}

// StartLogDelivery runs a background goroutine that delivers accumulated access
// log records from the s3_access_log table to the configured target buckets as
// log objects every 5 minutes.
func (at *S3AccessLogTracker) StartLogDelivery(ctx context.Context, eng *engine.CoreEngine) {
	if at.db == nil || eng == nil {
		return
	}
	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				at.deliverLogs(ctx, eng)
			}
		}
	}()
}

func (at *S3AccessLogTracker) deliverLogs(ctx context.Context, eng *engine.CoreEngine) {
	deliverCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	// Find all buckets with logging enabled.
	rows, err := at.db.QueryContext(deliverCtx, `
		SELECT DISTINCT b.tenant_id, b.name, b.logging_target_bucket, b.logging_prefix
		FROM buckets b
		INNER JOIN s3_access_log l ON l.tenant_id = b.tenant_id AND l.bucket = b.name
		WHERE b.logging_enabled = TRUE AND b.logging_target_bucket IS NOT NULL
	`)
	if err != nil {
		if at.logger != nil {
			at.logger.Error("query logging-enabled buckets", zap.Error(err))
		}
		return
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

	for _, c := range configs {
		at.deliverBucketLogs(deliverCtx, eng, c.tenantID, c.bucket, c.targetBucket, c.prefix)
	}
}

func (at *S3AccessLogTracker) deliverBucketLogs(ctx context.Context, eng *engine.CoreEngine, tenantID, bucket, targetBucket, prefix string) {
	rows, err := at.db.QueryContext(ctx, `
		SELECT id, tenant_id, bucket, object_key, operation, status_code,
			bytes_sent, bytes_received, source_ip, user_agent, request_id, error_code, logged_at
		FROM s3_access_log
		WHERE tenant_id = $1 AND bucket = $2
		ORDER BY logged_at ASC
		LIMIT 1000
	`, tenantID, bucket)
	if err != nil {
		if at.logger != nil {
			at.logger.Error("query access log records", zap.Error(err))
		}
		return
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

	if len(records) == 0 {
		return
	}

	if err := at.writeAccessLogObject(ctx, eng, tenantID, targetBucket, prefix, records); err != nil {
		if at.logger != nil {
			at.logger.Error("write access log object",
				zap.String("tenant_id", tenantID),
				zap.String("target_bucket", targetBucket),
				zap.Error(err))
		}
		return
	}

	// Delete delivered records.
	for _, id := range ids {
		_, _ = at.db.ExecContext(ctx, `DELETE FROM s3_access_log WHERE id = $1`, id)
	}

	if at.logger != nil {
		at.logger.Info("delivered access log records",
			zap.String("tenant_id", tenantID),
			zap.String("bucket", bucket),
			zap.String("target_bucket", targetBucket),
			zap.Int("records", len(records)))
	}
}
