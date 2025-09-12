// internal/api/quota_management.go
package api

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/gorilla/mux"
	"go.uber.org/zap"
)

type QuotaRequest struct {
	TenantID       string `json:"tenant_id"`
	Plan           string `json:"plan"`
	StorageLimit   int64  `json:"storage_limit"`
	BandwidthLimit int64  `json:"bandwidth_limit,omitempty"`
}

type QuotaUpdateRequest struct {
	StorageLimit   int64 `json:"storage_limit,omitempty"`
	BandwidthLimit int64 `json:"bandwidth_limit,omitempty"`
}

type QuotaResponse struct {
	TenantID       string `json:"tenant_id"`
	Plan           string `json:"plan"`
	StorageLimit   int64  `json:"storage_limit"`
	StorageUsed    int64  `json:"storage_used"`
	BandwidthLimit int64  `json:"bandwidth_limit,omitempty"`
	BandwidthUsed  int64  `json:"bandwidth_used,omitempty"`
}

// Middleware to check admin privileges
func (s *Server) requireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		isAdmin, ok := r.Context().Value("is_admin").(bool)
		if !ok || !isAdmin {
			http.Error(w, "admin access required", http.StatusForbidden)
			return
		}
		next(w, r)
	}
}

func (s *Server) handleCreateQuota(w http.ResponseWriter, r *http.Request) {
	var req QuotaRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}

	// Create quota in database
	err := s.quotaManager.CreateTenant(
		r.Context(), req.TenantID, req.Plan, req.StorageLimit)
	if err != nil {
		http.Error(w, "failed to create quota", http.StatusInternalServerError)
		return
	}

	response := QuotaResponse{
		TenantID:     req.TenantID,
		Plan:         req.Plan,
		StorageLimit: req.StorageLimit,
		StorageUsed:  0,
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	if err := json.NewEncoder(w).Encode(response); err != nil {
		http.Error(w, "failed to encode response", http.StatusInternalServerError)
	}
}

func (s *Server) handleUpdateQuota(w http.ResponseWriter, r *http.Request) {
	// Extract tenant ID from URL
	parts := strings.Split(r.URL.Path, "/")
	if len(parts) < 5 {
		http.Error(w, "invalid URL", http.StatusBadRequest)
		return
	}
	tenantID := parts[len(parts)-1]

	var req QuotaUpdateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}

	// Update quota
	err := s.quotaManager.UpdateQuota(r.Context(), tenantID, req.StorageLimit)
	if err != nil {
		// Log the actual error for debugging
		s.logger.Error("failed to update quota",
			zap.String("tenant_id", tenantID),
			zap.Error(err))
		http.Error(w, "failed to update quota: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"updated"}`))
}

func (s *Server) handleListQuotas(w http.ResponseWriter, r *http.Request) {
	// Get all quotas from database
	quotas, err := s.quotaManager.ListQuotas(r.Context())
	if err != nil {
		http.Error(w, "failed to list quotas", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(quotas); err != nil {
		http.Error(w, "failed to encode response", http.StatusInternalServerError)
	}
}

func (s *Server) handleDeleteQuota(w http.ResponseWriter, r *http.Request) {
	// Extract tenant ID from URL
	vars := mux.Vars(r)
	tenantID := vars["tenant_id"]

	err := s.quotaManager.DeleteQuota(r.Context(), tenantID)
	if err != nil {
		http.Error(w, "failed to delete quota", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// Add these routes to setupRoutes() in server.go
func (s *Server) setupQuotaManagementRoutes() {
	// Admin-only quota management endpoints
	// 	s.router.Get("/api/v1/admin/quotas", s.requireAdmin(s.handleListQuotas))
	// 	s.router.Post("/api/v1/admin/quotas", s.requireAdmin(s.handleCreateQuota))
	// 	s.router.Put("/api/v1/admin/quotas/{tenant_id}", s.requireAdmin(s.handleUpdateQuota))
	// 	s.router.Delete("/api/v1/admin/quotas/{tenant_id}", s.requireAdmin(s.handleDeleteQuota))
}
