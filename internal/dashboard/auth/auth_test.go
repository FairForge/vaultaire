package auth

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestBasicAuth(t *testing.T) {
	t.Run("validates correct credentials", func(t *testing.T) {
		auth := NewBasicAuth("admin", "password123")
		req := httptest.NewRequest("GET", "/", nil)
		req.SetBasicAuth("admin", "password123")

		if !auth.Validate(req) {
			t.Error("expected valid credentials to pass")
		}
	})

	t.Run("rejects incorrect credentials", func(t *testing.T) {
		auth := NewBasicAuth("admin", "password123")
		req := httptest.NewRequest("GET", "/", nil)
		req.SetBasicAuth("admin", "wrongpass")

		if auth.Validate(req) {
			t.Error("expected invalid credentials to fail")
		}
	})
}

func TestSessionManager(t *testing.T) {
	t.Run("creates and validates session", func(t *testing.T) {
		sm := NewSessionManager("secret-key", 24*time.Hour)

		// Create session
		sessionID := sm.CreateSession("user123")
		if sessionID == "" {
			t.Fatal("expected session ID")
		}

		// Validate session
		userID := sm.GetSession(sessionID)
		if userID != "user123" {
			t.Errorf("expected user123, got %s", userID)
		}
	})

	t.Run("expires old sessions", func(t *testing.T) {
		sm := NewSessionManager("secret-key", 1*time.Millisecond)
		sessionID := sm.CreateSession("user123")

		time.Sleep(2 * time.Millisecond)

		userID := sm.GetSession(sessionID)
		if userID != "" {
			t.Error("expected expired session to return empty")
		}
	})
}

func TestAuthMiddleware(t *testing.T) {
	t.Run("redirects to login when not authenticated", func(t *testing.T) {
		sm := NewSessionManager("secret", time.Hour)
		middleware := AuthMiddleware(sm)

		handler := middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte("protected"))
		}))

		req := httptest.NewRequest("GET", "/dashboard", nil)
		w := httptest.NewRecorder()

		handler.ServeHTTP(w, req)

		if w.Code != http.StatusSeeOther {
			t.Errorf("expected redirect, got %d", w.Code)
		}
		if loc := w.Header().Get("Location"); !strings.Contains(loc, "/login") {
			t.Errorf("expected redirect to login, got %s", loc)
		}
	})
}
