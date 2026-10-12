package api

import (
	"net/http"
	"strings"

	"github.com/FairForge/vaultaire/internal/hosts"
)

// hostCanonicalMiddleware 301s a BROWSER page view on a web alias
// (VAULTAIRE_ALIAS_HOSTS: stored.ge, www.stored.ge, www.stored.cloud) to the
// canonical origin (WP-DOMAIN-1, F-17). It lives in Go, not HAProxy, so it is
// tested.
//
// The whole safety of it is the gate: stored.ge is also an S3 endpoint
// forever, a SigV4 signature covers the Host, and no S3 client follows a 301
// on a PUT — so ANY sign that a request could come from an S3 client passes
// it through unchanged. It redirects only when every one of these holds: the
// host is an alias (never an s3.* host); GET or HEAD; no Authorization
// header; no presigned query (X-Amz-Signature / X-Amz-Credential, or the v2
// AWSAccessKeyId / Signature); no x-amz-* header; Accept names text/html;
// and the path is not an operational or machine one (/health, /version,
// /metrics, /.well-known/, /webhook/, /api/, /cdn/, /auth/ — an OAuth
// callback must land on the host whose cookie holds its state).
//
// The 301 carries Cache-Control: no-store so the edge never caches a
// redirect for a path the S3 API also serves. Nothing is logged per request.
func hostCanonicalMiddleware(h hosts.Hosts) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		if len(h.Aliases) == 0 {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if shouldRedirectToCanonical(h, r) {
				w.Header().Set("Cache-Control", "no-store")
				http.Redirect(w, r, h.Canonical+r.URL.RequestURI(), http.StatusMovedPermanently)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// hostSet is the server's host set; a Server built without NewServer (unit
// tests) reads the process's (hosts.Get).
func (s *Server) hostSet() hosts.Hosts {
	if s.hosts.Canonical != "" {
		return s.hosts
	}
	return hosts.Get()
}

// billingURL is where the S3 errors and the egress e-mail send a customer to
// raise a quota: the canonical origin's billing page.
func billingURL() string { return hosts.Get().URL("/dashboard/billing") }

// noRedirectPrefixes are served on every name, alias or not.
var noRedirectPrefixes = []string{"/.well-known/", "/webhook/", "/api/", "/cdn/", "/auth/"}

// noRedirectPaths are served on every name, with or without a sub-path.
var noRedirectPaths = []string{"/health", "/version", "/metrics"}

func shouldRedirectToCanonical(h hosts.Hosts, r *http.Request) bool {
	if h.IsS3Host(r.Host) || !h.IsAlias(r.Host) {
		return false
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	if r.Header.Get("Authorization") != "" {
		return false
	}
	for k := range r.Header {
		if strings.HasPrefix(strings.ToLower(k), "x-amz-") {
			return false
		}
	}
	q := r.URL.Query()
	for _, k := range []string{"X-Amz-Signature", "X-Amz-Credential", "AWSAccessKeyId", "Signature"} {
		if q.Has(k) {
			return false
		}
	}
	for k := range q {
		if strings.HasPrefix(strings.ToLower(k), "x-amz-") {
			return false
		}
	}
	if !strings.Contains(strings.ToLower(r.Header.Get("Accept")), "text/html") {
		return false
	}
	p := r.URL.Path
	for _, pre := range noRedirectPrefixes {
		if strings.HasPrefix(p, pre) {
			return false
		}
	}
	for _, exact := range noRedirectPaths {
		if p == exact || strings.HasPrefix(p, exact+"/") {
			return false
		}
	}
	return true
}
