package api

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/config"
	dashmw "github.com/FairForge/vaultaire/internal/dashboard/middleware"
	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/FairForge/vaultaire/internal/hosts"
	"github.com/FairForge/vaultaire/internal/modes"
)

// WP-DOMAIN-1 part 3A (contract rule 11; docs/surfaces/DEVELOPER.md "The
// first ten" 1, 3 and 6): docs.stored.cloud serves the guides, the API
// reference and the files a developer's tool fetches — a closed list — and
// nothing of the app. A request carrying S3 credentials is a 404 there
// (Architecture Decision 8 as on the trust host), never a page, never the
// S3 API.

var docsHostNames = []string{"docs.stored.cloud", "docs.stored.ge", "docs.www.stored.ge"}

// docsHostList is every path the docs host serves, GET and HEAD.
var docsHostList = []string{
	"/", "/getting-started", "/rclone", "/faq", "/encryption",
	"/api", "/openapi.json", "/llms.txt", "/.well-known/security.txt", "/robots.txt", "/sitemap.xml",
	"/static/css/style.css", "/static/js/htmx.min.js", "/static/js/dashboard.js",
	"/static/fonts/montserrat.woff2", "/static/fonts/silk400.woff2", "/static/fonts/silk700.woff2",
}

// credentialShapes are the four ways a request carries S3 credentials
// (the gate's predicate: SigV4, SigV4A, SigV2 headers; a presigned query).
func credentialShapes(method, host, path string) map[string]*http.Request {
	out := map[string]*http.Request{}
	for name, auth := range map[string]string{
		"sigv4":  "AWS4-HMAC-SHA256 Credential=AKIA/20261101/us-east-1/s3/aws4_request, SignedHeaders=host;x-amz-date, Signature=00",
		"sigv4a": "AWS4-ECDSA-P256-SHA256 Credential=AKIA/20261101/s3/aws4_request, SignedHeaders=host, Signature=00",
		"sigv2":  "AWS AKIA:c2lnbmF0dXJl",
	} {
		r := httptest.NewRequest(method, path, nil)
		r.Host = host
		r.Header.Set("Authorization", auth)
		r.Header.Set("X-Amz-Date", "20261101T000000Z")
		out[name] = r
	}
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	r := httptest.NewRequest(method, path+sep+"X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Credential=AKIA%2F20261101&X-Amz-Signature=00", nil)
	r.Host = host
	out["presigned"] = r
	return out
}

func assertPixel404(t *testing.T, w *httptest.ResponseRecorder, msg string) {
	t.Helper()
	assert.Equal(t, http.StatusNotFound, w.Code, msg)
	assert.Equal(t, "no-store", w.Header().Get("Cache-Control"), msg)
	assert.True(t, strings.HasPrefix(w.Header().Get("Content-Type"), "text/html"), "%s: %s", msg, w.Header().Get("Content-Type"))
	assert.NotContains(t, w.Body.String(), "<Error>", "%s: never the S3 API's XML", msg)
}

