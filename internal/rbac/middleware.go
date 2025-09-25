package rbac

import (
	"context"
	"net/http"

	"github.com/google/uuid"
)

// ContextKey for storing RBAC data in context
type contextKey string

const (
	UserIDKey      contextKey = "userID"
	UserRolesKey   contextKey = "userRoles"
	PermissionsKey contextKey = "permissions"
)

// Middleware provides RBAC middleware for HTTP handlers
type Middleware struct {
	manager *TemplateManager
	auditor *PermissionAuditor
}

// NewMiddleware creates new RBAC middleware
func NewMiddleware(manager *TemplateManager, auditor *PermissionAuditor) *Middleware {
	return &Middleware{
		manager: manager,
		auditor: auditor,
	}
}

// RequirePermission creates middleware that requires specific permission
func (m *Middleware) RequirePermission(permission string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			userID := GetUserID(r.Context())
			if userID == uuid.Nil {
				http.Error(w, "Unauthorized", http.StatusUnauthorized)
				return
			}

			// Check permission
			hasPermission := m.manager.UserHasPermission(userID, permission)

			// Audit the check
			if m.auditor != nil {
				m.auditor.LogPermissionCheck(userID, permission, hasPermission)
			}

			if !hasPermission {
				http.Error(w, "Forbidden", http.StatusForbidden)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

// RequireRole creates middleware that requires specific role
func (m *Middleware) RequireRole(role string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			userID := GetUserID(r.Context())
			if userID == uuid.Nil {
				http.Error(w, "Unauthorized", http.StatusUnauthorized)
				return
			}

			// Check role
			hasRole := m.manager.UserHasRole(userID, role)

			if !hasRole {
				http.Error(w, "Forbidden", http.StatusForbidden)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

// RequireAnyPermission requires at least one of the specified permissions
func (m *Middleware) RequireAnyPermission(permissions ...string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			userID := GetUserID(r.Context())
			if userID == uuid.Nil {
				http.Error(w, "Unauthorized", http.StatusUnauthorized)
				return
			}

			// Check if user has any of the permissions
			hasPermission := false
			for _, perm := range permissions {
				if m.manager.UserHasPermission(userID, perm) {
					hasPermission = true
					if m.auditor != nil {
						m.auditor.LogPermissionCheck(userID, perm, true)
					}
					break
				}
			}

			if !hasPermission {
				http.Error(w, "Forbidden", http.StatusForbidden)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

// LoadUserPermissions loads user permissions into context
func (m *Middleware) LoadUserPermissions(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		userID := GetUserID(r.Context())
		if userID != uuid.Nil {
			// Get user roles
			roles := m.manager.GetUserRoles(userID)
			ctx := context.WithValue(r.Context(), UserRolesKey, roles)

			// Get effective permissions
			perms := m.manager.GetEffectivePermissions(userID)
			ctx = context.WithValue(ctx, PermissionsKey, perms)

			r = r.WithContext(ctx)
		}

		next.ServeHTTP(w, r)
	})
}

// Helper functions for context

// GetUserID extracts user ID from context
func GetUserID(ctx context.Context) uuid.UUID {
	if id, ok := ctx.Value(UserIDKey).(uuid.UUID); ok {
		return id
	}
	return uuid.Nil
}

// SetUserID sets user ID in context
func SetUserID(ctx context.Context, userID uuid.UUID) context.Context {
	return context.WithValue(ctx, UserIDKey, userID)
}

// GetUserRoles extracts user roles from context
func GetUserRoles(ctx context.Context) []string {
	if roles, ok := ctx.Value(UserRolesKey).([]string); ok {
		return roles
	}
	return []string{}
}

// GetUserPermissions extracts user permissions from context
func GetUserPermissions(ctx context.Context) PermissionSet {
	if perms, ok := ctx.Value(PermissionsKey).(PermissionSet); ok {
		return perms
	}
	return make(PermissionSet)
}
