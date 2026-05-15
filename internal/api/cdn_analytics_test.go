package api

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCDNAnalyticsTracker_RecordAndBuffer(t *testing.T) {
	// Arrange
	ct := NewCDNAnalyticsTracker(nil)

	// Act
	ct.Record(context.Background(), "tenant-1", "my-bucket", "photo.jpg", 1024, "US", "https://example.com")
	ct.Record(context.Background(), "tenant-1", "my-bucket", "video.mp4", 5000, "GB", "")
	ct.Record(context.Background(), "tenant-2", "other", "file.txt", 256, "", "")

	// Assert
	ct.mu.Lock()
	defer ct.mu.Unlock()
	require.Len(t, ct.buffer, 3)
	assert.Equal(t, "tenant-1", ct.buffer[0].tenantID)
	assert.Equal(t, "my-bucket", ct.buffer[0].bucket)
	assert.Equal(t, "photo.jpg", ct.buffer[0].objectKey)
	assert.Equal(t, int64(1024), ct.buffer[0].bytesSent)
	assert.Equal(t, "US", ct.buffer[0].country)
	assert.Equal(t, "https://example.com", ct.buffer[0].referer)
}

func TestCDNAnalyticsTracker_SkipsEmptyTenantOrBucket(t *testing.T) {
	ct := NewCDNAnalyticsTracker(nil)

	// Act — empty tenant
	ct.Record(context.Background(), "", "bucket", "key", 100, "", "")
	// Act — empty bucket
	ct.Record(context.Background(), "tenant", "", "key", 100, "", "")

	// Assert
	ct.mu.Lock()
	defer ct.mu.Unlock()
	assert.Empty(t, ct.buffer)
}

func TestCDNAnalyticsTracker_FlushClearsBuffer(t *testing.T) {
	// Arrange
	ct := NewCDNAnalyticsTracker(nil) // nil DB — flush drains buffer silently

	ct.Record(context.Background(), "t1", "b1", "k1", 100, "", "")
	ct.Record(context.Background(), "t1", "b1", "k2", 200, "", "")

	// Act
	ct.Flush()

	// Assert
	ct.mu.Lock()
	defer ct.mu.Unlock()
	assert.Empty(t, ct.buffer)
}

func TestCDNAnalyticsTracker_FlushNoopWhenEmpty(t *testing.T) {
	ct := NewCDNAnalyticsTracker(nil)

	// Should not panic or error
	ct.Flush()

	ct.mu.Lock()
	defer ct.mu.Unlock()
	assert.Empty(t, ct.buffer)
}

func TestCDNAnalyticsTracker_AutoFlushAt100(t *testing.T) {
	ct := NewCDNAnalyticsTracker(nil) // nil DB — auto-flush drains silently

	for i := 0; i < 100; i++ {
		ct.Record(context.Background(), "t1", "b1", "key", 10, "", "")
	}

	// After 100 records, auto-flush should have cleared the buffer
	ct.mu.Lock()
	defer ct.mu.Unlock()
	assert.Empty(t, ct.buffer)
}

func TestCDNAnalyticsTracker_CheckBudget_NilDB(t *testing.T) {
	ct := NewCDNAnalyticsTracker(nil)

	used, limit, exceeded := ct.CheckBudget(context.Background(), "t1", "b1")

	assert.Equal(t, int64(0), used)
	assert.Equal(t, int64(0), limit)
	assert.False(t, exceeded)
}