// TestDocsHost_RouteListIsClosed lists every route the docs host serves; a
// new one must be added here on purpose (contract rule 11).
func TestDocsHost_RouteListIsClosed(t *testing.T) {
	t.Run("the walk is the list", func(t *testing.T) {
		s := &Server{logger: zap.NewNop(), router: chi.NewRouter(), config: &config.Config{}, hosts: prodHosts()}
		var got []string
		require.NoError(t, chi.Walk(s.newDocsRouter().(chi.Routes), func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
			got = append(got, method+" "+route)
			return nil
		}))
		sort.Strings(got)
		var want []string
		for _, p := range docsHostList {
			want = append(want, "GET "+p, "HEAD "+p)
		}
		sort.Strings(want)
		assert.Equal(t, want, got)
	})

	h := fullServer(t)
	t.Run("every listed path answers 200 on every docs host", func(t *testing.T) {
		for _, host := range docsHostNames {
			for _, p := range docsHostList {
				w := serve(h, hostRequest(http.MethodGet, host, p, false, true))
				assert.Equal(t, http.StatusOK, w.Code, "%s%s", host, p)
			}
		}
	})

	t.Run("everything else is the pixel 404, no-store", func(t *testing.T) {
		others := []struct{ method, path string }{
			{http.MethodGet, "/docs"}, {http.MethodGet, "/docs/rclone"}, {http.MethodGet, "/login"},
			{http.MethodGet, "/dashboard"}, {http.MethodGet, "/pricing"}, {http.MethodGet, "/status"},
			{http.MethodGet, "/my-bucket/key"}, {http.MethodGet, "/metrics"}, {http.MethodGet, "/health"},
			{http.MethodGet, "/api/v1/manage/buckets"}, {http.MethodGet, "/translog/checkpoint"},
			{http.MethodGet, "/static/css/other.css"}, {http.MethodGet, "/cdn/x/y/z"},
			{http.MethodPost, "/auth/login"}, {http.MethodPut, "/my-bucket/key"},
			{http.MethodDelete, "/rclone"}, {http.MethodPost, "/"}, {http.MethodOptions, "/api"},
		}
		for _, host := range docsHostNames {
			for _, c := range others {
				w := serve(h, hostRequest(c.method, host, c.path, false, true))
				assertPixel404(t, w, c.method+" "+host+c.path)
			}
		}
	})

	t.Run("a request with S3 credentials is a 404 on every path, listed or not", func(t *testing.T) {
		paths := append([]string{"/my-bucket/key", "/docs", "/x"}, docsHostList...)
		for _, host := range docsHostNames {
			for _, p := range paths {
				for name, req := range credentialShapes(http.MethodGet, host, p) {
					w := serve(h, req)
					assertPixel404(t, w, name+" "+host+p)
				}
			}
		}
	})

	t.Run("the canonical origin's /docs is unchanged and the S3 host is still the S3 API's", func(t *testing.T) {
		for slug, p := range docsPages {
			w := serve(h, hostRequest(http.MethodGet, "stored.cloud", p, false, true))
			assert.Equal(t, http.StatusOK, w.Code, slug)
		}
		w := serve(h, hostRequest(http.MethodGet, "s3.stored.cloud", "/rclone", true, false))
		assert.NotEqual(t, http.StatusOK, w.Code, "s3.stored.cloud/rclone is a bucket, never a guide")
		assert.NotContains(t, w.Body.String(), "<!DOCTYPE html>")
	})
}

// TestDocsHost_ServesTheSameBytesAsTheCanonicalDocs: one rendering, two
// addresses — and the HEAD shape of the canonical pages.
// TestDocsHost_SecurityHeaders (RP91-11): every docs-host answer carries the
// dashboard's security headers (middleware.SecurityHeaders), and its CSP
// lets swagger-ui load from unpkg.com on /api — only there.
func TestDocsHost_SecurityHeaders(t *testing.T) {
	h := fullServer(t)
	for _, host := range docsHostNames {
		for _, p := range append(append([]string{}, docsHostList...), "/no-such-guide") {
			w := serve(h, hostRequest(http.MethodGet, host, p, false, true))
			where := host + p
			assert.Equal(t, "DENY", w.Header().Get("X-Frame-Options"), where)
			assert.Equal(t, "nosniff", w.Header().Get("X-Content-Type-Options"), where)
			assert.Equal(t, "strict-origin-when-cross-origin", w.Header().Get("Referrer-Policy"), where)
			assert.Equal(t, "camera=(), microphone=(), geolocation=()", w.Header().Get("Permissions-Policy"), where)
			csp := w.Header().Get("Content-Security-Policy")
			require.NotEmpty(t, csp, where)
			assert.Contains(t, csp, "frame-ancestors 'none'", where)
			if p == "/api" {
				assert.Equal(t, docsAPIContentSecurityPolicy, csp, where)
				for _, d := range []string{"script-src", "style-src"} {
					assert.Regexp(t, d+" [^;]*https://unpkg.com", csp, where)
				}
			} else {
				assert.Equal(t, dashmw.ContentSecurityPolicy, csp, where)
				assert.NotContains(t, csp, "unpkg", where)
			}
		}
	}
}

