package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/FairForge/vaultaire/internal/flags"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// openSignupsFlags builds a flag service whose signups flag resolves true
// (registered default, no DB rows).
func openSignupsFlags(t *testing.T) *flags.Service {
	t.Helper()
	svc := flags.New(nil, zap.NewNop())
	svc.Register(flagSignups, true)
	return svc
}

func TestHandleRoot_BrowserGetsLandingPage(t *testing.T) {
	s := &Server{}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()

	s.handleRoot(w, req)

	res := w.Result()
	defer func() { _ = res.Body.Close() }()
	require.Equal(t, http.StatusOK, res.StatusCode)
	assert.Contains(t, res.Header.Get("Content-Type"), "text/html")
	assert.Contains(t, w.Body.String(), "stored.ge", "embedded landing page should render")
}

func TestHandleRoot_HeadReturnsNoBody(t *testing.T) {
	s := &Server{}
	req := httptest.NewRequest(http.MethodHead, "/", nil)
	w := httptest.NewRecorder()

	s.handleRoot(w, req)

	require.Equal(t, http.StatusOK, w.Result().StatusCode)
	assert.Empty(t, w.Body.String())
}

// The landing page is rendered in two variants from the signups flag: closed →
// waitlist capture ("Launching July 31"), open → working /register CTA. Nil
// flags (zero-value Server, tests) must fall back to the closed variant.
func TestHandleRoot_SignupsClosed_ShowsWaitlist(t *testing.T) {
	s := &Server{} // nil flags = closed
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()

	s.handleRoot(w, req)

	body := w.Body.String()
	require.Equal(t, http.StatusOK, w.Result().StatusCode)
	assert.Contains(t, body, "/api/waitlist", "closed variant posts to the waitlist")
	assert.Contains(t, body, "July 31", "closed variant announces the launch date")
	assert.NotContains(t, body, `href="/register"`, "closed variant must not link a gated register page")
}

func TestHandleRoot_SignupsOpen_ShowsRegisterCTA(t *testing.T) {
	s := &Server{flags: openSignupsFlags(t)}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()

	s.handleRoot(w, req)

	body := w.Body.String()
	require.Equal(t, http.StatusOK, w.Result().StatusCode)
	assert.Contains(t, body, `href="/register"`, "open variant links the register page")
	assert.NotContains(t, body, "/api/waitlist", "open variant drops the waitlist form")
}

// The old page marketed a dropped Lyve/VPS product with fabricated stats and a
// dead "Sign In Coming Soon" modal. None of that may ever come back.
func TestLanding_NoDeadProduct(t *testing.T) {
	for _, page := range []string{string(landingClosed), string(landingOpen)} {
		for _, banned := range []string{"VPS", "Lyve", "Coming Soon", "10PB", "waitlist-modal"} {
			assert.NotContains(t, page, banned)
		}
	}
}

// B1 requires the footer to finally link the built-but-orphaned pages.
func TestLanding_FooterLinks(t *testing.T) {
	for _, link := range []string{
		`href="/login"`, `href="/status"`, `href="/changelog"`, `href="/abuse"`,
		`href="/legal/terms"`, `href="/legal/privacy"`, `href="/legal/aup"`,
		`href="/legal/cookies"`, `href="/legal/dpa"`, `href="/legal/gdpr"`,
		`href="/legal/baa"`, `href="/legal/data-act"`,
	} {
		assert.Contains(t, string(landingClosed), link)
		assert.Contains(t, string(landingOpen), link)
	}
}

// isS3RootRequest must keep authenticated ListBuckets working: a request to "/"
// with SigV4 auth (header or presigned query) is an S3 call, not a browser visit.
func TestIsS3RootRequest(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*http.Request)
		want  bool
	}{
		{"plain browser GET /", func(*http.Request) {}, false},
		{"browser with html accept", func(r *http.Request) { r.Header.Set("Accept", "text/html") }, false},
		{"sigv4 authorization header", func(r *http.Request) {
			r.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=...")
		}, true},
		{"presigned query", func(r *http.Request) {
			q := r.URL.Query()
			q.Set("X-Amz-Algorithm", "AWS4-HMAC-SHA256")
			r.URL.RawQuery = q.Encode()
		}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			tt.setup(req)
			assert.Equal(t, tt.want, isS3RootRequest(req))
		})
	}
}
