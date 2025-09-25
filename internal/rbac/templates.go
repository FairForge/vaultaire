package rbac

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
)

// RoleWithPermissions extends Role with permissions list
type RoleWithPermissions struct {
	*Role
	Permissions []string `json:"permissions"`
}

// RoleTemplate defines a reusable role configuration
type RoleTemplate struct {
	Name         string            `json:"name"`
	DisplayName  string            `json:"display_name"`
	Description  string            `json:"description"`
	Permissions  []string          `json:"permissions"`
	InheritsFrom []string          `json:"inherits_from"`
	Metadata     map[string]string `json:"metadata"`
	Version      int               `json:"version"`
	CreatedAt    time.Time         `json:"created_at"`
	UpdatedAt    time.Time         `json:"updated_at"`
}

// BulkApplyResult represents the result of a bulk operation
type BulkApplyResult struct {
	UserID  uuid.UUID `json:"user_id"`
	Success bool      `json:"success"`
	Error   error     `json:"error,omitempty"`
}

// TemplateManager manages role templates
type TemplateManager struct {
	*DynamicRoleManager
	templates        map[string]*RoleTemplate
	templateVersions map[string]map[int]*RoleTemplate // name -> version -> template
	userTemplates    map[uuid.UUID][]string           // user -> template names
	mu               sync.RWMutex
}

// NewTemplateManager creates a new template manager
func NewTemplateManager() *TemplateManager {
	tm := &TemplateManager{
		DynamicRoleManager: NewDynamicRoleManager(),
		templates:          make(map[string]*RoleTemplate),
		templateVersions:   make(map[string]map[int]*RoleTemplate),
		userTemplates:      make(map[uuid.UUID][]string),
	}

	// Register default templates
	tm.registerDefaultTemplates()
	return tm
}

func (tm *TemplateManager) registerDefaultTemplates() {
	defaultTemplates := []RoleTemplate{
		{
			Name:        "developer",
			DisplayName: "Developer",
			Description: "Software developer with code and storage access",
			Permissions: []string{
				"storage.read", "storage.write", "storage.delete",
				"bucket.create", "bucket.delete",
				"apikey.create", "apikey.delete",
				"user.read", "profile.read", "profile.write",
			},
			InheritsFrom: []string{RoleUser},
			Version:      1,
		},
		{
			Name:        "analyst",
			DisplayName: "Data Analyst",
			Description: "Read access with reporting capabilities",
			Permissions: []string{
				"storage.read", "storage.list",
				"reports.read", "reports.generate",
				"user.read", "billing.read",
				"quota.read",
			},
			InheritsFrom: []string{RoleViewer},
			Version:      1,
		},
		{
			Name:        "support",
			DisplayName: "Support Staff",
			Description: "Customer support with limited admin access",
			Permissions: []string{
				"user.read", "user.write",
				"ticket.manage", "ticket.view",
				"storage.read", "bucket.list",
				"activity.view",
			},
			InheritsFrom: []string{RoleViewer},
			Version:      1,
		},
		{
			Name:        "auditor",
			DisplayName: "Compliance Auditor",
			Description: "Read-only access with audit capabilities",
			Permissions: []string{
				"*.read", // Read everything
				"audit.view", "audit.export",
				"compliance.view",
			},
			InheritsFrom: []string{RoleViewer},
			Version:      1,
		},
	}

	now := time.Now()
	for _, template := range defaultTemplates {
		template.CreatedAt = now
		template.UpdatedAt = now
		tm.templates[template.Name] = &template

		// Initialize version history
		if tm.templateVersions[template.Name] == nil {
			tm.templateVersions[template.Name] = make(map[int]*RoleTemplate)
		}
		templateCopy := template
		tm.templateVersions[template.Name][1] = &templateCopy

		// Register permissions if they don't exist
		for _, perm := range template.Permissions {
			if perm == "*.read" {
				continue // Special wildcard
			}
			category := tm.extractCategory(perm)
			_ = tm.RegisterPermission(perm, perm, category)
		}
	}
}

func (tm *TemplateManager) extractCategory(permission string) string {
	for i, ch := range permission {
		if ch == '.' {
			return permission[:i]
		}
	}
	return "general"
}

