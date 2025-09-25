package rbac

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRoleAssignment(t *testing.T) {
	t.Run("assign role to user", func(t *testing.T) {
		manager := NewRoleManager()
		userID := uuid.New()

		// Assign user role
		err := manager.AssignRole(userID, RoleUser)
		require.NoError(t, err)

		// Check user has role
		hasRole := manager.UserHasRole(userID, RoleUser)
		assert.True(t, hasRole)

		// Check user doesn't have other roles
		assert.False(t, manager.UserHasRole(userID, RoleAdmin))
		assert.False(t, manager.UserHasRole(userID, RoleViewer))
	})

	t.Run("assign multiple roles", func(t *testing.T) {
		manager := NewRoleManager()
		userID := uuid.New()

		// Assign multiple roles
		err := manager.AssignRole(userID, RoleUser)
		require.NoError(t, err)
		err = manager.AssignRole(userID, RoleViewer)
		require.NoError(t, err)

		// Check user has both roles
		assert.True(t, manager.UserHasRole(userID, RoleUser))
		assert.True(t, manager.UserHasRole(userID, RoleViewer))
	})

	t.Run("revoke role from user", func(t *testing.T) {
		manager := NewRoleManager()
		userID := uuid.New()

		// Assign and then revoke
		err := manager.AssignRole(userID, RoleUser)
		require.NoError(t, err)

		err = manager.RevokeRole(userID, RoleUser)
		require.NoError(t, err)

		// Check user no longer has role
		assert.False(t, manager.UserHasRole(userID, RoleUser))
	})

	t.Run("get user roles", func(t *testing.T) {
		manager := NewRoleManager()
		userID := uuid.New()

		// Assign multiple roles
		err := manager.AssignRole(userID, RoleUser)
		require.NoError(t, err)
		err = manager.AssignRole(userID, RoleViewer)
		require.NoError(t, err)

		// Get all roles
		roles := manager.GetUserRoles(userID)
		assert.Len(t, roles, 2)
		assert.Contains(t, roles, RoleUser)
		assert.Contains(t, roles, RoleViewer)
	})

	t.Run("get highest role", func(t *testing.T) {
		manager := NewRoleManager()
		userID := uuid.New()

		// Assign multiple roles with different priorities
		err := manager.AssignRole(userID, RoleViewer) // Priority: 50
		require.NoError(t, err)
		err = manager.AssignRole(userID, RoleUser) // Priority: 100
		require.NoError(t, err)

		// Should return role with highest priority
		highest := manager.GetHighestRole(userID)
		assert.Equal(t, RoleUser, highest)

		// Add admin role
		err = manager.AssignRole(userID, RoleAdmin) // Priority: 1000
		require.NoError(t, err)
		highest = manager.GetHighestRole(userID)
		assert.Equal(t, RoleAdmin, highest)
	})
}

func TestRoleAssignmentValidation(t *testing.T) {
	t.Run("prevent duplicate assignment", func(t *testing.T) {
		manager := NewRoleManager()
		userID := uuid.New()

		// First assignment succeeds
		err := manager.AssignRole(userID, RoleUser)
		require.NoError(t, err)

		// Duplicate assignment should be idempotent (no error)
		err = manager.AssignRole(userID, RoleUser)
		assert.NoError(t, err)

		// User still has only one instance of the role
		roles := manager.GetUserRoles(userID)
		count := 0
		for _, r := range roles {
			if r == RoleUser {
				count++
			}
		}
		assert.Equal(t, 1, count)
	})

	t.Run("invalid role name", func(t *testing.T) {
		manager := NewRoleManager()
		userID := uuid.New()

		// Try to assign non-existent role
		err := manager.AssignRole(userID, "invalid_role")
		assert.Error(t, err)
	})

	t.Run("revoke non-existent assignment", func(t *testing.T) {
		manager := NewRoleManager()
		userID := uuid.New()

		// Revoking a role that wasn't assigned should not error
		err := manager.RevokeRole(userID, RoleUser)
		assert.NoError(t, err)
	})
}

func TestRoleInheritance(t *testing.T) {
	t.Run("check effective permissions", func(t *testing.T) {
		manager := NewRoleManager()
		userID := uuid.New()

		// Assign viewer role
		err := manager.AssignRole(userID, RoleViewer)
		require.NoError(t, err)

		// Check effective permissions
		perms := manager.GetEffectivePermissions(userID)

		// Should have viewer permissions
		assert.True(t, perms[PermStorageRead])
		assert.False(t, perms[PermStorageWrite])

		// Add user role
		err = manager.AssignRole(userID, RoleUser)
		require.NoError(t, err)
		perms = manager.GetEffectivePermissions(userID)

		// Should now have write permission too (from user role)
		assert.True(t, perms[PermStorageRead])
		assert.True(t, perms[PermStorageWrite])
	})

	t.Run("permission union across roles", func(t *testing.T) {
		manager := NewRoleManager()
		userID := uuid.New()

		// User with both viewer and user roles
		err := manager.AssignRole(userID, RoleViewer)
		require.NoError(t, err)
		err = manager.AssignRole(userID, RoleUser)
		require.NoError(t, err)

		// Should have union of permissions
		assert.True(t, manager.UserHasPermission(userID, "storage.read"))
		assert.True(t, manager.UserHasPermission(userID, "storage.write"))
	})
}
