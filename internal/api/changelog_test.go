package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// The public /changelog page (1.13): rendered once at boot from the
// embedded internal/api/changelog.md via goldmark, served as HTML.
func TestChangelog_ServesHTML(t *testing.T) {
	s := &Server{logger: zap.NewNop()}

	req := httptest.NewRequest("GET", "/changelog", nil)
	rec := httptest.NewRecorder()
	s.handleChangelog(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Header().Get("Content-Type"), "text/html")
	body := rec.Body.String()
	// The markdown is rendered, not served raw.
	assert.Contains(t, body, "<h", "markdown headings must be rendered to HTML")
	assert.NotContains(t, body, "\n# ", "raw markdown must not leak through")
	// The first entry ships with 1.13 itself.
	assert.Contains(t, body, "Changelog")
}

func TestChangelog_HeadRequest(t *testing.T) {
	s := &Server{logger: zap.NewNop()}

	req := httptest.NewRequest("HEAD", "/changelog", nil)
	rec := httptest.NewRecorder()
	s.handleChangelog(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Header().Get("Content-Type"), "text/html")
	assert.Zero(t, rec.Body.Len(), "HEAD must not carry a body")
}