// CreateRoleFromTemplate creates a new role based on a template
func (tm *TemplateManager) CreateRoleFromTemplate(templateName string) (*RoleWithPermissions, error) {
	tm.mu.RLock()
	template, exists := tm.templates[templateName]
	tm.mu.RUnlock()

	if !exists {
		return nil, fmt.Errorf("template %s not found", templateName)
	}

	// Create the base role
	baseRole := NewRole(template.Name, template.DisplayName)
	baseRole.Description = template.Description

	// Create extended role with permissions
	role := &RoleWithPermissions{
		Role: baseRole,
	}

	// Set up inheritance
	if len(template.InheritsFrom) > 0 {
		err := tm.CreateCustomRole(template.Name, template.DisplayName, template.InheritsFrom...)
		if err != nil {
			return nil, fmt.Errorf("failed to create role with inheritance: %w", err)
		}
	}

	// Grant permissions
	for _, perm := range template.Permissions {
		if perm == "*.read" {
			// Grant all read permissions
			for p := range tm.registry.permissions {
				if len(p) > 5 && p[len(p)-5:] == ".read" {
					_ = tm.GrantDynamicPermission(template.Name, p)
				}
			}
		} else {
			_ = tm.GrantDynamicPermission(template.Name, perm)
		}
	}

	// Collect all permissions
	role.Permissions = tm.collectRolePermissions(template.Name)

	return role, nil
}

func (tm *TemplateManager) collectRolePermissions(roleName string) []string {
	var perms []string

	// Get dynamic permissions
	tm.mu.RLock()
	if grants, exists := tm.dynamicGrants[roleName]; exists {
		for perm := range grants {
			perms = append(perms, perm)
		}
	}
	tm.mu.RUnlock()

	// Get inherited permissions
	inherited := tm.inheritance.GetInheritedRoles(roleName)
	for _, parent := range inherited {
		matrix := GetDefaultPermissionMatrix()
		if parentPerms, exists := matrix[parent]; exists {
			for perm, granted := range parentPerms {
				if granted {
					perms = append(perms, string(perm))
				}
			}
		}
	}

	return perms
}

// ListTemplates returns all available templates
func (tm *TemplateManager) ListTemplates() []*RoleTemplate {
	tm.mu.RLock()
	defer tm.mu.RUnlock()

	templates := make([]*RoleTemplate, 0, len(tm.templates))
	for _, template := range tm.templates {
		templateCopy := *template
		templates = append(templates, &templateCopy)
	}
	return templates
}

// GetTemplate returns a specific template
func (tm *TemplateManager) GetTemplate(name string) (*RoleTemplate, error) {
	tm.mu.RLock()
	defer tm.mu.RUnlock()

	template, exists := tm.templates[name]
	if !exists {
		return nil, fmt.Errorf("template %s not found", name)
	}

	templateCopy := *template
	return &templateCopy, nil
}

// RegisterTemplate registers a custom template
func (tm *TemplateManager) RegisterTemplate(template RoleTemplate) error {
	tm.mu.Lock()
	defer tm.mu.Unlock()

	if template.Name == "" {
		return errors.New("template name required")
	}

	template.CreatedAt = time.Now()
	template.UpdatedAt = time.Now()
	template.Version = 1

	tm.templates[template.Name] = &template

	// Initialize version history
	if tm.templateVersions[template.Name] == nil {
		tm.templateVersions[template.Name] = make(map[int]*RoleTemplate)
	}
	templateCopy := template
	tm.templateVersions[template.Name][1] = &templateCopy

	return nil
}

// ApplyTemplateToRole applies template permissions to an existing role
func (tm *TemplateManager) ApplyTemplateToRole(roleName, templateName string) error {
	template, err := tm.GetTemplate(templateName)
	if err != nil {
		return err
	}

	// Grant all template permissions
	for _, perm := range template.Permissions {
		if perm == "*.read" {
			// Grant all read permissions
			for p := range tm.registry.permissions {
				if len(p) > 5 && p[len(p)-5:] == ".read" {
					_ = tm.GrantDynamicPermission(roleName, p)
				}
			}
		} else {
			_ = tm.GrantDynamicPermission(roleName, perm)
		}
	}

	return nil
}

// GetRolePermissions returns all permissions for a role
func (tm *TemplateManager) GetRolePermissions(roleName string) []string {
	return tm.collectRolePermissions(roleName)
}

// CombineTemplates creates a new template from multiple templates
func (tm *TemplateManager) CombineTemplates(name string, templateNames []string) *RoleTemplate {
	combined := &RoleTemplate{
		Name:        name,
		DisplayName: name,
		Permissions: []string{},
		CreatedAt:   time.Now(),
		UpdatedAt:   time.Now(),
		Version:     1,
	}

	// Collect all permissions from templates
	permSet := make(map[string]bool)
	for _, tName := range templateNames {
		if template, err := tm.GetTemplate(tName); err == nil {
			for _, perm := range template.Permissions {
				permSet[perm] = true
			}
			combined.InheritsFrom = append(combined.InheritsFrom, template.InheritsFrom...)
		}
	}

	// Convert set to slice
	for perm := range permSet {
		combined.Permissions = append(combined.Permissions, perm)
	}

	return combined
}

