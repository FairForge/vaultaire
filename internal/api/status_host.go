package api

import (
	"html"
	"net/http"
	"net/url"
	"strings"

	"github.com/FairForge/vaultaire/internal/dashboard"
	"github.com/FairForge/vaultaire/internal/hosts"
)

// WP-DOMAIN-1 part 3B (contract rule 11, docs/surfaces/DEVELOPER.md "The
// first ten" 2): the status page is the Worker on status.stored.cloud
// (edge/status). It never asks the origin anything — the box pushes to it
// (deploy/scripts/status-push.sh) — so the status ORIGIN never reaches us.
// Another status name that does (status.stored.ge, status.<alias>) gets this
// router: a browser's page view 301s to the status origin (no-store; the
// alias middleware's gate, isBrowserPageView), and everything else — any
// method, a script's fetch, an operational path, a request carrying S3
// credentials (Architecture Decision 8 as a 404, as on the trust and docs
// hosts) — is the pixel 404, never cached. The status origin's own name
// reaching us (a DNS mistake) is a 404 too, never a redirect to itself.

// StatusHostRouter answers status hosts when the canonical origin is a public
// name; every other host, and every host on a development origin, goes to
// fallback.
func StatusHostRouter(h hosts.Hosts, fallback http.Handler) http.Handler {
	if !h.StatusPublic() {
		return fallback
	}
	assets := dashboard.LayoutAssetsHandler()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !h.IsStatusHost(r.Host) {
			fallback.ServeHTTP(w, r)
			return
		}
		// The pixel 404's layout loads its six files from this host.
		if isAnonymousFetch(r) && isLayoutAsset(r.URL.Path) {
			assets.ServeHTTP(w, r)
			return
		}
		if !h.IsStatusOrigin(r.Host) && isBrowserPageView(r) && !signedS3Request(r) {
			w.Header().Set("Cache-Control", "no-store")
			// #nosec G710 -- not an open redirect: scheme and host are the configured status origin; only the path (leading slashes collapsed) and the query come from the request
			http.Redirect(w, r, statusTarget(h, r), http.StatusMovedPermanently)
			return
		}
		dashboard.WebNotFound(w, r)
	})
}

func isLayoutAsset(p string) bool {
	for _, a := range dashboard.LegalLEAssets {
		if p == a {
			return true
		}
	}
	return false
}

// statusTarget is the status origin with the request's path and query, the
// path's leading slashes collapsed (canonicalTarget's rule).
func statusTarget(h hosts.Hosts, r *http.Request) string {
	u, err := url.Parse(h.Status)
	if err != nil {
		return h.Status + "/"
	}
	u.Path = "/" + strings.TrimLeft(r.URL.Path, "/\\")
	u.RawPath = ""
	u.RawQuery = r.URL.RawQuery
	return u.String()
}

// statusPageLink is the /status page's line naming the live status page, or
// "" while the status page is no host of its own (a development origin).
func (s *Server) statusPageLink() string {
	h := s.hostSet()
	if !h.StatusPublic() || h.Status == h.Canonical {
		return ""
	}
	return `<p class="detail">Live status: <a href="` + html.EscapeString(h.Status) + `">` + html.EscapeString(h.StatusHost()) + `</a></p>`
}
