package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHandleRoot_BrowserGetsLandingPage(t *testing.T) {
	s := &Server{}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()

	s.handleRoot(w, req)

	res := w.Result()
	defer func() { _ = res.Body.Close() }()
	require.Equal(t, http.StatusOK, res.StatusCode)
	assert.Contains(t, res.Header.Get("Content-Type"), "text/html")
	assert.Contains(t, w.Body.String(), "stored.ge", "embedded landing page should render")
}

func TestHandleRoot_HeadReturnsNoBody(t *testing.T) {
	s := &Server{}
	req := httptest.NewRequest(http.MethodHead, "/", nil)
	w := httptest.NewRecorder()

	s.handleRoot(w, req)

	require.Equal(t, http.StatusOK, w.Result().StatusCode)
	assert.Empty(t, w.Body.String())
}

// isS3RootRequest must keep authenticated ListBuckets working: a request to "/"
// with SigV4 auth (header or presigned query) is an S3 call, not a browser visit.
func TestIsS3RootRequest(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*http.Request)
		want  bool
	}{
		{"plain browser GET /", func(*http.Request) {}, false},
		{"browser with html accept", func(r *http.Request) { r.Header.Set("Accept", "text/html") }, false},
		{"sigv4 authorization header", func(r *http.Request) {
			r.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=...")
		}, true},
		{"presigned query", func(r *http.Request) {
			q := r.URL.Query()
			q.Set("X-Amz-Algorithm", "AWS4-HMAC-SHA256")
			r.URL.RawQuery = q.Encode()
		}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			tt.setup(req)
			assert.Equal(t, tt.want, isS3RootRequest(req))
		})
	}
}
