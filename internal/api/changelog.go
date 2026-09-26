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

// changelogShellPre/Post wrap the rendered markdown in the shared site shell
// (site_shell.go: landing-page styling, light + dark).
const changelogShellPre = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>Changelog — stored.ge</title>
` + siteShellStyle + `</head>
<body>
<header class="top"><div class="top-in">` + siteShellBrand + `<a class="crumb" href="/changelog">Changelog</a></div></header>
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
