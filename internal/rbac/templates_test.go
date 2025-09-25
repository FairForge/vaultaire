package rbac

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRoleTemplates(t *testing.T) {
	t.Run("create role from template", func(t *testing.T) {
		manager := NewTemplateManager()

		// Use predefined template
		role, err := manager.CreateRoleFromTemplate("developer")
		require.NoError(t, err)

		assert.Equal(t, "developer", role.Name)
		assert.NotEmpty(t, role.Permissions)

		// Should have developer permissions
		assert.Contains(t, role.Permissions, "storage.read")
		assert.Contains(t, role.Permissions, "storage.write")
		assert.Contains(t, role.Permissions, "apikey.create")
	})

	t.Run("list available templates", func(t *testing.T) {
		manager := NewTemplateManager()

		templates := manager.ListTemplates()
		assert.Greater(t, len(templates), 0)

		// Should have standard templates
		var names []string
		for _, t := range templates {
			names = append(names, t.Name)
		}
		assert.Contains(t, names, "developer")
		assert.Contains(t, names, "analyst")
		assert.Contains(t, names, "support")
	})

	t.Run("register custom template", func(t *testing.T) {
		manager := NewTemplateManager()

		template := RoleTemplate{
			Name:         "custom_role",
			DisplayName:  "Custom Role",
			Description:  "A custom role template",
			Permissions:  []string{"storage.read", "user.read"},
			InheritsFrom: []string{RoleViewer},
		}

		err := manager.RegisterTemplate(template)
		require.NoError(t, err)

		// Should be available
		retrieved, err := manager.GetTemplate("custom_role")
		require.NoError(t, err)
		assert.Equal(t, "custom_role", retrieved.Name)
	})

	t.Run("apply template to existing role", func(t *testing.T) {
		manager := NewTemplateManager()

		// Apply developer template permissions to a role
		err := manager.ApplyTemplateToRole("existing_role", "developer")
		require.NoError(t, err)

		// Role should have developer permissions
		perms := manager.GetRolePermissions("existing_role")
		assert.Contains(t, perms, "storage.read")
		assert.Contains(t, perms, "storage.write")
	})
}

func TestTemplateInheritance(t *testing.T) {
	t.Run("template with inheritance", func(t *testing.T) {
		manager := NewTemplateManager()

		// Create role from template that inherits
		role, err := manager.CreateRoleFromTemplate("support")
		require.NoError(t, err)

		// Should have both support and inherited viewer permissions
		assert.Contains(t, role.Permissions, "user.read")
		assert.Contains(t, role.Permissions, "storage.read")
		assert.Contains(t, role.Permissions, "ticket.manage") // support specific
	})

	t.Run("composite template", func(t *testing.T) {
		manager := NewTemplateManager()

		// Create composite template from multiple templates
		composite := manager.CombineTemplates("devops", []string{"developer", "analyst"})

		// Should have permissions from both
		assert.Contains(t, composite.Permissions, "storage.write") // developer
		assert.Contains(t, composite.Permissions, "reports.read")  // analyst
	})
}

func TestTemplateBulkOperations(t *testing.T) {
	t.Run("apply template to multiple users", func(t *testing.T) {
		manager := NewTemplateManager()

		userIDs := []uuid.UUID{
			uuid.New(),
			uuid.New(),
			uuid.New(),
		}

		// Apply developer template to all users
		results := manager.BulkApplyTemplate("developer", userIDs)

		// All should succeed
		for _, result := range results {
			assert.True(t, result.Success)
			assert.NoError(t, result.Error)
		}

		// All users should have developer role
		for _, userID := range userIDs {
			assert.True(t, manager.UserHasTemplate(userID, "developer"))
		}
	})

	t.Run("template versioning", func(t *testing.T) {
		manager := NewTemplateManager()

		// Get template version
		v1 := manager.GetTemplateVersion("developer")

		// Update template
		err := manager.UpdateTemplate("developer", RoleTemplate{
			Name:        "developer",
			Permissions: []string{"storage.read", "storage.write", "new.permission"},
		})
		require.NoError(t, err)

		v2 := manager.GetTemplateVersion("developer")
		assert.Greater(t, v2, v1)

		// Can revert to previous version
		err = manager.RevertTemplate("developer", v1)
		require.NoError(t, err)
	})
}
