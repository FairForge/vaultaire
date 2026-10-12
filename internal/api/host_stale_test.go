package api

import (
	"bytes"
	"html/template"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/dashboard"
	"github.com/FairForge/vaultaire/internal/dashboard/handlers"
	"github.com/FairForge/vaultaire/internal/hosts"
)

// typedHosts are the domains no template, guide or landing source may type:
// they come from internal/hosts (WP-DOMAIN-1, F-17).
var typedHosts = []string{"stored.ge", "stored.cloud", "stored.pics"}

// staleAliasPhrases are the only sentences a RENDERED page may name a
// legacy domain in: the explicit "also served at" alias sentence.
var staleAliasPhrases = []string{
	"stored.ge and stored.pics", // {{host "aliases"}} (privacy policy)
}

func assertNoStaleHost(t *testing.T, name, body string) {
	t.Helper()
	for _, p := range staleAliasPhrases {
		body = strings.ReplaceAll(body, p, "")
	}
	assert.NotContains(t, body, "stored.ge", "%s names the legacy domain outside an alias sentence", name)
}

func TestNoStaleHostInTemplatesAndLanding(t *testing.T) {
	prev := hosts.Get()
	t.Cleanup(func() { hosts.Set(prev) })
	h := prodHosts()
	hosts.Set(h)

	t.Run("sources type no host", func(t *testing.T) {
		require.NoError(t, fs.WalkDir(dashboard.Templates, "templates", func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			b, err := fs.ReadFile(dashboard.Templates, p)
			require.NoError(t, err)
			for _, typed := range typedHosts {
				assert.NotContains(t, string(b), typed, "%s types a host — use {{host ...}}", p)
			}
			return nil
		}))
		for _, p := range []string{
			"landing/landing.src.html", "landing/builder.html", "landing/builder.js", "landing/page.js",
			"docs_getting_started.md", "docs_rclone.md", "docs_faq.md",
		} {
			b, err := os.ReadFile(filepath.FromSlash(p))
			require.NoError(t, err)
			for _, typed := range typedHosts {
				assert.NotContains(t, string(b), typed, "%s types a host — use the host tokens", p)
			}
		}
	})

	t.Run("rendered pages name the canonical hosts", func(t *testing.T) {
		pages := landingPages(h)
		for i, page := range pages {
			assertNoStaleHost(t, []string{"landing (closed)", "landing (open)"}[i], string(page))
			assert.Contains(t, string(page), "https://s3.stored.cloud")
			assert.Contains(t, string(page), `content="https://stored.cloud/og.png"`)
		}

		llms := llmsTxtBody(h)
		assertNoStaleHost(t, "llms.txt", llms)
		assert.Contains(t, llms, "Endpoint https://s3.stored.cloud, region us-east-1")
		assert.Contains(t, llms, "GET https://cdn.stored.cloud/<slug>/<bucket>/<key>")
		assert.Contains(t, llms, "## JSON API (base https://stored.cloud/api/v1,")

		for slug, page := range renderDocsPages(zap.NewNop(), h) {
			assertNoStaleHost(t, "docs/"+slug, string(page))
		}

		legal, err := fs.Glob(dashboard.Templates, "templates/legal/*.html")
		require.NoError(t, err)
		require.NotEmpty(t, legal)
		for _, p := range legal {
			tmpl := template.Must(template.New("").Funcs(handlers.TemplateFuncs()).ParseFS(dashboard.Templates, p))
			var buf bytes.Buffer
			require.NoError(t, tmpl.ExecuteTemplate(&buf, "content", nil), p)
			assertNoStaleHost(t, p, buf.String())
		}
	})

	t.Run("the privacy policy names the controller, the domain and the aliases", func(t *testing.T) {
		tmpl := template.Must(template.New("").Funcs(handlers.TemplateFuncs()).ParseFS(dashboard.Templates, "templates/legal/privacy.html"))
		var buf bytes.Buffer
		require.NoError(t, tmpl.ExecuteTemplate(&buf, "content", nil))
		body := buf.String()
		assert.Contains(t, body, "a product of FairForge")
		assert.Contains(t, body, "<strong>stored.cloud</strong> (also served at stored.ge and stored.pics)")
		assert.Contains(t, body, "privacy@stored.cloud")
	})
}
