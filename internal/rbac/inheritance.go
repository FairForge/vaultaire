package rbac

import (
	"errors"
	"fmt"
	"sync"

	"github.com/google/uuid"
)

// InheritanceManager manages role inheritance relationships
type InheritanceManager struct {
	inheritances map[string][]string // role -> parent roles it inherits from
	mu           sync.RWMutex
}

// NewInheritanceManager creates a new inheritance manager
func NewInheritanceManager() *InheritanceManager {
	return &InheritanceManager{
		inheritances: make(map[string][]string),
	}
}

// AddInheritance adds an inheritance relationship (child inherits from parent)
func (im *InheritanceManager) AddInheritance(childRole, parentRole string) error {
	im.mu.Lock()
	defer im.mu.Unlock()

	// Prevent self-inheritance
	if childRole == parentRole {
		return errors.New("role cannot inherit from itself")
	}

	// Check for circular dependency BEFORE adding
	// Temporarily add to check for cycles
	tempInheritances := make(map[string][]string)
	for k, v := range im.inheritances {
		tempInheritances[k] = append([]string{}, v...)
	}

	// Add the new inheritance to temp map
	if tempInheritances[childRole] == nil {
		tempInheritances[childRole] = []string{}
	}
	tempInheritances[childRole] = append(tempInheritances[childRole], parentRole)

	// Check if this creates a cycle
	if im.hasCycle(tempInheritances, childRole, make(map[string]bool)) {
		return fmt.Errorf("would create circular inheritance: %s -> %s", childRole, parentRole)
	}

	// No cycle, safe to add
	if im.inheritances[childRole] == nil {
		im.inheritances[childRole] = []string{}
	}

	// Check if already exists
	for _, role := range im.inheritances[childRole] {
		if role == parentRole {
			return nil // Already exists, idempotent
		}
	}

	im.inheritances[childRole] = append(im.inheritances[childRole], parentRole)
	return nil
}

// hasCycle checks if there's a cycle starting from the given role
func (im *InheritanceManager) hasCycle(inheritances map[string][]string, role string, visiting map[string]bool) bool {
	if visiting[role] {
		return true // Found a cycle
	}

	visiting[role] = true
	defer func() { visiting[role] = false }()

	if parents, exists := inheritances[role]; exists {
		for _, parent := range parents {
			if im.hasCycle(inheritances, parent, visiting) {
				return true
			}
		}
	}

	return false
}

// RemoveInheritance removes an inheritance relationship
func (im *InheritanceManager) RemoveInheritance(childRole, parentRole string) error {
	im.mu.Lock()
	defer im.mu.Unlock()

	parents := im.inheritances[childRole]
	for i, role := range parents {
		if role == parentRole {
			im.inheritances[childRole] = append(parents[:i], parents[i+1:]...)
			if len(im.inheritances[childRole]) == 0 {
				delete(im.inheritances, childRole)
			}
			return nil
		}
	}

	return nil
}

// GetDirectInheritances returns immediate parent roles
func (im *InheritanceManager) GetDirectInheritances(role string) []string {
	im.mu.RLock()
	defer im.mu.RUnlock()

	if parents, exists := im.inheritances[role]; exists {
		result := make([]string, len(parents))
		copy(result, parents)
		return result
	}
	return []string{}
}

// GetInheritedRoles returns all inherited roles (transitive closure)
func (im *InheritanceManager) GetInheritedRoles(role string) []string {
	im.mu.RLock()
	defer im.mu.RUnlock()

	visited := make(map[string]bool)
	var result []string
	im.collectInheritedRoles(role, visited, &result)
	return result
}

func (im *InheritanceManager) collectInheritedRoles(role string, visited map[string]bool, result *[]string) {
	if parents, exists := im.inheritances[role]; exists {
		for _, parent := range parents {
			if !visited[parent] {
				visited[parent] = true
				*result = append(*result, parent)
				im.collectInheritedRoles(parent, visited, result)
			}
		}
	}
}

// RoleManagerWithInheritance extends RoleManager with inheritance support
type RoleManagerWithInheritance struct {
	*RoleManager
	inheritance     *InheritanceManager
	customRoles     map[string]*Role
	rolePermissions map[string]PermissionSet
	deniedPerms     map[string]PermissionSet
	mu              sync.RWMutex
}

// NewRoleManagerWithInheritance creates a new role manager with inheritance
func NewRoleManagerWithInheritance() *RoleManagerWithInheritance {
	return &RoleManagerWithInheritance{
		RoleManager:     NewRoleManager(),
		inheritance:     NewInheritanceManager(),
		customRoles:     make(map[string]*Role),
		rolePermissions: make(map[string]PermissionSet),
		deniedPerms:     make(map[string]PermissionSet),
	}
}

// CreateCustomRole creates a custom role that inherits from parent
func (rm *RoleManagerWithInheritance) CreateCustomRole(name, displayName string, inheritsFrom ...string) error {
	rm.mu.Lock()
	defer rm.mu.Unlock()

	// Create the custom role
	role := NewRole(name, displayName)
	rm.customRoles[name] = role
	rm.rolePermissions[name] = make(PermissionSet)

	// Add to the base role manager's roles list so it's valid
	rm.roles = append(rm.roles, *role)

	// Set up inheritance
	for _, parent := range inheritsFrom {
		if err := rm.inheritance.AddInheritance(name, parent); err != nil {
			return fmt.Errorf("failed to add inheritance from %s: %w", parent, err)
		}
	}

	return nil
}

