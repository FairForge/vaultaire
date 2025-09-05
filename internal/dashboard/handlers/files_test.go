package handlers

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestFilesHandler(t *testing.T) {
	t.Run("lists files in bucket", func(t *testing.T) {
		handler := NewFilesHandler(nil)
		req := httptest.NewRequest("GET", "/dashboard/files?bucket=test-bucket", nil)
		w := httptest.NewRecorder()

		handler.ListFiles(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("expected 200, got %d", w.Code)
		}

		body := w.Body.String()
		if !strings.Contains(body, "File Browser") {
			t.Error("expected File Browser title")
		}
	})

	t.Run("shows file details", func(t *testing.T) {
		handler := NewFilesHandler(nil)
		req := httptest.NewRequest("GET", "/dashboard/files/details?bucket=test&key=file.txt", nil)
		w := httptest.NewRecorder()

		handler.ShowDetails(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("expected 200, got %d", w.Code)
		}
	})
}
