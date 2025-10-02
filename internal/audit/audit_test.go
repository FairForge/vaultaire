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

func setupTestDB(t *testing.T) *database.Postgres {
	if testing.Short() {
		t.Skip("skipping database test in short mode")
	}

	config := database.GetTestConfig()
	logger := zap.NewNop()

	db, err := database.NewPostgres(config, logger)
	require.NoError(t, err)

	return db
}

func TestAuditService(t *testing.T) {
	db := setupTestDB(t)
	defer func() {
		if err := db.Close(); err != nil {
			t.Errorf("failed to close database: %v", err)
		}
	}()

	auditor := NewAuditService(db)
	ctx := context.Background()

	t.Run("log system event", func(t *testing.T) {
		userID := uuid.New()
		event := &AuditEvent{
			UserID:    userID,
			TenantID:  "tenant-001",
			EventType: EventTypeFileUpload,
			Action:    "PUT",
			Resource:  "s3://bucket/file.txt",
			Result:    ResultSuccess,
			IP:        "192.168.1.1",
			UserAgent: "aws-cli/2.0",
			Metadata: map[string]string{
				"size": "1024",
			},
		}

		// Log event
		err := auditor.LogEvent(ctx, event)
		require.NoError(t, err)

		// Query it back
		query := &AuditQuery{
			UserID: &userID,
			Limit:  10,
		}
		logs, err := auditor.Query(ctx, query)
		require.NoError(t, err)
		require.NotEmpty(t, logs)

		found := false
		for _, log := range logs {
			if log.EventType == EventTypeFileUpload && log.UserID == userID {
				assert.Equal(t, "tenant-001", log.TenantID)
				assert.Equal(t, ResultSuccess, log.Result)
				found = true
				break
			}
		}
		assert.True(t, found, "uploaded event should be found")
	})

	t.Run("query by event type", func(t *testing.T) {
		userID := uuid.New()

		// Log multiple events
		events := []EventType{
			EventTypeLogin,
			EventTypeFileUpload,
			EventTypeFileDelete,
		}

		for _, et := range events {
			err := auditor.LogEvent(ctx, &AuditEvent{
				UserID:    userID,
				TenantID:  "tenant-001",
				EventType: et,
				Action:    "test",
				Resource:  "test",
				Result:    ResultSuccess,
			})
			require.NoError(t, err)
		}

		// Query only uploads
		eventType := EventTypeFileUpload
		query := &AuditQuery{
			EventType: &eventType,
			Limit:     10,
		}
		logs, err := auditor.Query(ctx, query)
		require.NoError(t, err)

		// Should find at least one upload
		found := false
		for _, log := range logs {
			if log.EventType == EventTypeFileUpload {
				found = true
				break
			}
		}
		assert.True(t, found)
	})

	t.Run("query by time range", func(t *testing.T) {
		userID := uuid.New()
		now := time.Now()

		// Log event
		err := auditor.LogEvent(ctx, &AuditEvent{
			UserID:    userID,
			TenantID:  "tenant-001",
			EventType: EventTypeLogin,
			Action:    "login",
			Resource:  "/auth/login",
			Result:    ResultSuccess,
		})
		require.NoError(t, err)

		// Query with time range
		start := now.Add(-1 * time.Hour)
		end := now.Add(1 * time.Hour)
		query := &AuditQuery{
			StartTime: &start,
			EndTime:   &end,
			Limit:     100,
		}

		logs, err := auditor.Query(ctx, query)
		require.NoError(t, err)
		assert.NotEmpty(t, logs)
	})

	t.Run("pagination", func(t *testing.T) {
		userID := uuid.New()

		// Log 15 events
		for i := 0; i < 15; i++ {
			err := auditor.LogEvent(ctx, &AuditEvent{
				UserID:    userID,
				TenantID:  "tenant-001",
				EventType: EventTypeAPIKeyCreated,
				Action:    "create_key",
				Resource:  "apikey",
				Result:    ResultSuccess,
			})
			require.NoError(t, err)
		}

		// First page
		query := &AuditQuery{
			UserID: &userID,
			Limit:  10,
		}
		page1, err := auditor.Query(ctx, query)
		require.NoError(t, err)
		assert.LessOrEqual(t, len(page1), 10)

		// Second page
		query.Offset = 10
		page2, err := auditor.Query(ctx, query)
		require.NoError(t, err)

		// Should have some results (at least 5 from the 15 we inserted)
		totalResults := len(page1) + len(page2)
		assert.GreaterOrEqual(t, totalResults, 15)
	})
}
