package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/FairForge/vaultaire/internal/dashboard"
	"github.com/FairForge/vaultaire/internal/hosts"
	"github.com/FairForge/vaultaire/internal/translog"
)

// TrustHostRouter routes requests on a trust host (hosts.IsTrustHost:
// TRUST_PUBLIC_HOST, trust.<canonical>, trust.<alias>, trust.stored.cloud and
// trust.stored.ge whatever the environment) to trust when the canonical origin
// is a public name (RP42-2, Stage 1a Prompt 33). Contract rule 11 makes
// trust. its own surface: it serves the transparency log and nothing of the
// app — no landing page, no dashboard login, no S3 API. Every other host,
// and every host on a development origin, goes to fallback (labs unchanged).
func TrustHostRouter(h hosts.Hosts, trust, fallback http.Handler) http.Handler {
	if !h.TrustPublic() {
		return fallback
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h.IsTrustHost(r.Host) {
			trust.ServeHTTP(w, r)
			return
		}
		fallback.ServeHTTP(w, r)
	})
}

// newTrustRouter is what a trust host serves: GET and HEAD of
// /translog/checkpoint, /translog/tile/… and /translog/anchor/<size>.ots
// (K4c-3a) (behind translog_public, a
// signed or presigned S3 request 404), /legal/law-enforcement and
// /legal/what-we-can-produce with their layout's six static files (43.4,
// behind legal_le_pages), /.well-known/security.txt, and a 404
// for everything else — any path, any method, signed or not (D-P33-6: a 404,
// not a 301 to the canonical origin; no trust page exists yet). Its own
// middleware is the main router's minus the alias redirect and the site
// statistics: a request id, the request limits, the version header, the
// access log.
func (s *Server) newTrustRouter() http.Handler {
	r := chi.NewRouter()
	r.Use(s.requestIDMiddleware, s.requestLimitsMiddleware, s.versionMiddleware, s.loggingMiddleware)
	r.NotFound(trustNotFound)
	r.MethodNotAllowed(trustNotFound)
	if s.translog != nil {
		h := http.StripPrefix("/translog/", translog.NewHandler(s.translog))
		serve := func(w http.ResponseWriter, r *http.Request) {
			if signedS3Request(r) || s.flags == nil || !s.flags.Enabled(flagTranslogPublic, "") {
				trustNotFound(w, r)
				return
			}
			h.ServeHTTP(w, r)
		}
		for _, p := range []string{"/translog/checkpoint", "/translog/tile/*", "/translog/anchor/*"} {
			r.Get(p, serve)
			r.Head(p, serve)
		}
	}
	// 43.4: the law-enforcement policy and "what we can produce" with the
	// files their layout loads, behind legal_le_pages (OFF = this 404).
	dashboard.NewLegalLE(s.flags).Mount(r, trustNotFound, true)
	r.Get("/.well-known/security.txt", s.handleSecurityTxt)
	r.Head("/.well-known/security.txt", s.handleSecurityTxt)
	return r
}

// trustNotFound is the trust host's 404: plain text, never cached (a 404
// served while translog_public is OFF must not outlive the flip at an edge).
func trustNotFound(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusNotFound)
	_, _ = w.Write([]byte("404 not found: this host serves the transparency log (/translog/checkpoint, /translog/tile/…, /translog/anchor/…) and its legal pages only\n"))
}
