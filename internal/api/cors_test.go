package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestMatchOrigin_Wildcard(t *testing.T) {
	assert.Equal(t, "*", matchOrigin("https://example.com", "*"))
}

func TestMatchOrigin_ExactMatch(t *testing.T) {
	assert.Equal(t, "https://example.com", matchOrigin("https://example.com", "https://example.com"))
}

func TestMatchOrigin_CaseInsensitive(t *testing.T) {
	assert.Equal(t, "https://Example.COM", matchOrigin("https://Example.COM", "https://example.com"))
}

func TestMatchOrigin_CommaSeparated(t *testing.T) {
	origins := "https://foo.com, https://bar.com, https://baz.com"
	assert.Equal(t, "https://bar.com", matchOrigin("https://bar.com", origins))
	assert.Equal(t, "https://baz.com", matchOrigin("https://baz.com", origins))
}

func TestMatchOrigin_NoMatch(t *testing.T) {
	assert.Equal(t, "", matchOrigin("https://evil.com", "https://example.com"))
}

func TestMatchOrigin_EmptyOrigin(t *testing.T) {
	assert.Equal(t, "", matchOrigin("", "https://example.com"))
}

func TestMatchOrigin_EmptyAllowed(t *testing.T) {
	assert.Equal(t, "", matchOrigin("https://example.com", ""))
}

func TestMatchOrigin_WildcardInList(t *testing.T) {
	assert.Equal(t, "*", matchOrigin("https://anything.com", "https://specific.com, *"))
}

func TestSetCORSHeaders_WithOrigin(t *testing.T) {
	req := httptest.NewRequest("GET", "/test", nil)
	req.Header.Set("Origin", "https://example.com")
	w := httptest.NewRecorder()

	setCORSHeaders(w, req, "https://example.com")

	assert.Equal(t, "https://example.com", w.Header().Get("Access-Control-Allow-Origin"))
	assert.Equal(t, cdnAllowMethods, w.Header().Get("Access-Control-Allow-Methods"))
	assert.Equal(t, cdnExposeHeaders, w.Header().Get("Access-Control-Expose-Headers"))
	assert.Equal(t, cdnMaxAge, w.Header().Get("Access-Control-Max-Age"))
	assert.Equal(t, "Origin", w.Header().Get("Vary"))
}

func TestSetCORSHeaders_Wildcard(t *testing.T) {
	req := httptest.NewRequest("GET", "/test", nil)
	req.Header.Set("Origin", "https://example.com")
	w := httptest.NewRecorder()

	setCORSHeaders(w, req, "*")

	assert.Equal(t, "*", w.Header().Get("Access-Control-Allow-Origin"))
	assert.Empty(t, w.Header().Get("Vary"), "wildcard should not set Vary: Origin")
}

func TestSetCORSHeaders_NoOriginHeader(t *testing.T) {
	req := httptest.NewRequest("GET", "/test", nil)
	w := httptest.NewRecorder()

	setCORSHeaders(w, req, "https://example.com")

	assert.Empty(t, w.Header().Get("Access-Control-Allow-Origin"))
}

func TestSetCORSHeaders_OriginNotAllowed(t *testing.T) {
	req := httptest.NewRequest("GET", "/test", nil)
	req.Header.Set("Origin", "https://evil.com")
	w := httptest.NewRecorder()

	setCORSHeaders(w, req, "https://example.com")

	assert.Empty(t, w.Header().Get("Access-Control-Allow-Origin"))
}

func TestHandleCDNPreflight(t *testing.T) {
	req := httptest.NewRequest("OPTIONS", "/test", nil)
	req.Header.Set("Origin", "https://example.com")
	w := httptest.NewRecorder()

	handleCDNPreflight(w, req, "https://example.com")

	assert.Equal(t, http.StatusNoContent, w.Code)
	assert.Equal(t, "0", w.Header().Get("Content-Length"))
	assert.Equal(t, "https://example.com", w.Header().Get("Access-Control-Allow-Origin"))
	assert.Equal(t, cdnAllowMethods, w.Header().Get("Access-Control-Allow-Methods"))
}
