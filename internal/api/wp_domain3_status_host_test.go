package api

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/FairForge/vaultaire/internal/hosts"
)

// WP-DOMAIN-1 part 3B (contract rule 11; docs/surfaces/DEVELOPER.md "The
// first ten" 2): the status page is the Worker on status.stored.cloud, which
// the box pushes to and which never reaches the origin. A status name that
// does reach the origin — status.stored.ge, status.<alias> — sends a
// browser's page view to the status origin (301, no-store) and answers
// everything else with the pixel 404: never the landing page, never the
// dashboard, never the S3 API.

var statusAliasNames = []string{"status.stored.ge", "status.www.stored.ge", "status.www.stored.cloud"}

func TestStatusAlias_301(t *testing.T) {
	h := fullServer(t)

	t.Run("a browser page view 301s to the status origin, path and query kept, no-store", func(t *testing.T) {
		for _, host := range statusAliasNames {
			for _, c := range []struct{ method, target, want string }{
				{http.MethodGet, "/", "https://status.stored.cloud/"},
				{http.MethodHead, "/", "https://status.stored.cloud/"},
				{http.MethodGet, "/state.json?x=1", "https://status.stored.cloud/state.json?x=1"},
				{http.MethodGet, "//evil.example/", "https://status.stored.cloud/evil.example/"},
			} {
				w := serve(h, hostRequest(c.method, host, c.target, false, true))
				assert.Equal(t, http.StatusMovedPermanently, w.Code, "%s %s%s", c.method, host, c.target)
				assert.Equal(t, c.want, w.Header().Get("Location"), "%s%s", host, c.target)
				assert.Equal(t, "no-store", w.Header().Get("Cache-Control"), "%s%s", host, c.target)
			}
		}
	})

	t.Run("anything that is not a browser page view is the pixel 404", func(t *testing.T) {
		for _, host := range statusAliasNames {
			for _, c := range []struct {
				method, path string
				browser      bool
			}{
				{http.MethodGet, "/", false}, // a script's fetch: no Accept: text/html
				{http.MethodGet, "/state.json", false},
				{http.MethodPost, "/push", false}, // a push is never forwarded
				{http.MethodPost, "/push", true},
				{http.MethodPut, "/my-bucket/key", false},
				{http.MethodGet, "/health", true}, // an operational path: never redirected
				{http.MethodGet, "/api/v1/manage/buckets", true},
				{http.MethodGet, "/.well-known/security.txt", true},
			} {
				w := serve(h, hostRequest(c.method, host, c.path, false, c.browser))
				assertPixel404(t, w, c.method+" "+host+c.path)
			}
			for _, p := range []string{"/", "/my-bucket/key", "/push"} {
				for name, req := range credentialShapes(http.MethodGet, host, p) {
					assertPixel404(t, serve(h, req), name+" "+host+p)
				}
			}
		}
	})

	t.Run("the status origin itself reaching the origin is a 404, never a redirect loop", func(t *testing.T) {
		for _, p := range []string{"/", "/state.json", "/login"} {
			w := serve(h, hostRequest(http.MethodGet, "status.stored.cloud", p, false, true))
			assertPixel404(t, w, "status.stored.cloud"+p)
		}
	})

	t.Run("the canonical /status is unchanged and names the live status page", func(t *testing.T) {
		w := serve(h, hostRequest(http.MethodGet, "stored.cloud", "/status", false, true))
		assert.Equal(t, http.StatusOK, w.Code)
		assert.Equal(t, "no-store", w.Header().Get("Cache-Control"))
		assert.Contains(t, w.Body.String(), `Live status: <a href="https://status.stored.cloud">status.stored.cloud</a>`)

		w = serve(h, hostRequest(http.MethodGet, "stored.ge", "/status", false, true))
		assert.Equal(t, http.StatusMovedPermanently, w.Code)
		assert.Equal(t, "https://stored.cloud/status", w.Header().Get("Location"), "the web alias still goes to the canonical origin")
	})
}

// TestStatusHost_DevelopmentOriginKeepsTheOneOrigin: a lab has no status
// router and its /status page names no other host.
func TestStatusHost_DevelopmentOriginKeepsTheOneOrigin(t *testing.T) {
	dev := hosts.FromEnv(func(string) string { return "" }, 8000, nil)
	fallback := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) })
	for _, host := range []string{"localhost:8000", "status.localhost", "status.stored.ge"} {
		w := serve(StatusHostRouter(dev, fallback), hostRequest(http.MethodGet, host, "/", false, true))
		assert.Equal(t, http.StatusTeapot, w.Code, host)
	}
	s := &Server{hosts: dev}
	assert.NotContains(t, s.statusPageLink(), "status.")
	assert.Empty(t, s.statusPageLink())
}
