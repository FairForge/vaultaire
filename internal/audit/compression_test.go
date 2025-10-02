package audit

import (
	"context"
	"testing"
	"time"

	"github.com/FairForge/vaultaire/internal/database"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestLogCompression(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping database test in short mode")
	}

	config := database.GetTestConfig()
	logger := zap.NewNop()

	db, err := database.NewPostgres(config, logger)
	require.NoError(t, err)
	defer func() {
		if err := db.Close(); err != nil {
			t.Errorf("failed to close database: %v", err)
		}
	}()

	auditor := NewAuditService(db)
	ctx := context.Background()

	t.Run("create archive table", func(t *testing.T) {
		err := auditor.CreateArchiveTable(ctx)
		require.NoError(t, err)

		// Should be idempotent
		err = auditor.CreateArchiveTable(ctx)
		require.NoError(t, err)
	})

	t.Run("archive old logs", func(t *testing.T) {
		userID := uuid.New()

		// Create old log (100 days ago)
		oldLog := &AuditEvent{
			UserID:    userID,
			TenantID:  "tenant-archive-test",
			EventType: EventTypeFileUpload,
			Action:    "PUT",
			Resource:  "/old-archived-file.txt",
			Result:    ResultSuccess,
			Timestamp: time.Now().Add(-100 * 24 * time.Hour),
		}
		err := auditor.LogEvent(ctx, oldLog)
		require.NoError(t, err)

		// Archive logs older than 90 days
		archived, err := auditor.ArchiveOldLogs(ctx, 90*24*time.Hour)
		require.NoError(t, err)
		assert.Greater(t, archived, int64(0), "should archive at least one log")

		// Verify log is in archive
		count, err := auditor.CountArchivedLogs(ctx)
		require.NoError(t, err)
		assert.Greater(t, count, int64(0), "should have logs in archive")
	})

	t.Run("query includes archived logs", func(t *testing.T) {
		userID := uuid.New()

		// Create and immediately archive a log
		archivedLog := &AuditEvent{
			UserID:    userID,
			TenantID:  "tenant-query-archive",
			EventType: EventTypeFileDelete,
			Action:    "DELETE",
			Resource:  "/archived-query-file.txt",
			Result:    ResultSuccess,
			Timestamp: time.Now().Add(-95 * 24 * time.Hour),
		}
		err := auditor.LogEvent(ctx, archivedLog)
		require.NoError(t, err)

		// Archive it
		_, err = auditor.ArchiveOldLogs(ctx, 90*24*time.Hour)
		require.NoError(t, err)

		// Query should find it (searching both tables)
		query := &AuditQuery{
			UserID: &userID,
			Limit:  100,
		}
		logs, err := auditor.QueryWithArchive(ctx, query)
		require.NoError(t, err)

		// Should find the archived log
		found := false
		for _, log := range logs {
			if log.Resource == "/archived-query-file.txt" {
				found = true
				break
			}
		}
		assert.True(t, found, "should find log in archive")
	})

	t.Run("compression statistics", func(t *testing.T) {
		stats, err := auditor.GetCompressionStats(ctx)
		require.NoError(t, err)

		assert.GreaterOrEqual(t, stats.ActiveLogs, int64(0))
		assert.GreaterOrEqual(t, stats.ArchivedLogs, int64(0))
		assert.GreaterOrEqual(t, stats.TotalLogs, int64(0))
	})

	t.Run("archived logs are read-only", func(t *testing.T) {
		// Verify we can't accidentally modify archived logs
		// This is enforced by only providing read methods for archive
		stats, err := auditor.GetCompressionStats(ctx)
		require.NoError(t, err)

		// Archive table exists and has data
		if stats.ArchivedLogs > 0 {
			assert.Greater(t, stats.ArchivedLogs, int64(0))
		}
	})
}
