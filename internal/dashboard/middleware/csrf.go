package middleware

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"net"
	"net/http"
	"net/url"
	"strings"

	dashauth "github.com/FairForge/vaultaire/internal/dashboard/auth"
)

type csrfKey struct{}

// csrfKeyLabel separates the CSRF key from every other use of the secret it
// is derived from. Changing it invalidates the token of every open page.
const csrfKeyLabel = "stored.ge dashboard csrf token key v1"

// legacyCSRFCookie is the cookie the old double-submit scheme set. Nothing
// reads it any more; a browser that still sends it is told to drop it.
const legacyCSRFCookie = "csrf_token"

// DeriveCSRFKey derives the dashboard's CSRF key from a persistent server
// secret (prod: JWT_SECRET — the one secret every deployment must set):
// HMAC-SHA256(secret, label). The secret itself is never used as the key, so
// a CSRF token is not a MAC under the key that signs JWTs and verification
// links. The key is the same after every restart — the box redeploys several
// times a day and a form opened before a deploy must still post after it.
//
// With no secret (tests, a dev server without JWT_SECRET) the key is random:
// tokens then live as long as the process.
func DeriveCSRFKey(secret string) []byte {
	if secret == "" {
		k := make([]byte, sha256.Size)
		if _, err := rand.Read(k); err != nil {
			panic("csrf: failed to generate a random key")
		}
		return k
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(csrfKeyLabel))
	return mac.Sum(nil)
}

// NewCSRF protects the state-changing methods of the routes behind it
// (WP-R12-5, Review R12-19). It runs after the session middleware.
//
// The token is HMAC-SHA256(key, session id), hex: bound to the session, so a
// token minted for one session is refused for another and changes whenever
// the session does (sign-in, the second factor, sign-out and in again), and
// unforgeable without the key. It is carried by the page — the hidden
// `csrf_token` field of every form, the `<meta name="csrf-token">` htmx sends
// as X-CSRF-Token — and by nothing else: there is NO cookie. The scheme this
// replaces compared the submitted value with a bare random cookie; whoever
// could set a cookie for the registrable domain (any page on a `*.stored.ge`
// host) chose both halves.
//
// On POST, PUT, PATCH and DELETE, in this order:
//
//  1. Sec-Fetch-Site, when the browser sends it, must be `same-origin` or
//     `none` (typed address, bookmark). `cross-site` is another site;
//     `same-site` is a sibling subdomain — cdn.stored.ge serves customer
//     content — and is refused as well.
//  2. Origin, when present, must be one of the dashboard's own origins: the
//     ones passed here (VAULTAIRE_BASE_URL) or `https://` + the request's
//     Host (the dashboard answers on more than one name); plain `http://` +
//     Host only on a loopback host (local development). `null` is refused.
//  3. The token must match, compared in constant time.
//
// A request with neither header (curl, a browser older than 2020) is decided
// by the token alone. The two header checks never replace it.
func NewCSRF(key []byte, origins ...string) func(http.Handler) http.Handler {
	allowed := map[string]bool{}
	for _, o := range origins {
		if n := normalizeOrigin(o); n != "" {
			allowed[n] = true
		}
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token := sessionCSRFToken(key, r)
			if _, err := r.Cookie(legacyCSRFCookie); err == nil {
				http.SetCookie(w, &http.Cookie{Name: legacyCSRFCookie, Value: "", Path: "/", MaxAge: -1,
					HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode})
			}

			// The token for handlers and templates.
			r = r.WithContext(context.WithValue(r.Context(), csrfKey{}, token))

			// Safe methods pass through without validation.
			if r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions {
				next.ServeHTTP(w, r)
				return
			}

			if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
				http.Error(w, "Cross-site request refused", http.StatusForbidden)
				return
			}
			if origin := r.Header.Get("Origin"); origin != "" && !originAllowed(origin, r.Host, allowed) {
				http.Error(w, "Cross-site request refused", http.StatusForbidden)
				return
			}

			submitted := r.Header.Get("X-CSRF-Token")
			if submitted == "" {
				submitted = r.FormValue("csrf_token")
			}
			if token == "" || subtle.ConstantTimeCompare([]byte(submitted), []byte(token)) != 1 {
				http.Error(w, "CSRF token invalid — reload the page and try again", http.StatusForbidden)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

// sessionCSRFToken is the token of the request's session; "" when the
// request carries no session cookie (never the MAC of an empty string — an
// empty token validates nothing).
func sessionCSRFToken(key []byte, r *http.Request) string {
	c, err := r.Cookie(dashauth.SessionCookieName)
	if err != nil || c.Value == "" {
		return ""
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(c.Value))
	return hex.EncodeToString(mac.Sum(nil))
}

// normalizeOrigin reduces a URL or an Origin header to scheme://host[:port],
// lower-cased; "" when it is not an http(s) origin.
func normalizeOrigin(s string) string {
	u, err := url.Parse(strings.TrimSpace(s))
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return ""
	}
	return strings.ToLower(u.Scheme + "://" + u.Host)
}

func originAllowed(origin, host string, allowed map[string]bool) bool {
	o := normalizeOrigin(origin)
	if o == "" {
		return false // "null", or not an origin at all
	}
	if allowed[o] {
		return true
	}
	host = strings.ToLower(host)
	if o == "https://"+host {
		return true
	}
	return o == "http://"+host && isLoopbackHost(host)
}

func isLoopbackHost(hostport string) bool {
	h := hostport
	if hh, _, err := net.SplitHostPort(hostport); err == nil {
		h = hh
	}
	if h == "localhost" {
		return true
	}
	ip := net.ParseIP(strings.Trim(h, "[]"))
	return ip != nil && ip.IsLoopback()
}

// Token returns the CSRF token from the request context.
func Token(ctx context.Context) string {
	if v, ok := ctx.Value(csrfKey{}).(string); ok {
		return v
	}
	return ""
}
