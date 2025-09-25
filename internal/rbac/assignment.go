package rbac

import (
	"errors"
	"fmt"
	"sync"

	"github.com/google/uuid"
)

// RoleAssignment represents a role assigned to a user
type RoleAssignment struct {
	UserID    uuid.UUID `json:"user_id" db:"user_id"`
	RoleID    string    `json:"role_id" db:"role_id"`
	GrantedAt int64     `json:"granted_at" db:"granted_at"`
	GrantedBy uuid.UUID `json:"granted_by" db:"granted_by"`
}

// RoleManager manages role assignments
type RoleManager struct {
	assignments map[uuid.UUID]map[string]bool // userID -> roleNames
	roles       []Role
	checker     *PermissionChecker
	mu          sync.RWMutex
}

// NewRoleManager creates a new role manager
func NewRoleManager() *RoleManager {
	return &RoleManager{
		assignments: make(map[uuid.UUID]map[string]bool),
		roles:       GetDefaultRoles(),
		checker:     NewPermissionChecker(),
	}
}

// AssignRole assigns a role to a user
func (rm *RoleManager) AssignRole(userID uuid.UUID, roleName string) error {
	rm.mu.Lock()
	defer rm.mu.Unlock()

	// Validate role exists
	if !rm.isValidRole(roleName) {
		return fmt.Errorf("invalid role: %s", roleName)
	}

	// Initialize user's roles if needed
	if rm.assignments[userID] == nil {
		rm.assignments[userID] = make(map[string]bool)
	}

	// Assign the role (idempotent)
	rm.assignments[userID][roleName] = true

	return nil
}

// RevokeRole removes a role from a user
func (rm *RoleManager) RevokeRole(userID uuid.UUID, roleName string) error {
	rm.mu.Lock()
	defer rm.mu.Unlock()

	if userRoles, exists := rm.assignments[userID]; exists {
		delete(userRoles, roleName)
		if len(userRoles) == 0 {
			delete(rm.assignments, userID)
		}
	}

	return nil
}

// UserHasRole checks if a user has a specific role
func (rm *RoleManager) UserHasRole(userID uuid.UUID, roleName string) bool {
	rm.mu.RLock()
	defer rm.mu.RUnlock()

	if userRoles, exists := rm.assignments[userID]; exists {
		return userRoles[roleName]
	}
	return false
}

// GetUserRoles returns all roles assigned to a user
func (rm *RoleManager) GetUserRoles(userID uuid.UUID) []string {
	rm.mu.RLock()
	defer rm.mu.RUnlock()

	var roles []string
	if userRoles, exists := rm.assignments[userID]; exists {
		for role := range userRoles {
			roles = append(roles, role)
		}
	}
	return roles
}

// GetHighestRole returns the role with highest priority for a user
func (rm *RoleManager) GetHighestRole(userID uuid.UUID) string {
	rm.mu.RLock()
	defer rm.mu.RUnlock()

	userRoles := rm.assignments[userID]
	if len(userRoles) == 0 {
		return ""
	}

	var highestRole string
	highestPriority := -1

	for roleName := range userRoles {
		for _, role := range rm.roles {
			if role.Name == roleName && role.Priority > highestPriority {
				highestPriority = role.Priority
				highestRole = roleName
			}
		}
	}

	return highestRole
}

// GetEffectivePermissions returns all permissions a user has across all their roles
func (rm *RoleManager) GetEffectivePermissions(userID uuid.UUID) PermissionSet {
	rm.mu.RLock()
	defer rm.mu.RUnlock()

	effectivePerms := make(PermissionSet)
	userRoles := rm.assignments[userID]

	// Get permission matrix
	matrix := GetDefaultPermissionMatrix()

	// Union all permissions from all roles
	for roleName := range userRoles {
		if rolePerms, exists := matrix[roleName]; exists {
			for perm, allowed := range rolePerms {
				if allowed {
					effectivePerms[perm] = true
				}
			}
		}
	}

	return effectivePerms
}

// UserHasPermission checks if a user has a specific permission through their roles
func (rm *RoleManager) UserHasPermission(userID uuid.UUID, permission string) bool {
	rm.mu.RLock()
	defer rm.mu.RUnlock()

	userRoles := rm.assignments[userID]

	// Check each role for the permission
	for roleName := range userRoles {
		if rm.checker.HasPermission(roleName, permission) {
			return true
		}
	}

	return false
}

// isValidRole checks if a role name is valid
func (rm *RoleManager) isValidRole(roleName string) bool {
	for _, role := range rm.roles {
		if role.Name == roleName {
			return true
		}
	}
	return false
}

// SetUserDefaultRole assigns the default user role to a new user
func (rm *RoleManager) SetUserDefaultRole(userID uuid.UUID) error {
	return rm.AssignRole(userID, RoleUser)
}

// RevokeAllRoles removes all roles from a user
func (rm *RoleManager) RevokeAllRoles(userID uuid.UUID) error {
	rm.mu.Lock()
	defer rm.mu.Unlock()

	delete(rm.assignments, userID)
	return nil
}

// GetUsersWithRole returns all users who have a specific role
func (rm *RoleManager) GetUsersWithRole(roleName string) []uuid.UUID {
	rm.mu.RLock()
	defer rm.mu.RUnlock()

	var users []uuid.UUID
	for userID, roles := range rm.assignments {
		if roles[roleName] {
			users = append(users, userID)
		}
	}
	return users
}

// CountUsersWithRole returns the number of users with a specific role
func (rm *RoleManager) CountUsersWithRole(roleName string) int {
	return len(rm.GetUsersWithRole(roleName))
}

// Errors for role operations
var (
	ErrRoleNotFound = errors.New("role not found")
	ErrUserNotFound = errors.New("user not found")
	ErrInvalidRole  = errors.New("invalid role")
	ErrSystemRole   = errors.New("cannot modify system role")
)
