package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/FairForge/vaultaire/internal/usage"
)

// Define context key locally for api package

type UsageStats struct {
	TenantID      string  `json:"tenant_id"`
	StorageUsed   int64   `json:"storage_used"`
	StorageLimit  int64   `json:"storage_limit"`
	UsagePercent  float64 `json:"usage_percent"`
	BandwidthUsed int64   `json:"bandwidth_used,omitempty"`

	// The egress allowance (WP-R10-9): the month's allowance in bytes, what
	// has been downloaded of it (the live counter — responses in flight
	// count), whether downloads are being rate-limited right now, the cap
	// that applies past the allowance and when the allowance resets (the
	// start of the next UTC month). Absent when the tenant has no quota row.
	EgressAllowance *int64     `json:"egress_allowance,omitempty"`
	EgressUsed      *int64     `json:"egress_used,omitempty"`
	EgressThrottled *bool      `json:"egress_throttled,omitempty"`
	EgressRateLimit *int64     `json:"egress_rate_limit_bytes_per_sec,omitempty"`
	EgressResetsAt  *time.Time `json:"egress_resets_at,omitempty"`

	LastUpdated time.Time `json:"last_updated"`
}

type UsageAlert struct {
	Level     string    `json:"level"`
	Message   string    `json:"message"`
	Threshold float64   `json:"threshold"`
	Current   float64   `json:"current"`
	Timestamp time.Time `json:"timestamp"`
}

// egressStatus is the tenant's egress position from the live month counter
// (or, on a server built without one, from the database).
func (s *Server) egressStatus(ctx context.Context, tenantID string) (usage.EgressStatus, error) {
	if s.egress != nil {
		return s.egress.EgressStatus(ctx, tenantID)
	}
	if s.db == nil {
		return usage.EgressStatus{}, errors.New("egress status: no database")
	}
	return usage.EgressStatusFromDB(ctx, s.db, tenantID, time.Now())
}

func (s *Server) handleGetUsageStats(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := r.Context().Value(tenantIDKey).(string)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	used, limit, err := s.quotaManager.GetUsage(r.Context(), tenantID)
	if err != nil {
		http.Error(w, "failed to get usage", http.StatusInternalServerError)
		return
	}

	percent := 0.0
	if limit > 0 {
		percent = float64(used) / float64(limit) * 100
	}

	stats := UsageStats{
		TenantID:     tenantID,
		StorageUsed:  used,
		StorageLimit: limit,
		UsagePercent: percent,
		LastUpdated:  time.Now(),
	}
	if st, err := s.egressStatus(r.Context(), tenantID); err == nil {
		resets := st.ResetAt.UTC()
		stats.EgressAllowance = &st.Allowance.Bytes
		stats.EgressUsed = &st.UsedBytes
		stats.EgressThrottled = &st.Throttled
		stats.EgressRateLimit = &st.RateBytesPerSec
		stats.EgressResetsAt = &resets
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(stats); err != nil {
		http.Error(w, "failed to encode response", http.StatusInternalServerError)
	}
}

func (s *Server) handleGetUsageAlerts(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := r.Context().Value(tenantIDKey).(string)
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
