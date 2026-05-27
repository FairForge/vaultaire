package api

import (
	"context"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestScheduleDeletion_SetsDate(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	// Check existing.
	mock.ExpectQuery(`SELECT deletion_scheduled_at FROM users WHERE id`).
		WithArgs("user-1").
		WillReturnRows(sqlmock.NewRows([]string{"deletion_scheduled_at"}).AddRow(nil))

	// Schedule.
	mock.ExpectExec(`UPDATE users SET deletion_scheduled_at`).
		WithArgs(sqlmock.AnyArg(), "leaving", "user-1").
		WillReturnResult(sqlmock.NewResult(0, 1))

	svc := NewAccountDeletionService(db, zap.NewNop())
	scheduledAt, err := svc.ScheduleDeletion(context.Background(), "user-1", "tenant-1", "leaving")

	require.NoError(t, err)
	// Should be ~30 days in the future.
	expected := time.Now().Add(30 * 24 * time.Hour)
	assert.WithinDuration(t, expected, scheduledAt, 5*time.Second)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestScheduleDeletion_AlreadyScheduled(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	existingDate := time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC)
	mock.ExpectQuery(`SELECT deletion_scheduled_at FROM users WHERE id`).
		WithArgs("user-1").
		WillReturnRows(sqlmock.NewRows([]string{"deletion_scheduled_at"}).AddRow(existingDate))

	svc := NewAccountDeletionService(db, zap.NewNop())
	scheduledAt, err := svc.ScheduleDeletion(context.Background(), "user-1", "tenant-1", "leaving again")

	require.NoError(t, err)
	assert.Equal(t, existingDate, scheduledAt)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestCancelDeletion_ClearsSchedule(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	mock.ExpectExec(`UPDATE users SET deletion_scheduled_at = NULL`).
		WithArgs("user-1").
		WillReturnResult(sqlmock.NewResult(0, 1))

	svc := NewAccountDeletionService(db, zap.NewNop())
	err = svc.CancelDeletion(context.Background(), "user-1")

	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestCancelDeletion_NoPending(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	mock.ExpectExec(`UPDATE users SET deletion_scheduled_at = NULL`).
		WithArgs("user-1").
		WillReturnResult(sqlmock.NewResult(0, 0))

	svc := NewAccountDeletionService(db, zap.NewNop())
	err = svc.CancelDeletion(context.Background(), "user-1")

	assert.Error(t, err)
	assert.Contains(t, err.Error(), "no pending deletion")
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestExecuteDeletion_RemovesAllData(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	mock.ExpectBegin()
	mock.ExpectExec(`DELETE FROM object_head_cache WHERE tenant_id`).WithArgs("tenant-1").WillReturnResult(sqlmock.NewResult(0, 5))
	mock.ExpectExec(`DELETE FROM buckets WHERE tenant_id`).WithArgs("tenant-1").WillReturnResult(sqlmock.NewResult(0, 2))
	mock.ExpectExec(`DELETE FROM api_keys WHERE user_id`).WithArgs("user-1").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`DELETE FROM tenant_quotas WHERE tenant_id`).WithArgs("tenant-1").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`DELETE FROM dashboard_sessions WHERE user_id`).WithArgs("user-1").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`DELETE FROM events WHERE tenant_id`).WithArgs("tenant-1").WillReturnResult(sqlmock.NewResult(0, 10))
	mock.ExpectExec(`DELETE FROM webhook_endpoints WHERE tenant_id`).WithArgs("tenant-1").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`DELETE FROM bandwidth_usage_daily WHERE tenant_id`).WithArgs("tenant-1").WillReturnResult(sqlmock.NewResult(0, 30))
	mock.ExpectExec(`DELETE FROM account_exports WHERE user_id`).WithArgs("user-1").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`DELETE FROM users WHERE id`).WithArgs("user-1").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`DELETE FROM tenants WHERE id`).WithArgs("tenant-1").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	svc := NewAccountDeletionService(db, zap.NewNop())
	err = svc.ExecuteDeletion(context.Background(), "user-1", "tenant-1")

	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestGetDeletionStatus_Scheduled(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	scheduledAt := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	mock.ExpectQuery(`SELECT deletion_scheduled_at, deletion_reason FROM users`).
		WithArgs("user-1").
		WillReturnRows(sqlmock.NewRows([]string{"deletion_scheduled_at", "deletion_reason"}).
			AddRow(scheduledAt, "switching providers"))

	svc := NewAccountDeletionService(db, zap.NewNop())
	status, err := svc.GetDeletionStatus(context.Background(), "user-1")

	require.NoError(t, err)
	assert.True(t, status.Scheduled)
	assert.Equal(t, scheduledAt, status.ScheduledAt)
	assert.Equal(t, "switching providers", status.Reason)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestGetDeletionStatus_NotScheduled(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	mock.ExpectQuery(`SELECT deletion_scheduled_at, deletion_reason FROM users`).
		WithArgs("user-1").
		WillReturnRows(sqlmock.NewRows([]string{"deletion_scheduled_at", "deletion_reason"}).
			AddRow(nil, nil))

	svc := NewAccountDeletionService(db, zap.NewNop())
	status, err := svc.GetDeletionStatus(context.Background(), "user-1")

	require.NoError(t, err)
	assert.False(t, status.Scheduled)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestScheduleDeletion_NilDB(t *testing.T) {
	svc := NewAccountDeletionService(nil, zap.NewNop())
	_, err := svc.ScheduleDeletion(context.Background(), "user-1", "tenant-1", "test")

	assert.Error(t, err)
	assert.Contains(t, err.Error(), "database unavailable")
}
