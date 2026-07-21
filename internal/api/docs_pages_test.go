package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func docsGet(t *testing.T, slug string) *httptest.ResponseRecorder {
	t.Helper()
	s := &Server{logger: zap.NewNop()}
	req := httptest.NewRequest(http.MethodGet, "/docs/"+slug, nil)
	w := httptest.NewRecorder()
	s.handleDocsPage(slug)(w, req)
	return w
}

func TestDocsHub_LinksGuidesAndReference(t *testing.T) {
	w := docsGet(t, "")
	require.Equal(t, http.StatusOK, w.Result().StatusCode)
	assert.Contains(t, w.Header().Get("Content-Type"), "text/html")
	for _, link := range []string{
		`href="/docs/getting-started"`, `href="/docs/rclone"`,
		`href="/docs/faq"`, `href="/docs/api"`, "support@stored.ge",
	} {
		assert.Contains(t, w.Body.String(), link)
	}
}

func TestDocsGuides_RenderMarkdown(t *testing.T) {
	tests := []struct {
		slug string
		want []string
	}{
		{"getting-started", []string{
			"<h1", "Getting Started", "https://stored.ge", "us-east-1", "aws configure",
		}},
		{"rclone", []string{
			"<h1", "rclone", "rclone config create stored s3", "endpoint=https://stored.ge",
		}},
		{"faq", []string{
			"<h1", "Frequently Asked", "$3.99/TB", "$5.99/TB", "support@stored.ge",
		}},
	}
	for _, tt := range tests {
		t.Run(tt.slug, func(t *testing.T) {
			w := docsGet(t, tt.slug)
			require.Equal(t, http.StatusOK, w.Result().StatusCode)
			for _, want := range tt.want {
				assert.Contains(t, w.Body.String(), want)
			}
			// Raw markdown must not leak through the renderer.
			assert.NotContains(t, w.Body.String(), "\n## ")
		})
	}
}

func TestDocsPage_HeadReturnsNoBody(t *testing.T) {
	s := &Server{logger: zap.NewNop()}
	req := httptest.NewRequest(http.MethodHead, "/docs/faq", nil)
	w := httptest.NewRecorder()
	s.handleDocsPage("faq")(w, req)
	require.Equal(t, http.StatusOK, w.Result().StatusCode)
	assert.Empty(t, w.Body.String())
}