func TestDocsHost_ServesTheSameBytesAsTheCanonicalDocs(t *testing.T) {
	h := fullServer(t)
	for slug, canonical := range docsPages {
		docsPath := docsHostPath(slug)
		c := serve(h, hostRequest(http.MethodGet, "stored.cloud", canonical, false, true))
		d := serve(h, hostRequest(http.MethodGet, "docs.stored.cloud", docsPath, false, true))
		require.Equal(t, http.StatusOK, c.Code, canonical)
		require.Equal(t, http.StatusOK, d.Code, docsPath)
		assert.Equal(t, c.Body.Bytes(), d.Body.Bytes(), "docs.stored.cloud%s == stored.cloud%s byte for byte", docsPath, canonical)
		assert.Equal(t, "public, max-age=300", d.Header().Get("Cache-Control"), docsPath)
		assert.Equal(t, c.Header().Get("Content-Type"), d.Header().Get("Content-Type"), docsPath)

		head := serve(h, hostRequest(http.MethodHead, "docs.stored.cloud", docsPath, false, true))
		assert.Equal(t, http.StatusOK, head.Code, docsPath)
		assert.Empty(t, head.Body.Bytes(), docsPath)
		assert.Equal(t, "public, max-age=300", head.Header().Get("Cache-Control"), docsPath)
	}
	// Since the owner's proof the pages link the docs host (hosts.DocsBase),
	// never the canonical /docs.
	d := serve(h, hostRequest(http.MethodGet, "docs.stored.cloud", "/", false, true))
	assert.Contains(t, d.Body.String(), `href="https://docs.stored.cloud/rclone"`)
	assert.NotContains(t, d.Body.String(), "https://stored.cloud/docs")
	for _, p := range []string{"/", "/pricing"} {
		body := serve(h, hostRequest(http.MethodGet, "stored.cloud", p, false, true)).Body.String()
		assert.Contains(t, body, `href="https://docs.stored.cloud`, "%s links the docs host", p)
		assert.NotContains(t, body, `href="https://stored.cloud/docs`, "%s never the canonical /docs", p)
	}
	md := serve(h, hostRequest(http.MethodGet, "stored.cloud", "/.well-known/oauth-authorization-server", false, false)).Body.String()
	assert.Contains(t, md, `"service_documentation":"https://docs.stored.cloud/api#agents"`)

	// Its own robots.txt and sitemap.
	r := serve(h, hostRequest(http.MethodGet, "docs.stored.cloud", "/robots.txt", false, false))
	assert.Contains(t, r.Body.String(), "Sitemap: https://docs.stored.cloud/sitemap.xml")
	sm := serve(h, hostRequest(http.MethodGet, "docs.stored.cloud", "/sitemap.xml", false, false)).Body.String()
	for _, p := range docsHostPaths() {
		assert.Contains(t, sm, "<loc>https://docs.stored.cloud"+p+"</loc>")
	}
	assert.NotContains(t, sm, "https://stored.cloud/")
}

