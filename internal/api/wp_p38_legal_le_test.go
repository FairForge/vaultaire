package api

import (
	"fmt"
	"html/template"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/config"
	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/FairForge/vaultaire/internal/hosts"
	"github.com/FairForge/vaultaire/internal/modes"
	"github.com/FairForge/vaultaire/internal/produce"
)

// Plan 43.4 (Stage 1a Prompt 38 Part A): GET /legal/law-enforcement and
// /legal/what-we-can-produce, generated from produce.json, behind
// legal_le_pages (default OFF = 404) on the canonical origin AND the trust
// host — the same answer on both.

var legalLEPaths = []string{"/legal/law-enforcement", "/legal/what-we-can-produce"}

// legalServer is NewServer with the owner's prod hosts (no database: the
// flags serve their registered defaults). on models legal_le_pages ON with
// the fixture's own default — never a '*' row of a real flag.
func legalServer(t *testing.T, flagsOn ...string) http.Handler {
	t.Helper()
	prev := hosts.Get()
	t.Cleanup(func() { hosts.Set(prev) })
	t.Setenv("VAULTAIRE_PUBLIC_HOST", "https://stored.cloud")
	t.Setenv("VAULTAIRE_ENDPOINT", "https://s3.stored.cloud")
	t.Setenv("VAULTAIRE_BASE_URL", "https://stored.cloud")
	t.Setenv("CDN_PUBLIC_HOST", "https://cdn.stored.cloud")
	t.Setenv("VAULTAIRE_ALIAS_HOSTS", "stored.ge,www.stored.ge,www.stored.cloud")
	srv := NewServer(&config.Config{Server: config.ServerConfig{Port: 0}}, zap.NewNop(),
		engine.NewEngine(nil, zap.NewNop(), &engine.Config{DefaultBackend: "local"}), nil, nil)
	require.NotNil(t, srv.flags)
	for _, k := range flagsOn {
		srv.flags.Register(k, true)
	}
	return srv.httpServer.Handler
}

