package rbac

import (
	"errors"
	"fmt"
	"sync"
	"time"
)

// DynamicPermission represents a runtime-defined permission
type DynamicPermission struct {
	Name        string                               `json:"name"`
	DisplayName string                               `json:"display_name"`
	Category    string                               `json:"category"`
	Description string                               `json:"description"`
	Condition   func(context PermissionContext) bool `json:"-"`
	CreatedAt   time.Time                            `json:"created_at"`
}

// PermissionContext provides context for conditional permissions
type PermissionContext struct {
	UserID     string            `json:"user_id"`
	UserTier   string            `json:"user_tier"`
	ResourceID string            `json:"resource_id"`
	Action     string            `json:"action"`
	Metadata   map[string]string `json:"metadata"`
}

// PermissionRegistry manages dynamic permissions
type PermissionRegistry struct {
	permissions map[string]*DynamicPermission
	categories  map[string][]string // category -> permission names
	mu          sync.RWMutex
}

// NewPermissionRegistry creates a new permission registry
func NewPermissionRegistry() *PermissionRegistry {
	pr := &PermissionRegistry{
		permissions: make(map[string]*DynamicPermission),
		categories:  make(map[string][]string),
	}

	// Register default permissions as dynamic
	pr.registerDefaults()
	return pr
}

func (pr *PermissionRegistry) registerDefaults() {
	// Convert static permissions to dynamic
	for perm := range GetDefaultPermissions() {
		name := string(perm)
		category := pr.extractCategory(name)
		pr.permissions[name] = &DynamicPermission{
			Name:        name,
			DisplayName: name,
			Category:    category,
			CreatedAt:   time.Now(),
		}

		if pr.categories[category] == nil {
			pr.categories[category] = []string{}
		}
		pr.categories[category] = append(pr.categories[category], name)
	}
}

func (pr *PermissionRegistry) extractCategory(permission string) string {
	for i, ch := range permission {
		if ch == '.' {
			return permission[:i]
		}
	}
	return "general"
}

// RegisterPermission registers a new permission
func (pr *PermissionRegistry) RegisterPermission(name, displayName, category string) error {
	pr.mu.Lock()
	defer pr.mu.Unlock()

	if _, exists := pr.permissions[name]; exists {
		return fmt.Errorf("permission %s already registered", name)
	}

	pr.permissions[name] = &DynamicPermission{
		Name:        name,
		DisplayName: displayName,
		Category:    category,
		CreatedAt:   time.Now(),
	}

	if pr.categories[category] == nil {
		pr.categories[category] = []string{}
	}
	pr.categories[category] = append(pr.categories[category], name)

	return nil
}

// UnregisterPermission removes a permission
func (pr *PermissionRegistry) UnregisterPermission(name string) error {
	pr.mu.Lock()
	defer pr.mu.Unlock()

	perm, exists := pr.permissions[name]
	if !exists {
		return fmt.Errorf("permission %s not found", name)
	}

	// Remove from category list
	if perms, ok := pr.categories[perm.Category]; ok {
		for i, p := range perms {
			if p == name {
				pr.categories[perm.Category] = append(perms[:i], perms[i+1:]...)
				break
			}
		}
	}

	delete(pr.permissions, name)
	return nil
}

// PermissionExists checks if a permission is registered
func (pr *PermissionRegistry) PermissionExists(name string) bool {
	pr.mu.RLock()
	defer pr.mu.RUnlock()
	_, exists := pr.permissions[name]
	return exists
}

// GetPermission returns a permission by name
func (pr *PermissionRegistry) GetPermission(name string) (*DynamicPermission, error) {
	pr.mu.RLock()
	defer pr.mu.RUnlock()

	perm, exists := pr.permissions[name]
	if !exists {
		return nil, fmt.Errorf("permission %s not found", name)
	}

	// Return a copy
	copy := *perm
	return &copy, nil
}

// GetPermissionsByCategory returns permissions in a category
func (pr *PermissionRegistry) GetPermissionsByCategory(category string) []*DynamicPermission {
	pr.mu.RLock()
	defer pr.mu.RUnlock()

	names, exists := pr.categories[category]
	if !exists {
		return []*DynamicPermission{}
	}

	result := make([]*DynamicPermission, 0, len(names))
	for _, name := range names {
		if perm, ok := pr.permissions[name]; ok {
			copy := *perm
			result = append(result, &copy)
		}
	}

	return result
}

// DynamicRoleManager extends role management with dynamic permissions
type DynamicRoleManager struct {
	*RoleManagerWithInheritance
	registry         *PermissionRegistry
	dynamicGrants    map[string]map[string]bool      // role -> permission -> granted
	temporaryGrants  map[string]map[string]time.Time // role -> permission -> expiry
	permissionGroups map[string][]string             // group name -> permissions
	mu               sync.RWMutex
}

// NewDynamicRoleManager creates a new dynamic role manager
func NewDynamicRoleManager() *DynamicRoleManager {
	return &DynamicRoleManager{
		RoleManagerWithInheritance: NewRoleManagerWithInheritance(),
		registry:                   NewPermissionRegistry(),
		dynamicGrants:              make(map[string]map[string]bool),
		temporaryGrants:            make(map[string]map[string]time.Time),
		permissionGroups:           make(map[string][]string),
	}
}

