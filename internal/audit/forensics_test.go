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

func TestForensicTools(t *testing.T) {
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

	t.Run("reconstruct user session", func(t *testing.T) {
		userID := uuid.New()
		sessionStart := time.Now().Add(-1 * time.Hour)

		// Create a series of events representing a session
		events := []struct {
			eventType EventType
			resource  string
			offset    time.Duration
		}{
			{EventTypeLogin, "/auth/login", 0},
			{EventTypeFileList, "/bucket", 5 * time.Minute},
			{EventTypeFileDownload, "/bucket/file1.txt", 10 * time.Minute},
			{EventTypeFileUpload, "/bucket/file2.txt", 15 * time.Minute},
			{EventTypeLogout, "/auth/logout", 20 * time.Minute},
		}

		for _, e := range events {
			err := auditor.LogEvent(ctx, &AuditEvent{
				UserID:    userID,
				TenantID:  "tenant-forensics",
				EventType: e.eventType,
				Action:    "test",
				Resource:  e.resource,
				Result:    ResultSuccess,
				Timestamp: sessionStart.Add(e.offset),
			})
			require.NoError(t, err)
		}

		// Reconstruct the session
		timeline, err := auditor.ReconstructUserSession(ctx, userID, sessionStart, sessionStart.Add(30*time.Minute))
		require.NoError(t, err)

		assert.GreaterOrEqual(t, len(timeline), 5)
		assert.Equal(t, EventTypeLogin, timeline[0].EventType)
		assert.Equal(t, EventTypeLogout, timeline[len(timeline)-1].EventType)
	})

	t.Run("trace IP address activity", func(t *testing.T) {
		ip := "203.0.113.42"

		// Create events from this IP
		for i := 0; i < 3; i++ {
			err := auditor.LogEvent(ctx, &AuditEvent{
				UserID:    uuid.New(),
				TenantID:  "tenant-ip-trace",
				EventType: EventTypeFileDownload,
				Action:    "GET",
				Resource:  "/file.txt",
				Result:    ResultSuccess,
				IP:        ip,
			})
			require.NoError(t, err)
		}

		// Trace the IP
		activity, err := auditor.TraceIPActivity(ctx, ip, 24*time.Hour)
		require.NoError(t, err)

		assert.NotEmpty(t, activity)
		assert.GreaterOrEqual(t, activity.EventCount, int64(3))
		assert.Equal(t, ip, activity.IP)
	})

	t.Run("find related events", func(t *testing.T) {
		userID := uuid.New()
		resource := "/sensitive/data.txt"

		// Create related events
		err := auditor.LogEvent(ctx, &AuditEvent{
			UserID:    userID,
			TenantID:  "tenant-related",
			EventType: EventTypeFileDownload,
			Action:    "GET",
			Resource:  resource,
			Result:    ResultSuccess,
		})
		require.NoError(t, err)

		// Find events related to this resource
		related, err := auditor.FindRelatedEvents(ctx, resource, 24*time.Hour)
		require.NoError(t, err)

		assert.NotEmpty(t, related)
	})

	t.Run("analyze attack pattern", func(t *testing.T) {
		attackerIP := "198.51.100.1"

		// Simulate attack pattern
		for i := 0; i < 10; i++ {
			err := auditor.LogEvent(ctx, &AuditEvent{
				UserID:    uuid.New(),
				TenantID:  "tenant-attack",
				EventType: EventTypeLogin,
				Action:    "LOGIN",
				Resource:  "/auth/login",
				Result:    ResultFailure,
				IP:        attackerIP,
			})
			require.NoError(t, err)
		}

		// Analyze pattern
		pattern, err := auditor.AnalyzeAttackPattern(ctx, attackerIP, 1*time.Hour)
		require.NoError(t, err)

		assert.Equal(t, attackerIP, pattern.IP)
		assert.Greater(t, pattern.FailedAttempts, int64(5))
	})

	t.Run("generate incident report", func(t *testing.T) {
		userID := uuid.New()

		// Create incident events
		err := auditor.LogEvent(ctx, &AuditEvent{
			UserID:    userID,
			TenantID:  "tenant-incident",
			EventType: EventTypeSecurityAlert,
			Action:    "ALERT",
			Resource:  "/admin",
			Result:    ResultFailure,
			Severity:  SeverityCritical,
		})
		require.NoError(t, err)

		// Generate incident report
		report, err := auditor.GenerateIncidentReport(ctx, userID, 1*time.Hour)
		require.NoError(t, err)

		assert.Equal(t, userID, report.UserID)
		assert.Greater(t, report.EventCount, int64(0))
	})
}
