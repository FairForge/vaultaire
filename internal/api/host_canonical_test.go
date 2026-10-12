package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/config"
	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/FairForge/vaultaire/internal/hosts"
)

// prodHosts is the owner click-list's .env (WP-DOMAIN-1).
func prodHosts() hosts.Hosts {
	return hosts.Hosts{
		Canonical: "https://stored.cloud",
		S3:        "https://s3.stored.cloud",
		CDN:       "https://cdn.stored.cloud",
		BaseURL:   "https://stored.cloud",
		Aliases:   []string{"stored.ge", "www.stored.ge", "www.stored.cloud"},
	}
}

func canonicalHandler(t *testing.T) http.Handler {
	t.Helper()
	return hostCanonicalMiddleware(prodHosts())(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot) // "served, not redirected"
	}))
}

func browserGET(host, target string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, target, nil)
	r.Host = host
	r.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	return r
}

func TestHostCanonical_RedirectsAliasBrowserGET(t *testing.T) {
	h := canonicalHandler(t)
	for _, tc := range []struct{ host, target, want string }{
		{"stored.ge", "/", "https://stored.cloud/"},
		{"stored.ge:443", "/pricing?utm_source=let", "https://stored.cloud/pricing?utm_source=let"},
		{"www.stored.ge", "/legal/privacy", "https://stored.cloud/legal/privacy"},
		{"www.stored.cloud", "/dashboard", "https://stored.cloud/dashboard"},
	} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, browserGET(tc.host, tc.target))
		assert.Equal(t, http.StatusMovedPermanently, w.Code, tc.host+tc.target)
		assert.Equal(t, tc.want, w.Header().Get("Location"), tc.host+tc.target)
		assert.Equal(t, "no-store", w.Header().Get("Cache-Control"), "the edge must never cache a redirect for a path the S3 API also serves")
	}

	// HEAD from a browser-like client redirects too.
	r := browserGET("stored.ge", "/")
	r.Method = http.MethodHead
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	assert.Equal(t, http.StatusMovedPermanently, w.Code)

	// The canonical host itself is served.
	w = httptest.NewRecorder()
	h.ServeHTTP(w, browserGET("stored.cloud", "/"))
	assert.Equal(t, http.StatusTeapot, w.Code)
}

func TestHostCanonical_NeverRedirectsS3(t *testing.T) {
	h := canonicalHandler(t)
	cases := map[string]func() *http.Request{
		"SigV4 Authorization header": func() *http.Request {
			r := browserGET("stored.ge", "/bucket/key")
			r.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=AK/20261012/us-east-1/s3/aws4_request, SignedHeaders=host, Signature=abc")
			return r
		},
		"presigned query (credential)": func() *http.Request {
			return browserGET("stored.ge", "/bucket/key?X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Credential=AK%2F20261012")
		},
		"presigned query (signature)": func() *http.Request {
			return browserGET("stored.ge", "/bucket/key?X-Amz-Signature=deadbeef")
		},
		"presigned v2 query": func() *http.Request {
			return browserGET("stored.ge", "/bucket/key?AWSAccessKeyId=AK&Signature=x&Expires=1")
		},
		"x-amz-* header": func() *http.Request {
			r := browserGET("stored.ge", "/bucket/key")
			r.Header.Set("X-Amz-Content-Sha256", "UNSIGNED-PAYLOAD")
			return r
		},
		"PUT": func() *http.Request {
			r := browserGET("stored.ge", "/bucket/key")
			r.Method = http.MethodPut
			return r
		},
		"POST": func() *http.Request {
			r := browserGET("stored.ge", "/bucket?delete")
			r.Method = http.MethodPost
			return r
		},
		"an S3 client's GET (no text/html)": func() *http.Request {
			r := httptest.NewRequest(http.MethodGet, "/bucket/key", nil)
			r.Host = "stored.ge"
			return r
		},
		"anything on s3.stored.ge":    func() *http.Request { return browserGET("s3.stored.ge", "/") },
		"anything on s3.stored.cloud": func() *http.Request { return browserGET("s3.stored.cloud", "/dashboard") },
	}
	for name, mk := range cases {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, mk())
		assert.Equal(t, http.StatusTeapot, w.Code, name)
		assert.Empty(t, w.Header().Get("Location"), name)
	}
}

func TestHostCanonical_SkipsHealthWellKnownWebhook(t *testing.T) {
	h := canonicalHandler(t)
	for _, p := range []string{
		"/health", "/health/ready", "/version", "/metrics",
		"/.well-known/security.txt", "/webhook/stripe", "/api/v1/user",
		"/cdn/slug/bucket/a.html", "/auth/google/callback?code=x&state=y",
	} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, browserGET("stored.ge", p))
		assert.Equal(t, http.StatusTeapot, w.Code, p)
	}
}

func TestHostCanonical_NoAliasesIsANoOp(t *testing.T) {
	h := hostCanonicalMiddleware(hosts.Defaults(8000))(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, browserGET("stored.ge", "/"))
	assert.Equal(t, http.StatusTeapot, w.Code)
}

// The middleware is wired into the server: a browser GET on the alias is a
// 301, the same GET signed by an S3 client is served, and the CDN host
// (routed before the main router) still reaches the CDN handler.
func TestHostCanonical_WiredIntoServer(t *testing.T) {
	prev := hosts.Get()
	t.Cleanup(func() { hosts.Set(prev) })
	t.Setenv("VAULTAIRE_PUBLIC_HOST", "https://stored.cloud")
	t.Setenv("VAULTAIRE_ENDPOINT", "https://s3.stored.cloud")
	t.Setenv("VAULTAIRE_BASE_URL", "https://stored.cloud")
	t.Setenv("CDN_PUBLIC_HOST", "https://cdn.stored.cloud")
	t.Setenv("VAULTAIRE_ALIAS_HOSTS", "stored.ge,www.stored.ge,www.stored.cloud")

	srv := NewServer(&config.Config{Server: config.ServerConfig{Port: 0}}, zap.NewNop(),
		engine.NewEngine(nil, zap.NewNop(), &engine.Config{DefaultBackend: "local"}), nil, nil)
	require.NotNil(t, srv.httpServer)
	assert.Equal(t, "https://stored.cloud", srv.hosts.Canonical)
	assert.Equal(t, srv.hosts, hosts.Get(), "the server installs its hosts for templates and copy")

	w := httptest.NewRecorder()
	srv.httpServer.Handler.ServeHTTP(w, browserGET("stored.ge", "/"))
	assert.Equal(t, http.StatusMovedPermanently, w.Code)
	assert.Equal(t, "https://stored.cloud/", w.Header().Get("Location"))
	assert.NotEmpty(t, w.Header().Get("X-Request-Id"), "outermost after requestIDMiddleware")
	assert.Equal(t, "Stored", w.Header().Get("Server"))

	w = httptest.NewRecorder()
	srv.httpServer.Handler.ServeHTTP(w, browserGET("stored.ge", "/.well-known/security.txt"))
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "Contact: mailto:security@stored.cloud")
}
