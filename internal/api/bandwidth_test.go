package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/FairForge/vaultaire/internal/common"
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

func TestCheckBandwidthLimit_NilDB(t *testing.T) {
	// Nil DB should always allow (fail open).
	bt := NewBandwidthTracker(nil)
	assert.False(t, bt.IsOverLimit(context.Background(), "tenant-1"))
}

func TestWriteS3Error_BandwidthExceeded(t *testing.T) {
	w := httptest.NewRecorder()
	WriteS3Error(w, ErrSlowDown, "/test", "req-1")

	assert.Equal(t, http.StatusTooManyRequests, w.Code)
	assert.Contains(t, w.Body.String(), "<Code>SlowDown</Code>")
	assert.Contains(t, w.Body.String(), "bandwidth")
}

// --- Per-backend egress attribution ---

func TestBackendNote_RoundTrip(t *testing.T) {
	ctx, _ := common.WithBackendNote(context.Background())

	assert.Equal(t, "", common.BackendUsed(ctx), "empty before any Set")
	common.SetBackendUsed(ctx, "lyve")
	assert.Equal(t, "lyve", common.BackendUsed(ctx))

	// Last writer wins — failover may try several drivers.
	common.SetBackendUsed(ctx, "idrive")
	assert.Equal(t, "idrive", common.BackendUsed(ctx))
}

func TestBackendNote_NoHolderIsNoop(t *testing.T) {
	ctx := context.Background()
	common.SetBackendUsed(ctx, "lyve") // must not panic
	assert.Equal(t, "", common.BackendUsed(ctx))
}

func TestBandwidthTracker_RecordWithBackend_BuffersBackend(t *testing.T) {
	bt := NewBandwidthTracker(nil)

	bt.RecordWithBackend(context.Background(), "tenant-1", "lyve", 0, 1000)
	bt.RecordWithBackend(context.Background(), "tenant-1", "lyve", 0, 500)
	bt.RecordWithBackend(context.Background(), "tenant-2", "geyser", 200, 0)
	bt.RecordWithBackend(context.Background(), "tenant-3", "", 0, 300) // unattributed

	bt.mu.Lock()
	defer bt.mu.Unlock()
	require.Len(t, bt.buffer, 4)
	assert.Equal(t, "lyve", bt.buffer[0].backend)
	assert.Equal(t, "", bt.buffer[3].backend, "unattributed events keep empty backend")
}

func TestBandwidthTracker_FlushWritesBackendRows(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	bt := NewBandwidthTracker(db)
	bt.RecordWithBackend(context.Background(), "tenant-1", "lyve", 0, 1000)
	bt.RecordWithBackend(context.Background(), "tenant-1", "lyve", 0, 500)
	bt.RecordWithBackend(context.Background(), "tenant-1", "", 0, 250) // no backend → tenant row only

	// Per-tenant upsert (existing behaviour, all 3 events aggregated).
	mock.ExpectExec(`INSERT INTO bandwidth_usage_daily`).
		WithArgs("tenant-1", int64(0), int64(1750), 3).
		WillReturnResult(sqlmock.NewResult(0, 1))
	// Per-backend upsert: only the attributed 1500 bytes, single lyve row.
	mock.ExpectExec(`INSERT INTO backend_bandwidth_daily`).
		WithArgs("lyve", int64(0), int64(1500), 2).
		WillReturnResult(sqlmock.NewResult(0, 1))

	bt.Flush()

	require.NoError(t, mock.ExpectationsWereMet())
}

func TestBandwidthTracker_RecordDelegatesWithoutBackend(t *testing.T) {
	bt := NewBandwidthTracker(nil)
	bt.Record(context.Background(), "tenant-1", 10, 20)

	bt.mu.Lock()
	defer bt.mu.Unlock()
	require.Len(t, bt.buffer, 1)
	assert.Equal(t, "", bt.buffer[0].backend)
}
