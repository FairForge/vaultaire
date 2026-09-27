package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// /og.png is the landing page's social preview: a real PNG, cacheable,
// and HEAD carries the headers without a body.
func TestOGImage_ServesPNG(t *testing.T) {
	s := &Server{}
	w := httptest.NewRecorder()
	s.handleOGImage(w, httptest.NewRequest(http.MethodGet, "/og.png", nil))

	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "image/png", w.Header().Get("Content-Type"))
	assert.Contains(t, w.Header().Get("Cache-Control"), "max-age")
	body := w.Body.Bytes()
	require.Greater(t, len(body), 8)
	assert.Equal(t, []byte("\x89PNG\r\n\x1a\n"), body[:8], "must be a PNG")

	h := httptest.NewRecorder()
	s.handleOGImage(h, httptest.NewRequest(http.MethodHead, "/og.png", nil))
	require.Equal(t, http.StatusOK, h.Code)
	assert.Zero(t, h.Body.Len(), "HEAD must not carry a body")
}

// The landing page must point social cards at the image.
func TestLanding_HasSocialPreview(t *testing.T) {
	for _, page := range []string{string(landingClosed), string(landingOpen)} {
		assert.Contains(t, page, `property="og:image" content="https://stored.ge/og.png"`)
		assert.Contains(t, page, `name="twitter:card" content="summary_large_image"`)
	}
}
