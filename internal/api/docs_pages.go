package api

import (
	"bytes"
	_ "embed"
	"net/http"
	"strings"
	"sync"

	"github.com/yuin/goldmark"
	"go.uber.org/zap"
)

// Customer docs (Stage 1B item B3). Three hand-written guides embedded as
// markdown and rendered ONCE per process via goldmark into the same shell the
// changelog uses — publishing an edit is: change the markdown + merge (the
// auto-deploy is the publish step). The hub at /docs links the guides and the
// API reference (Swagger, moved to /docs/api).

//go:embed docs_getting_started.md
var docsGettingStartedMD []byte

//go:embed docs_rclone.md
var docsRcloneMD []byte

//go:embed docs_faq.md
var docsFAQMD []byte

// docsShellPre mirrors the changelog shell (landing palette, light + dark);
// __TITLE__ is replaced with the page title (Fprintf would fight the % signs
// in the CSS).
const docsShellPre = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>__TITLE__ — stored.ge</title>
<style>
* { margin: 0; padding: 0; box-sizing: border-box; }
:root {
  --primary: #5B21B6; --secondary: #8B5CF6; --accent: #10B981;
  --bg: #FFFFFF; --text-primary: #111827; --text-secondary: #6B7280;
  --border-color: #E5E7EB; --code-bg: #F9FAFB;
}
@media (prefers-color-scheme: dark) {
  :root {
    --primary: #7C3AED; --secondary: #A78BFA;
    --bg: #111827; --text-primary: #F9FAFB; --text-secondary: #9CA3AF;
    --border-color: #374151; --code-bg: #1F2937;
  }
}
body {
  font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, 'Helvetica Neue', Arial, sans-serif;
  background: var(--bg); color: var(--text-primary); line-height: 1.65;
}
.top { border-bottom: 1px solid var(--border-color); padding: 1rem 1.5rem; }
.top a { color: var(--secondary); text-decoration: none; font-weight: 700; }
.top a.crumb { font-weight: 400; color: var(--text-secondary); margin-left: 1rem; }
main { max-width: 760px; margin: 0 auto; padding: 2.5rem 1.5rem 4rem; }
main h1 { font-size: 2rem; margin-bottom: 0.75rem; }
main h2 { font-size: 1.25rem; margin: 2.25rem 0 0.75rem; padding-top: 1.25rem; border-top: 1px solid var(--border-color); }
main h3 { font-size: 1.05rem; margin: 1.5rem 0 0.5rem; }
main p { margin: 0.75rem 0; color: var(--text-secondary); }
main li { margin: 0.5rem 0 0.5rem 1.25rem; color: var(--text-secondary); }
main li strong, main p strong { color: var(--text-primary); }
main code { background: var(--code-bg); border: 1px solid var(--border-color); border-radius: 4px; padding: 0.1rem 0.35rem; font-size: 0.9em; }
main pre { background: var(--code-bg); border: 1px solid var(--border-color); border-radius: 8px; padding: 1rem 1.2rem; overflow-x: auto; margin: 0.75rem 0; }
main pre code { background: none; border: none; padding: 0; }
main table { border-collapse: collapse; margin: 0.75rem 0; width: 100%; }
main th, main td { padding: 0.5rem 0.8rem; text-align: left; border-bottom: 1px solid var(--border-color); font-size: 0.95em; }
main th { color: var(--text-secondary); }
main td { color: var(--text-secondary); }
main hr { border: none; border-top: 1px solid var(--border-color); margin: 1.5rem 0; }
main a { color: var(--secondary); }
</style>
</head>
<body>
<div class="top"><a href="/">stored.ge</a><a class="crumb" href="/docs">Docs</a></div>
<main>
`

const docsShellPost = `</main>
</body>
</html>
`

// docsHubHTML is the /docs index: hand-written, no markdown source needed.
const docsHubHTML = `<h1>Documentation</h1>
<p>Everything here assumes one thing: your tool speaks S3. If it does, it works.</p>
<h2>Guides</h2>
<ul>
<li><a href="/docs/getting-started"><strong>Getting Started</strong></a> — signup to first upload in about two minutes (aws-cli, boto3)</li>
<li><a href="/docs/rclone"><strong>rclone</strong></a> — sync, mount, and migrate; the fastest path for backups</li>
<li><a href="/docs/faq"><strong>FAQ</strong></a> — pricing, trust &amp; reliability, technical details</li>
</ul>
<h2>Reference</h2>
<ul>
<li><a href="/docs/api"><strong>API reference</strong></a> — interactive OpenAPI/Swagger documentation</li>
<li><a href="/llms.txt"><strong>llms.txt</strong></a> — plain-text API summary for AI assistants</li>
</ul>
<h2>Support</h2>
<p>Stuck? <a href="mailto:support@stored.ge">support@stored.ge</a> — founder-direct, target response under 4 hours.</p>
`

// docsPages maps rendered page bytes by route suffix, built once at first request.
var (
	docsOnce  sync.Once
	docsPages map[string][]byte
)

func renderDocsPages(logger *zap.Logger) map[string][]byte {
	docsOnce.Do(func() {
		shell := func(title string) string {
			return strings.ReplaceAll(docsShellPre, "__TITLE__", title)
		}
		render := func(title string, md []byte) []byte {
			var buf bytes.Buffer
			buf.WriteString(shell(title))
			if err := goldmark.Convert(md, &buf); err != nil {
				logger.Error("render docs markdown", zap.String("title", title), zap.Error(err))
				buf.Reset()
				buf.WriteString(shell(title))
				buf.WriteString("<h1>" + title + "</h1><p>Temporarily unavailable.</p>")
			}
			buf.WriteString(docsShellPost)
			return buf.Bytes()
		}

		var hub bytes.Buffer
		hub.WriteString(shell("Documentation"))
		hub.WriteString(docsHubHTML)
		hub.WriteString(docsShellPost)

		docsPages = map[string][]byte{
			"":                hub.Bytes(),
			"getting-started": render("Getting Started", docsGettingStartedMD),
			"rclone":          render("rclone Setup", docsRcloneMD),
			"faq":             render("FAQ", docsFAQMD),
		}
	})
	return docsPages
}

// handleDocsPage serves /docs (hub) and /docs/{getting-started,rclone,faq}.
// Public GET/HEAD, registered before the S3 catch-all (changelog pattern).
func (s *Server) handleDocsPage(slug string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body := renderDocsPages(s.logger)[slug]
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=300")
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusOK)
			return
		}
		_, _ = w.Write(body)
	}
}
