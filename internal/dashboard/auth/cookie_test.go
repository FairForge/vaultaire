package auth

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// WP-R12-14: the session cookie carries the `__Host-` prefix, so it is
// host-only by the browser's rules — Secure, Path=/, no Domain — and a
// sibling subdomain cannot plant one. The clearing cookie must satisfy the
// same rules or the browser keeps the session; the pre-prefix cookie is
// expired alongside so a browser carries one session cookie, not two.
func TestSessionCookie_HostPrefixAttributes(t *testing.T) {
	require.True(t, strings.HasPrefix(SessionCookieName, "__Host-"), SessionCookieName)
	assert.NotEqual(t, LegacySessionCookieName, SessionCookieName)

	byName := func(w *httptest.ResponseRecorder) map[string]*http.Cookie {
		m := map[string]*http.Cookie{}
		for _, c := range w.Result().Cookies() {
			m[c.Name] = c
		}
		return m
	}
	hostPrefixOK := func(t *testing.T, c *http.Cookie) {
		t.Helper()
		assert.True(t, c.Secure, "Secure is required by the prefix")
		assert.Equal(t, "/", c.Path, "Path=/ is required by the prefix")
		assert.Empty(t, c.Domain, "a Domain attribute is forbidden by the prefix")
		assert.True(t, c.HttpOnly)
		assert.Equal(t, http.SameSiteLaxMode, c.SameSite)
	}

	t.Run("set", func(t *testing.T) {
		w := httptest.NewRecorder()
		SetSessionCookie(w, "tok-123", 24*time.Hour)
		cookies := byName(w)
		c := cookies[SessionCookieName]
		require.NotNil(t, c, "the session cookie is set under the prefixed name")
		hostPrefixOK(t, c)
		assert.Equal(t, "tok-123", c.Value)
		assert.Equal(t, int(24*time.Hour/time.Second), c.MaxAge)

		legacy := cookies[LegacySessionCookieName]
		require.NotNil(t, legacy, "the legacy cookie is expired alongside")
		assert.Equal(t, "", legacy.Value)
		assert.Less(t, legacy.MaxAge, 0)
		hostPrefixOK(t, legacy)
	})

	t.Run("clear", func(t *testing.T) {
		w := httptest.NewRecorder()
		ClearSessionCookie(w)
		cookies := byName(w)
		c := cookies[SessionCookieName]
		require.NotNil(t, c)
		hostPrefixOK(t, c)
		assert.Equal(t, "", c.Value)
		assert.Less(t, c.MaxAge, 0, "cleared")
		legacy := cookies[LegacySessionCookieName]
		require.NotNil(t, legacy)
		assert.Less(t, legacy.MaxAge, 0)
	})

	t.Run("the middleware reads the prefixed name only", func(t *testing.T) {
		store := NewMemoryStore()
		tok, err := store.Create(t.Context(), SessionData{UserID: "u1", Email: "u1@stored.ge"}, time.Hour)
		require.NoError(t, err)
		h := RequireSession(store)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		}))

		req := httptest.NewRequest("GET", "/dashboard", nil)
		req.AddCookie(&http.Cookie{Name: LegacySessionCookieName, Value: tok})
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		assert.Equal(t, http.StatusSeeOther, w.Code, "a pre-prefix cookie no longer signs anyone in")

		req = httptest.NewRequest("GET", "/dashboard", nil)
		req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: tok})
		w = httptest.NewRecorder()
		h.ServeHTTP(w, req)
		assert.Equal(t, http.StatusNoContent, w.Code)
	})
}
