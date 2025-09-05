package handlers

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDashboardHandler_Home(t *testing.T) {
	tests := []struct {
		name     string
		path     string
		wantCode int
		wantBody []string
	}{
		{
			name:     "serves dashboard home",
			path:     "/dashboard",
			wantCode: http.StatusOK,
			wantBody: []string{"VAULTAIRE CONTROL PANEL", "terminal"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler := NewDashboardHandler()
			req := httptest.NewRequest("GET", tt.path, nil)
			w := httptest.NewRecorder()

			handler.Home(w, req)

			if w.Code != tt.wantCode {
				t.Errorf("want status %d, got %d", tt.wantCode, w.Code)
			}

			body := w.Body.String()
			for _, want := range tt.wantBody {
				if !strings.Contains(body, want) {
					t.Errorf("want %q in response", want)
				}
			}
		})
	}
}
