package account

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestCapReason(t *testing.T) {
	assert.Equal(t, "leaving", CapReason("  leaving \n"))
	long := strings.Repeat("é", MaxReasonLen+40)
	got := CapReason(long)
	assert.Equal(t, MaxReasonLen, len([]rune(got)), "capped at MaxReasonLen runes")
	assert.Equal(t, strings.Repeat("é", MaxReasonLen), got, "never cut mid-rune")
}

func TestSchedule_CapsReasonAndDefaults(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	fixed := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	svc := NewService(db, zap.NewNop())
	svc.now = func() time.Time { return fixed }

	long := strings.Repeat("x", MaxReasonLen+1)
	mock.ExpectQuery(`UPDATE users`).
		WithArgs(fixed.Add(GracePeriod), strings.Repeat("x", MaxReasonLen), StatusPending, "user-1").
		WillReturnRows(sqlmock.NewRows([]string{"deletion_scheduled_at"}).AddRow(fixed.Add(GracePeriod)))
	at, err := svc.Schedule(context.Background(), "user-1", "tenant-1", long)
	require.NoError(t, err)
	assert.Equal(t, fixed.Add(GracePeriod), at)

	mock.ExpectQuery(`UPDATE users`).
		WithArgs(sqlmock.AnyArg(), "User requested deletion", StatusPending, "user-1").
		WillReturnRows(sqlmock.NewRows([]string{"deletion_scheduled_at"}).AddRow(fixed.Add(GracePeriod)))
	_, err = svc.Schedule(context.Background(), "user-1", "tenant-1", "   ")
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestSchedule_ExistingDateWins(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	earlier := time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)
	mock.ExpectQuery(`UPDATE users`).
		WillReturnRows(sqlmock.NewRows([]string{"deletion_scheduled_at"}).AddRow(earlier))
	at, err := NewService(db, nil).Schedule(context.Background(), "user-1", "tenant-1", "again")
	require.NoError(t, err)
	assert.Equal(t, earlier, at, "the first schedule's date is returned, not moved")
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestSchedule_UnknownUser(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	mock.ExpectQuery(`UPDATE users`).WillReturnRows(sqlmock.NewRows([]string{"deletion_scheduled_at"}))
	_, err = NewService(db, nil).Schedule(context.Background(), "ghost", "t", "r")
	assert.ErrorContains(t, err, "not found")
}

func TestCancel_NoPending(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	mock.ExpectExec(`UPDATE users SET deletion_scheduled_at = NULL`).
		WithArgs("user-1").WillReturnResult(sqlmock.NewResult(0, 0))
	err = NewService(db, nil).Cancel(context.Background(), "user-1")
	assert.ErrorIs(t, err, ErrNoPendingDeletion)

	mock.ExpectExec(`UPDATE users SET deletion_scheduled_at = NULL`).
		WithArgs("user-1").WillReturnResult(sqlmock.NewResult(0, 1))
	assert.NoError(t, NewService(db, nil).Cancel(context.Background(), "user-1"))
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestGetStatus(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	at := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	mock.ExpectQuery(`SELECT deletion_scheduled_at, deletion_reason FROM users`).WithArgs("u").
		WillReturnRows(sqlmock.NewRows([]string{"deletion_scheduled_at", "deletion_reason"}).AddRow(at, "why"))
	st, err := NewService(db, nil).GetStatus(context.Background(), "u")
	require.NoError(t, err)
	assert.True(t, st.Scheduled)
	assert.Equal(t, at, st.ScheduledAt)
	assert.Equal(t, "why", st.Reason)

	mock.ExpectQuery(`SELECT deletion_scheduled_at, deletion_reason FROM users`).WithArgs("u").
		WillReturnRows(sqlmock.NewRows([]string{"deletion_scheduled_at", "deletion_reason"}).AddRow(nil, nil))
	st, err = NewService(db, nil).GetStatus(context.Background(), "u")
	require.NoError(t, err)
	assert.False(t, st.Scheduled)
}

func TestNilDB(t *testing.T) {
	svc := NewService(nil, nil)
	_, err := svc.Schedule(context.Background(), "u", "t", "r")
	assert.ErrorIs(t, err, ErrNoDatabase)
	assert.ErrorIs(t, svc.Cancel(context.Background(), "u"), ErrNoDatabase)
	st, err := svc.GetStatus(context.Background(), "u")
	require.NoError(t, err)
	assert.False(t, st.Scheduled)
	_, err = svc.ListDue(context.Background(), time.Now(), 10)
	assert.ErrorIs(t, err, ErrNoDatabase)
	_, err = svc.EraseRows(context.Background(), "u", "t", "e", time.Now())
	assert.ErrorIs(t, err, ErrNoDatabase)
}
