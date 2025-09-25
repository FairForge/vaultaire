package rbac

import (
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestDynamicPermissions(t *testing.T) {
	t.Run("register custom permission", func(t *testing.T) {
		registry := NewPermissionRegistry()

		// Register a new permission
		err := registry.RegisterPermission("custom.action", "Custom Action", "custom")
		require.NoError(t, err)

		// Permission should exist
		assert.True(t, registry.PermissionExists("custom.action"))

		// Get permission details
		perm, err := registry.GetPermission("custom.action")
		require.NoError(t, err)
		assert.Equal(t, "custom.action", perm.Name)
		assert.Equal(t, "Custom Action", perm.DisplayName)
		assert.Equal(t, "custom", perm.Category)
	})

	t.Run("prevent duplicate registration", func(t *testing.T) {
		registry := NewPermissionRegistry()

		// Register once
		err := registry.RegisterPermission("custom.action", "Custom Action", "custom")
		require.NoError(t, err)

		// Try to register again
		err = registry.RegisterPermission("custom.action", "Different Name", "other")
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "already registered")
	})

	t.Run("list permissions by category", func(t *testing.T) {
		registry := NewPermissionRegistry()

		// Register multiple permissions - check errors
		err := registry.RegisterPermission("feature.read", "Read Feature", "feature")
		require.NoError(t, err)
		err = registry.RegisterPermission("feature.write", "Write Feature", "feature")
		require.NoError(t, err)
		err = registry.RegisterPermission("other.action", "Other Action", "other")
		require.NoError(t, err)

		// Get by category
		featurePerms := registry.GetPermissionsByCategory("feature")
		assert.Len(t, featurePerms, 2)

		otherPerms := registry.GetPermissionsByCategory("other")
		assert.Len(t, otherPerms, 1)
	})

	t.Run("unregister permission", func(t *testing.T) {
		registry := NewPermissionRegistry()

		// Register and then unregister
		err := registry.RegisterPermission("temp.permission", "Temporary", "temp")
		require.NoError(t, err)
		assert.True(t, registry.PermissionExists("temp.permission"))

		err = registry.UnregisterPermission("temp.permission")
		require.NoError(t, err)
		assert.False(t, registry.PermissionExists("temp.permission"))
	})
}

func TestDynamicRolePermissions(t *testing.T) {
	t.Run("grant dynamic permission to role", func(t *testing.T) {
		manager := NewDynamicRoleManager()

		// Register a custom permission
		err := manager.RegisterPermission("reports.generate", "Generate Reports", "reports")
		require.NoError(t, err)

		// Grant to user role
		err = manager.GrantDynamicPermission(RoleUser, "reports.generate")
		require.NoError(t, err)

		// Check role has permission
		assert.True(t, manager.RoleHasPermission(RoleUser, "reports.generate"))
		assert.False(t, manager.RoleHasPermission(RoleViewer, "reports.generate"))
	})

	t.Run("revoke dynamic permission from role", func(t *testing.T) {
		manager := NewDynamicRoleManager()

		// Setup - check errors
		err := manager.RegisterPermission("reports.generate", "Generate Reports", "reports")
		require.NoError(t, err)
		err = manager.GrantDynamicPermission(RoleUser, "reports.generate")
		require.NoError(t, err)

		// Revoke
		err = manager.RevokeDynamicPermission(RoleUser, "reports.generate")
		require.NoError(t, err)

		// Should no longer have permission
		assert.False(t, manager.RoleHasPermission(RoleUser, "reports.generate"))
	})

	t.Run("permission groups", func(t *testing.T) {
		manager := NewDynamicRoleManager()

		// Create a permission group
		group := []string{
			"reports.read",
			"reports.write",
			"reports.delete",
		}

		// Register all permissions - check errors
		for _, p := range group {
			err := manager.RegisterPermission(p, p, "reports")
			require.NoError(t, err)
		}

		// Grant group to role
		err := manager.GrantPermissionGroup(RoleUser, "reports.*", group)
		require.NoError(t, err)

		// Check all permissions granted
		for _, p := range group {
			assert.True(t, manager.RoleHasPermission(RoleUser, p))
		}
	})
}

func TestPermissionRules(t *testing.T) {
	t.Run("conditional permissions", func(t *testing.T) {
		manager := NewDynamicRoleManager()

		// Grant the permission to User role first
		err := manager.RegisterPermission("storage.large_upload", "Upload Large Files", "storage")
		require.NoError(t, err)
		err = manager.GrantDynamicPermission(RoleUser, "storage.large_upload")
		require.NoError(t, err)

		// Register permission with condition (overwrites the basic one)
		err = manager.RegisterConditionalPermission(
			"storage.large_upload",
			"Upload Large Files",
			"storage",
			func(context PermissionContext) bool {
				// Only allow if user has premium tier
				return context.UserTier == "premium"
			},
		)
		require.NoError(t, err)

		// Check with different contexts
		premiumCtx := PermissionContext{UserTier: "premium"}
		assert.True(t, manager.EvaluatePermission(RoleUser, "storage.large_upload", premiumCtx))

		freeCtx := PermissionContext{UserTier: "free"}
		assert.False(t, manager.EvaluatePermission(RoleUser, "storage.large_upload", freeCtx))
	})

	t.Run("time-based permissions", func(t *testing.T) {
		manager := NewDynamicRoleManager()

		// Grant temporary permission (expires in 1 hour)
		err := manager.GrantTemporaryPermission(RoleUser, "admin.emergency", 3600)
		require.NoError(t, err)

		// Should have permission now
		assert.True(t, manager.RoleHasPermission(RoleUser, "admin.emergency"))

		// Should track expiration
		ttl := manager.GetPermissionTTL(RoleUser, "admin.emergency")
		assert.Greater(t, ttl, int64(3500))
		assert.LessOrEqual(t, ttl, int64(3600))
	})
}
