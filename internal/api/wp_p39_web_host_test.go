package api

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/account"
	"github.com/FairForge/vaultaire/internal/api/landing"
	"github.com/FairForge/vaultaire/internal/config"
	"github.com/FairForge/vaultaire/internal/dashboard"
	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/FairForge/vaultaire/internal/flags"
	"github.com/FairForge/vaultaire/internal/hosts"
	"github.com/FairForge/vaultaire/internal/modes"
	"github.com/FairForge/vaultaire/internal/testutil"
)

// Stage 1a Prompt 39 Part A: the web host answers like a website. Before it,
// every unmatched path on every host was the S3 API's, so a visitor's typo
// on stored.cloud — /pricing, /signup, /legal/refunds — got 403 AccessDenied
// XML. Now an ANONYMOUS browser page view (shouldRedirectToCanonical's
// predicate: GET/HEAD, Accept text/html, no Authorization, no presigned
// query, no x-amz-* header) on a web host gets the pixel 404 page; every
// other request, and every request on an s3./cdn./trust. host, gets exactly
// what it got before.

// prodTodayHosts is prod's .env on 2026-10-24: VAULTAIRE_BASE_URL and
// VAULTAIRE_ENDPOINT = https://stored.ge, nothing else. stored.cloud is then
// neither the canonical host nor an alias — and answers all the same.
var prodTodayHosts = map[string]string{
	"VAULTAIRE_BASE_URL": "https://stored.ge",
	"VAULTAIRE_ENDPOINT": "https://stored.ge",
}

// prodTargetHosts is WP-DOMAIN-1's target .env.
var prodTargetHosts = map[string]string{
	"VAULTAIRE_PUBLIC_HOST": "https://stored.cloud",
	"VAULTAIRE_ENDPOINT":    "https://s3.stored.cloud",
	"VAULTAIRE_BASE_URL":    "https://stored.cloud",
	"CDN_PUBLIC_HOST":       "https://cdn.stored.cloud",
	"VAULTAIRE_ALIAS_HOSTS": "stored.ge,www.stored.ge,www.stored.cloud",
}

// webServer is NewServer on the test database (anonymous requests reach the
// auth check only with a database: without one the S3 handler treats them
// as the test tenant). on models a flag ON with the fixture's own default —
// never a '*' row of a real flag.
func webServer(t *testing.T, env map[string]string, flagsOn ...string) http.Handler {
	t.Helper()
	db, err := sql.Open("postgres", testutil.DSN())
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Ping(); err != nil {
		t.Skipf("test database unreachable (%v) — make test-db", err)
	}
	prev := hosts.Get()
	t.Cleanup(func() { hosts.Set(prev) })
	for _, k := range []string{"VAULTAIRE_PUBLIC_HOST", "VAULTAIRE_ENDPOINT", "VAULTAIRE_BASE_URL", "CDN_PUBLIC_HOST", "TRUST_PUBLIC_HOST", "VAULTAIRE_ALIAS_HOSTS"} {
		t.Setenv(k, env[k])
	}
	srv := NewServer(&config.Config{Server: config.ServerConfig{Port: 0}}, zap.NewNop(),
		engine.NewEngine(nil, zap.NewNop(), &engine.Config{DefaultBackend: "local"}), nil, db)
	require.NotNil(t, srv.flags)
	for _, k := range flagsOn {
		srv.flags.Register(k, true)
	}
	return srv.httpServer.Handler
}

type webReq struct {
	method, host, path, accept string
	header                     map[string]string
}

