package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/auth"
	"github.com/FairForge/vaultaire/internal/config"
	"github.com/FairForge/vaultaire/internal/dashboard"
	dashauth "github.com/FairForge/vaultaire/internal/dashboard/auth"
	"github.com/FairForge/vaultaire/internal/email"
	"github.com/FairForge/vaultaire/internal/hosts"
)

// WP-DOMAIN-1 part 3C-1 (contract rule 11: `app.` on stored.cloud;
// docs/surfaces/DEVELOPER.md, the hosts of "The first ten" 1): the dashboard
// gets a host of its own. app.stored.cloud serves the dashboard's routes —
// exactly the ones dashboard.RegisterRoutes mounts — the agent consent's
// /oauth/authorize, the authorization-server metadata (issuer unchanged: the
// canonical origin), security.txt, and nothing else: no landing page, no docs,
// no management API, no S3 API. Dark: no link names it until the owner's
// proof (hosts.AppBase stays the canonical origin).

var appHostNames = []string{"app.stored.cloud", "app.stored.ge", "app.www.stored.ge"}

// appExtras are the routes the app host serves beside the dashboard's.
var appExtras = []string{
	"GET /", "HEAD /",
	"GET /oauth/authorize", "HEAD /oauth/authorize",
	"GET /.well-known/oauth-authorization-server", "HEAD /.well-known/oauth-authorization-server",
	"GET /.well-known/security.txt", "HEAD /.well-known/security.txt",
	"GET /robots.txt", "HEAD /robots.txt", // RP91-9: Disallow: /
}

func walk(t *testing.T, h http.Handler) []string {
	t.Helper()
	var got []string
	require.NoError(t, chi.Walk(h.(chi.Routes), func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		got = append(got, method+" "+route)
		return nil
	}))
	sort.Strings(got)
	return got
}

