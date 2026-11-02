package api

import (
	"net/http"
	"net/url"

	"github.com/go-chi/chi/v5"

	"github.com/FairForge/vaultaire/internal/dashboard"
	"github.com/FairForge/vaultaire/internal/hosts"
)

// WP-DOMAIN-1 part 3C-1 (contract rule 11: `app.` on stored.cloud): the
// dashboard gets a host of its own. app.stored.cloud serves exactly the routes
// dashboard.RegisterRoutes mounts — on a fresh router, with the canonical
// origin's sessions, lockout and pending-2FA store (dashboardDeps) — plus the
// agent consent's /oauth/authorize, the authorization-server metadata (its
// issuer stays the canonical origin, RFC 8414 § 3.3) and security.txt. `/`
// sends to /dashboard. Everything else — the landing, the docs, the
// management and auth JSON APIs, the S3 API — is the pixel 404, never cached,
// and a request carrying S3 credentials is a 404 on every path (Architecture
// Decision 8 as on the trust and docs hosts). Sessions stay __Host- cookies
// with no Domain attribute (internal/dashboard/auth): a session set on app.
// is app.'s alone, and stored.cloud's never reaches app.
//
// A social sign-in (/auth/google, /auth/github) STARTS on the canonical
// origin: its callback is VAULTAIRE_BASE_URL's and the flow's state cookie is
// host-only, so a start on app. could never finish. Dark: nothing links to
// app. until the owner's proof (hosts.AppBase stays the canonical origin).

// AppHostRouter routes requests on an app host (hosts.IsAppHost) to appRouter
// when the canonical origin is a public name; every other host, and every
// host on a development origin, goes to fallback.
func AppHostRouter(h hosts.Hosts, appRouter, fallback http.Handler) http.Handler {
	if !h.AppPublic() {
		return fallback
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h.IsAppHost(r.Host) {
			appRouter.ServeHTTP(w, r)
			return
		}
		fallback.ServeHTTP(w, r)
	})
}

// socialSignInStarts are the dashboard's OAuth entrances (their callbacks
// are VAULTAIRE_BASE_URL's).
var socialSignInStarts = map[string]bool{"/auth/google": true, "/auth/github": true}

// newAppRouter is what an app host serves. Its middleware is the docs
// router's (a request id, the request limits, the version header, the access
// log) plus the app gate.
func (s *Server) newAppRouter() http.Handler {
	h := s.hostSet()
	r := chi.NewRouter()
	r.Use(s.requestIDMiddleware, s.requestLimitsMiddleware, s.versionMiddleware, s.loggingMiddleware, appGate(h))
	r.NotFound(docsNotFound)
	r.MethodNotAllowed(docsNotFound)

	dashboard.RegisterRoutes(r, s.dashboardDeps())

	toDashboard := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
	}
	r.Get("/", toDashboard)
	r.Head("/", toDashboard)
	for path, fn := range map[string]http.HandlerFunc{
		"/oauth/authorize":                        s.handleOAuthAuthorize,
		"/.well-known/oauth-authorization-server": s.handleOAuthMetadata,
		"/.well-known/security.txt":               s.handleSecurityTxt,
	} {
		r.Get(path, fn)
		r.Head(path, fn)
	}
	return r
}

// appGate runs before every app-host route: a request carrying S3
// credentials is the pixel 404 (never a page, never the S3 API), and a social
// sign-in starts where its callback lands (socialSignInHome).
func appGate(h hosts.Hosts) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		home := socialSignInHome(h)(next)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if signedS3Request(r) {
				docsNotFound(w, r)
				return
			}
			home.ServeHTTP(w, r)
		})
	}
}

// socialSignInHome sends a social sign-in's START (/auth/google,
// /auth/github, GET or HEAD) to AppBase's host when it arrives on another
// dashboard host (303, no-store): its callback is VAULTAIRE_BASE_URL's and the
// flow's state cookie is host-only, so a start anywhere else could never
// finish. Mounted on the app router and, for the web hosts, on the canonical
// router (part 3C-2: with VAULTAIRE_BASE_URL on app., stored.cloud's start
// goes there).
func socialSignInHome(h hosts.Hosts) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if socialSignInStarts[r.URL.Path] && (r.Method == http.MethodGet || r.Method == http.MethodHead) &&
				!h.IsAppBaseHost(r.Host) && (h.IsWebHost(r.Host) || h.IsAppHost(r.Host)) {
				w.Header().Set("Cache-Control", "no-store")
				// #nosec G710 -- not an open redirect: scheme and host are the configured VAULTAIRE_BASE_URL; the path is one of two fixed paths
				http.Redirect(w, r, appBaseWithQuery(h, r), http.StatusSeeOther)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func appBaseWithQuery(h hosts.Hosts, r *http.Request) string {
	base := h.AppBase()
	u, err := url.Parse(base)
	if err != nil {
		return base + r.URL.Path
	}
	u.Path = r.URL.Path
	u.RawQuery = r.URL.RawQuery
	return u.String()
}

// consentURL is where /oauth/authorize sends the person: the dashboard's
// consent page, on AppBase's host when the request came to another of our
// dashboard hosts (the canonical origin, an alias, an app host) so it runs
// where their session lives; relative otherwise (already there, or a host
// that is none of ours — a lab, a test).
func consentURL(h hosts.Hosts, r *http.Request) string {
	target := "/dashboard/agents/connect?" + r.URL.RawQuery
	if h.IsAppBaseHost(r.Host) || !(h.IsWebHost(r.Host) || h.IsAppHost(r.Host)) {
		return target
	}
	return h.AppBase() + target
}
