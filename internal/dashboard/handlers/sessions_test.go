package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	dashauth "github.com/FairForge/vaultaire/internal/dashboard/auth"
	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// sessionCtxWithToken builds a request context carrying the session for
// a given token from an existing MemoryStore. It mirrors what
// RequireSession middleware would do in production.
func sessionCtxWithToken(t *testing.T, store *dashauth.MemoryStore, token string) context.Context {
	t.Helper()
	sd, err := store.Get(context.Background(), token)
	require.NoError(t, err)
	require.NotNil(t, sd, "token must be valid in store")
	return context.WithValue(context.Background(), dashauth.SessionKey, sd)
}

func TestBuildSessionRows_MarksCurrentDevice(t *testing.T) {
	now := time.Now()
	rows := buildSessionRows([]dashauth.SessionInfo{
		{
			ID: "tok-a", UserID: "u1", IPAddress: "10.0.0.1",
			UserAgent: "Mozilla/5.0 (Macintosh; Intel Mac OS X) Chrome/124.0",
			CreatedAt: now.Add(-48 * time.Hour), LastActiveAt: now.Add(-1 * time.Minute),
		},
		{
			ID: "tok-b", UserID: "u1", IPAddress: "192.168.1.50",
			UserAgent: "Mozilla/5.0 (iPhone) Safari/604.1",
			CreatedAt: now.Add(-7 * 24 * time.Hour), LastActiveAt: now.Add(-3 * time.Hour),
		},
	}, "tok-a")

	require.Len(t, rows, 2)
	assert.True(t, rows[0].IsCurrent, "tok-a should be current")
	assert.False(t, rows[1].IsCurrent)
	assert.Contains(t, rows[0].Device, "Chrome")
	assert.Contains(t, rows[0].Device, "macOS")
	assert.Contains(t, rows[1].Device, "Safari")
	assert.Contains(t, rows[1].Device, "iOS")
	assert.Equal(t, "10.0.0.1", rows[0].IPAddress)
}

func TestBuildSessionRows_UnknownUA(t *testing.T) {
	rows := buildSessionRows([]dashauth.SessionInfo{
		{ID: "x", UserID: "u1", IPAddress: "", UserAgent: "", CreatedAt: time.Now(), LastActiveAt: time.Now()},
	}, "")
	require.Len(t, rows, 1)
	assert.Equal(t, "unknown", rows[0].IPAddress)
	assert.Equal(t, "Unknown device", rows[0].Device)
}

func TestHandleRevokeSession_DeletesOwnedSession(t *testing.T) {
	store := dashauth.NewMemoryStore()
	ctx := context.Background()

	keep, _ := store.Create(ctx, dashauth.SessionData{UserID: "u1", Email: "a@b.com"}, time.Hour)
	target, _ := store.Create(ctx, dashauth.SessionData{UserID: "u1", Email: "a@b.com"}, time.Hour)

	handler := HandleRevokeSession(store, zap.NewNop())

	// chi routes the {id} URL param via RouteContext.
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", target)

	req := httptest.NewRequest("POST", "/dashboard/settings/sessions/"+target+"/revoke", nil)
	req.AddCookie(&http.Cookie{Name: dashauth.SessionCookieName, Value: keep})
	req = req.WithContext(context.WithValue(sessionCtxWithToken(t, store, keep), chi.RouteCtxKey, rctx))

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusSeeOther, w.Code)
	assert.Equal(t, "/dashboard/settings", w.Header().Get("Location"))

	if sd, _ := store.Get(ctx, target); sd != nil {
		t.Error("expected target session deleted")
	}
	if sd, _ := store.Get(ctx, keep); sd == nil {
		t.Error("expected current session to survive")
	}
}