func TestAppHost_ServesOnlyTheDashboard(t *testing.T) {
	t.Run("the walk is the dashboard's routes plus the listed extras", func(t *testing.T) {
		s := &Server{logger: zap.NewNop(), router: chi.NewRouter(), config: &config.Config{}, hosts: prodHosts()}
		dash := chi.NewRouter()
		dashboard.RegisterRoutes(dash, s.dashboardDeps())
		want := append(walk(t, dash), appExtras...)
		sort.Strings(want)
		assert.Equal(t, want, walk(t, s.newAppRouter()))
	})

	h := fullServer(t)
	t.Run("the dashboard answers on every app host", func(t *testing.T) {
		for _, host := range appHostNames {
			for _, c := range []struct {
				path string
				code int
				loc  string
			}{
				{"/", http.StatusSeeOther, "/dashboard"},
				{"/login", http.StatusOK, ""},
				{"/register", 0, ""}, // open or closed, it is the dashboard's answer, never the 404
				{"/forgot-password", http.StatusOK, ""},
				{"/dashboard", http.StatusSeeOther, "/login"},
				{"/dashboard/buckets", http.StatusSeeOther, "/login"},
				{"/admin", http.StatusSeeOther, "/login"},
				{"/static/css/style.css", http.StatusOK, ""},
				{"/.well-known/security.txt", http.StatusOK, ""},
				{"/.well-known/oauth-authorization-server", http.StatusOK, ""},
			} {
				w := serve(h, hostRequest(http.MethodGet, host, c.path, false, true))
				if c.code == 0 {
					assert.NotEqual(t, http.StatusNotFound, w.Code, host+c.path)
					continue
				}
				assert.Equal(t, c.code, w.Code, host+c.path)
				if c.loc != "" {
					assert.True(t, strings.HasPrefix(w.Header().Get("Location"), c.loc), "%s%s → %s", host, c.path, w.Header().Get("Location"))
				}
			}
		}
		md := serve(h, hostRequest(http.MethodGet, "app.stored.cloud", "/.well-known/oauth-authorization-server", false, false)).Body.String()
		assert.Contains(t, md, `"issuer":"https://stored.cloud"`, "the issuer stays the canonical origin")
		w := serve(h, hostRequest(http.MethodGet, "app.stored.cloud", "/oauth/authorize", false, true))
		assert.NotEqual(t, http.StatusNotFound, w.Code, "the agent consent's entrance answers on the app host")
	})

	t.Run("a social sign-in starts on the canonical origin, where its callback lands", func(t *testing.T) {
		for _, p := range []string{"/auth/google", "/auth/github", "/auth/google?return_to=%2Fdashboard"} {
			w := serve(h, hostRequest(http.MethodGet, "app.stored.cloud", p, false, true))
			assert.Equal(t, http.StatusSeeOther, w.Code, p)
			u, err := url.Parse(w.Header().Get("Location"))
			require.NoError(t, err)
			assert.Equal(t, "stored.cloud", u.Host, p)
			assert.Equal(t, strings.SplitN(p, "?", 2)[0], u.Path, p)
		}
	})

	t.Run("everything else is the pixel 404, no-store", func(t *testing.T) {
		others := []struct{ method, path string }{
			{http.MethodGet, "/pricing"}, {http.MethodGet, "/docs"}, {http.MethodGet, "/docs/rclone"},
			{http.MethodGet, "/status"}, {http.MethodGet, "/my-bucket/key"}, {http.MethodGet, "/metrics"},
			{http.MethodGet, "/health"}, {http.MethodGet, "/version"}, {http.MethodGet, "/api/v1/manage/buckets"},
			{http.MethodGet, "/translog/checkpoint"}, {http.MethodGet, "/cdn/x/y/z"}, {http.MethodGet, "/llms.txt"},
			{http.MethodGet, "/openapi.json"}, {http.MethodGet, "/sitemap.xml"},
			{http.MethodPost, "/auth/login"}, {http.MethodPost, "/auth/register"}, {http.MethodPost, "/oauth/token"},
			{http.MethodPost, "/oauth/register"}, {http.MethodPut, "/my-bucket/key"}, {http.MethodDelete, "/my-bucket"},
			{http.MethodPost, "/webhook/stripe"},
		}
		for _, host := range appHostNames {
			for _, c := range others {
				assertPixel404(t, serve(h, hostRequest(c.method, host, c.path, false, true)), c.method+" "+host+c.path)
			}
		}
	})

	t.Run("a request with S3 credentials is a 404 on every path, dashboard routes included", func(t *testing.T) {
		for _, host := range appHostNames {
			for _, p := range []string{"/", "/login", "/dashboard", "/static/css/style.css", "/my-bucket/key", "/.well-known/security.txt"} {
				for name, req := range credentialShapes(http.MethodGet, host, p) {
					assertPixel404(t, serve(h, req), name+" "+host+p)
				}
			}
		}
	})

	t.Run("the canonical origin's dashboard is unchanged", func(t *testing.T) {
		assert.Equal(t, http.StatusOK, serve(h, hostRequest(http.MethodGet, "stored.cloud", "/login", false, true)).Code)
		assert.Equal(t, http.StatusOK, serve(h, hostRequest(http.MethodGet, "stored.cloud", "/pricing", false, true)).Code)
		w := serve(h, hostRequest(http.MethodGet, "stored.cloud", "/auth/google", false, true))
		assert.NotEqual(t, "app.stored.cloud", w.Header().Get("Location"))
	})

	t.Run("dark: the canonical pages name no app host", func(t *testing.T) {
		for _, p := range []string{"/", "/login", "/pricing", "/docs"} {
			body := serve(h, hostRequest(http.MethodGet, "stored.cloud", p, false, true)).Body.String()
			assert.NotContains(t, body, "app.stored.cloud", p)
		}
	})
}

