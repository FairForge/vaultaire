package api

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/FairForge/vaultaire/internal/common"
)

// Define context key locally for api package

type UsageStats struct {
	TenantID      string    `json:"tenant_id"`
	StorageUsed   int64     `json:"storage_used"`
	StorageLimit  int64     `json:"storage_limit"`
	UsagePercent  float64   `json:"usage_percent"`
	BandwidthUsed int64     `json:"bandwidth_used,omitempty"`
	LastUpdated   time.Time `json:"last_updated"`
}

type UsageAlert struct {
	Level     string    `json:"level"`
	Message   string    `json:"message"`
	Threshold float64   `json:"threshold"`
	Current   float64   `json:"current"`
	Timestamp time.Time `json:"timestamp"`
}

func (s *Server) handleGetUsageStats(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := r.Context().Value(common.TenantIDKey).(string)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	used, limit, err := s.quotaManager.GetUsage(r.Context(), tenantID)
	if err != nil {
		http.Error(w, "failed to get usage", http.StatusInternalServerError)
		return
	}

	percent := float64(used) / float64(limit) * 100

	stats := UsageStats{
		TenantID:     tenantID,
		StorageUsed:  used,
		StorageLimit: limit,
		UsagePercent: percent,
		LastUpdated:  time.Now(),
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(stats); err != nil {
		http.Error(w, "failed to encode response", http.StatusInternalServerError)
	}
}

func (s *Server) handleGetUsageAlerts(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := r.Context().Value(common.TenantIDKey).(string)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	used, limit, err := s.quotaManager.GetUsage(r.Context(), tenantID)
	if err != nil {
		http.Error(w, "failed to get usage", http.StatusInternalServerError)
		return
	}

	percent := float64(used) / float64(limit) * 100
	alerts := []UsageAlert{}

	if percent >= 90 {
		alerts = append(alerts, UsageAlert{
			Level:     "CRITICAL",
			Message:   "Storage usage at 90% or above",
			Threshold: 90,
			Current:   percent,
			Timestamp: time.Now(),
		})
	} else if percent >= 80 {
		alerts = append(alerts, UsageAlert{
			Level:     "WARNING",
			Message:   "Storage usage at 80% or above",
			Threshold: 80,
			Current:   percent,
			Timestamp: time.Now(),
		})
	} else if percent >= 70 {
		alerts = append(alerts, UsageAlert{
			Level:     "INFO",
			Message:   "Storage usage at 70% or above",
			Threshold: 70,
			Current:   percent,
			Timestamp: time.Now(),
		})
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(alerts); err != nil {
		http.Error(w, "failed to encode response", http.StatusInternalServerError)
	}
}
