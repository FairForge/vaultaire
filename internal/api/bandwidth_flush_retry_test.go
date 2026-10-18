package api

// RP11-3 (WP-P11): every upsert of one flush shared one 10 s context, so a
// slow statement made the ones after it fail at once and that flush's counts
// were dropped (prod 2026-10-18 03:52:08, `flush backend bandwidth …
// context deadline exceeded`). Each statement has its own deadline now, and
// a statement that fails keeps its delta for the next flush — bounded, and
// never dropped silently.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

func TestBandwidthTracker_ASlowStatementDoesNotFailTheOnesAfterIt(t *testing.T) {
	// Arrange: the tenant upsert outlives one statement's deadline.
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	bt := NewBandwidthTracker(db)
	bt.stmtTimeout = 150 * time.Millisecond
	day := bandwidthDay(time.Now())
	bt.RecordWithBackend(context.Background(), "tenant-1", "lyve", 0, 1000)

	mock.ExpectExec(`INSERT INTO bandwidth_usage_daily`).
		WithArgs("tenant-1", day, int64(0), int64(1000), 1).
		WillDelayFor(400 * time.Millisecond).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO backend_bandwidth_daily`).
		WithArgs("lyve", day, int64(0), int64(1000), 1).
		WillReturnResult(sqlmock.NewResult(0, 1))

	// Act
	bt.Flush()

	// Assert: the backend row was written although the tenant row timed out.
	require.NoError(t, mock.ExpectationsWereMet())

	// Act: the next flush re-sends the tenant delta the timeout kept, merged
	// with what arrived since.
	bt.RecordWithBackend(context.Background(), "tenant-1", "", 0, 50)
	mock.ExpectExec(`INSERT INTO bandwidth_usage_daily`).
		WithArgs("tenant-1", day, int64(0), int64(1050), 2).
		WillReturnResult(sqlmock.NewResult(0, 1))
	bt.Flush()

	// Assert
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestBandwidthTracker_AStatementFailingEveryFlushIsDroppedWithAWarn(t *testing.T) {
	// Arrange
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	core, logs := observer.New(zapcore.DebugLevel)
	bt := NewBandwidthTracker(db)
	bt.SetLogger(zap.New(core))
	day := bandwidthDay(time.Now())
	bt.RecordWithBackend(context.Background(), "tenant-1", "lyve", 7, 3000)

	// Act: the backend upsert fails on every flush; the tenant upsert lands.
	mock.ExpectExec(`INSERT INTO bandwidth_usage_daily`).
		WithArgs("tenant-1", day, int64(7), int64(3000), 1).
		WillReturnResult(sqlmock.NewResult(0, 1))
	for i := 0; i < bandwidthFlushMaxAttempts; i++ {
		mock.ExpectExec(`INSERT INTO backend_bandwidth_daily`).
			WithArgs("lyve", day, int64(7), int64(3000), 1).
			WillReturnError(errors.New("connection refused"))
		bt.Flush()
	}
	bt.Flush() // nothing left to send

	// Assert
	require.NoError(t, mock.ExpectationsWereMet())
	dropped := logs.FilterMessage("bandwidth delta dropped after repeated flush failures").All()
	require.Len(t, dropped, 1, "one Warn names the dropped bytes")
	assert.Equal(t, zapcore.WarnLevel, dropped[0].Level)
	fields := dropped[0].ContextMap()
	assert.Equal(t, "lyve", fields["backend"])
	assert.EqualValues(t, 3000, fields["egress_bytes"])
	assert.EqualValues(t, 7, fields["ingress_bytes"])
	assert.Empty(t, logs.FilterLevelExact(zapcore.ErrorLevel).All(), "a kept delta is a Warn, not an Error")
}

func TestBandwidthTracker_TheShutdownFlushDropsAFailedDeltaWithAWarnNamingItsBytes(t *testing.T) {
	// Arrange (RP13-3): the shutdown's flush is the last one — a delta it
	// cannot write has no "next flush" to be kept for.
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	core, logs := observer.New(zapcore.DebugLevel)
	bt := NewBandwidthTracker(db)
	bt.SetLogger(zap.New(core))
	day := bandwidthDay(time.Now())
	bt.RecordWithBackend(context.Background(), "tenant-1", "lyve", 11, 4000)
	mock.ExpectExec(`INSERT INTO bandwidth_usage_daily`).
		WithArgs("tenant-1", day, int64(11), int64(4000), 1).
		WillReturnError(errors.New("connection refused"))
	mock.ExpectExec(`INSERT INTO backend_bandwidth_daily`).
		WithArgs("lyve", day, int64(11), int64(4000), 1).
		WillReturnResult(sqlmock.NewResult(0, 1))
	s := &Server{bandwidthTracker: bt, logger: zap.NewNop()}

	// Act: the shutdown's flush.
	s.flushTrackers(context.Background())

	// Assert: nothing is kept, and one Warn names the dropped bytes.
	require.NoError(t, mock.ExpectationsWereMet())
	bt.mu.Lock()
	assert.Empty(t, bt.pendingTenant)
	assert.Empty(t, bt.pendingBackend)
	assert.Empty(t, bt.buffer)
	bt.mu.Unlock()
	dropped := logs.FilterMessage("bandwidth delta dropped at shutdown").All()
	require.Len(t, dropped, 1)
	fields := dropped[0].ContextMap()
	assert.Equal(t, "tenant-1", fields["tenant_id"])
	assert.EqualValues(t, 4000, fields["egress_bytes"])
	assert.EqualValues(t, 11, fields["ingress_bytes"])
	assert.Empty(t, logs.FilterMessage("bandwidth flush failed — delta kept for the next flush").All(),
		"there is no next flush")
}
