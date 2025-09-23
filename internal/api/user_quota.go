package api

import (
	"encoding/json"
	"net/http"

	"github.com/FairForge/vaultaire/internal/common"
)

// QuotaInfo represents user quota information
type QuotaInfo struct {
	TenantID     string  `json:"tenant_id"`
	StorageUsed  int64   `json:"storage_used_bytes"`
	StorageLimit int64   `json:"storage_limit_bytes"`
	Percentage   float64 `json:"usage_percentage"`
	Tier         string  `json:"tier"`
	CanUpgrade   bool    `json:"can_upgrade"`
}

// handleGetQuota returns the current quota for a user
func (s *Server) handleGetQuota(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := r.Context().Value(common.TenantIDKey).(string)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	used, limit, err := s.quotaManager.GetUsage(r.Context(), tenantID)
	if err != nil {
		http.Error(w, "failed to get quota", http.StatusInternalServerError)
		return
	}

	tier, _ := s.quotaManager.GetTier(r.Context(), tenantID)

	quota := QuotaInfo{
		TenantID:     tenantID,
		StorageUsed:  used,
		StorageLimit: limit,
		Percentage:   float64(used) / float64(limit) * 100,
		Tier:         tier,
		CanUpgrade:   tier != "enterprise",
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(quota)
}

// handleUpgradeQuota upgrades a user's quota tier
func (s *Server) handleUpgradeQuota(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := r.Context().Value(common.TenantIDKey).(string)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	var req struct {
		NewTier string `json:"new_tier"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}

	// Update the tier
	err := s.quotaManager.UpdateTier(r.Context(), tenantID, req.NewTier)
	if err != nil {
		http.Error(w, "failed to upgrade", http.StatusInternalServerError)
		return
	}

	// Return updated quota
	s.handleGetQuota(w, r)
}

// handleGetQuotaHistory returns usage history
func (s *Server) handleGetQuotaHistory(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := r.Context().Value(common.TenantIDKey).(string)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	// Get last 30 days of usage
	history, err := s.quotaManager.GetUsageHistory(r.Context(), tenantID, 30)
	if err != nil {
		http.Error(w, "failed to get history", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(history)
}
