package api

import (
	"bytes"
	_ "embed"
	"net/http"
	"sync"

	"github.com/yuin/goldmark"
	"go.uber.org/zap"
)

// The public /changelog page (1.13 live-iteration kit). Source of truth is
// the checked-in internal/api/changelog.md (go:embed cannot reach the repo
// root), rendered ONCE per process via goldmark into a landing-styled shell.
// Publishing an entry = edit the markdown + merge; the auto-deploy is the
// publish step. Entries carry requested-by credits (LET usernames).

//go:embed changelog.md
var changelogMD []byte

var (
	changelogOnce sync.Once
	changelogHTML []byte
)

// changelogShellPre/Post wrap the rendered markdown in a minimal shell
// using the landing page's palette (light + dark).
const changelogShellPre = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>Changelog — stored.ge</title>
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
main { max-width: 760px; margin: 0 auto; padding: 2.5rem 1.5rem 4rem; }
main h1 { font-size: 2rem; margin-bottom: 0.75rem; }
main h2 { font-size: 1.25rem; margin: 2.25rem 0 0.75rem; padding-top: 1.25rem; border-top: 1px solid var(--border-color); }
main h3 { font-size: 1.05rem; margin: 1.5rem 0 0.5rem; }
main p { margin: 0.75rem 0; color: var(--text-secondary); }
main li { margin: 0.5rem 0 0.5rem 1.25rem; color: var(--text-secondary); }
main li strong, main p strong { color: var(--text-primary); }
main code { background: var(--code-bg); border: 1px solid var(--border-color); border-radius: 4px; padding: 0.1rem 0.35rem; font-size: 0.9em; }
main hr { border: none; border-top: 1px solid var(--border-color); margin: 1.5rem 0; }
main a { color: var(--secondary); }
</style>
</head>
<body>
<div class="top"><a href="/">stored.ge</a></div>
<main>
`

const changelogShellPost = `</main>
</body>
</html>
`

// renderChangelog converts the embedded markdown to HTML exactly once.
// A render failure (malformed markdown cannot realistically fail goldmark,
// but belt-and-braces) falls back to an empty page body, never a panic.
func renderChangelog(logger *zap.Logger) []byte {
	changelogOnce.Do(func() {
		var buf bytes.Buffer
		buf.WriteString(changelogShellPre)
		if err := goldmark.Convert(changelogMD, &buf); err != nil {
			logger.Error("render changelog markdown", zap.Error(err))
			buf.Reset()
			buf.WriteString(changelogShellPre)
			buf.WriteString("<h1>Changelog</h1><p>Temporarily unavailable.</p>")
		}
		buf.WriteString(changelogShellPost)
		changelogHTML = buf.Bytes()
	})
	return changelogHTML
}

// handleChangelog serves the customer-facing changelog. Public GET/HEAD,
// registered before the S3 catch-all (same pattern as the landing page).
func (s *Server) handleChangelog(w http.ResponseWriter, r *http.Request) {
	body := renderChangelog(s.logger)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=300")
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	_, _ = w.Write(body)
}
