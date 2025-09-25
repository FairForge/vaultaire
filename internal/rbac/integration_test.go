package rbac

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRBACIntegration(t *testing.T) {
	t.Run("complete workflow", func(t *testing.T) {
		// Initialize all components
		manager := NewTemplateManager()
		auditor := NewPermissionAuditor()

		// Create users
		admin := uuid.New()
		developer := uuid.New()
		viewer := uuid.New()

		// Assign roles using templates
		err := manager.AssignRole(admin, RoleAdmin)
		require.NoError(t, err)

		role, err := manager.CreateRoleFromTemplate("developer")
		require.NoError(t, err)
		err = manager.AssignRole(developer, role.Name)
		require.NoError(t, err)

		err = manager.AssignRole(viewer, RoleViewer)
		require.NoError(t, err)

		// Verify permissions - use actual permissions from matrix
		assert.True(t, manager.UserHasPermission(admin, "admin.users"))
		assert.True(t, manager.UserHasPermission(developer, "storage.write"))
		assert.False(t, manager.UserHasPermission(viewer, "storage.write"))

		// Test dynamic permissions
		err = manager.RegisterPermission("feature.beta", "Beta Feature", "feature")
		require.NoError(t, err)

		err = manager.GrantDynamicPermission(role.Name, "feature.beta")
		require.NoError(t, err)

		assert.True(t, manager.UserHasPermission(developer, "feature.beta"))

		// Test temporary permissions
		err = manager.GrantTemporaryPermission(RoleViewer, "storage.write", 1)
		require.NoError(t, err)

		assert.True(t, manager.UserHasPermission(viewer, "storage.write"))
		time.Sleep(2 * time.Second)
		assert.False(t, manager.UserHasPermission(viewer, "storage.write"))

		// Audit logging
		auditor.LogRoleAssignment(developer, admin, role.Name, "assigned")
		auditor.LogPermissionCheck(developer, "storage.write", true)

		logs := auditor.GetUserAuditLogs(developer, 10)
		assert.Len(t, logs, 2)
	})

	t.Run("permission inheritance chain", func(t *testing.T) {
		manager := NewTemplateManager()

		// Create custom role hierarchy
		err := manager.CreateCustomRole("team_lead", "Team Lead", RoleUser)
		require.NoError(t, err)

		err = manager.CreateCustomRole("architect", "Architect", "team_lead")
		require.NoError(t, err)

		user := uuid.New()
		err = manager.AssignRole(user, "architect")
		require.NoError(t, err)

		// Should have permissions from entire chain
		assert.True(t, manager.UserHasPermission(user, "user.read"))    // from User role
		assert.True(t, manager.UserHasPermission(user, "storage.read")) // inherited
	})
}

func TestRBACPerformance(t *testing.T) {
	t.Run("permission check performance", func(t *testing.T) {
		manager := NewTemplateManager()

		// Create many users with various roles
		users := make([]uuid.UUID, 1000)
		for i := range users {
			users[i] = uuid.New()
			role := RoleUser
			if i%10 == 0 {
				role = RoleAdmin
			} else if i%5 == 0 {
				role = RoleViewer
			}
			err := manager.AssignRole(users[i], role)
			require.NoError(t, err)
		}

		// Measure permission checks
		start := time.Now()
		for _, user := range users {
			_ = manager.UserHasPermission(user, "storage.read")
		}
		elapsed := time.Since(start)

		// Should complete 1000 checks in under 100ms
		assert.Less(t, elapsed, 100*time.Millisecond)
	})

	t.Run("audit log query performance", func(t *testing.T) {
		auditor := NewPermissionAuditor()

		// Add many audit logs
		for i := 0; i < 10000; i++ {
			userID := uuid.New()
			auditor.LogPermissionCheck(userID, "storage.read", i%2 == 0)
		}

		// Query should be fast
		start := time.Now()
		query := AuditQuery{
			Permission: strPtr("storage.read"),
			Limit:      100,
		}
		results := auditor.QueryAuditLogs(query)
		elapsed := time.Since(start)

		assert.Len(t, results, 100)
		assert.Less(t, elapsed, 10*time.Millisecond)
	})
}

func strPtr(s string) *string {
	return &s
}
