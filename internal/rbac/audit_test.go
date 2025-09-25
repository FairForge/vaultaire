package rbac

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPermissionAudit(t *testing.T) {
	t.Run("log permission check", func(t *testing.T) {
		auditor := NewPermissionAuditor()
		userID := uuid.New()

		// Log a permission check
		auditor.LogPermissionCheck(userID, "storage.read", true)

		// Get audit logs
		logs := auditor.GetUserAuditLogs(userID, 10)
		require.Len(t, logs, 1)

		assert.Equal(t, userID, logs[0].UserID)
		assert.Equal(t, "storage.read", logs[0].Permission)
		assert.True(t, logs[0].Granted)
		assert.Equal(t, "check", logs[0].Action)
	})

	t.Run("log role assignment", func(t *testing.T) {
		auditor := NewPermissionAuditor()
		userID := uuid.New()
		grantedBy := uuid.New()

		// Log role assignment
		auditor.LogRoleAssignment(userID, grantedBy, RoleUser, "assigned")

		// Get audit logs
		logs := auditor.GetUserAuditLogs(userID, 10)
		require.Len(t, logs, 1)

		assert.Equal(t, "role_assigned", logs[0].Action)
		assert.Equal(t, RoleUser, logs[0].Role)
		assert.Equal(t, grantedBy, logs[0].PerformedBy)
	})

	t.Run("query audit logs by time range", func(t *testing.T) {
		auditor := NewPermissionAuditor()
		userID := uuid.New()

		// Log multiple events
		auditor.LogPermissionCheck(userID, "storage.read", true)
		time.Sleep(10 * time.Millisecond)
		auditor.LogPermissionCheck(userID, "storage.write", false)
		time.Sleep(10 * time.Millisecond)
		auditor.LogPermissionCheck(userID, "storage.delete", true)

		// Query by time range
		startTime := time.Now().Add(-1 * time.Minute)
		endTime := time.Now().Add(1 * time.Minute)
		logs := auditor.QueryAuditLogs(AuditQuery{
			UserID:    &userID,
			StartTime: &startTime,
			EndTime:   &endTime,
		})

		assert.Len(t, logs, 3)
	})

	t.Run("audit log retention", func(t *testing.T) {
		auditor := NewPermissionAuditor()
		userID := uuid.New()

		// Add old log
		oldLog := AuditLogEntry{
			ID:        uuid.New(),
			UserID:    userID,
			Timestamp: time.Now().Add(-91 * 24 * time.Hour), // 91 days old
			Action:    "check",
		}
		auditor.logs = append(auditor.logs, oldLog)

		// Add recent log
		auditor.LogPermissionCheck(userID, "storage.read", true)

		// Clean old logs (default 90 days retention)
		auditor.CleanOldLogs(90 * 24 * time.Hour)

		// Should only have recent log
		logs := auditor.GetUserAuditLogs(userID, 10)
		assert.Len(t, logs, 1)
	})
}

func TestAuditAnalytics(t *testing.T) {
	t.Run("permission usage statistics", func(t *testing.T) {
		auditor := NewPermissionAuditor()

		// Log various permission checks
		user1 := uuid.New()
		user2 := uuid.New()

		auditor.LogPermissionCheck(user1, "storage.read", true)
		auditor.LogPermissionCheck(user1, "storage.read", true)
		auditor.LogPermissionCheck(user2, "storage.read", true)
		auditor.LogPermissionCheck(user1, "storage.write", false)

		// Get statistics
		stats := auditor.GetPermissionStats("storage.read")
		assert.Equal(t, 3, stats.TotalChecks)
		assert.Equal(t, 3, stats.Granted)
		assert.Equal(t, 0, stats.Denied)
		assert.Equal(t, 2, stats.UniqueUsers)
	})

	t.Run("role usage statistics", func(t *testing.T) {
		auditor := NewPermissionAuditor()

		// Log role assignments
		auditor.LogRoleAssignment(uuid.New(), uuid.New(), RoleUser, "assigned")
		auditor.LogRoleAssignment(uuid.New(), uuid.New(), RoleUser, "assigned")
		auditor.LogRoleAssignment(uuid.New(), uuid.New(), RoleAdmin, "assigned")

		// Get role statistics
		stats := auditor.GetRoleStats()
		assert.Equal(t, 2, stats[RoleUser])
		assert.Equal(t, 1, stats[RoleAdmin])
	})
}
