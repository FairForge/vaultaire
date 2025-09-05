package handlers

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSettingsHandler(t *testing.T) {
	t.Run("shows settings page", func(t *testing.T) {
		handler := NewSettingsHandler(nil)
		req := httptest.NewRequest("GET", "/dashboard/settings", nil)
		w := httptest.NewRecorder()

		handler.ShowSettings(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("expected 200, got %d", w.Code)
		}

		body := w.Body.String()
		if !strings.Contains(body, "Settings") {
			t.Error("expected Settings title")
		}
	})

	t.Run("updates settings", func(t *testing.T) {
		handler := NewSettingsHandler(nil)
		req := httptest.NewRequest("POST", "/dashboard/settings", nil)
		w := httptest.NewRecorder()

		handler.UpdateSettings(w, req)

		if w.Code != http.StatusSeeOther {
			t.Errorf("expected redirect 303, got %d", w.Code)
		}
	})
}
