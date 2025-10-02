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

func TestComplianceReporting(t *testing.T) {
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

	t.Run("generate SOC2 report", func(t *testing.T) {
		userID := uuid.New()

		// Create relevant events for SOC2
		events := []struct {
			eventType EventType
			severity  Severity
			result    Result
		}{
			{EventTypeLogin, SeverityInfo, ResultSuccess},
			{EventTypeAccessDenied, SeverityWarning, ResultDenied},
			{EventTypeAPIKeyCreated, SeverityInfo, ResultSuccess},
			{EventTypeSecurityAlert, SeverityCritical, ResultFailure},
		}

		for _, e := range events {
			err := auditor.LogEvent(ctx, &AuditEvent{
				UserID:    userID,
				TenantID:  "tenant-soc2",
				EventType: e.eventType,
				Action:    "test",
				Resource:  "/test",
				Result:    e.result,
				Severity:  e.severity,
			})
			require.NoError(t, err)
		}

		// Generate SOC2 report
		report, err := auditor.GenerateSOC2Report(ctx, time.Now().Add(-24*time.Hour), time.Now())
		require.NoError(t, err)

		assert.NotEmpty(t, report.Summary)
		assert.Greater(t, report.TotalEvents, int64(0))
		assert.GreaterOrEqual(t, report.AccessEvents, int64(0))
		assert.GreaterOrEqual(t, report.SecurityEvents, int64(0))
		assert.NotEmpty(t, report.GeneratedAt)
	})

	t.Run("generate GDPR report", func(t *testing.T) {
		userID := uuid.New()

		// Create GDPR-relevant events
		err := auditor.LogEvent(ctx, &AuditEvent{
			UserID:    userID,
			TenantID:  "tenant-gdpr",
			EventType: EventTypeDataExport,
			Action:    "EXPORT",
			Resource:  "/user/data",
			Result:    ResultSuccess,
		})
		require.NoError(t, err)

		// Generate GDPR report
		report, err := auditor.GenerateGDPRReport(ctx, userID, time.Now().Add(-30*24*time.Hour), time.Now())
		require.NoError(t, err)

		assert.Equal(t, userID, report.UserID)
		assert.Greater(t, report.TotalAccesses, int64(0))
		assert.NotEmpty(t, report.DataExports)
	})

	t.Run("generate access report for user", func(t *testing.T) {
		userID := uuid.New()

		// Create various access events
		resources := []string{"/file1.txt", "/file2.pdf", "/file3.doc"}
		for _, res := range resources {
			err := auditor.LogEvent(ctx, &AuditEvent{
				UserID:    userID,
				TenantID:  "tenant-access",
				EventType: EventTypeFileDownload,
				Action:    "GET",
				Resource:  res,
				Result:    ResultSuccess,
			})
			require.NoError(t, err)
		}

		// Generate access report
		report, err := auditor.GenerateAccessReport(ctx, userID, time.Now().Add(-7*24*time.Hour), time.Now())
		require.NoError(t, err)

		assert.Equal(t, userID, report.UserID)
		assert.GreaterOrEqual(t, len(report.AccessedResources), 3)
		assert.Greater(t, report.TotalAccesses, int64(0))
	})

	t.Run("generate security incident report", func(t *testing.T) {
		// Create security events
		events := []EventType{
			EventTypeSecurityAlert,
			EventTypeAccessDenied,
			EventTypeSuspiciousActivity,
		}

		for _, et := range events {
			err := auditor.LogEvent(ctx, &AuditEvent{
				UserID:    uuid.New(),
				TenantID:  "tenant-security",
				EventType: et,
				Action:    "test",
				Resource:  "/test",
				Result:    ResultFailure,
				Severity:  SeverityCritical,
			})
			require.NoError(t, err)
		}

		// Generate security report
		report, err := auditor.GenerateSecurityReport(ctx, time.Now().Add(-24*time.Hour), time.Now())
		require.NoError(t, err)

		assert.Greater(t, report.TotalIncidents, int64(0))
		assert.GreaterOrEqual(t, report.CriticalIncidents, int64(0))
		assert.NotEmpty(t, report.IncidentsByType)
	})

	t.Run("export report to CSV", func(t *testing.T) {
		userID := uuid.New()

		// Create some events
		err := auditor.LogEvent(ctx, &AuditEvent{
			UserID:    userID,
			TenantID:  "tenant-csv",
			EventType: EventTypeFileUpload,
			Action:    "PUT",
			Resource:  "/test.txt",
			Result:    ResultSuccess,
		})
		require.NoError(t, err)

		// Generate report
		report, err := auditor.GenerateAccessReport(ctx, userID, time.Now().Add(-24*time.Hour), time.Now())
		require.NoError(t, err)

		// Export to CSV
		csv, err := report.ExportToCSV()
		require.NoError(t, err)
		assert.Contains(t, csv, "Timestamp")
		assert.Contains(t, csv, "Resource")
	})
}
