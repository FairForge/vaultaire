package handlers

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAPIKeysHandler(t *testing.T) {
	t.Run("lists API keys", func(t *testing.T) {
		handler := NewAPIKeysHandler(nil)
		req := httptest.NewRequest("GET", "/dashboard/apikeys", nil)
		w := httptest.NewRecorder()

		handler.ListKeys(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("expected 200, got %d", w.Code)
		}

		body := w.Body.String()
		if !strings.Contains(body, "API Keys") {
			t.Error("expected API Keys title")
		}
	})

	t.Run("generates new API key", func(t *testing.T) {
		handler := NewAPIKeysHandler(nil)
		req := httptest.NewRequest("POST", "/dashboard/apikeys/generate", nil)
		w := httptest.NewRecorder()

		handler.GenerateKey(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("expected 200, got %d", w.Code)
		}

		body := w.Body.String()
		// Should show a newly generated key
		if !strings.Contains(body, "vlt_") {
			t.Error("expected generated key prefix")
		}
	})

	t.Run("revokes API key", func(t *testing.T) {
		handler := NewAPIKeysHandler(nil)
		req := httptest.NewRequest("POST", "/dashboard/apikeys/revoke?key=vlt_test123", nil)
		w := httptest.NewRecorder()

		handler.RevokeKey(w, req)

		if w.Code != http.StatusSeeOther {
			t.Errorf("expected redirect 303, got %d", w.Code)
		}
	})
}