// AssignRole overrides to handle custom roles
func (rm *RoleManagerWithInheritance) AssignRole(userID uuid.UUID, roleName string) error {
	// Check if it's a custom role first
	rm.mu.RLock()
	_, isCustom := rm.customRoles[roleName]
	rm.mu.RUnlock()

	if isCustom {
		// Add directly to assignments without validation
		rm.mu.Lock()
		defer rm.mu.Unlock()

		if rm.assignments[userID] == nil {
			rm.assignments[userID] = make(map[string]bool)
		}
		rm.assignments[userID][roleName] = true
		return nil
	}

	// Otherwise use base implementation
	return rm.RoleManager.AssignRole(userID, roleName)
}

// GrantPermissionToRole grants a specific permission to a role
func (rm *RoleManagerWithInheritance) GrantPermissionToRole(roleName string, permission Permission) {
	rm.mu.Lock()
	defer rm.mu.Unlock()

	if rm.rolePermissions[roleName] == nil {
		rm.rolePermissions[roleName] = make(PermissionSet)
	}
	rm.rolePermissions[roleName][permission] = true
}

// DenyPermissionForRole explicitly denies a permission for a role (overrides inheritance)
func (rm *RoleManagerWithInheritance) DenyPermissionForRole(roleName string, permission Permission) {
	rm.mu.Lock()
	defer rm.mu.Unlock()

	if rm.deniedPerms[roleName] == nil {
		rm.deniedPerms[roleName] = make(PermissionSet)
	}
	rm.deniedPerms[roleName][permission] = true
}

// UserHasPermission checks if user has permission (with inheritance)
func (rm *RoleManagerWithInheritance) UserHasPermission(userID uuid.UUID, permission string) bool {
	rm.mu.RLock()
	defer rm.mu.RUnlock()

	userRoles := rm.GetUserRoles(userID)
	perm := Permission(permission)

	for _, roleName := range userRoles {
		// Check if explicitly denied for this role
		if denied := rm.deniedPerms[roleName]; denied != nil && denied[perm] {
			continue // Skip this role, check others
		}

		// Check role's direct permissions
		if perms := rm.rolePermissions[roleName]; perms != nil && perms[perm] {
			return true
		}

		// Check default permissions for standard roles
		if rm.checker.HasPermission(roleName, permission) {
			return true
		}

		// Check inherited permissions
		inherited := rm.inheritance.GetInheritedRoles(roleName)
		for _, parentRole := range inherited {
			// Check parent's custom permissions
			if perms := rm.rolePermissions[parentRole]; perms != nil && perms[perm] {
				return true
			}
			// Check parent's default permissions
			if rm.checker.HasPermission(parentRole, permission) {
				return true
			}
		}
	}

	return false
}

// RoleHierarchy manages role hierarchy depth and relationships
type RoleHierarchy struct {
	nodes map[string]*HierarchyNode
	mu    sync.RWMutex
}

type HierarchyNode struct {
	Name     string
	Priority int
	Parents  []string
	Depth    int
}

// NewRoleHierarchy creates a new role hierarchy manager
func NewRoleHierarchy() *RoleHierarchy {
	return &RoleHierarchy{
		nodes: make(map[string]*HierarchyNode),
	}
}

// AddRole adds a role to the hierarchy
func (rh *RoleHierarchy) AddRole(name string, priority int, parents []string) {
	rh.mu.Lock()
	defer rh.mu.Unlock()

	// Calculate depth based on parents
	maxParentDepth := -1
	for _, parent := range parents {
		if parentNode, exists := rh.nodes[parent]; exists {
			if parentNode.Depth > maxParentDepth {
				maxParentDepth = parentNode.Depth
			}
		}
	}

	rh.nodes[name] = &HierarchyNode{
		Name:     name,
		Priority: priority,
		Parents:  parents,
		Depth:    maxParentDepth + 1,
	}
}

// GetDepth returns the depth of a role in the hierarchy
func (rh *RoleHierarchy) GetDepth(role string) int {
	rh.mu.RLock()
	defer rh.mu.RUnlock()

	if node, exists := rh.nodes[role]; exists {
		return node.Depth
	}
	return -1
}

// FindCommonAncestor finds the lowest common ancestor of two roles
func (rh *RoleHierarchy) FindCommonAncestor(role1, role2 string) string {
	rh.mu.RLock()
	defer rh.mu.RUnlock()

	// Get all ancestors of role1
	ancestors1 := make(map[string]bool)
	rh.collectAncestors(role1, ancestors1)

	// Find first common ancestor by checking role2's ancestors
	visited := make(map[string]bool)
	queue := []string{role2}

	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]

		if visited[current] {
			continue
		}
		visited[current] = true

		if ancestors1[current] {
			return current
		}

		if node, exists := rh.nodes[current]; exists {
			queue = append(queue, node.Parents...)
		}
	}

	return ""
}

func (rh *RoleHierarchy) collectAncestors(role string, ancestors map[string]bool) {
	if node, exists := rh.nodes[role]; exists {
		for _, parent := range node.Parents {
			if !ancestors[parent] {
				ancestors[parent] = true
				rh.collectAncestors(parent, ancestors)
			}
		}
	}
}