func TestHandleRevokeSession_RefusesOtherUser(t *testing.T) {
	// Attempting to revoke another user's session must silently no-op
	// — i.e., the target token must still exist after the request.
	store := dashauth.NewMemoryStore()
	ctx := context.Background()

	myToken, _ := store.Create(ctx, dashauth.SessionData{UserID: "u1"}, time.Hour)
	victimToken, _ := store.Create(ctx, dashauth.SessionData{UserID: "attacker-target"}, time.Hour)

	handler := HandleRevokeSession(store, zap.NewNop())

	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", victimToken)

	req := httptest.NewRequest("POST", "/dashboard/settings/sessions/"+victimToken+"/revoke", nil)
	req.AddCookie(&http.Cookie{Name: dashauth.SessionCookieName, Value: myToken})
	req = req.WithContext(context.WithValue(sessionCtxWithToken(t, store, myToken), chi.RouteCtxKey, rctx))

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusSeeOther, w.Code)
	if sd, _ := store.Get(ctx, victimToken); sd == nil {
		t.Error("victim session must NOT be deleted — cross-user revoke attempted")
	}
}

func TestHandleRevokeSession_RefusesSelf(t *testing.T) {
	// Revoking the issuing session would immediately log the user out
	// mid-request — disallowed; they should use /logout instead.
	store := dashauth.NewMemoryStore()
	ctx := context.Background()

	myToken, _ := store.Create(ctx, dashauth.SessionData{UserID: "u1"}, time.Hour)

	handler := HandleRevokeSession(store, zap.NewNop())

	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", myToken)

	req := httptest.NewRequest("POST", "/dashboard/settings/sessions/"+myToken+"/revoke", nil)
	req.AddCookie(&http.Cookie{Name: dashauth.SessionCookieName, Value: myToken})
	req = req.WithContext(context.WithValue(sessionCtxWithToken(t, store, myToken), chi.RouteCtxKey, rctx))

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusSeeOther, w.Code)
	if sd, _ := store.Get(ctx, myToken); sd == nil {
		t.Error("current session must NOT be revoked via session-revoke handler")
	}
}

func TestHandleRevokeAllOtherSessions(t *testing.T) {
	store := dashauth.NewMemoryStore()
	ctx := context.Background()

	keep, _ := store.Create(ctx, dashauth.SessionData{UserID: "u1"}, time.Hour)
	a, _ := store.Create(ctx, dashauth.SessionData{UserID: "u1"}, time.Hour)
	b, _ := store.Create(ctx, dashauth.SessionData{UserID: "u1"}, time.Hour)
	// Another user — must not be affected.
	other, _ := store.Create(ctx, dashauth.SessionData{UserID: "u2"}, time.Hour)

	handler := HandleRevokeAllOtherSessions(store, zap.NewNop())

	req := httptest.NewRequest("POST", "/dashboard/settings/sessions/revoke-all", nil)
	req.AddCookie(&http.Cookie{Name: dashauth.SessionCookieName, Value: keep})
	req = req.WithContext(sessionCtxWithToken(t, store, keep))

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusSeeOther, w.Code)
	assert.Equal(t, "/dashboard/settings", w.Header().Get("Location"))

	if sd, _ := store.Get(ctx, keep); sd == nil {
		t.Error("expected current session to survive")
	}
	if sd, _ := store.Get(ctx, a); sd != nil {
		t.Error("expected other session A to be revoked")
	}
	if sd, _ := store.Get(ctx, b); sd != nil {
		t.Error("expected other session B to be revoked")
	}
	if sd, _ := store.Get(ctx, other); sd == nil {
		t.Error("expected other user's session to survive")
	}
}

func TestRelativeAgo(t *testing.T) {
	now := time.Now()
	cases := []struct {
		t        time.Time
		contains string
	}{
		{now.Add(-10 * time.Second), "just now"},
		{now.Add(-5 * time.Minute), "5 minutes ago"},
		{now.Add(-1 * time.Minute), "1 minute ago"},
		{now.Add(-3 * time.Hour), "3 hours ago"},
		{now.Add(-2 * 24 * time.Hour), "2 days ago"},
	}
	for _, tc := range cases {
		got := relativeAgo(now, tc.t)
		if !strings.Contains(got, tc.contains) {
			t.Errorf("relativeAgo(%v) = %q, want contains %q", tc.t, got, tc.contains)
		}
	}
}