// TestAppHost_NeverIndexed (RP91-9): the app host is no page to index —
// /login, /verify, /reset-password and its copy of /legal/privacy were
// indexable. Its robots.txt disallows everything, and every answer (a page, a
// redirect, the pixel 404, a signed request's 404) says noindex.
func TestAppHost_NeverIndexed(t *testing.T) {
	h := fullServer(t)
	for _, host := range appHostNames {
		for _, m := range []string{http.MethodGet, http.MethodHead} {
			w := serve(h, hostRequest(m, host, "/robots.txt", false, false))
			require.Equal(t, http.StatusOK, w.Code, m+" "+host)
			assert.True(t, strings.HasPrefix(w.Header().Get("Content-Type"), "text/plain"), w.Header().Get("Content-Type"))
			if m == http.MethodGet {
				assert.Equal(t, "User-agent: *\nDisallow: /\n", w.Body.String())
			}
		}
		for _, p := range []string{"/", "/login", "/forgot-password", "/legal/privacy", "/dashboard", "/robots.txt", "/no-such-page", "/.well-known/security.txt"} {
			w := serve(h, hostRequest(http.MethodGet, host, p, false, true))
			assert.Equal(t, "noindex", w.Header().Get("X-Robots-Tag"), "%s%s (%d)", host, p, w.Code)
		}
		for name, req := range credentialShapes(http.MethodGet, host, "/login") {
			assert.Equal(t, "noindex", serve(h, req).Header().Get("X-Robots-Tag"), name)
		}
	}
	// The canonical origin's pages are indexable as before.
	assert.Empty(t, serve(h, hostRequest(http.MethodGet, "stored.cloud", "/login", false, true)).Header().Get("X-Robots-Tag"))
}

// TestAppHost_SessionIsHostOnly signs in on app.stored.cloud for real (the
// dashboard's own POST /login) and holds the session cookie to the __Host-
// rules: Secure, Path=/, no Domain — it never reaches stored.cloud or any
// other name, and stored.cloud's never reaches app.
func TestAppHost_SessionIsHostOnly(t *testing.T) {
	ctx := context.Background()
	authSvc := auth.NewAuthService(nil, nil)
	_, err := authSvc.CreateUser(ctx, "app-host@stored.test", "securepass123")
	require.NoError(t, err)
	s := &Server{logger: zap.NewNop(), router: chi.NewRouter(), config: &config.Config{}, hosts: prodHosts(),
		auth: authSvc, sessionStore: dashauth.NewMemoryStore(), emailSender: email.NewSender(zap.NewNop())}
	h := AppHostRouter(s.hosts, s.newAppRouter(), http.NotFoundHandler())

	form := url.Values{"email": {"app-host@stored.test"}, "password": {"securepass123"}}
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
	req.Host = "app.stored.cloud"
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "https://app.stored.cloud")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	w := serve(h, req)
	require.Equal(t, http.StatusSeeOther, w.Code, w.Body.String())

	var session *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == dashauth.SessionCookieName {
			session = c
		}
	}
	require.NotNil(t, session, "the sign-in sets the session cookie: %v", w.Header().Values("Set-Cookie"))
	assert.True(t, strings.HasPrefix(session.Name, "__Host-"))
	assert.True(t, session.Secure)
	assert.True(t, session.HttpOnly)
	assert.Equal(t, "/", session.Path)
	assert.Empty(t, session.Domain, "no Domain attribute, ever: the cookie is the app host's alone")
	for _, line := range w.Header().Values("Set-Cookie") {
		assert.NotContains(t, strings.ToLower(line), "domain=", line)
	}

	// The cookie opens the dashboard on the host that set it.
	get := hostRequest(http.MethodGet, "app.stored.cloud", "/dashboard", false, true)
	get.AddCookie(session)
	assert.Equal(t, http.StatusOK, serve(h, get).Code)
}

// TestAppHost_DevelopmentOriginKeepsTheOneOrigin: a lab has no app router.
func TestAppHost_DevelopmentOriginKeepsTheOneOrigin(t *testing.T) {
	dev := hosts.FromEnv(func(string) string { return "" }, 8000, nil)
	fallback := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) })
	app := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	for _, host := range []string{"localhost:8000", "app.localhost", "app.stored.cloud"} {
		assert.Equal(t, http.StatusTeapot, serve(AppHostRouter(dev, app, fallback), hostRequest(http.MethodGet, host, "/", false, true)).Code, host)
	}
}
