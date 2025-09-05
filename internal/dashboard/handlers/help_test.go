package handlers

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHelpHandler(t *testing.T) {
	t.Run("shows help documentation", func(t *testing.T) {
		handler := NewHelpHandler()
		req := httptest.NewRequest("GET", "/dashboard/help", nil)
		w := httptest.NewRecorder()

		handler.ShowHelp(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("expected 200, got %d", w.Code)
		}

		body := w.Body.String()
		if !strings.Contains(body, "Help & Documentation") {
			t.Error("expected Help & Documentation title")
		}
		if !strings.Contains(body, "S3 API") {
			t.Error("expected S3 API documentation")
		}
	})
}