// RegisterPermission registers a new dynamic permission
func (drm *DynamicRoleManager) RegisterPermission(name, displayName, category string) error {
	return drm.registry.RegisterPermission(name, displayName, category)
}

// RegisterConditionalPermission registers a permission with a condition
func (drm *DynamicRoleManager) RegisterConditionalPermission(name, displayName, category string, condition func(PermissionContext) bool) error {
	drm.mu.Lock()
	defer drm.mu.Unlock()

	// Check if permission already exists
	if !drm.registry.PermissionExists(name) {
		// Register it if it doesn't exist
		err := drm.registry.RegisterPermission(name, displayName, category)
		if err != nil {
			return err
		}
	}

	// Update with condition (works for both new and existing permissions)
	if perm, exists := drm.registry.permissions[name]; exists {
		perm.Condition = condition
		// Update display name and category if needed
		if perm.DisplayName != displayName {
			perm.DisplayName = displayName
		}
		if perm.Category != category {
			perm.Category = category
		}
	}

	return nil
}

// GrantDynamicPermission grants a dynamic permission to a role
func (drm *DynamicRoleManager) GrantDynamicPermission(role, permission string) error {
	drm.mu.Lock()
	defer drm.mu.Unlock()

	if !drm.registry.PermissionExists(permission) {
		return fmt.Errorf("permission %s not registered", permission)
	}

	if drm.dynamicGrants[role] == nil {
		drm.dynamicGrants[role] = make(map[string]bool)
	}
	drm.dynamicGrants[role][permission] = true

	return nil
}

// RevokeDynamicPermission revokes a dynamic permission from a role
func (drm *DynamicRoleManager) RevokeDynamicPermission(role, permission string) error {
	drm.mu.Lock()
	defer drm.mu.Unlock()

	if grants, exists := drm.dynamicGrants[role]; exists {
		delete(grants, permission)
	}

	return nil
}

// GrantPermissionGroup grants multiple permissions as a group
func (drm *DynamicRoleManager) GrantPermissionGroup(role, groupName string, permissions []string) error {
	drm.mu.Lock()
	defer drm.mu.Unlock()

	// Store group definition
	drm.permissionGroups[groupName] = permissions

	// Grant each permission
	if drm.dynamicGrants[role] == nil {
		drm.dynamicGrants[role] = make(map[string]bool)
	}

	for _, perm := range permissions {
		drm.dynamicGrants[role][perm] = true
	}

	return nil
}

// GrantTemporaryPermission grants a permission for a limited time (seconds)
func (drm *DynamicRoleManager) GrantTemporaryPermission(role, permission string, ttlSeconds int64) error {
	drm.mu.Lock()
	defer drm.mu.Unlock()

	if drm.temporaryGrants[role] == nil {
		drm.temporaryGrants[role] = make(map[string]time.Time)
	}

	drm.temporaryGrants[role][permission] = time.Now().Add(time.Duration(ttlSeconds) * time.Second)
	return nil
}

// GetPermissionTTL returns remaining TTL in seconds for a temporary permission
func (drm *DynamicRoleManager) GetPermissionTTL(role, permission string) int64 {
	drm.mu.RLock()
	defer drm.mu.RUnlock()

	if grants, exists := drm.temporaryGrants[role]; exists {
		if expiry, ok := grants[permission]; ok {
			remaining := time.Until(expiry).Seconds()
			if remaining > 0 {
				return int64(remaining)
			}
		}
	}

	return 0
}

// RoleHasPermission checks if a role has a permission (including dynamic)
func (drm *DynamicRoleManager) RoleHasPermission(role, permission string) bool {
	drm.mu.RLock()
	defer drm.mu.RUnlock()

	// Check temporary grants first (and clean expired)
	if grants, exists := drm.temporaryGrants[role]; exists {
		if expiry, ok := grants[permission]; ok {
			if time.Now().Before(expiry) {
				return true
			}
			// Clean up expired grant
			delete(grants, permission)
		}
	}

	// Check dynamic grants
	if grants, exists := drm.dynamicGrants[role]; exists && grants[permission] {
		return true
	}

	// Fall back to base implementation
	return drm.checker.HasPermission(role, permission)
}

// EvaluatePermission evaluates a permission with context
func (drm *DynamicRoleManager) EvaluatePermission(role, permission string, context PermissionContext) bool {
	drm.mu.RLock()
	defer drm.mu.RUnlock()

	// First check if role has the permission at all
	if !drm.RoleHasPermission(role, permission) {
		return false
	}

	// Then check condition if it exists
	if perm, exists := drm.registry.permissions[permission]; exists && perm.Condition != nil {
		return perm.Condition(context)
	}

	return true
}

// Errors for dynamic permissions
var (
	ErrPermissionNotFound    = errors.New("permission not found")
	ErrPermissionExists      = errors.New("permission already exists")
	ErrInvalidPermissionName = errors.New("invalid permission name")
)
