package handlers

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestUploadHandler(t *testing.T) {
	t.Run("shows upload form", func(t *testing.T) {
		handler := NewUploadHandler(nil)
		req := httptest.NewRequest("GET", "/dashboard/upload", nil)
		w := httptest.NewRecorder()

		handler.ShowUploadForm(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("expected 200, got %d", w.Code)
		}

		body := w.Body.String()
		if !strings.Contains(body, "Upload Files") {
			t.Error("expected Upload Files title")
		}
	})

	t.Run("handles file upload", func(t *testing.T) {
		handler := NewUploadHandler(nil)
		req := httptest.NewRequest("POST", "/dashboard/upload", nil)
		w := httptest.NewRecorder()

		handler.HandleUpload(w, req)

		// Should redirect after upload
		if w.Code != http.StatusSeeOther {
			t.Errorf("expected redirect 303, got %d", w.Code)
		}
	})
}
