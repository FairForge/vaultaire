package handlers

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestActivityHandler(t *testing.T) {
	t.Run("shows activity log", func(t *testing.T) {
		handler := NewActivityHandler(nil)
		req := httptest.NewRequest("GET", "/dashboard/activity", nil)
		w := httptest.NewRecorder()

		handler.ShowActivityLog(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("expected 200, got %d", w.Code)
		}

		body := w.Body.String()
		if !strings.Contains(body, "Activity Log") {
			t.Error("expected Activity Log title")
		}
	})

	t.Run("filters activity by type", func(t *testing.T) {
		handler := NewActivityHandler(nil)
		req := httptest.NewRequest("GET", "/dashboard/activity?type=upload", nil)
		w := httptest.NewRecorder()

		handler.ShowActivityLog(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("expected 200, got %d", w.Code)
		}
	})
}
