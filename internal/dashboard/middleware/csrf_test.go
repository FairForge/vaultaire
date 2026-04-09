package middleware

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func okHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok := Token(r.Context())
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(tok))
	}
}

func TestCSRF_GET_SetsTokenCookieAndContext(t *testing.T) {
	handler := CSRF(okHandler())

	req := httptest.NewRequest("GET", "/dashboard", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	// Should set csrf_token cookie.
	cookies := w.Result().Cookies()
	var csrfCookie *http.Cookie
	for _, c := range cookies {
		if c.Name == "csrf_token" {
			csrfCookie = c
		}
	}
	require.NotNil(t, csrfCookie)
	assert.Len(t, csrfCookie.Value, 64) // 32 bytes hex-encoded
	assert.True(t, csrfCookie.HttpOnly)
	assert.Equal(t, http.SameSiteLaxMode, csrfCookie.SameSite)

	// Body should contain the token (handler writes it).
	assert.Equal(t, csrfCookie.Value, w.Body.String())
}

func TestCSRF_GET_ReusesExistingCookie(t *testing.T) {
	handler := CSRF(okHandler())

	req := httptest.NewRequest("GET", "/dashboard", nil)
	req.AddCookie(&http.Cookie{Name: "csrf_token", Value: "existing-token-value-0123456789ab"})
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "existing-token-value-0123456789ab", w.Body.String())
}

func TestCSRF_POST_ValidFormField(t *testing.T) {
	handler := CSRF(okHandler())
	token := "abc123def456abc123def456abc123def456abc123def456abc123def456abc12345"

	body := strings.NewReader("csrf_token=" + token + "&name=test")
	req := httptest.NewRequest("POST", "/dashboard/settings/profile", body)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: "csrf_token", Value: token})
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
}

func TestCSRF_POST_ValidHeader(t *testing.T) {
	handler := CSRF(okHandler())
	token := "abc123def456abc123def456abc123def456abc123def456abc123def456abc12345"

	req := httptest.NewRequest("POST", "/admin/tenants/t-1/suspend", nil)
	req.Header.Set("X-CSRF-Token", token)
	req.AddCookie(&http.Cookie{Name: "csrf_token", Value: token})
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
}

func TestCSRF_POST_MissingToken(t *testing.T) {
	handler := CSRF(okHandler())

	req := httptest.NewRequest("POST", "/dashboard/settings/profile", nil)
	req.AddCookie(&http.Cookie{Name: "csrf_token", Value: "some-token"})
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusForbidden, w.Code)
	assert.Contains(t, w.Body.String(), "CSRF")
}

func TestCSRF_POST_MismatchedToken(t *testing.T) {
	handler := CSRF(okHandler())

	body := strings.NewReader("csrf_token=wrong-token")
	req := httptest.NewRequest("POST", "/dashboard/settings/profile", body)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: "csrf_token", Value: "correct-token"})
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestCSRF_POST_NoCookie(t *testing.T) {
	handler := CSRF(okHandler())

	body := strings.NewReader("csrf_token=some-token")
	req := httptest.NewRequest("POST", "/dashboard/settings/profile", body)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestCSRF_SkipsGET(t *testing.T) {
	handler := CSRF(okHandler())

	req := httptest.NewRequest("GET", "/dashboard", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
}

func TestCSRF_SkipsHEAD(t *testing.T) {
	handler := CSRF(okHandler())

	req := httptest.NewRequest("HEAD", "/dashboard", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
}

func TestCSRF_SkipsOPTIONS(t *testing.T) {
	handler := CSRF(okHandler())

	req := httptest.NewRequest("OPTIONS", "/dashboard", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
}
