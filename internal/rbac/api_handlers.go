package rbac

import (
	"encoding/json"
	"net/http"

	"github.com/google/uuid"
)

// RBACHandlers provides HTTP handlers for RBAC management
type RBACHandlers struct {
	manager *TemplateManager
	auditor *PermissionAuditor
}

// NewRBACHandlers creates new RBAC handlers
func NewRBACHandlers(manager *TemplateManager, auditor *PermissionAuditor) *RBACHandlers {
	return &RBACHandlers{
		manager: manager,
		auditor: auditor,
	}
}

// HandleGetRoles returns all available roles
func (h *RBACHandlers) HandleGetRoles(w http.ResponseWriter, r *http.Request) {
	roles := []map[string]interface{}{
		{"name": RoleAdmin, "display": "Administrator", "priority": 100},
		{"name": RoleUser, "display": "User", "priority": 50},
		{"name": RoleViewer, "display": "Viewer", "priority": 20},
		{"name": RoleGuest, "display": "Guest", "priority": 10},
	}

	// Add custom roles
	templates := h.manager.ListTemplates()
	for _, tmpl := range templates {
		roles = append(roles, map[string]interface{}{
			"name":     tmpl.Name,
			"display":  tmpl.DisplayName,
			"priority": 30,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(roles); err != nil {
		http.Error(w, "Failed to encode response", http.StatusInternalServerError)
	}
}

// HandleGetUserRoles returns roles for a specific user
func (h *RBACHandlers) HandleGetUserRoles(w http.ResponseWriter, r *http.Request) {
	userID, err := uuid.Parse(r.PathValue("userID"))
	if err != nil {
		http.Error(w, "Invalid user ID", http.StatusBadRequest)
		return
	}

	roles := h.manager.GetUserRoles(userID)

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]interface{}{
		"user_id": userID,
		"roles":   roles,
	}); err != nil {
		http.Error(w, "Failed to encode response", http.StatusInternalServerError)
	}
}

// HandleAssignRole assigns a role to a user
func (h *RBACHandlers) HandleAssignRole(w http.ResponseWriter, r *http.Request) {
	userID, err := uuid.Parse(r.PathValue("userID"))
	if err != nil {
		http.Error(w, "Invalid user ID", http.StatusBadRequest)
		return
	}

	var req struct {
		Role string `json:"role"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request", http.StatusBadRequest)
		return
	}

	// Assign the role
	if err := h.manager.AssignRole(userID, req.Role); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Audit the action
	adminID := GetUserID(r.Context())
	h.auditor.LogRoleAssignment(userID, adminID, req.Role, "assigned")

	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(map[string]string{"status": "success"}); err != nil {
		// Log error but response is already sent
		_ = err
	}
}

// HandleRevokeRole revokes a role from a user
func (h *RBACHandlers) HandleRevokeRole(w http.ResponseWriter, r *http.Request) {
	userID, err := uuid.Parse(r.PathValue("userID"))
	if err != nil {
		http.Error(w, "Invalid user ID", http.StatusBadRequest)
		return
	}

	var req struct {
		Role string `json:"role"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request", http.StatusBadRequest)
		return
	}

	// Revoke the role
	if err := h.manager.RevokeRole(userID, req.Role); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Audit the action
	adminID := GetUserID(r.Context())
	h.auditor.LogRoleAssignment(userID, adminID, req.Role, "revoked")

	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(map[string]string{"status": "success"}); err != nil {
		// Log error but response is already sent
		_ = err
	}
}

// HandleGetPermissions returns all permissions for a role
func (h *RBACHandlers) HandleGetPermissions(w http.ResponseWriter, r *http.Request) {
	role := r.URL.Query().Get("role")
	if role == "" {
		http.Error(w, "Role parameter required", http.StatusBadRequest)
		return
	}

	perms := h.manager.GetRolePermissions(role)

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]interface{}{
		"role":        role,
		"permissions": perms,
	}); err != nil {
		http.Error(w, "Failed to encode response", http.StatusInternalServerError)
	}
}

// HandleGetAuditLogs returns audit logs
func (h *RBACHandlers) HandleGetAuditLogs(w http.ResponseWriter, r *http.Request) {
	query := AuditQuery{
		Limit: 100,
	}

	// Parse query parameters
	if userParam := r.URL.Query().Get("user_id"); userParam != "" {
		if uid, err := uuid.Parse(userParam); err == nil {
			query.UserID = &uid
		}
	}

	if roleParam := r.URL.Query().Get("role"); roleParam != "" {
		query.Role = &roleParam
	}

	if actionParam := r.URL.Query().Get("action"); actionParam != "" {
		query.Action = &actionParam
	}

	logs := h.auditor.QueryAuditLogs(query)

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(logs); err != nil {
		http.Error(w, "Failed to encode response", http.StatusInternalServerError)
	}
}

// RegisterRoutes registers all RBAC routes
func (h *RBACHandlers) RegisterRoutes(mux *http.ServeMux, middleware *Middleware) {
	// Public endpoints (still require authentication)
	mux.HandleFunc("GET /api/rbac/roles", h.HandleGetRoles)
	mux.HandleFunc("GET /api/rbac/users/{userID}/roles", h.HandleGetUserRoles)

	// Admin endpoints
	mux.Handle("POST /api/rbac/users/{userID}/roles",
		middleware.RequireRole(RoleAdmin)(http.HandlerFunc(h.HandleAssignRole)))
	mux.Handle("DELETE /api/rbac/users/{userID}/roles",
		middleware.RequireRole(RoleAdmin)(http.HandlerFunc(h.HandleRevokeRole)))
	mux.Handle("GET /api/rbac/permissions",
		middleware.RequirePermission("admin.users")(http.HandlerFunc(h.HandleGetPermissions)))
	mux.Handle("GET /api/rbac/audit",
		middleware.RequirePermission("audit.view")(http.HandlerFunc(h.HandleGetAuditLogs)))
}
