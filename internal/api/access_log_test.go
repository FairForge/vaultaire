package api

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestS3AccessLogTracker_Record(t *testing.T) {
	// Arrange
	at := NewS3AccessLogTracker(nil)

	// Act
	at.Record(context.Background(), s3AccessEvent{
		tenantID:   "t1",
		bucket:     "my-bucket",
		objectKey:  "photo.jpg",
		operation:  "GetObject",
		statusCode: 200,
		bytesSent:  1024,
	})
	at.Record(context.Background(), s3AccessEvent{
		tenantID:      "t1",
		bucket:        "my-bucket",
		objectKey:     "doc.pdf",
		operation:     "PutObject",
		statusCode:    200,
		bytesReceived: 5000,
	})
	at.Record(context.Background(), s3AccessEvent{
		tenantID:   "t2",
		bucket:     "other",
		operation:  "ListObjects",
		statusCode: 200,
	})

	// Assert
	at.mu.Lock()
	defer at.mu.Unlock()
	require.Len(t, at.buffer, 3)
	assert.Equal(t, "t1", at.buffer[0].tenantID)
	assert.Equal(t, "my-bucket", at.buffer[0].bucket)
	assert.Equal(t, "photo.jpg", at.buffer[0].objectKey)
	assert.Equal(t, "GetObject", at.buffer[0].operation)
	assert.Equal(t, 200, at.buffer[0].statusCode)
	assert.Equal(t, int64(1024), at.buffer[0].bytesSent)
}

func TestS3AccessLogTracker_AutoFlushAt100(t *testing.T) {
	// Arrange
	at := NewS3AccessLogTracker(nil)

	// Act — record 100 events to trigger auto-flush
	for i := 0; i < 100; i++ {
		at.Record(context.Background(), s3AccessEvent{
			tenantID:   "t1",
			bucket:     "b1",
			operation:  "GetObject",
			statusCode: 200,
		})
	}

	// Assert — buffer should be empty after auto-flush (nil DB drains silently)
	at.mu.Lock()
	defer at.mu.Unlock()
	assert.Empty(t, at.buffer)
}

func TestS3AccessLogTracker_Flush(t *testing.T) {
	// Arrange
	at := NewS3AccessLogTracker(nil)
	at.Record(context.Background(), s3AccessEvent{
		tenantID:   "t1",
		bucket:     "b1",
		operation:  "GetObject",
		statusCode: 200,
		bytesSent:  100,
	})
	at.Record(context.Background(), s3AccessEvent{
		tenantID:   "t1",
		bucket:     "b1",
		operation:  "PutObject",
		statusCode: 200,
	})

	// Act
	at.Flush()

	// Assert
	at.mu.Lock()
	defer at.mu.Unlock()
	assert.Empty(t, at.buffer)
}

func TestS3AccessLogTracker_NilDB(t *testing.T) {
	// Arrange
	at := NewS3AccessLogTracker(nil)

	// Act — should not panic with nil DB
	at.Record(context.Background(), s3AccessEvent{
		tenantID:   "t1",
		bucket:     "b1",
		operation:  "GetObject",
		statusCode: 200,
	})
	at.Flush()

	// Assert — no panic = pass
	at.mu.Lock()
	defer at.mu.Unlock()
	assert.Empty(t, at.buffer)
}

func TestS3AccessLogTracker_SkipsEmptyTenantOrBucket(t *testing.T) {
	at := NewS3AccessLogTracker(nil)

	at.Record(context.Background(), s3AccessEvent{tenantID: "", bucket: "b", operation: "Get", statusCode: 200})
	at.Record(context.Background(), s3AccessEvent{tenantID: "t", bucket: "", operation: "Get", statusCode: 200})

	at.mu.Lock()
	defer at.mu.Unlock()
	assert.Empty(t, at.buffer)
}

func TestAccessLogFormat(t *testing.T) {
	// Arrange
	ts := time.Date(2026, 5, 27, 14, 30, 45, 0, time.UTC)
	event := s3AccessEvent{
		tenantID:      "tenant-abc",
		bucket:        "my-bucket",
		objectKey:     "photos/sunset.jpg",
		operation:     "GetObject",
		statusCode:    200,
		bytesSent:     4096,
		bytesReceived: 0,
		sourceIP:      "192.168.1.1",
		userAgent:     "aws-cli/2.0",
		requestID:     "req-12345",
		errorCode:     "",
		loggedAt:      ts,
	}

	// Act
	line := formatAccessLogLine(event)

	// Assert
	assert.Contains(t, line, "tenant-abc")
	assert.Contains(t, line, "my-bucket")
	assert.Contains(t, line, "[27/May/2026:14:30:45 +0000]")
	assert.Contains(t, line, "192.168.1.1")
	assert.Contains(t, line, "GetObject")
	assert.Contains(t, line, "photos/sunset.jpg")
	assert.Contains(t, line, "200")
	assert.Contains(t, line, "4096")
	assert.Contains(t, line, "req-12345")
	assert.Contains(t, line, `"aws-cli/2.0"`)
	// Error code should be "-" when empty
	assert.Contains(t, line, " - ")
}

func TestAccessLogFormat_EmptyFields(t *testing.T) {
	event := s3AccessEvent{
		tenantID:   "t1",
		bucket:     "b1",
		operation:  "ListObjects",
		statusCode: 200,
		loggedAt:   time.Now(),
	}

	line := formatAccessLogLine(event)

	// Empty objectKey should be "-"
	assert.Contains(t, line, "ListObjects -")
	// Empty user agent should be "-" (quoted)
	assert.Contains(t, line, `"-"`)
}
