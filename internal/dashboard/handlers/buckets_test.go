package handlers

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBucketsHandler(t *testing.T) {
	t.Run("lists buckets", func(t *testing.T) {
		handler := NewBucketsHandler(nil)
		req := httptest.NewRequest("GET", "/dashboard/buckets", nil)
		w := httptest.NewRecorder()

		handler.ListBuckets(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("expected 200, got %d", w.Code)
		}

		body := w.Body.String()
		if !strings.Contains(body, "S3 Buckets") {
			t.Error("expected S3 Buckets title")
		}
	})

	t.Run("shows create bucket form", func(t *testing.T) {
		handler := NewBucketsHandler(nil)
		req := httptest.NewRequest("GET", "/dashboard/buckets/new", nil)
		w := httptest.NewRecorder()

		handler.ShowCreateForm(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("expected 200, got %d", w.Code)
		}

		body := w.Body.String()
		if !strings.Contains(body, "Create Bucket") {
			t.Error("expected create bucket form")
		}
	})

	t.Run("creates new bucket", func(t *testing.T) {
		handler := NewBucketsHandler(nil)
		req := httptest.NewRequest("POST", "/dashboard/buckets/create?name=test-bucket", nil)
		w := httptest.NewRecorder()

		handler.CreateBucket(w, req)

		if w.Code != http.StatusSeeOther {
			t.Errorf("expected redirect 303, got %d", w.Code)
		}
	})
}
