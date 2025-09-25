package rbac

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPermissionInheritance(t *testing.T) {
	t.Run("basic inheritance chain", func(t *testing.T) {
		inheritance := NewInheritanceManager()

		// Admin inherits from User
		err := inheritance.AddInheritance(RoleAdmin, RoleUser)
		require.NoError(t, err)

		// User inherits from Viewer
		err = inheritance.AddInheritance(RoleUser, RoleViewer)
		require.NoError(t, err)

		// Admin should inherit from both User and Viewer (transitively)
		inherited := inheritance.GetInheritedRoles(RoleAdmin)
		assert.Contains(t, inherited, RoleUser)
		assert.Contains(t, inherited, RoleViewer)
	})

	t.Run("prevent circular inheritance", func(t *testing.T) {
		inheritance := NewInheritanceManager()

		// Set up a chain: Admin -> User -> Viewer
		err := inheritance.AddInheritance(RoleAdmin, RoleUser)
		require.NoError(t, err)

		err = inheritance.AddInheritance(RoleUser, RoleViewer)
		require.NoError(t, err)

		// Try to create a cycle: Viewer -> Admin
		err = inheritance.AddInheritance(RoleViewer, RoleAdmin)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "circular")
	})

	t.Run("multiple inheritance paths", func(t *testing.T) {
		inheritance := NewInheritanceManager()

		// Admin inherits from both User and Viewer directly
		err := inheritance.AddInheritance(RoleAdmin, RoleUser)
		require.NoError(t, err)

		err = inheritance.AddInheritance(RoleAdmin, RoleViewer)
		require.NoError(t, err)

		// Should get both direct inheritances
		inherited := inheritance.GetDirectInheritances(RoleAdmin)
		assert.Len(t, inherited, 2)
		assert.Contains(t, inherited, RoleUser)
		assert.Contains(t, inherited, RoleViewer)
	})

	t.Run("remove inheritance", func(t *testing.T) {
		inheritance := NewInheritanceManager()

		// Add and then remove
		err := inheritance.AddInheritance(RoleAdmin, RoleUser)
		require.NoError(t, err)

		err = inheritance.RemoveInheritance(RoleAdmin, RoleUser)
		require.NoError(t, err)

		// Should no longer inherit
		inherited := inheritance.GetInheritedRoles(RoleAdmin)
		assert.NotContains(t, inherited, RoleUser)
	})
}

func TestInheritedPermissions(t *testing.T) {
	t.Run("permissions through inheritance", func(t *testing.T) {
		manager := NewRoleManagerWithInheritance()
		userID := uuid.New()

		// Create custom role that inherits from viewer
		customRole := "content_moderator"
		err := manager.CreateCustomRole(customRole, "Content Moderator", RoleViewer)
		require.NoError(t, err)

		// Assign custom role to user
		err = manager.AssignRole(userID, customRole)
		require.NoError(t, err)

		// User should have viewer permissions through inheritance
		assert.True(t, manager.UserHasPermission(userID, "storage.read"))
		assert.False(t, manager.UserHasPermission(userID, "storage.write"))

		// Add specific permission to custom role
		manager.GrantPermissionToRole(customRole, PermStorageWrite)

		// Now user should have write permission too
		assert.True(t, manager.UserHasPermission(userID, "storage.write"))
	})

	t.Run("override inherited permissions", func(t *testing.T) {
		manager := NewRoleManagerWithInheritance()

		// Create role that inherits but denies specific permission
		restrictedRole := "restricted_user"
		err := manager.CreateCustomRole(restrictedRole, "Restricted User", RoleUser)
		require.NoError(t, err)

		manager.DenyPermissionForRole(restrictedRole, PermStorageDelete)

		userID := uuid.New()
		err = manager.AssignRole(userID, restrictedRole)
		require.NoError(t, err)

		// Should have user permissions except delete (explicitly denied)
		assert.True(t, manager.UserHasPermission(userID, "storage.read"))
		assert.True(t, manager.UserHasPermission(userID, "storage.write"))
		assert.False(t, manager.UserHasPermission(userID, "storage.delete"))
	})
}

func TestRoleHierarchy(t *testing.T) {
	t.Run("hierarchy depth calculation", func(t *testing.T) {
		hierarchy := NewRoleHierarchy()

		// Build a hierarchy (Admin is root with no parents)
		hierarchy.AddRole(RoleAdmin, 1000, nil)
		hierarchy.AddRole(RoleUser, 100, []string{RoleAdmin})
		hierarchy.AddRole(RoleViewer, 50, []string{RoleUser})
		hierarchy.AddRole(RoleGuest, 10, []string{RoleViewer})

		// Check depths
		assert.Equal(t, 0, hierarchy.GetDepth(RoleAdmin))
		assert.Equal(t, 1, hierarchy.GetDepth(RoleUser))
		assert.Equal(t, 2, hierarchy.GetDepth(RoleViewer))
		assert.Equal(t, 3, hierarchy.GetDepth(RoleGuest))
	})

	t.Run("find common ancestor", func(t *testing.T) {
		hierarchy := NewRoleHierarchy()

		hierarchy.AddRole(RoleAdmin, 1000, nil)
		hierarchy.AddRole(RoleUser, 100, []string{RoleAdmin})
		hierarchy.AddRole(RoleViewer, 50, []string{RoleAdmin})

		// Common ancestor of User and Viewer should be Admin
		ancestor := hierarchy.FindCommonAncestor(RoleUser, RoleViewer)
		assert.Equal(t, RoleAdmin, ancestor)
	})
}
