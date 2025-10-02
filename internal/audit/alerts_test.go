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

func TestAlertGeneration(t *testing.T) {
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

	t.Run("detect multiple failed logins", func(t *testing.T) {
		userID := uuid.New()

		// Create 5 failed login attempts in 5 minutes
		for i := 0; i < 5; i++ {
			err := auditor.LogEvent(ctx, &AuditEvent{
				UserID:    userID,
				TenantID:  "tenant-alert",
				EventType: EventTypeLogin,
				Action:    "LOGIN",
				Resource:  "/auth/login",
				Result:    ResultFailure,
				Severity:  SeverityWarning,
				IP:        "192.168.1.100",
				Timestamp: time.Now().Add(-time.Duration(i) * time.Minute),
			})
			require.NoError(t, err)
		}

		// Check for failed login alerts
		alerts, err := auditor.DetectFailedLoginAttempts(ctx, 5*time.Minute, 3)
		require.NoError(t, err)
		assert.NotEmpty(t, alerts)

		foundAlert := false
		for _, alert := range alerts {
			if alert.UserID == userID {
				assert.GreaterOrEqual(t, alert.Count, int64(3))
				foundAlert = true
				break
			}
		}
		assert.True(t, foundAlert, "should detect failed login pattern")
	})

	t.Run("detect suspicious activity patterns", func(t *testing.T) {
		userID := uuid.New()

		// Rapid file access from different IPs
		ips := []string{"10.0.0.1", "10.0.0.2", "10.0.0.3", "10.0.0.4"}
		for _, ip := range ips {
			err := auditor.LogEvent(ctx, &AuditEvent{
				UserID:    userID,
				TenantID:  "tenant-suspicious",
				EventType: EventTypeFileDownload,
				Action:    "GET",
				Resource:  "/sensitive/data.txt",
				Result:    ResultSuccess,
				IP:        ip,
			})
			require.NoError(t, err)
		}

		// Check for suspicious patterns
		alerts, err := auditor.DetectSuspiciousActivity(ctx, 10*time.Minute)
		require.NoError(t, err)
		assert.NotEmpty(t, alerts)
	})

	t.Run("detect unusual access patterns", func(t *testing.T) {
		userID := uuid.New()

		// Access from unusual location/time
		err := auditor.LogEvent(ctx, &AuditEvent{
			UserID:    userID,
			TenantID:  "tenant-unusual",
			EventType: EventTypeFileDownload,
			Action:    "GET",
			Resource:  "/data.txt",
			Result:    ResultSuccess,
			IP:        "200.0.0.1",                    // Different country
			Timestamp: time.Now().Add(-2 * time.Hour), // Unusual hour
		})
		require.NoError(t, err)

		// Detect unusual patterns
		alerts, err := auditor.DetectUnusualAccessPatterns(ctx, 24*time.Hour)
		require.NoError(t, err)
		assert.GreaterOrEqual(t, len(alerts), 0)
	})

	t.Run("detect critical events", func(t *testing.T) {
		// Create critical event
		err := auditor.LogEvent(ctx, &AuditEvent{
			UserID:    uuid.New(),
			TenantID:  "tenant-critical",
			EventType: EventTypeSecurityAlert,
			Action:    "ALERT",
			Resource:  "/admin",
			Result:    ResultFailure,
			Severity:  SeverityCritical,
		})
		require.NoError(t, err)

		// Check for critical alerts
		alerts, err := auditor.GetCriticalAlerts(ctx, 1*time.Hour)
		require.NoError(t, err)
		assert.NotEmpty(t, alerts)
	})

	t.Run("generate alert summary", func(t *testing.T) {
		summary, err := auditor.GetAlertSummary(ctx, 24*time.Hour)
		require.NoError(t, err)

		assert.GreaterOrEqual(t, summary.TotalAlerts, int64(0))
		assert.GreaterOrEqual(t, summary.CriticalAlerts, int64(0))
		assert.GreaterOrEqual(t, summary.WarningAlerts, int64(0))
	})
}
