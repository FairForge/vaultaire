package api

import (
	"encoding/xml"
	"net/http"
	"sort"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/FairForge/vaultaire/internal/dashboard"
	"github.com/FairForge/vaultaire/internal/dashboard/middleware"
	"github.com/FairForge/vaultaire/internal/docs"
	"github.com/FairForge/vaultaire/internal/hosts"
)

// WP-DOMAIN-1 part 3A (contract rule 11, docs/surfaces/DEVELOPER.md "The
// first ten" 1 and 6): docs.stored.cloud is its own surface. It serves the
// guides, the API reference and the files a developer's tool fetches, and
// nothing of the app — no landing page, no dashboard, no S3 API. Dark: no
// link names it until the owner's proof (hosts.DocsBase stays on the
// canonical origin's /docs, which answers for ever).

// DocsHostRouter routes requests on a docs host (hosts.IsDocsHost:
// DOCS_PUBLIC_HOST, docs.<canonical>, docs.<alias>, docs.stored.cloud and
// docs.stored.ge whatever the environment) to docsRouter when the canonical
// origin is a public name. Every other host, and every host on a development
// origin, goes to fallback (labs keep /docs on their one origin).
func DocsHostRouter(h hosts.Hosts, docsRouter, fallback http.Handler) http.Handler {
	if !h.DocsPublic() {
		return fallback
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h.IsDocsHost(r.Host) {
			docsRouter.ServeHTTP(w, r)
			return
		}
		fallback.ServeHTTP(w, r)
	})
}

// docsHostPath is a guide's path on the docs host: the hub at /, a guide at
// /<slug>. The canonical origin keeps /docs/<slug> (docsPages) for ever.
func docsHostPath(slug string) string { return "/" + slug }

// newDocsRouter is what a docs host serves, GET and HEAD only: the hub and
// every guide at their docs-host paths — the same bytes as the canonical
// origin's /docs pages (handleDocsPage) — the API reference at /api with
// /openapi.json beside it (the Swagger page fetches that relative path),
// /llms.txt, /.well-known/security.txt, its own /robots.txt and
// /sitemap.xml, and the six files the pixel 404 page's layout loads (the
// docs shell itself loads none: inline style, a data: icon). Everything else
// — any path, any method, and a signed or presigned S3 request on any path,
// listed or not (Architecture Decision 8 holds here as a 404, as on the trust
// host) — is the pixel 404, never cached (D-P33-6: a 404, never a 301 to the
// canonical origin). Its middleware is the main router's minus the alias
// redirect and the site statistics: a request id, the request limits, the
// version header, the access log — plus the dashboard's security headers
// (docsSecurityHeaders, RP91-11).
func (s *Server) newDocsRouter() http.Handler {
	r := chi.NewRouter()
	r.Use(s.requestIDMiddleware, s.requestLimitsMiddleware, s.versionMiddleware, s.loggingMiddleware, docsSecurityHeaders)
	r.NotFound(docsNotFound)
	r.MethodNotAllowed(docsNotFound)
	mount := func(path string, h http.HandlerFunc) {
		serve := func(w http.ResponseWriter, r *http.Request) {
			if signedS3Request(r) {
				docsNotFound(w, r)
				return
			}
			h(w, r)
		}
		r.Get(path, serve)
		r.Head(path, serve)
	}
	for slug := range docsPages {
		mount(docsHostPath(slug), s.handleDocsPage(slug))
	}
	mount("/api", docs.SwaggerUIHandler())
	mount("/openapi.json", docs.OpenAPIJSONHandler(s.hosts))
	mount("/llms.txt", s.handleLlmsTxt)
	mount("/.well-known/security.txt", s.handleSecurityTxt)
	mount("/robots.txt", s.handleDocsRobotsTxt)
	mount("/sitemap.xml", s.handleDocsSitemapXML)
	assets := dashboard.LayoutAssetsHandler()
	for _, p := range dashboard.LegalLEAssets {
		mount(p, assets.ServeHTTP)
	}
	return r
}

// docsNotFound is the docs host's 404: the pixel 404 page, no-store (a 404
// must not outlive a page that starts to exist at an edge).
func docsNotFound(w http.ResponseWriter, r *http.Request) { dashboard.WebNotFound(w, r) }

// docsHostPaths are the docs host's pages in sitemap order: the hub, the
// guides by slug, the API reference.
func docsHostPaths() []string {
	slugs := make([]string, 0, len(docsPages))
	for slug := range docsPages {
		if slug != "" {
			slugs = append(slugs, slug)
		}
	}
	sort.Strings(slugs)
	out := []string{"/"}
	for _, slug := range slugs {
		out = append(out, docsHostPath(slug))
	}
	return append(out, "/api")
}

// handleDocsRobotsTxt: the docs host is all public reading; its Sitemap is
// its own.
func (s *Server) handleDocsRobotsTxt(w http.ResponseWriter, r *http.Request) {
	body := "User-agent: *\nAllow: /\n\nSitemap: " + s.hostSet().Docs + "/sitemap.xml\n"
	writeWebFile(w, r, "text/plain; charset=utf-8", "public, max-age=3600", []byte(body))
}

func (s *Server) handleDocsSitemapXML(w http.ResponseWriter, r *http.Request) {
	var b strings.Builder
	b.WriteString(xml.Header + `<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">` + "\n")
	origin := s.hostSet().Docs
	for _, p := range docsHostPaths() {
		b.WriteString("  <url><loc>")
		_ = xml.EscapeText(&b, []byte(origin+p))
		b.WriteString("</loc></url>\n")
	}
	b.WriteString("</urlset>\n")
	writeWebFile(w, r, "application/xml; charset=utf-8", "public, max-age=3600", []byte(b.String()))
}

// docsAPIContentSecurityPolicy is the API reference's policy (RP91-11): the
// dashboard's (middleware.ContentSecurityPolicy) with unpkg.com added to
// script-src and style-src, because swagger-ui loads from there. /api only:
// no other docs page loads anything from another host.
const docsAPIContentSecurityPolicy = "default-src 'self'; script-src 'self' 'unsafe-inline' https://unpkg.com; style-src 'self' 'unsafe-inline' https://unpkg.com; img-src 'self' data:; frame-ancestors 'none'"

// docsSecurityHeaders gives every docs-host answer the dashboard's security
// headers (CSP, X-Frame-Options DENY, nosniff, Referrer-Policy,
// Permissions-Policy — RP91-11: the guides carried none), with the API
// reference's own CSP on /api.
func docsSecurityHeaders(next http.Handler) http.Handler {
	return middleware.SecurityHeaders(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api" {
			w.Header().Set("Content-Security-Policy", docsAPIContentSecurityPolicy)
		}
		next.ServeHTTP(w, r)
	}))
}
