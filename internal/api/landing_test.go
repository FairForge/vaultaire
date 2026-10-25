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
	assert.Contains(t, w.Body.String(), "Stored", "embedded landing page should render")
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
// waitlist capture ("Launching October 31"), open → working /register CTA. Nil
// flags (zero-value Server, tests) must fall back to the closed variant.
func TestHandleRoot_SignupsClosed_ShowsWaitlist(t *testing.T) {
	s := &Server{} // nil flags = closed
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()

	s.handleRoot(w, req)

	body := w.Body.String()
	require.Equal(t, http.StatusOK, w.Result().StatusCode)
	assert.Contains(t, body, "/api/waitlist", "closed variant posts to the waitlist")
	assert.Contains(t, body, "October 31", "closed variant announces the launch date")
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
	for _, page := range []string{string(landingPages(prodHosts())[0]), string(landingPages(prodHosts())[1])} {
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
		assert.Contains(t, string(landingPages(prodHosts())[0]), link)
		assert.Contains(t, string(landingPages(prodHosts())[1]), link)
	}
}

// signedS3Request must keep authenticated ListBuckets working: a request to
// "/" with AWS-scheme credentials (header or presigned query) is an S3 call,
// not a browser visit — and, since RP56-1, nothing else is: a Bearer JWT or
// a Basic header is never S3 (the credential gate's predicate).
func TestSignedS3Request(t *testing.T) {
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
		{"sigv4 scheme in lower case", func(r *http.Request) {
			r.Header.Set("Authorization", "aws4-hmac-sha256 Credential=...")
		}, true},
		{"sigv2 authorization header", func(r *http.Request) { r.Header.Set("Authorization", "AWS AKID:c2ln") }, true},
		{"bearer jwt", func(r *http.Request) { r.Header.Set("Authorization", "Bearer eyJ...") }, false},
		{"basic", func(r *http.Request) { r.Header.Set("Authorization", "Basic dTpw") }, false},
		{"a scheme that only starts with AWS", func(r *http.Request) { r.Header.Set("Authorization", "AWSX foo") }, false},
		{"sigv4a authorization header (RP58-1)", func(r *http.Request) {
			r.Header.Set("Authorization", "AWS4-ECDSA-P256-SHA256 Credential=AKID/20261025/*/s3/aws4_request, SignedHeaders=host, Signature=00")
		}, true},
		{"a comma after the scheme is no AWS header to the verifier either (RP58-8 c)", func(r *http.Request) {
			r.Header.Set("Authorization", "AWS4-HMAC-SHA256,Credential=...")
		}, false},
		{"presigned query", func(r *http.Request) { setQuery(r, "X-Amz-Algorithm", "AWS4-HMAC-SHA256") }, true},
		{"presigned signature", func(r *http.Request) { setQuery(r, "X-Amz-Signature", "00") }, true},
		{"presigned credential", func(r *http.Request) { setQuery(r, "X-Amz-Credential", "AKID/20261024/us-east-1/s3/aws4_request") }, true},
		{"sigv2 presigned", func(r *http.Request) { setQuery(r, "AWSAccessKeyId", "AKID", "Signature", "c2ln") }, true},
		{"a bare Signature (an OAuth callback, a share link)", func(r *http.Request) { setQuery(r, "Signature", "x") }, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			tt.setup(req)
			assert.Equal(t, tt.want, signedS3Request(req))
		})
	}
}

func setQuery(r *http.Request, kv ...string) {
	q := r.URL.Query()
	for i := 0; i+1 < len(kv); i += 2 {
		q.Set(kv[i], kv[i+1])
	}
	r.URL.RawQuery = q.Encode()
}

// The page tells one example story everywhere it shows storage: photos and
// projects downstairs, backups in the attic — the hero terminal's buckets, the
// hero scene's tags, the builder's starter and its receipt all agree. The
// beacon for the cookieless site statistics is wired in too.
func TestLanding_ExampleStory(t *testing.T) {
	for _, page := range []string{string(landingPages(prodHosts())[0]), string(landingPages(prodHosts())[1])} {
		for _, want := range []string{
			"rclone copy ~/backups stored:backups", "03:00:12 backups", "21:44:08 projects", "09:15:51 photos",
			"PHOTOS 1 TB", "PROJECTS 1 TB", `data-pack="b"`, "1 dresser of backups", "1 storage box of photos", "$18.98",
			"navigator.sendBeacon('/api/ping'", "b: 'backups'",
		} {
			assert.Contains(t, page, want)
		}
		for _, gone := range []string{"stored:archive", "postgres-wal", "$28.94", "1 dresser, 1 storage box"} {
			assert.NotContains(t, page, gone)
		}
	}
}
