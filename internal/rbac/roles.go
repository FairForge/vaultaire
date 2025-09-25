package rbac

import (
	"errors"
	"fmt"
	"github.com/google/uuid"
	"regexp"
	"time"
)

// Role represents a user role in the system
type Role struct {
	ID          string    `json:"id" db:"id"`
	Name        string    `json:"name" db:"name"`
	DisplayName string    `json:"display_name" db:"display_name"`
	Description string    `json:"description" db:"description"`
	Priority    int       `json:"priority" db:"priority"`   // Higher = more permissions
	IsSystem    bool      `json:"is_system" db:"is_system"` // System roles cannot be deleted
	CreatedAt   time.Time `json:"created_at" db:"created_at"`
	UpdatedAt   time.Time `json:"updated_at" db:"updated_at"`
}

var (
	// Default system roles
	defaultRoles = []Role{
		{
			ID:          "role_admin",
			Name:        "admin",
			DisplayName: "Administrator",
			Description: "Full system access",
			Priority:    1000,
			IsSystem:    true,
		},
		{
			ID:          "role_user",
			Name:        "user",
			DisplayName: "Standard User",
			Description: "Regular user with standard permissions",
			Priority:    100,
			IsSystem:    true,
		},
		{
			ID:          "role_viewer",
			Name:        "viewer",
			DisplayName: "Read-Only Viewer",
			Description: "Can view but not modify resources",
			Priority:    50,
			IsSystem:    true,
		},
		{
			ID:          "role_guest",
			Name:        "guest",
			DisplayName: "Guest",
			Description: "Limited access for unauthenticated users",
			Priority:    10,
			IsSystem:    true,
		},
	}

	roleNameRegex = regexp.MustCompile(`^[a-z][a-z0-9_-]{2,31}$`)
)

// GetDefaultRoles returns the default system roles
func GetDefaultRoles() []Role {
	// Return a copy to prevent modification
	roles := make([]Role, len(defaultRoles))
	copy(roles, defaultRoles)

	// Set timestamps
	now := time.Now()
	for i := range roles {
		roles[i].CreatedAt = now
		roles[i].UpdatedAt = now
	}

	return roles
}

// FindRole finds a role by name in a slice of roles
func FindRole(roles []Role, name string) *Role {
	for i := range roles {
		if roles[i].Name == name {
			return &roles[i]
		}
	}
	return nil
}

// NewRole creates a new custom role
func NewRole(name, displayName string) *Role {
	now := time.Now()
	return &Role{
		ID:          fmt.Sprintf("role_%s", uuid.New().String()),
		Name:        name,
		DisplayName: displayName,
		Priority:    75, // Between viewer and user
		IsSystem:    false,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
}

// ValidateRoleName validates a role name
func ValidateRoleName(name string) error {
	if name == "" {
		return errors.New("role name cannot be empty")
	}

	if len(name) < 3 {
		return errors.New("role name must be at least 3 characters")
	}

	if len(name) > 32 {
		return errors.New("role name cannot exceed 32 characters")
	}

	if !roleNameRegex.MatchString(name) {
		return errors.New("role name must start with letter and contain only lowercase letters, numbers, hyphens and underscores")
	}

	return nil
}

// Common role name constants for easy reference
const (
	RoleAdmin  = "admin"
	RoleUser   = "user"
	RoleViewer = "viewer"
	RoleGuest  = "guest"
)
