package rbac

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRBACMiddleware(t *testing.T) {
	t.Run("require permission", func(t *testing.T) {
		manager := NewTemplateManager()
		auditor := NewPermissionAuditor()
		middleware := NewMiddleware(manager, auditor)

		userID := uuid.New()

		// Assign user role
		err := manager.AssignRole(userID, RoleUser)
		require.NoError(t, err)

		// Create test handler
		handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		})

		// Wrap with permission middleware
		protected := middleware.RequirePermission("storage.read")(handler)

		// Test with authenticated user
		req := httptest.NewRequest("GET", "/test", nil)
		ctx := SetUserID(context.Background(), userID)
		req = req.WithContext(ctx)

		w := httptest.NewRecorder()
		protected.ServeHTTP(w, req)

		assert.Equal(t, http.StatusOK, w.Code)

		// Check audit log
		logs := auditor.GetUserAuditLogs(userID, 1)
		assert.Len(t, logs, 1)
		assert.Equal(t, "storage.read", logs[0].Permission)
	})

	t.Run("deny without permission", func(t *testing.T) {
		manager := NewTemplateManager()
		middleware := NewMiddleware(manager, nil)

		userID := uuid.New()

		// Assign guest role (no storage.write)
		err := manager.AssignRole(userID, RoleGuest)
		require.NoError(t, err)

		handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		})

		protected := middleware.RequirePermission("storage.write")(handler)

		req := httptest.NewRequest("POST", "/test", nil)
		ctx := SetUserID(context.Background(), userID)
		req = req.WithContext(ctx)

		w := httptest.NewRecorder()
		protected.ServeHTTP(w, req)

		assert.Equal(t, http.StatusForbidden, w.Code)
	})

	t.Run("require any permission", func(t *testing.T) {
		manager := NewTemplateManager()
		middleware := NewMiddleware(manager, nil)

		userID := uuid.New()
		err := manager.AssignRole(userID, RoleViewer)
		require.NoError(t, err)

		handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		})

		// Require either read or write
		protected := middleware.RequireAnyPermission("storage.write", "storage.read")(handler)

		req := httptest.NewRequest("GET", "/test", nil)
		ctx := SetUserID(context.Background(), userID)
		req = req.WithContext(ctx)

		w := httptest.NewRecorder()
		protected.ServeHTTP(w, req)

		// Viewer has read, so should pass
		assert.Equal(t, http.StatusOK, w.Code)
	})
}
