package api

import (
	"net/http"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/config"
	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/FairForge/vaultaire/internal/hosts"
)

// WP-DOMAIN-1 part 3C-2 (contract rule 11: `app.` on stored.cloud): the
// owner signed in on app.stored.cloud with TOTP (2026-11-02), so every link
// to a dashboard page names the dashboard's host. The switch is
// VAULTAIRE_BASE_URL (the OAuth callbacks' and e-mail links' origin since
// part 1): hosts.AppBase() IS it, and the landing's and pricing page's sign-in
// links, the "upgrade at …/dashboard/billing" hint, the agent consent's
// redirect and a social sign-in's start follow it. With VAULTAIRE_BASE_URL on
// the canonical origin (today's .env) nothing moves; the canonical dashboard
// answers for ever either way.

// serverWithHosts is fullServer with env overrides.
func serverWithHosts(t *testing.T, over map[string]string) http.Handler {
	t.Helper()
	prev := hosts.Get()
	t.Cleanup(func() { hosts.Set(prev) })
	env := map[string]string{
		"VAULTAIRE_PUBLIC_HOST": "https://stored.cloud",
		"VAULTAIRE_ENDPOINT":    "https://s3.stored.cloud",
		"VAULTAIRE_BASE_URL":    "https://stored.cloud",
		"CDN_PUBLIC_HOST":       "https://cdn.stored.cloud",
		"VAULTAIRE_ALIAS_HOSTS": "stored.ge,www.stored.ge,www.stored.cloud",
	}
	for k, v := range over {
		env[k] = v
	}
	for k, v := range env {
		t.Setenv(k, v)
	}
	srv := NewServer(&config.Config{Server: config.ServerConfig{Port: 0}}, zap.NewNop(),
		engine.NewEngine(nil, zap.NewNop(), &engine.Config{DefaultBackend: "local"}), nil, nil)
	require.NotNil(t, srv.httpServer)
	return srv.httpServer.Handler
}

func TestAppLinks_FollowTheBaseURL(t *testing.T) {
	t.Run("flipped: VAULTAIRE_BASE_URL = https://app.stored.cloud", func(t *testing.T) {
		h := serverWithHosts(t, map[string]string{"VAULTAIRE_BASE_URL": "https://app.stored.cloud", "SIGNUPS_ENABLED": "false"})
		assert.Equal(t, "https://app.stored.cloud", hosts.Get().AppBase())
		assert.Equal(t, "https://stored.cloud", hosts.Get().Canonical, "the canonical origin does not move")

		for _, p := range []string{"/", "/pricing"} {
			body := serve(h, hostRequest(http.MethodGet, "stored.cloud", p, false, true)).Body.String()
			assert.Contains(t, body, `href="https://app.stored.cloud/login"`, p)
			assert.NotContains(t, body, `href="/login"`, p)
			assert.NotContains(t, body, `href="https://stored.cloud/login"`, p)
		}
		assert.Equal(t, "https://app.stored.cloud/dashboard/billing", billingURL())

		// The agent consent runs where the dashboard session lives
		// (handleOAuthAuthorize's redirect; the flow itself is
		// TestAgent_ConsentMintsOneGrant's, which needs the database).
		hs := hosts.Get()
		assert.Equal(t, "https://app.stored.cloud/dashboard/agents/connect?client_id=x&state=s",
			consentURL(hs, hostRequest(http.MethodGet, "stored.cloud", "/oauth/authorize?client_id=x&state=s", false, true)))
		assert.Equal(t, "/dashboard/agents/connect?client_id=x&state=s",
			consentURL(hs, hostRequest(http.MethodGet, "app.stored.cloud", "/oauth/authorize?client_id=x&state=s", false, true)), "already on the dashboard's host")

		// A social sign-in starts where its callback (VAULTAIRE_BASE_URL's) lands.
		for _, p := range []string{"/auth/google", "/auth/github?return_to=%2Fdashboard"} {
			w := serve(h, hostRequest(http.MethodGet, "stored.cloud", p, false, true))
			assert.Equal(t, http.StatusSeeOther, w.Code, p)
			u, err := url.Parse(w.Header().Get("Location"))
			require.NoError(t, err)
			assert.Equal(t, "app.stored.cloud", u.Host, p)
			w = serve(h, hostRequest(http.MethodGet, "app.stored.cloud", p, false, true))
			assert.NotContains(t, w.Header().Get("Location"), "://stored.cloud/", "never sent back to the canonical origin: %s", p)
		}

		// Sign-ups closed: the waitlist is the landing's, on the canonical origin.
		w := serve(h, hostRequest(http.MethodGet, "app.stored.cloud", "/register", false, true))
		assert.Equal(t, http.StatusSeeOther, w.Code)
		assert.Equal(t, "https://stored.cloud/#waitlist", w.Header().Get("Location"))

		// The canonical dashboard still answers (bookmarks).
		assert.Equal(t, http.StatusOK, serve(h, hostRequest(http.MethodGet, "stored.cloud", "/login", false, true)).Code)
		assert.Equal(t, http.StatusOK, serve(h, hostRequest(http.MethodGet, "app.stored.cloud", "/login", false, true)).Code)
	})

	t.Run("flipped, sign-ups open: the register links name app. too", func(t *testing.T) {
		h := serverWithHosts(t, map[string]string{"VAULTAIRE_BASE_URL": "https://app.stored.cloud", "SIGNUPS_ENABLED": "true"})
		body := serve(h, hostRequest(http.MethodGet, "stored.cloud", "/pricing", false, true)).Body.String()
		assert.Contains(t, body, `href="https://app.stored.cloud/register"`)
		assert.NotContains(t, body, `href="/register"`)
		// The landing reads the signups flag from the database (none here):
		// its open variant, rendered as the server renders it.
		open := string(landingPages(hosts.Get())[1])
		assert.Contains(t, open, `href="https://app.stored.cloud/register"`)
		assert.NotContains(t, open, `href="/register"`)
	})

	t.Run("not flipped: VAULTAIRE_BASE_URL on the canonical origin moves nothing to app.", func(t *testing.T) {
		h := serverWithHosts(t, map[string]string{"SIGNUPS_ENABLED": "false"})
		assert.Equal(t, "https://stored.cloud", hosts.Get().AppBase())
		for _, p := range []string{"/", "/pricing"} {
			body := serve(h, hostRequest(http.MethodGet, "stored.cloud", p, false, true)).Body.String()
			assert.NotContains(t, body, "app.stored.cloud", p)
			assert.Contains(t, body, `href="https://stored.cloud/login"`, p)
		}
		assert.Equal(t, "https://stored.cloud/dashboard/billing", billingURL())
		assert.Equal(t, "/dashboard/agents/connect?client_id=x",
			consentURL(hosts.Get(), hostRequest(http.MethodGet, "stored.cloud", "/oauth/authorize?client_id=x", false, true)))
		w := serve(h, hostRequest(http.MethodGet, "app.stored.cloud", "/auth/google", false, true))
		assert.Equal(t, "https://stored.cloud/auth/google", w.Header().Get("Location"), "the callback is the canonical's")
		w = serve(h, hostRequest(http.MethodGet, "stored.cloud", "/register", false, true))
		assert.Equal(t, "/#waitlist", w.Header().Get("Location"), "on the canonical origin the anchor stays relative")
	})
}
