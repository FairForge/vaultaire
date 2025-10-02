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

func TestRetentionPolicies(t *testing.T) {
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

	t.Run("default retention policy", func(t *testing.T) {
		policies := DefaultRetentionPolicies()

		// Should have multiple policies
		assert.NotEmpty(t, policies)

		// Last policy should be the default (30 days)
		defaultPolicy := policies[len(policies)-1]
		assert.Equal(t, 30*24*time.Hour, defaultPolicy.Duration)
		assert.Empty(t, defaultPolicy.EventType)
		assert.Empty(t, defaultPolicy.Severity)

		// First policies should be specific (longer retention)
		assert.Equal(t, EventTypeAccessDenied, policies[0].EventType)
		assert.Equal(t, 365*24*time.Hour, policies[0].Duration)
	})

	t.Run("cleanup old logs", func(t *testing.T) {
		userID := uuid.New()

		// Insert old event (35 days ago)
		oldEvent := &AuditEvent{
			UserID:    userID,
			TenantID:  "tenant-old",
			EventType: EventTypeFileUpload,
			Action:    "PUT",
			Resource:  "/old-file.txt",
			Result:    ResultSuccess,
			Timestamp: time.Now().Add(-35 * 24 * time.Hour),
		}
		err := auditor.LogEvent(ctx, oldEvent)
		require.NoError(t, err)

		// Insert recent event (5 days ago)
		recentEvent := &AuditEvent{
			UserID:    userID,
			TenantID:  "tenant-recent",
			EventType: EventTypeFileUpload,
			Action:    "PUT",
			Resource:  "/recent-file.txt",
			Result:    ResultSuccess,
			Timestamp: time.Now().Add(-5 * 24 * time.Hour),
		}
		err = auditor.LogEvent(ctx, recentEvent)
		require.NoError(t, err)

		// Run cleanup with 30-day retention
		deleted, err := auditor.CleanupOldLogs(ctx, 30*24*time.Hour)
		require.NoError(t, err)
		assert.Greater(t, deleted, int64(0), "should delete at least one old log")

		// Verify old event is gone
		query := &AuditQuery{
			TenantID: strPtr("tenant-old"),
			Limit:    10,
		}
		logs, err := auditor.Query(ctx, query)
		require.NoError(t, err)
		assert.Empty(t, logs, "old logs should be deleted")

		// Verify recent event still exists
		query = &AuditQuery{
			TenantID: strPtr("tenant-recent"),
			Limit:    10,
		}
		logs, err = auditor.Query(ctx, query)
		require.NoError(t, err)
		assert.NotEmpty(t, logs, "recent logs should be kept")
	})

	t.Run("cleanup by event type", func(t *testing.T) {
		userID := uuid.New()

		// Insert old security event (100 days ago)
		securityEvent := &AuditEvent{
			UserID:    userID,
			TenantID:  "tenant-security",
			EventType: EventTypeSecurityAlert,
			Action:    "alert",
			Resource:  "/security",
			Result:    ResultFailure,
			Severity:  SeverityCritical,
			Timestamp: time.Now().Add(-100 * 24 * time.Hour),
		}
		err := auditor.LogEvent(ctx, securityEvent)
		require.NoError(t, err)

		// Cleanup with 90-day retention for security events
		deleted, err := auditor.CleanupOldLogsByType(ctx, EventTypeSecurityAlert, 90*24*time.Hour)
		require.NoError(t, err)
		assert.Greater(t, deleted, int64(0), "should delete old security logs")

		// Verify security event is gone
		query := &AuditQuery{
			TenantID: strPtr("tenant-security"),
			Limit:    10,
		}
		logs, err := auditor.Query(ctx, query)
		require.NoError(t, err)
		assert.Empty(t, logs, "old security logs should be deleted")
	})

	t.Run("apply retention policies", func(t *testing.T) {
		userID := uuid.New()

		// Insert various old events
		events := []struct {
			eventType EventType
			daysAgo   int
			severity  Severity
			resource  string
		}{
			{EventTypeFileUpload, 35, SeverityInfo, "/test-file-1.txt"},             // Should be deleted (30 day default)
			{EventTypeSecurityAlert, 400, SeverityCritical, "/test-security.txt"},   // Should be deleted (365 day security)
			{EventTypeAccessDenied, 400, SeverityWarning, "/test-denied.txt"},       // Should be deleted (365 day compliance)
			{EventTypeFileUpload, 25, SeverityInfo, "/test-file-2.txt"},             // Should be kept
			{EventTypeSecurityAlert, 350, SeverityCritical, "/test-security-2.txt"}, // Should be kept
		}

		for _, e := range events {
			event := &AuditEvent{
				UserID:    userID,
				TenantID:  "tenant-policy-test",
				EventType: e.eventType,
				Action:    "test",
				Resource:  e.resource,
				Result:    ResultSuccess,
				Severity:  e.severity,
				Timestamp: time.Now().Add(-time.Duration(e.daysAgo) * 24 * time.Hour),
			}
			err := auditor.LogEvent(ctx, event)
			require.NoError(t, err)
		}

		// Apply default retention policies
		policies := DefaultRetentionPolicies()
		totalDeleted, err := auditor.ApplyRetentionPolicies(ctx, policies)
		require.NoError(t, err)
		assert.Greater(t, totalDeleted, int64(0), "should delete some logs")

		// Query remaining logs
		query := &AuditQuery{
			TenantID: strPtr("tenant-policy-test"),
			Limit:    100,
		}
		logs, err := auditor.Query(ctx, query)
		require.NoError(t, err)

		// Should have kept the recent ones
		assert.NotEmpty(t, logs, "should have some logs remaining")

		// Verify specific files were kept/deleted
		resourceMap := make(map[string]bool)
		for _, log := range logs {
			resourceMap[log.Resource] = true
		}

		assert.True(t, resourceMap["/test-file-2.txt"], "recent file should be kept")
		assert.False(t, resourceMap["/test-file-1.txt"], "old file should be deleted")
	})

	t.Run("cleanup by severity", func(t *testing.T) {
		userID := uuid.New()

		// Insert old critical event (100 days ago)
		criticalEvent := &AuditEvent{
			UserID:    userID,
			TenantID:  "tenant-critical",
			EventType: EventTypeFileUpload,
			Action:    "PUT",
			Resource:  "/critical-file.txt",
			Result:    ResultSuccess,
			Severity:  SeverityCritical,
			Timestamp: time.Now().Add(-100 * 24 * time.Hour),
		}
		err := auditor.LogEvent(ctx, criticalEvent)
		require.NoError(t, err)

		// Cleanup critical events older than 90 days
		deleted, err := auditor.CleanupOldLogsBySeverity(ctx, SeverityCritical, 90*24*time.Hour)
		require.NoError(t, err)
		assert.Greater(t, deleted, int64(0), "should delete old critical logs")
	})
}

func strPtr(s string) *string {
	return &s
}