// BulkApplyTemplate applies a template to multiple users
func (tm *TemplateManager) BulkApplyTemplate(templateName string, userIDs []uuid.UUID) []BulkApplyResult {
	results := make([]BulkApplyResult, len(userIDs))

	// Create role from template
	role, err := tm.CreateRoleFromTemplate(templateName)
	if err != nil {
		for i := range results {
			results[i] = BulkApplyResult{
				UserID:  userIDs[i],
				Success: false,
				Error:   err,
			}
		}
		return results
	}

	// Apply to each user
	for i, userID := range userIDs {
		err := tm.AssignRole(userID, role.Name)
		results[i] = BulkApplyResult{
			UserID:  userID,
			Success: err == nil,
			Error:   err,
		}

		// Track template assignment
		if err == nil {
			tm.mu.Lock()
			if tm.userTemplates[userID] == nil {
				tm.userTemplates[userID] = []string{}
			}
			tm.userTemplates[userID] = append(tm.userTemplates[userID], templateName)
			tm.mu.Unlock()
		}
	}

	return results
}

// UserHasTemplate checks if a user has been assigned a template
func (tm *TemplateManager) UserHasTemplate(userID uuid.UUID, templateName string) bool {
	tm.mu.RLock()
	defer tm.mu.RUnlock()

	templates, exists := tm.userTemplates[userID]
	if !exists {
		return false
	}

	for _, t := range templates {
		if t == templateName {
			return true
		}
	}
	return false
}

// GetTemplateVersion returns the current version of a template
func (tm *TemplateManager) GetTemplateVersion(name string) int {
	tm.mu.RLock()
	defer tm.mu.RUnlock()

	if template, exists := tm.templates[name]; exists {
		return template.Version
	}
	return 0
}

// UpdateTemplate updates a template and increments its version
func (tm *TemplateManager) UpdateTemplate(name string, template RoleTemplate) error {
	tm.mu.Lock()
	defer tm.mu.Unlock()

	current, exists := tm.templates[name]
	if !exists {
		return fmt.Errorf("template %s not found", name)
	}

	// Increment version
	template.Name = name
	template.Version = current.Version + 1
	template.CreatedAt = current.CreatedAt
	template.UpdatedAt = time.Now()

	// Store version history
	if tm.templateVersions[name] == nil {
		tm.templateVersions[name] = make(map[int]*RoleTemplate)
	}
	templateCopy := template
	tm.templateVersions[name][template.Version] = &templateCopy

	// Update current
	tm.templates[name] = &template

	return nil
}

// RevertTemplate reverts a template to a previous version
func (tm *TemplateManager) RevertTemplate(name string, version int) error {
	tm.mu.Lock()
	defer tm.mu.Unlock()

	versions, exists := tm.templateVersions[name]
	if !exists {
		return fmt.Errorf("template %s not found", name)
	}

	previous, exists := versions[version]
	if !exists {
		return fmt.Errorf("version %d not found for template %s", version, name)
	}

	// Create new version as revert
	revert := *previous
	revert.Version = tm.templates[name].Version + 1
	revert.UpdatedAt = time.Now()

	// Store as new version
	versions[revert.Version] = &revert
	tm.templates[name] = &revert

	return nil
}

// UserHasPermission checks if a user has a specific permission
func (tm *TemplateManager) UserHasPermission(userID uuid.UUID, permission string) bool {
	// Get user's roles
	roles := tm.GetUserRoles(userID)

	// Check each role for the permission (including inheritance)
	for _, role := range roles {
		// Use inheritance-aware check
		if tm.RoleHasPermissionWithInheritance(role, permission) {
			return true
		}
	}

	return false
}

// UserHasRole checks if a user has a specific role
func (tm *TemplateManager) UserHasRole(userID uuid.UUID, role string) bool {
	roles := tm.GetUserRoles(userID)
	for _, r := range roles {
		if r == role {
			return true
		}
	}
	return false
}

// GetEffectivePermissions returns all permissions for a user
func (tm *TemplateManager) GetEffectivePermissions(userID uuid.UUID) PermissionSet {
	perms := make(PermissionSet)

	roles := tm.GetUserRoles(userID)
	for _, role := range roles {
		// Get dynamic permissions for this role
		rolePerms := tm.collectRolePermissions(role)
		for _, p := range rolePerms {
			perms[Permission(p)] = true
		}

		// Also check static permissions from matrix
		matrix := GetDefaultPermissionMatrix()
		if rolePerms, exists := matrix[role]; exists {
			for perm, granted := range rolePerms {
				if granted {
					perms[perm] = true
				}
			}
		}
	}

	return perms
}