func (q webReq) do(h http.Handler) *httptest.ResponseRecorder {
	method := q.method
	if method == "" {
		method = http.MethodGet
	}
	req := httptest.NewRequest(method, q.path, nil)
	req.Host = q.host
	if q.accept != "" {
		req.Header.Set("Accept", q.accept)
	}
	for k, v := range q.header {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

// browserAccept is what a browser sends for a page view.
const browserAccept = "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8"

func assertWeb404(t *testing.T, w *httptest.ResponseRecorder, msg string) {
	t.Helper()
	assert.Equal(t, http.StatusNotFound, w.Code, msg)
	assert.True(t, strings.HasPrefix(w.Header().Get("Content-Type"), "text/html"), "%s: %s", msg, w.Header().Get("Content-Type"))
	assert.Equal(t, "no-store", w.Header().Get("Cache-Control"), msg)
	assert.NotContains(t, w.Body.String(), "<Error>", msg)
}

func assertS3XML(t *testing.T, w *httptest.ResponseRecorder, msg string) {
	t.Helper()
	assert.Equal(t, http.StatusForbidden, w.Code, msg)
	assert.Contains(t, w.Body.String(), "<Error>", msg)
	assert.NotContains(t, w.Body.String(), `id="web-404"`, msg)
}

func TestWebHost_BrowserTypoIsAnHTML404(t *testing.T) {
	for _, tc := range []struct {
		name  string
		env   map[string]string
		hosts []string
	}{
		{"prod today", prodTodayHosts, []string{"stored.cloud", "stored.ge", "www.stored.cloud", "www.stored.ge", "stored.pics"}},
		{"prod target", prodTargetHosts, []string{"stored.cloud"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := webServer(t, tc.env)
			for _, host := range tc.hosts {
				for _, p := range []string{"/nonsense", "/pricing-page", "/a/b/c.html"} {
					w := webReq{host: host, path: p, accept: browserAccept}.do(h)
					assertWeb404(t, w, host+p)
					assert.Contains(t, w.Body.String(), `id="web-404"`, host+p)
					assert.Contains(t, w.Header().Get("Content-Security-Policy"), "default-src 'self'", "the dashboard's security headers")
				}
				head := webReq{method: http.MethodHead, host: host, path: "/nonsense", accept: browserAccept}.do(h)
				assertWeb404(t, head, "HEAD "+host)
				assert.Empty(t, head.Body.String(), "HEAD has no body")
			}
		})
	}
}

// Anything that could be an S3 client — and every request on an s3., cdn.
// or trust. host — gets exactly what it got before.
func TestWebHost_S3ClientsAndOtherHostsAreUnchanged(t *testing.T) {
	h := webServer(t, prodTargetHosts)
	for _, tc := range []struct {
		name string
		q    webReq
	}{
		{"curl's Accept", webReq{host: "stored.cloud", path: "/nonsense", accept: "*/*"}},
		{"no Accept", webReq{host: "stored.cloud", path: "/nonsense"}},
		{"an unsigned x-amz header", webReq{host: "stored.cloud", path: "/nonsense", accept: browserAccept, header: map[string]string{"X-Amz-Content-Sha256": "UNSIGNED-PAYLOAD"}}},
		{"an Authorization header", webReq{host: "stored.cloud", path: "/nonsense", accept: browserAccept, header: map[string]string{"Authorization": "AWS4-HMAC-SHA256 Credential=VLT_NOPE/20261024/us-east-1/s3/aws4_request, SignedHeaders=host, Signature=00"}}},
		{"a presigned query", webReq{host: "stored.cloud", path: "/nonsense/key?X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Credential=VLT_NOPE%2F20261024%2Fus-east-1%2Fs3%2Faws4_request&X-Amz-Date=20261024T000000Z&X-Amz-Expires=60&X-Amz-SignedHeaders=host&X-Amz-Signature=00", accept: browserAccept}},
		{"the s3 host", webReq{host: "s3.stored.cloud", path: "/nonsense", accept: browserAccept}},
		{"a PUT", webReq{method: http.MethodPut, host: "stored.cloud", path: "/nonsense/key", accept: browserAccept}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertS3XML(t, tc.q.do(h), tc.name)
		})
	}
	// The CDN and trust hosts keep their own answers (never the web page).
	for _, host := range []string{"cdn.stored.cloud", "trust.stored.cloud"} {
		w := webReq{host: host, path: "/nonsense", accept: browserAccept}.do(h)
		assert.NotContains(t, w.Body.String(), `id="web-404"`, host)
	}
}

// The two paths a launch reader types: browser page views only.
// /pricing itself is a page since WP-PRICE-1 (pricing_page_test.go).
func TestWebHost_SignupAndPricingRedirect(t *testing.T) {
	for _, env := range []map[string]string{prodTodayHosts, prodTargetHosts} {
		h := webServer(t, env)
		for path, want := range map[string]string{"/signup": "/register", "/pricing/": "/pricing"} {
			w := webReq{host: "stored.cloud", path: path, accept: browserAccept}.do(h)
			assert.Equal(t, http.StatusMovedPermanently, w.Code, path)
			assert.Equal(t, want, w.Header().Get("Location"), path)
			assert.Equal(t, "no-store", w.Header().Get("Cache-Control"), path)
			// An S3 client asking for a bucket of that name gets the S3 API.
			assertS3XML(t, webReq{host: "stored.cloud", path: path, accept: "*/*"}.do(h), "curl "+path)
		}
		assertS3XML(t, webReq{host: "s3.stored.cloud", path: "/pricing", accept: browserAccept}.do(h), "s3 host /pricing")
	}
}

// --- /legal/refunds and /legal/dmca, dark behind legal_draft_pages --------

var legalDraftPaths = []string{"/legal/refunds", "/legal/dmca"}

func TestLegalDraftPages_FlagOffIsTheWeb404(t *testing.T) {
	for _, env := range []map[string]string{prodTodayHosts, prodTargetHosts} {
		h := webServer(t, env)
		for _, p := range legalDraftPaths {
			assertWeb404(t, webReq{host: "stored.cloud", path: p, accept: browserAccept}.do(h), p)
			if env["VAULTAIRE_ALIAS_HOSTS"] == "" { // an alias 301s to the canonical origin first
				assertWeb404(t, webReq{host: "stored.ge", path: p, accept: browserAccept}.do(h), "stored.ge"+p)
			}
		}
	}
	// Anything else at these paths is the S3 API's, flag ON or OFF: a GetObject
	// of key "refunds" in a bucket named "legal" (the lab found the dashboard
	// mount shadowing it on every host).
	for _, on := range [][]string{nil, {dashboard.FlagLegalDraftPages}} {
		h := webServer(t, prodTargetHosts, on...)
		for _, p := range legalDraftPaths {
			for _, q := range []webReq{
				{host: "stored.cloud", path: p, accept: "*/*"},
				{host: "s3.stored.cloud", path: p, accept: browserAccept},
				{host: "stored.cloud", path: p, accept: browserAccept, header: map[string]string{"Authorization": "AWS4-HMAC-SHA256 Credential=VLT_NOPE/20261024/us-east-1/s3/aws4_request, SignedHeaders=host, Signature=00"}},
			} {
				assertS3XML(t, q.do(h), fmt.Sprintf("%v %s%s %s", on, q.host, p, q.accept))
			}
		}
	}
	// The trust host's route list is closed: neither page is on it.
	h := webServer(t, prodTargetHosts, dashboard.FlagLegalDraftPages)
	for _, p := range legalDraftPaths {
		w := webReq{host: "trust.stored.cloud", path: p, accept: browserAccept}.do(h)
		assert.Equal(t, http.StatusNotFound, w.Code, p)
		assert.NotContains(t, w.Body.String(), "DRAFT", p)
	}
}

func TestLegalDraftPages_FlagOnRendersOnlyCodeFactsAndDrafts(t *testing.T) {
	h := webServer(t, prodTargetHosts, dashboard.FlagLegalDraftPages)
	bodies := map[string]string{}
	for _, p := range legalDraftPaths {
		w := webReq{host: "stored.cloud", path: p, accept: browserAccept}.do(h)
		require.Equal(t, http.StatusOK, w.Code, p)
		assert.Contains(t, w.Header().Get("Content-Type"), "text/html")
		b := w.Body.String()
		bodies[p] = b
		assert.Contains(t, b, "<strong>DRAFT.</strong>", "the F-36 banner: %s", p)
		assert.Contains(t, b, `href="https://stored.cloud/legal/privacy"`, "footer: the canonical origin's privacy page")
		assert.Contains(t, b, `href="https://stored.cloud/legal/terms"`, "footer: the canonical origin's terms")
	}
	refunds, dmca := bodies["/legal/refunds"], bodies["/legal/dmca"]
	// Refunds: the owner's words are DRAFT (F-28); the facts are code's.
	assert.Contains(t, refunds, "F-28")
	assert.Contains(t, refunds, fmt.Sprintf("%d days", int(account.GracePeriod.Hours()/24)), "the deletion grace, internal/account")
	e := landing.Get().Egress
	assert.Contains(t, refunds, fmt.Sprintf("%g", e.StandardFreeRatio), "the egress ratios, prices.json")
	assert.Contains(t, refunds, fmt.Sprintf("%g", e.VaultRestoreFreeRatio))
	assert.Contains(t, refunds, e.PastAllowance, "prices.json's one wording for past the allowance")
	assert.Contains(t, refunds, "/legal/terms#billing", "what the Terms say today is linked, never restated")
	// DMCA: the agent is the owner's (F-37); the form and the modes are code's.
	assert.Contains(t, dmca, "F-37")
	assert.Contains(t, dmca, `href="/abuse"`)
	assert.Contains(t, dmca, hosts.Get().AbuseEmail())
	for _, m := range modes.All().Modes {
		assert.Contains(t, dmca, m.Name, "mode names from modes.json")
	}
	// Every paragraph that is not a code fact is a marked DRAFT.
	for p, b := range bodies {
		for _, para := range strings.Split(b, "<p>")[1:] {
			if strings.Contains(para, "data-fact") {
				continue
			}
			assert.Contains(t, para, "DRAFT", "%s: an unmarked paragraph: %.120s", p, para)
		}
	}
}

func TestLegalDraftPagesFlag_DefaultOffGlobalOnly(t *testing.T) {
	db, err := sql.Open("postgres", testutil.DSN())
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	if err := db.Ping(); err != nil {
		t.Skipf("test database unreachable (%v) — make test-db", err)
	}
	fl := flags.New(db, nil)
	registerFlags(fl)
	assert.False(t, fl.Enabled(dashboard.FlagLegalDraftPages, ""))
	assert.ErrorIs(t, fl.Set(context.Background(), dashboard.FlagLegalDraftPages, "t-1", true, "test"), flags.ErrGlobalOnly)
}

// Nothing links the dark pages while they are dark: not llms.txt, the docs,
// the landing or base.html's default footer — nor any other template.
func TestLegalDraftPages_NothingLinksThem(t *testing.T) {
	files := []string{"llms_txt.go", "docs_pages.go", "landing.html", "landing/landing.src.html", "../dashboard/templates/layouts/base.html"}
	tmpls, err := filepath.Glob("../dashboard/templates/*/*.html")
	require.NoError(t, err)
	for _, f := range tmpls {
		if strings.HasSuffix(f, "legal/refunds.html") || strings.HasSuffix(f, "legal/dmca.html") {
			continue
		}
		files = append(files, f)
	}
	require.Greater(t, len(files), 10)
	for _, f := range files {
		b, err := os.ReadFile(f)
		require.NoError(t, err, f)
		for _, p := range legalDraftPaths {
			assert.NotContains(t, string(b), p, "%s links %s while legal_draft_pages is dark", f, p)
		}
	}
}
