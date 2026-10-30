package api

import (
	"bytes"
	_ "embed"
	"net/http"
	"strings"

	"github.com/FairForge/vaultaire/internal/api/landing"
	"github.com/FairForge/vaultaire/internal/hosts"
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

// docsShellPre uses the shared site shell (site_shell.go, light + dark);
// __TITLE__ is replaced with the page title (Fprintf would fight the % signs
// in the CSS).
const docsShellPre = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>__TITLE__ — Stored</title>
` + siteShellStyle + `</head>
<body>
<header class="top"><div class="top-in">` + siteShellBrand + `<a class="crumb" href="/docs">Docs</a>` + siteShellToggle + `</div></header>
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
<p>Stuck? <a href="mailto:__SUPPORT_EMAIL__">__SUPPORT_EMAIL__</a> — founder-direct, target response under 4 hours.</p>
`

// renderDocsPages renders every page (by route suffix) for a host set; the
// server keeps the result (Server.docsPages, built once at first request).
func renderDocsPages(logger *zap.Logger, h hosts.Hosts) map[string][]byte {
	shell := func(title string) string {
		return strings.ReplaceAll(docsShellPre, "__TITLE__", title)
	}
	render := func(title string, md []byte) []byte {
		// The egress rule is written once, in prices.json; the guides
		// carry placeholders (WP-R10-9).
		for token, value := range landing.EgressTokens() {
			md = bytes.ReplaceAll(md, []byte(token), []byte(value))
		}
		// So is a Vault restore's time (prices.json vault.restore_time).
		for token, value := range landing.VaultTokens() {
			md = bytes.ReplaceAll(md, []byte(token), []byte(value))
		}
		// Hosts too (internal/hosts, WP-DOMAIN-1): __HOST_S3__ etc.
		md = []byte(h.Expand(string(md)))
		var buf bytes.Buffer
		buf.WriteString(shell(title))
		if err := siteMarkdown.Convert(md, &buf); err != nil {
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
	hub.WriteString(h.Expand(docsHubHTML))
	hub.WriteString(docsShellPost)

	return map[string][]byte{
		"":                hub.Bytes(),
		"getting-started": render("Getting Started", docsGettingStartedMD),
		"rclone":          render("rclone Setup", docsRcloneMD),
		"faq":             render("FAQ", docsFAQMD),
	}
}

// handleDocsPage serves /docs (hub) and /docs/{getting-started,rclone,faq}.
// Public GET/HEAD, registered before the S3 catch-all (changelog pattern).
func (s *Server) handleDocsPage(slug string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.docsOnce.Do(func() { s.docsPages = renderDocsPages(s.logger, s.hostSet()) })
		body := s.docsPages[slug]
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=300")
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusOK)
			return
		}
		_, _ = w.Write(body)
	}
}