// TestDocsHost_DevelopmentOriginKeepsTheOneOrigin: on a development origin
// there is no docs router; the lab keeps /docs on its one origin.
func TestDocsHost_DevelopmentOriginKeepsTheOneOrigin(t *testing.T) {
	dev := hosts.FromEnv(func(string) string { return "" }, 8000, nil)
	fallback := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) })
	docsRouter := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	for _, host := range []string{"localhost:8000", "docs.localhost", "docs.stored.cloud"} {
		w := serve(DocsHostRouter(dev, docsRouter, fallback), hostRequest(http.MethodGet, host, "/", false, true))
		assert.Equal(t, http.StatusTeapot, w.Code, host)
	}

	prev := hosts.Get()
	t.Cleanup(func() { hosts.Set(prev) })
	for _, k := range []string{"VAULTAIRE_PUBLIC_HOST", "VAULTAIRE_ENDPOINT", "VAULTAIRE_BASE_URL", "CDN_PUBLIC_HOST", "TRUST_PUBLIC_HOST", "DOCS_PUBLIC_HOST", "VAULTAIRE_ALIAS_HOSTS"} {
		t.Setenv(k, "")
	}
	srv := NewServer(&config.Config{Server: config.ServerConfig{Port: 0}}, zap.NewNop(),
		engine.NewEngine(nil, zap.NewNop(), &engine.Config{DefaultBackend: "local"}), nil, nil)
	w := serve(srv.httpServer.Handler, hostRequest(http.MethodGet, "localhost:8000", "/docs/encryption", false, true))
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Regexp(t, `href="http://localhost:[0-9]+/docs/faq"`, w.Body.String())
}

// TestEncryptionGuide_CopyRules holds the client-side encryption guide to
// contract rule 8: "cannot read" is said of restic, Kopia and rclone crypt
// only; never "zero-knowledge"; the mode words and Standard's sentence come
// from modes.json, as true now (with and without a master key).
func TestEncryptionGuide_CopyRules(t *testing.T) {
	prev := modes.MasterKeyLoaded()
	t.Cleanup(func() { modes.SetMasterKeyLoaded(prev) })
	tags := regexp.MustCompile(`<[^>]+>`)
	cannot := regexp.MustCompile(`(?i)(cannot|can't|can’t|can not) (read|decrypt|see)`)
	tool := regexp.MustCompile(`(?i)restic|kopia|rclone crypt`)
	standard, ok := modes.Get("standard")
	require.True(t, ok)
	private, ok := modes.Get("private")
	require.True(t, ok)

	for _, key := range []bool{false, true} {
		modes.SetMasterKeyLoaded(key)
		page := string(renderDocsPages(zap.NewNop(), prodHosts())["encryption"])
		text := tags.ReplaceAllString(page, " ")

		assert.Contains(t, page, "https://s3.stored.cloud", "the endpoint is the S3 host")
		assert.Contains(t, page, "--endpoint=s3.stored.cloud", "Kopia takes the bare host")
		assert.NotContains(t, page, "stored.ge")
		assert.NotRegexp(t, `__[A-Z][A-Z0-9_]*__`, page, "every token expanded")
		assert.NotRegexp(t, `(?i)zero[- ‑]?knowledge`, page)
		assert.Contains(t, text, standard.Name)
		assert.Contains(t, text, private.Name)
		assert.Contains(t, page, standard.Customer.Render(key), "Standard's sentence, as true now (key=%v)", key)
		if !key {
			assert.NotContains(t, page, standard.Customer.Text, "no 'at rest with keys we hold' without a master key")
		}
		for _, sentence := range regexp.MustCompile(`[.!?]\s+`).Split(text, -1) {
			if cannot.MatchString(sentence) {
				assert.Regexp(t, tool, sentence, "a 'cannot read' names restic, Kopia or rclone crypt: %q", sentence)
			}
		}
		// The proof's numbers (bench-results/RESTIC-KOPIA-E2EE-2026-11-01.md).
		for _, n := range []string{"0 of 98", "3,043 files", "restic 0.18.1", "Kopia 0.23.1", "52", "46"} {
			assert.Contains(t, text, n)
		}
	}

	// The source types no mode word (TestModeNamesOnlyFromModesJSON reads it).
	assert.NotRegexp(t, `\b(Private|Standard|Public)\b`, string(docsEncryptionMD))
}
