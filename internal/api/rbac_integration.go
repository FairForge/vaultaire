package api

import (
	"context"
	"net/http"
	"strings"

	"github.com/FairForge/vaultaire/internal/rbac"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// RBACService integrates RBAC with the API
type RBACService struct {
	manager    *rbac.TemplateManager
	auditor    *rbac.PermissionAuditor
	middleware *rbac.Middleware
	logger     *zap.Logger
}

// NewRBACService creates a new RBAC service
func NewRBACService(logger *zap.Logger) *RBACService {
	manager := rbac.NewTemplateManager()
	auditor := rbac.NewPermissionAuditor()
	middleware := rbac.NewMiddleware(manager, auditor)

	// Setup default admin for testing
	// This should be replaced with actual user management
	systemAdmin := uuid.MustParse("00000000-0000-0000-0000-000000000001")
	_ = manager.AssignRole(systemAdmin, rbac.RoleAdmin)

	// For testing: create a test user with storage permissions
	testUser := uuid.MustParse("00000000-0000-0000-0000-000000000002")
	_ = manager.AssignRole(testUser, rbac.RoleUser)

	return &RBACService{
		manager:    manager,
		auditor:    auditor,
		middleware: middleware,
		logger:     logger,
	}
}

// InjectUserContext extracts user from request and adds to context
func (rs *RBACService) InjectUserContext(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Extract user ID from Authorization header
		// For now, we'll use a simple mapping for testing
		userID := rs.getUserFromAuth(r)

		if userID != uuid.Nil {
			ctx := context.WithValue(r.Context(), rbac.UserIDKey, userID)
			r = r.WithContext(ctx)
		}

		next.ServeHTTP(w, r)
	})
}

func (rs *RBACService) getUserFromAuth(r *http.Request) uuid.UUID {
	auth := r.Header.Get("Authorization")

	// For testing purposes, map different auth tokens to users
	if strings.Contains(auth, "admin") || strings.Contains(auth, "AKIAIOSFODNN7EXAMPLE") {
		return uuid.MustParse("00000000-0000-0000-0000-000000000001") // Admin
	} else if auth != "" {
		return uuid.MustParse("00000000-0000-0000-0000-000000000002") // Regular user
	}

	// Check for X-User-ID header (for testing)
	if userID := r.Header.Get("X-User-ID"); userID != "" {
		if uid, err := uuid.Parse(userID); err == nil {
			return uid
		}
	}

	return uuid.Nil
}

// RequirePermission creates middleware that checks for a specific permission
func (rs *RBACService) RequirePermission(permission string) func(http.Handler) http.Handler {
	return rs.middleware.RequirePermission(permission)
}

// RequireRole creates middleware that checks for a specific role
func (rs *RBACService) RequireRole(role string) func(http.Handler) http.Handler {
	return rs.middleware.RequireRole(role)
}

// AddRBACToServer integrates RBAC into the server
func AddRBACToServer(s *Server) *RBACService {
	rbacSvc := NewRBACService(s.logger)

	// Add user context injection as early middleware
	s.router.Use(rbacSvc.InjectUserContext)

	// Add RBAC management endpoints
	handlers := rbac.NewRBACHandlers(rbacSvc.manager, rbacSvc.auditor)
	s.router.Route("/api/rbac", func(r chi.Router) {
		r.Get("/roles", handlers.HandleGetRoles)
		r.Get("/users/{userID}/roles", handlers.HandleGetUserRoles)
		r.With(rbacSvc.RequireRole(rbac.RoleAdmin)).
			Post("/users/{userID}/roles", handlers.HandleAssignRole)
		r.With(rbacSvc.RequireRole(rbac.RoleAdmin)).
			Delete("/users/{userID}/roles", handlers.HandleRevokeRole)
		r.Get("/permissions", handlers.HandleGetPermissions)
		r.Get("/audit", handlers.HandleGetAuditLogs)
	})

	return rbacSvc
}
