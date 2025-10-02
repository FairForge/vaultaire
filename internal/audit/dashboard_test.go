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

func TestDashboardAPI(t *testing.T) {
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

	// Seed test data
	userID := uuid.New()
	now := time.Now()

	events := []struct {
		eventType EventType
		result    Result
		severity  Severity
		resource  string
		timestamp time.Time
	}{
		{EventTypeLogin, ResultSuccess, SeverityInfo, "/auth/login", now.Add(-1 * time.Hour)},
		{EventTypeLogin, ResultFailure, SeverityWarning, "/auth/login", now.Add(-2 * time.Hour)},
		{EventTypeFileUpload, ResultSuccess, SeverityInfo, "/bucket/file1.txt", now.Add(-30 * time.Minute)},
		{EventTypeFileDownload, ResultSuccess, SeverityInfo, "/bucket/file1.txt", now.Add(-15 * time.Minute)},
		{EventTypeSecurityAlert, ResultFailure, SeverityCritical, "/admin", now.Add(-5 * time.Minute)},
	}

	for _, e := range events {
		err := auditor.LogEvent(ctx, &AuditEvent{
			UserID:    userID,
			TenantID:  "tenant-dashboard",
			EventType: e.eventType,
			Action:    "test",
			Resource:  e.resource,
			Result:    e.result,
			Severity:  e.severity,
			Timestamp: e.timestamp,
		})
		require.NoError(t, err)
	}

	t.Run("get overview statistics", func(t *testing.T) {
		stats, err := auditor.GetOverviewStats(ctx)
		require.NoError(t, err)

		assert.Greater(t, stats.TotalEvents, int64(0))
		assert.GreaterOrEqual(t, stats.EventsToday, int64(0))
		assert.GreaterOrEqual(t, stats.FailedEvents, int64(0))
		assert.GreaterOrEqual(t, stats.CriticalEvents, int64(0))
	})

	t.Run("get time series data", func(t *testing.T) {
		series, err := auditor.GetTimeSeriesData(ctx, now.Add(-24*time.Hour), now, time.Hour)
		require.NoError(t, err)

		assert.NotEmpty(t, series)
		for _, point := range series {
			assert.NotZero(t, point.Timestamp)
			assert.GreaterOrEqual(t, point.Count, int64(0))
		}
	})

	t.Run("get top users", func(t *testing.T) {
		users, err := auditor.GetTopUsers(ctx, now.Add(-24*time.Hour), now, 10)
		require.NoError(t, err)

		assert.NotEmpty(t, users)
		for _, user := range users {
			assert.NotEqual(t, uuid.Nil, user.UserID)
			assert.Greater(t, user.EventCount, int64(0))
		}
	})

	t.Run("get top resources", func(t *testing.T) {
		resources, err := auditor.GetTopResources(ctx, now.Add(-24*time.Hour), now, 10)
		require.NoError(t, err)

		assert.NotEmpty(t, resources)
		for _, res := range resources {
			assert.NotEmpty(t, res.Resource)
			assert.Greater(t, res.AccessCount, int64(0))
		}
	})

	t.Run("get event distribution", func(t *testing.T) {
		dist, err := auditor.GetEventDistribution(ctx, now.Add(-24*time.Hour), now)
		require.NoError(t, err)

		assert.NotEmpty(t, dist)
		totalCount := int64(0)
		for _, item := range dist {
			assert.NotEmpty(t, item.EventType)
			assert.Greater(t, item.Count, int64(0))
			totalCount += item.Count
		}
		assert.Greater(t, totalCount, int64(0))
	})

	t.Run("get recent events", func(t *testing.T) {
		recent, err := auditor.GetRecentEvents(ctx, 10)
		require.NoError(t, err)

		assert.NotEmpty(t, recent)
		assert.LessOrEqual(t, len(recent), 10)

		// Should be ordered by timestamp descending
		for i := 1; i < len(recent); i++ {
			assert.True(t, recent[i-1].Timestamp.After(recent[i].Timestamp) ||
				recent[i-1].Timestamp.Equal(recent[i].Timestamp))
		}
	})

	t.Run("get activity heatmap", func(t *testing.T) {
		heatmap, err := auditor.GetActivityHeatmap(ctx, now.Add(-7*24*time.Hour), now)
		require.NoError(t, err)

		assert.NotEmpty(t, heatmap)
		for _, hour := range heatmap {
			assert.GreaterOrEqual(t, hour.Hour, 0)
			assert.LessOrEqual(t, hour.Hour, 23)
			assert.GreaterOrEqual(t, hour.Count, int64(0))
		}
	})

	t.Run("get failure rate trends", func(t *testing.T) {
		trends, err := auditor.GetFailureRateTrends(ctx, now.Add(-24*time.Hour), now, time.Hour)
		require.NoError(t, err)

		assert.NotEmpty(t, trends)
		for _, point := range trends {
			assert.NotZero(t, point.Timestamp)
			assert.GreaterOrEqual(t, point.FailureRate, 0.0)
			assert.LessOrEqual(t, point.FailureRate, 100.0)
		}
	})
}
