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
	s := &Server{logger: zap.NewNop(), hosts: prodHosts()}
	req := httptest.NewRequest(http.MethodGet, "/docs/"+slug, nil)
	w := httptest.NewRecorder()
	s.handleDocsPage(slug)(w, req)
	return w
}

func TestDocsHub_LinksGuidesAndReference(t *testing.T) {
	w := docsGet(t, "")
	require.Equal(t, http.StatusOK, w.Result().StatusCode)
	assert.Contains(t, w.Header().Get("Content-Type"), "text/html")
	// Absolute through hosts.DocsBase (WP-DOMAIN-1 part 3A): the same bytes
	// serve the canonical /docs and the docs host; dark, they name the
	// canonical origin's /docs.
	for _, link := range []string{
		`href="https://stored.cloud/docs/getting-started"`, `href="https://stored.cloud/docs/rclone"`,
		`href="https://stored.cloud/docs/encryption"`,
		`href="https://stored.cloud/docs/faq"`, `href="https://stored.cloud/docs/api"`, "support@stored.cloud",
		`class="brand" href="https://stored.cloud/"`, `class="crumb" href="https://stored.cloud/docs"`,
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
			"<h1", "Getting Started", "https://s3.stored.cloud", "us-east-1", "aws configure",
		}},
		{"rclone", []string{
			"<h1", "rclone", "rclone config create stored s3", "endpoint=https://s3.stored.cloud",
		}},
		{"faq", []string{
			"<h1", "Frequently Asked", "$4.49/TB", "$6.99/TB", "sold as a quota", "support@stored.cloud",
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
	s := &Server{logger: zap.NewNop(), hosts: prodHosts()}
	req := httptest.NewRequest(http.MethodHead, "/docs/faq", nil)
	w := httptest.NewRecorder()
	s.handleDocsPage("faq")(w, req)
	require.Equal(t, http.StatusOK, w.Result().StatusCode)
	assert.Empty(t, w.Body.String())
}

// The FAQ pricing table is GitHub-style markdown; without the table extension
// goldmark printed it as a paragraph of raw pipes on the live page.
func TestDocsFAQ_RendersPricingTable(t *testing.T) {
	w := docsGet(t, "faq")
	require.Equal(t, http.StatusOK, w.Result().StatusCode)
	body := w.Body.String()
	assert.Contains(t, body, "<table>")
	assert.Contains(t, body, "<th>Tier</th>")
	assert.NotContains(t, body, "| Tier | Price |", "raw markdown table must not leak through")
}
