package rbac

import (
	"github.com/stretchr/testify/assert"
	"testing"
)

func TestPermissionDefinitions(t *testing.T) {
	t.Run("default permissions exist", func(t *testing.T) {
		perms := GetDefaultPermissions()

		// We should have a good set of permissions
		assert.Greater(t, len(perms), 20)

		// Check some key permissions exist (use Permission type)
		assert.Contains(t, perms, PermStorageRead)
		assert.Contains(t, perms, PermStorageWrite)
		assert.Contains(t, perms, PermStorageDelete)
		assert.Contains(t, perms, PermUserRead)
		assert.Contains(t, perms, PermUserWrite)
		assert.Contains(t, perms, PermAdminUsers)
		assert.Contains(t, perms, PermAdminBilling)
	})

	t.Run("permission categories", func(t *testing.T) {
		perms := GetPermissionsByCategory("storage")
		assert.Greater(t, len(perms), 0)

		for _, p := range perms {
			assert.Contains(t, string(p), "storage.")
		}
	})

	t.Run("permission validation", func(t *testing.T) {
		// These are valid permissions we've defined
		validPerms := []string{
			"storage.read",
			"user.read",
			"admin.system",
		}

		for _, p := range validPerms {
			assert.True(t, IsValidPermission(p), "Permission %s should be valid", p)
		}

		invalidPerms := []string{
			"",
			"invalid",
			"storage.",
			".read",
			"storage..read",
			"nonexistent.permission",
		}

		for _, p := range invalidPerms {
			assert.False(t, IsValidPermission(p), "Permission %s should be invalid", p)
		}
	})
}

func TestPermissionMatrix(t *testing.T) {
	t.Run("admin has all permissions", func(t *testing.T) {
		matrix := GetDefaultPermissionMatrix()
		adminPerms := matrix[RoleAdmin]

		// Admin should have all permissions
		allPerms := GetDefaultPermissions()
		for perm := range allPerms {
			assert.True(t, adminPerms[perm], "Admin should have %s permission", perm)
		}
	})

	t.Run("user has standard permissions", func(t *testing.T) {
		matrix := GetDefaultPermissionMatrix()
		userPerms := matrix[RoleUser]

		// User should have read/write but not admin
		assert.True(t, userPerms[PermStorageRead])
		assert.True(t, userPerms[PermStorageWrite])
		assert.True(t, userPerms[PermStorageDelete])
		assert.True(t, userPerms[PermUserRead])
		assert.True(t, userPerms[PermUserWrite])

		// User should NOT have admin permissions
		assert.False(t, userPerms[PermAdminUsers])
		assert.False(t, userPerms[PermAdminBilling])
		assert.False(t, userPerms[PermAdminSystem])
	})

	t.Run("viewer has read-only permissions", func(t *testing.T) {
		matrix := GetDefaultPermissionMatrix()
		viewerPerms := matrix[RoleViewer]

		// Viewer should have read permissions
		assert.True(t, viewerPerms[PermStorageRead])
		assert.True(t, viewerPerms[PermUserRead])
		assert.True(t, viewerPerms[PermBucketList])

		// Viewer should NOT have write/delete permissions
		assert.False(t, viewerPerms[PermStorageWrite])
		assert.False(t, viewerPerms[PermStorageDelete])
		assert.False(t, viewerPerms[PermUserWrite])
	})

	t.Run("guest has minimal permissions", func(t *testing.T) {
		matrix := GetDefaultPermissionMatrix()
		guestPerms := matrix[RoleGuest]

		// Guest should have very limited permissions
		assert.True(t, guestPerms[PermAuthLogin])
		assert.True(t, guestPerms[PermAuthRegister])

		// Guest should NOT have storage permissions
		assert.False(t, guestPerms[PermStorageRead])
		assert.False(t, guestPerms[PermStorageWrite])
	})
}

func TestPermissionCheck(t *testing.T) {
	t.Run("check single permission", func(t *testing.T) {
		checker := NewPermissionChecker()

		// Admin can do everything
		assert.True(t, checker.HasPermission(RoleAdmin, "storage.write"))
		assert.True(t, checker.HasPermission(RoleAdmin, "admin.users"))

		// User can read/write but not admin
		assert.True(t, checker.HasPermission(RoleUser, "storage.write"))
		assert.False(t, checker.HasPermission(RoleUser, "admin.users"))

		// Viewer can only read
		assert.True(t, checker.HasPermission(RoleViewer, "storage.read"))
		assert.False(t, checker.HasPermission(RoleViewer, "storage.write"))
	})

	t.Run("check multiple permissions (AND)", func(t *testing.T) {
		checker := NewPermissionChecker()

		// Admin has all permissions
		assert.True(t, checker.HasAllPermissions(RoleAdmin, []string{
			"storage.read", "storage.write", "admin.users",
		}))

		// User doesn't have admin permissions
		assert.False(t, checker.HasAllPermissions(RoleUser, []string{
			"storage.read", "storage.write", "admin.users",
		}))
	})

	t.Run("check multiple permissions (OR)", func(t *testing.T) {
		checker := NewPermissionChecker()

		// User has at least one of these
		assert.True(t, checker.HasAnyPermission(RoleUser, []string{
			"storage.write", "admin.users",
		}))

		// Guest has none of these
		assert.False(t, checker.HasAnyPermission(RoleGuest, []string{
			"storage.write", "storage.read",
		}))
	})
}
