package api

import (
	"net/http"

	"github.com/FairForge/vaultaire/internal/dashboard"
)

// The web host answers like a website (Stage 1a Prompt 39 Part A). The S3
// catch-all answers every unmatched path on every host, so before this a
// visitor's typo on stored.cloud got the S3 API's 403 AccessDenied XML.
//
// Only an ANONYMOUS browser page view on a web host is answered as a
// website: isBrowserPageView (the predicate the alias 301 uses — GET/HEAD,
// Accept text/html, no Authorization, no presigned query, no x-amz-* header,
// not an operational path) and hosts.IsWebHost (the canonical host, an
// alias, the brand and legacy domains and their www.; never an s3., cdn. or
// trust. host). Anything that could be an S3 client gets exactly what it got
// before: a SigV4 signature covers the Host, and the S3 API answers on every
// name.

// webPageView reports whether r is an anonymous browser page view on a web
// host.
func (s *Server) webPageView(r *http.Request) bool {
	return s.hostSet().IsWebHost(r.Host) && isBrowserPageView(r)
}

// serveWebNotFound answers a web page view the S3 API would refuse for want
// of a credential with the pixel 404 page; false = not a web page view, the
// caller answers as the S3 API. A request with no credential never reaches
// NoSuchBucket or NoSuchKey (it is refused first), so the refusal is the one
// place this hooks in.
func (s *Server) serveWebNotFound(w http.ResponseWriter, r *http.Request) bool {
	if !s.webPageView(r) {
		return false
	}
	dashboard.WebNotFound(w, r)
	return true
}

// webRedirect 301s a web page view to target (the paths a launch reader
// types: /signup, /pricing); any other request is the S3 API's — a bucket
// may be named "signup" or "pricing".
func (s *Server) webRedirect(target string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.webPageView(r) {
			s.handleS3Request(w, r)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		http.Redirect(w, r, target, http.StatusMovedPermanently)
	}
}

// registerWebRedirects mounts the launch paths before the S3 catch-all.
// /pricing goes to the landing's pricing section: prices.json feeds it, so
// it is the one place the prices are (TestWebHost_SignupAndPricingRedirect).
func (s *Server) registerWebRedirects() {
	for path, target := range map[string]string{"/signup": "/register", "/pricing": "/#pricing", "/pricing/": "/#pricing"} {
		h := s.webRedirect(target)
		s.router.Get(path, h)
		s.router.Head(path, h)
	}
}
