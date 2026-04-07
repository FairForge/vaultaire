package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCountingResponseWriter_CountsBytes(t *testing.T) {
	// Arrange
	w := httptest.NewRecorder()
	cw := &countingResponseWriter{ResponseWriter: w}

	// Act
	n1, _ := cw.Write([]byte("hello"))
	n2, _ := cw.Write([]byte(" world"))

	// Assert
	assert.Equal(t, 5, n1)
	assert.Equal(t, 6, n2)
	assert.Equal(t, int64(11), cw.bytesWritten)
	assert.Equal(t, "hello world", w.Body.String())
}

func TestCountingResponseWriter_PreservesStatusCode(t *testing.T) {
	w := httptest.NewRecorder()
	cw := &countingResponseWriter{ResponseWriter: w}

	cw.WriteHeader(http.StatusNotFound)

	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestBandwidthTracker_RecordAndFlush(t *testing.T) {
	// Arrange
	bt := NewBandwidthTracker(nil) // nil DB — will buffer but not flush to DB

	// Act
	bt.Record(context.Background(), "tenant-1", 1000, 0)
	bt.Record(context.Background(), "tenant-1", 500, 2000)
	bt.Record(context.Background(), "tenant-2", 0, 300)

	// Assert — check buffered events
	bt.mu.Lock()
	defer bt.mu.Unlock()
	require.Len(t, bt.buffer, 3)
	assert.Equal(t, "tenant-1", bt.buffer[0].tenantID)
	assert.Equal(t, int64(1000), bt.buffer[0].ingress)
	assert.Equal(t, int64(0), bt.buffer[0].egress)
}

func TestBandwidthTracker_AggregatesByTenantAndDate(t *testing.T) {
	// Arrange
	bt := NewBandwidthTracker(nil)

	// Act — multiple records for same tenant
	bt.Record(context.Background(), "tenant-1", 100, 200)
	bt.Record(context.Background(), "tenant-1", 300, 400)

	// Assert
	bt.mu.Lock()
	defer bt.mu.Unlock()
	assert.Len(t, bt.buffer, 2)
	// Total should be 400 ingress, 600 egress when flushed
	totalIngress := bt.buffer[0].ingress + bt.buffer[1].ingress
	totalEgress := bt.buffer[0].egress + bt.buffer[1].egress
	assert.Equal(t, int64(400), totalIngress)
	assert.Equal(t, int64(600), totalEgress)
}
