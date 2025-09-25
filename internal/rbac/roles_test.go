package rbac

import (
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestRoleDefinitions(t *testing.T) {
	t.Run("default roles exist", func(t *testing.T) {
		roles := GetDefaultRoles()

		assert.Len(t, roles, 4) // Admin, User, Viewer, Guest

		// Check Admin role
		admin := FindRole(roles, "admin")
		require.NotNil(t, admin)
		assert.Equal(t, "admin", admin.Name)
		assert.Equal(t, "Administrator", admin.DisplayName)
		assert.True(t, admin.IsSystem)

		// Check User role
		user := FindRole(roles, "user")
		require.NotNil(t, user)
		assert.Equal(t, "user", user.Name)
		assert.Equal(t, "Standard User", user.DisplayName)

		// Check Viewer role
		viewer := FindRole(roles, "viewer")
		require.NotNil(t, viewer)
		assert.Equal(t, "viewer", viewer.Name)
		assert.Equal(t, "Read-Only Viewer", viewer.DisplayName)

		// Check Guest role
		guest := FindRole(roles, "guest")
		require.NotNil(t, guest)
		assert.Equal(t, "guest", guest.Name)
		assert.Equal(t, "Guest", guest.DisplayName)
	})

	t.Run("role hierarchy", func(t *testing.T) {
		roles := GetDefaultRoles()

		admin := FindRole(roles, "admin")
		user := FindRole(roles, "user")
		viewer := FindRole(roles, "viewer")
		guest := FindRole(roles, "guest")

		// Admin > User > Viewer > Guest
		assert.Greater(t, admin.Priority, user.Priority)
		assert.Greater(t, user.Priority, viewer.Priority)
		assert.Greater(t, viewer.Priority, guest.Priority)
	})

	t.Run("custom role creation", func(t *testing.T) {
		role := NewRole("moderator", "Content Moderator")

		assert.Equal(t, "moderator", role.Name)
		assert.Equal(t, "Content Moderator", role.DisplayName)
		assert.False(t, role.IsSystem) // Custom roles are not system roles
		assert.NotEmpty(t, role.ID)
	})
}

func TestRoleValidation(t *testing.T) {
	t.Run("valid role names", func(t *testing.T) {
		testCases := []string{
			"admin",
			"user_manager",
			"content-editor",
			"role123",
		}

		for _, tc := range testCases {
			err := ValidateRoleName(tc)
			assert.NoError(t, err, "Role name %s should be valid", tc)
		}
	})

	t.Run("invalid role names", func(t *testing.T) {
		testCases := []string{
			"",  // empty
			"a", // too short
			"this-is-a-very-long-role-name-that-exceeds-limits", // too long
			"role with spaces",
			"role@special",
			"../admin", // path traversal attempt
		}

		for _, tc := range testCases {
			err := ValidateRoleName(tc)
			assert.Error(t, err, "Role name %s should be invalid", tc)
		}
	})
}