func legalGet(h http.Handler, method, host, path string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	req.Host = host
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

var legalHosts = []string{"stored.cloud", "stored.ge", "trust.stored.cloud", "trust.stored.ge"}

func TestLegalLEPages_FlagOff404OnEveryHost(t *testing.T) {
	h := legalServer(t)
	for _, host := range legalHosts {
		for _, p := range legalLEPaths {
			for _, m := range []string{http.MethodGet, http.MethodHead} {
				w := legalGet(h, m, host, p)
				assert.Equal(t, http.StatusNotFound, w.Code, "%s %s%s", m, host, p)
				assert.Equal(t, "no-store", w.Header().Get("Cache-Control"), "%s %s%s", m, host, p)
			}
		}
	}
}

func TestLegalLEPages_FlagOnServedOnEveryHost(t *testing.T) {
	h := legalServer(t, produce.FlagLegalLEPages)
	var bodies []string
	for _, host := range legalHosts {
		for _, p := range legalLEPaths {
			w := legalGet(h, http.MethodGet, host, p)
			require.Equal(t, http.StatusOK, w.Code, "%s%s: %s", host, p, w.Body.String())
			assert.Contains(t, w.Header().Get("Content-Type"), "text/html")
			assert.Contains(t, w.Header().Get("Content-Security-Policy"), "default-src 'self'")
			bodies = append(bodies, w.Body.String())
			head := legalGet(h, http.MethodHead, host, p)
			assert.Equal(t, http.StatusOK, head.Code, "HEAD %s%s", host, p)
		}
	}
	// The same page on every host.
	for i := 2; i < len(bodies); i++ {
		assert.Equal(t, bodies[i%2], bodies[i], "page %d differs by host", i)
	}
	produceBody := bodies[1]
	f := produce.All()
	for _, m := range modes.All().Modes {
		assert.Contains(t, produceBody, m.Name)
	}
	for _, a := range f.Account {
		assert.Contains(t, produceBody, "<code>"+a.Section+"</code>")
		for _, fld := range a.Fields {
			assert.Contains(t, produceBody, fld, a.Section)
		}
	}
	for _, r := range f.Retention {
		assert.Contains(t, produceBody, "<code>"+r.Table+"</code>")
		assert.Contains(t, produceBody, fmt.Sprintf("%d days", r.Days), r.Table)
	}
	assert.Contains(t, produceBody, fmt.Sprintf("%d days", f.Backups.OffBoxDays))
	for _, c := range f.PrivateSSEC.Holds {
		assert.Contains(t, produceBody, c.Column)
	}
	assert.Contains(t, bodies[0], "DRAFT", "the policy's procedure is counsel's, marked")
	assert.Contains(t, bodies[0], "/legal/what-we-can-produce")
}

func TestLegalLEPages_GatedSentencesFollowTheirFlags(t *testing.T) {
	f := produce.All()
	off := legalGet(legalServer(t, produce.FlagLegalLEPages), http.MethodGet, "stored.cloud", "/legal/what-we-can-produce").Body.String()
	assert.NotContains(t, off, template.HTMLEscapeString(f.AdminReads.Text))
	assert.NotContains(t, off, template.HTMLEscapeString(f.Builds.Text))

	on := legalGet(legalServer(t, produce.FlagLegalLEPages, f.AdminReads.RequiresFlag, f.Builds.RequiresFlag),
		http.MethodGet, "stored.cloud", "/legal/what-we-can-produce").Body.String()
	assert.Contains(t, on, template.HTMLEscapeString(f.AdminReads.Text))
	assert.Contains(t, on, template.HTMLEscapeString(f.Builds.Text))
}

// TestTrustHost_RouteListIsClosed lists every route the trust host serves;
// a new one must be added here on purpose (contract rule 11, RP42-2).
func TestTrustHost_RouteListIsClosed(t *testing.T) {
	s, _, _ := trustServer(t, prodHosts())
	var got []string
	require.NoError(t, chi.Walk(s.newTrustRouter().(chi.Routes), func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		got = append(got, method+" "+route)
		return nil
	}))
	sort.Strings(got)
	var want []string
	for _, p := range []string{
		"/.well-known/security.txt",
		"/translog/checkpoint", "/translog/tile/*", "/translog/anchor/*",
		"/legal/law-enforcement", "/legal/what-we-can-produce",
		"/static/css/style.css", "/static/js/htmx.min.js", "/static/js/dashboard.js",
		"/static/fonts/montserrat.woff2", "/static/fonts/silk400.woff2", "/static/fonts/silk700.woff2",
	} {
		want = append(want, "GET "+p, "HEAD "+p)
	}
	sort.Strings(want)
	assert.Equal(t, want, got)
}

// TestTrustHost_LegalAssetsFollowTheFlag: the layout's files are served on
// the trust host only while the pages are; OFF, the trust host is what it
// was (the log and security.txt).
func TestTrustHost_LegalAssetsFollowTheFlag(t *testing.T) {
	for _, on := range []bool{false, true} {
		var h http.Handler
		if on {
			h = legalServer(t, produce.FlagLegalLEPages)
		} else {
			h = legalServer(t)
		}
		for _, p := range []string{"/static/css/style.css", "/static/fonts/silk400.woff2", "/static/js/dashboard.js"} {
			w := legalGet(h, http.MethodGet, "trust.stored.cloud", p)
			if on {
				assert.Equal(t, http.StatusOK, w.Code, p)
				b, _ := io.ReadAll(w.Body)
				assert.NotEmpty(t, b, p)
			} else {
				assert.Equal(t, http.StatusNotFound, w.Code, p)
				assert.True(t, strings.HasPrefix(w.Header().Get("Content-Type"), "text/plain"), p)
			}
		}
		// Everything else of the app stays off the trust host.
		for _, p := range []string{"/static/js/house.js", "/legal/privacy", "/static/css/"} {
			assert.Equal(t, http.StatusNotFound, legalGet(h, http.MethodGet, "trust.stored.cloud", p).Code, "%v %s", on, p)
		}
	}
}
