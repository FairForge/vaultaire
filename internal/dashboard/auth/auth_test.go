package auth

import (
	"context"
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

func TestMemoryStore(t *testing.T) {
	t.Run("creates and retrieves session", func(t *testing.T) {
		store := NewMemoryStore()
		ctx := context.Background()

		token, err := store.Create(ctx, SessionData{
			UserID: "user123", TenantID: "t1", Email: "a@b.com", Role: "user",
		}, 24*time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		if token == "" {
			t.Fatal("expected non-empty token")
		}

		sd, err := store.Get(ctx, token)
		if err != nil {
			t.Fatal(err)
		}
		if sd == nil || sd.UserID != "user123" {
			t.Errorf("expected user123, got %+v", sd)
		}
	})

	t.Run("expires old sessions", func(t *testing.T) {
		store := NewMemoryStore()
		ctx := context.Background()

		token, _ := store.Create(ctx, SessionData{UserID: "user123"}, 1*time.Millisecond)
		time.Sleep(2 * time.Millisecond)

		sd, err := store.Get(ctx, token)
		if err != nil {
			t.Fatal(err)
		}
		if sd != nil {
			t.Error("expected expired session to return nil")
		}
	})

	t.Run("deletes session", func(t *testing.T) {
		store := NewMemoryStore()
		ctx := context.Background()

		token, _ := store.Create(ctx, SessionData{UserID: "user123"}, time.Hour)
		_ = store.Delete(ctx, token)

		sd, _ := store.Get(ctx, token)
		if sd != nil {
			t.Error("expected deleted session to return nil")
		}
	})
}

func TestRequireSession(t *testing.T) {
	t.Run("redirects when not authenticated", func(t *testing.T) {
		store := NewMemoryStore()
		middleware := RequireSession(store)

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

	t.Run("passes authenticated request with context", func(t *testing.T) {
		store := NewMemoryStore()
		ctx := context.Background()
		token, _ := store.Create(ctx, SessionData{
			UserID: "u1", TenantID: "t1", Email: "a@b.com", Role: "user",
		}, time.Hour)

		var got *SessionData
		middleware := RequireSession(store)
		handler := middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			got = GetSession(r.Context())
			w.WriteHeader(http.StatusOK)
		}))

		req := httptest.NewRequest("GET", "/dashboard", nil)
		req.AddCookie(&http.Cookie{Name: cookieName, Value: token})
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", w.Code)
		}
		if got == nil || got.UserID != "u1" {
			t.Errorf("expected session with user u1, got %+v", got)
		}
	})
}

func TestRequireAdmin(t *testing.T) {
	t.Run("blocks non-admin", func(t *testing.T) {
		store := NewMemoryStore()
		ctx := context.Background()
		token, _ := store.Create(ctx, SessionData{
			UserID: "u1", Role: "user",
		}, time.Hour)

		handler := RequireAdmin(store)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))

		req := httptest.NewRequest("GET", "/admin", nil)
		req.AddCookie(&http.Cookie{Name: cookieName, Value: token})
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)

		if w.Code != http.StatusForbidden {
			t.Errorf("expected 403, got %d", w.Code)
		}
	})

	t.Run("allows admin", func(t *testing.T) {
		store := NewMemoryStore()
		ctx := context.Background()
		token, _ := store.Create(ctx, SessionData{
			UserID: "u1", Role: "admin",
		}, time.Hour)

		handler := RequireAdmin(store)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))

		req := httptest.NewRequest("GET", "/admin", nil)
		req.AddCookie(&http.Cookie{Name: cookieName, Value: token})
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("expected 200, got %d", w.Code)
		}
	})
}
