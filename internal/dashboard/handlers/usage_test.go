package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type UsageStats struct {
	StorageGB   float64 `json:"storage_gb"`
	BandwidthGB float64 `json:"bandwidth_gb"`
	Requests    int64   `json:"requests"`
	Cost        float64 `json:"cost"`
	Period      string  `json:"period"`
}

func TestUsageHandler(t *testing.T) {
	t.Run("returns usage stats as JSON", func(t *testing.T) {
		handler := NewUsageHandler(nil) // Will pass mock later
		req := httptest.NewRequest("GET", "/api/usage", nil)
		w := httptest.NewRecorder()

		handler.GetUsage(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("expected 200, got %d", w.Code)
		}

		var stats UsageStats
		if err := json.NewDecoder(w.Body).Decode(&stats); err != nil {
			t.Fatal("failed to decode response:", err)
		}

		if stats.StorageGB < 0 {
			t.Error("storage should not be negative")
		}
		if stats.Cost < 0 {
			t.Error("cost should not be negative")
		}
	})

	t.Run("renders usage dashboard HTML", func(t *testing.T) {
		handler := NewUsageHandler(nil)
		req := httptest.NewRequest("GET", "/dashboard/usage", nil)
		w := httptest.NewRecorder()

		handler.UsageDashboard(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("expected 200, got %d", w.Code)
		}

		body := w.Body.String()
		if !strings.Contains(body, "Usage Statistics") {
			t.Error("expected usage statistics in response")
		}
	})
}

func TestBillingHandler(t *testing.T) {
	t.Run("displays current billing information", func(t *testing.T) {
		handler := NewBillingHandler(nil)
		req := httptest.NewRequest("GET", "/dashboard/billing", nil)
		w := httptest.NewRecorder()

		handler.ShowBilling(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("expected 200, got %d", w.Code)
		}

		body := w.Body.String()
		if !strings.Contains(body, "$3.99/TB") {
			t.Error("expected pricing information")
		}
	})
}
