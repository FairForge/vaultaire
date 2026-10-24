package api

import (
	_ "embed"
	"encoding/xml"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/FairForge/vaultaire/internal/dashboard"
)

// /robots.txt, /sitemap.xml and /favicon.ico on the web host (Stage 1a
// Prompt 40 Part 0, RP56-3). Crawlers and link unfurlers send Accept */* or
// image/*, so Prompt 39's browser predicate never fired and all three were
// the S3 API's 403 XML. They are served to an ANONYMOUS GET/HEAD
// (isAnonymousFetch) on a web host (hosts.IsWebHost: never s3., cdn. or
// trust.); a signed request never reaches them (the credential gate) and any
// other request is the S3 API's — "robots.txt" and "favicon.ico" are valid
// bucket names, and the S3 API has no anonymous read to shadow.
//
// Cache-Control: robots.txt public 1 h (it changes with the canonical origin
// only, i.e. a deploy); sitemap.xml public 5 min (it follows the legal-page
// flags; the flags cache ~15 s); favicon.ico public 1 day (it changes with
// the landing's icon, i.e. a deploy).

//go:embed favicon.ico
var faviconICO []byte

// webFetch serves page to an anonymous fetch on a web host; anything else
// is the S3 API's.
func (s *Server) webFetch(page http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.hostSet().IsWebHost(r.Host) || !isAnonymousFetch(r) {
			s.handleS3Request(w, r)
			return
		}
		page(w, r)
	}
}

func (s *Server) registerWebFiles() {
	for path, h := range map[string]http.HandlerFunc{
		"/robots.txt":  s.handleRobotsTxt,
		"/sitemap.xml": s.handleSitemapXML,
		"/favicon.ico": handleFaviconICO,
	} {
		s.router.Get(path, s.webFetch(h))
		s.router.Head(path, s.webFetch(h))
	}
}

// robotsDisallow are the paths no crawler needs: the signed-in app, the
// JSON API, the sign-in flows, public objects (a customer's files, linked
// where the customer links them).
var robotsDisallow = []string{"/dashboard", "/api/", "/auth/", "/cdn/"}

func (s *Server) handleRobotsTxt(w http.ResponseWriter, r *http.Request) {
	var b strings.Builder
	b.WriteString("User-agent: *\nAllow: /\n")
	for _, p := range robotsDisallow {
		b.WriteString("Disallow: " + p + "\n")
	}
	b.WriteString("\nSitemap: " + s.hostSet().URL("/sitemap.xml") + "\n")
	writeWebFile(w, r, "text/plain; charset=utf-8", "public, max-age=3600", []byte(b.String()))
}

// sitemapPaths are the live pages, in order: the landing, the guides and the
// API reference, the status page, the changelog, then every legal page the
// canonical origin serves now (dashboard.LiveLegalPaths: a dark page never).
// No <lastmod>: nothing here knows when a page last changed (the build's
// commit time is when the BINARY changed, not the page).
func (s *Server) sitemapPaths() []string {
	docs := make([]string, 0, len(docsPages)+1)
	for _, p := range docsPages {
		docs = append(docs, p)
	}
	docs = append(docs, "/docs/api")
	sort.Strings(docs)
	out := append([]string{"/"}, docs...)
	out = append(out, "/status", "/changelog")
	return append(out, dashboard.LiveLegalPaths(s.flags)...)
}

func (s *Server) handleSitemapXML(w http.ResponseWriter, r *http.Request) {
	var b strings.Builder
	b.WriteString(xml.Header + `<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">` + "\n")
	hs := s.hostSet()
	for _, p := range s.sitemapPaths() {
		b.WriteString("  <url><loc>")
		_ = xml.EscapeText(&b, []byte(hs.URL(p)))
		b.WriteString("</loc></url>\n")
	}
	b.WriteString("</urlset>\n")
	writeWebFile(w, r, "application/xml; charset=utf-8", "public, max-age=300", []byte(b.String()))
}

func handleFaviconICO(w http.ResponseWriter, r *http.Request) {
	writeWebFile(w, r, "image/x-icon", "public, max-age=86400", faviconICO)
}

func writeWebFile(w http.ResponseWriter, r *http.Request, contentType, cacheControl string, body []byte) {
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", cacheControl)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if r.Method == http.MethodHead {
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(http.StatusOK)
		return
	}
	_, _ = w.Write(body)
}
