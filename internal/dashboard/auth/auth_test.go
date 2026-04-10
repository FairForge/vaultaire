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

	t.Run("persists ip and user agent", func(t *testing.T) {
		store := NewMemoryStore()
		ctx := context.Background()

		token, err := store.Create(ctx, SessionData{
			UserID:    "user123",
			IPAddress: "203.0.113.5",
			UserAgent: "Mozilla/5.0 (Macintosh; Intel Mac OS X) test",
		}, time.Hour)
		if err != nil {
			t.Fatal(err)
		}

		sd, err := store.Get(ctx, token)
		if err != nil || sd == nil {
			t.Fatalf("expected session, got %+v err=%v", sd, err)
		}
		if sd.IPAddress != "203.0.113.5" {
			t.Errorf("expected IPAddress set, got %q", sd.IPAddress)
		}
		if sd.UserAgent == "" {
			t.Errorf("expected UserAgent set, got empty")
		}
	})

	t.Run("list by user id returns all active sessions newest first", func(t *testing.T) {
		store := NewMemoryStore()
		ctx := context.Background()

		// Two sessions for the same user, one for a different user.
		t1, _ := store.Create(ctx, SessionData{
			UserID: "u1", IPAddress: "10.0.0.1", UserAgent: "deviceA",
		}, time.Hour)
		// Nudge a tiny amount so Get updates last_active in a comparable way.
		time.Sleep(2 * time.Millisecond)
		t2, _ := store.Create(ctx, SessionData{
			UserID: "u1", IPAddress: "10.0.0.2", UserAgent: "deviceB",
		}, time.Hour)
		_, _ = store.Create(ctx, SessionData{UserID: "u2"}, time.Hour)

		list, err := store.ListByUserID(ctx, "u1")
		if err != nil {
			t.Fatal(err)
		}
		if len(list) != 2 {
			t.Fatalf("expected 2 sessions, got %d", len(list))
		}
		ids := map[string]bool{list[0].ID: true, list[1].ID: true}
		if !ids[t1] || !ids[t2] {
			t.Errorf("expected both tokens in list, got %+v", list)
		}
		// Newest first — the second-created session should sort ahead.
		if list[0].ID != t2 {
			t.Errorf("expected newest first (%s), got %s", t2, list[0].ID)
		}
	})

	t.Run("delete by user id except keeps current session", func(t *testing.T) {
		store := NewMemoryStore()
		ctx := context.Background()

		keep, _ := store.Create(ctx, SessionData{UserID: "u1"}, time.Hour)
		gone1, _ := store.Create(ctx, SessionData{UserID: "u1"}, time.Hour)
		gone2, _ := store.Create(ctx, SessionData{UserID: "u1"}, time.Hour)
		otherUser, _ := store.Create(ctx, SessionData{UserID: "u2"}, time.Hour)

		if err := store.DeleteByUserIDExcept(ctx, "u1", keep); err != nil {
			t.Fatal(err)
		}

		if sd, _ := store.Get(ctx, keep); sd == nil {
			t.Error("expected kept session to survive")
		}
		if sd, _ := store.Get(ctx, gone1); sd != nil {
			t.Error("expected other session 1 to be deleted")
		}
		if sd, _ := store.Get(ctx, gone2); sd != nil {
			t.Error("expected other session 2 to be deleted")
		}
		if sd, _ := store.Get(ctx, otherUser); sd == nil {
			t.Error("expected other user's session to survive")
		}
	})

	t.Run("get refreshes last_active_at", func(t *testing.T) {
		store := NewMemoryStore()
		ctx := context.Background()

		token, _ := store.Create(ctx, SessionData{UserID: "u1"}, time.Hour)
		list, _ := store.ListByUserID(ctx, "u1")
		if len(list) != 1 {
			t.Fatalf("expected 1 session, got %d", len(list))
		}
		firstActive := list[0].LastActiveAt

		time.Sleep(5 * time.Millisecond)
		if _, err := store.Get(ctx, token); err != nil {
			t.Fatal(err)
		}

		list, _ = store.ListByUserID(ctx, "u1")
		if !list[0].LastActiveAt.After(firstActive) {
			t.Errorf("expected LastActiveAt to advance after Get, was %v now %v",
				firstActive, list[0].LastActiveAt)
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
