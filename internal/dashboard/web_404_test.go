package dashboard

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FairForge/vaultaire/internal/hosts"
)

// TestWebNotFound_SignInLinksTheAppBase (RP91-12): the pixel 404 page's
// "sign in" link names hosts.AppBase()/login — the dashboard's own host once
// VAULTAIRE_BASE_URL names it — never the canonical /login; the home page
// link stays the canonical origin's.
func TestWebNotFound_SignInLinksTheAppBase(t *testing.T) {
	prev := hosts.Get()
	t.Cleanup(func() { hosts.Set(prev) })

	for _, c := range []struct{ base, wantLogin string }{
		{"https://app.stored.cloud", "https://app.stored.cloud/login"},
		{"", "https://stored.cloud/login"}, // not flipped: AppBase is the canonical origin
	} {
		hosts.Set(hosts.Hosts{Canonical: "https://stored.cloud", S3: "https://s3.stored.cloud", CDN: "https://cdn.stored.cloud", BaseURL: c.base})
		w := httptest.NewRecorder()
		WebNotFound(w, httptest.NewRequest(http.MethodGet, "/nope", nil))
		require.Equal(t, http.StatusNotFound, w.Code)
		body := w.Body.String()
		assert.Contains(t, body, `<a href="`+c.wantLogin+`">sign in</a>`, c.base)
		assert.Contains(t, body, `<a href="https://stored.cloud/">Go to the home page</a>`, c.base)
		if c.base != "" {
			assert.NotContains(t, body, `href="https://stored.cloud/login"`, c.base)
		}
	}
}
