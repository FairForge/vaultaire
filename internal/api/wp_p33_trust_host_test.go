package api

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/config"
	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/FairForge/vaultaire/internal/hosts"
)

// Stage 1a Prompt 33 Part 0b (RP42-2, P4 proven live): #913 gated
// /translog/… by host, but nothing gated the trust host by path — on the
// box trust.stored.cloud/ answered the landing page (200 text/html) and
// /dashboard/login a 303. Contract rule 11 makes trust. its own surface:
// the log and nothing of the app.

// fullServer is NewServer with the owner's prod hosts: every route of the
// app, through the top-level handler (httpServer.Handler).
func fullServer(t *testing.T) http.Handler {
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
	require.NotNil(t, srv.httpServer)
	return srv.httpServer.Handler
}

func hostRequest(method, host, path string, signed bool, browser bool) *http.Request {
	req := httptest.NewRequest(method, path, strings.NewReader(""))
	req.Host = host
	if signed {
		req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=AKIA/20261023/us-east-1/s3/aws4_request, SignedHeaders=host;x-amz-date, Signature=00")
		req.Header.Set("X-Amz-Date", "20261023T000000Z")
		req.Header.Set("X-Amz-Content-Sha256", "UNSIGNED-PAYLOAD")
	}
	if browser {
		req.Header.Set("Accept", "text/html")
	}
	return req
}

func TestTrustHost_ServesOnlyTheLog(t *testing.T) {
	h := fullServer(t)
	type call struct {
		method, path   string
		signed, browse bool
	}
	calls := []call{
		{http.MethodGet, "/", false, true},
		{http.MethodGet, "/dashboard/login", false, true},
		{http.MethodGet, "/api/v1/manage/buckets", false, false},
		{http.MethodPost, "/auth/register", false, false},
		{http.MethodPut, "/bucket/key", true, false},
		{http.MethodGet, "/bucket/key", true, false},
		{http.MethodGet, "/metrics", false, false},
		{http.MethodGet, "/translog", false, false},
	}
	for _, c := range calls {
		// The same request on the canonical host is the app (the contrast:
		// these paths exist — 200, 303, 401, the S3 API's answer …).
		w := httptest.NewRecorder()
		h.ServeHTTP(w, hostRequest(c.method, "stored.cloud", c.path, c.signed, c.browse))
		if c.path != "/translog" {
			assert.NotEqual(t, http.StatusNotFound, w.Code, "stored.cloud %s %s is the app", c.method, c.path)
		}
		for _, host := range []string{"trust.stored.cloud", "trust.stored.ge"} {
			w := httptest.NewRecorder()

			h.ServeHTTP(w, hostRequest(c.method, host, c.path, c.signed, c.browse))

			assert.Equal(t, http.StatusNotFound, w.Code, "%s %s%s", c.method, host, c.path)
			assert.Equal(t, "text/plain; charset=utf-8", w.Header().Get("Content-Type"), "%s %s%s", c.method, host, c.path)
			assert.Equal(t, "no-store", w.Header().Get("Cache-Control"), "%s %s%s", c.method, host, c.path)
			assert.NotContains(t, w.Body.String(), "<html", "%s %s%s", c.method, host, c.path)
		}
	}
	// security.txt stays.
	w := httptest.NewRecorder()
	h.ServeHTTP(w, hostRequest(http.MethodGet, "trust.stored.cloud", "/.well-known/security.txt", false, false))
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "Contact: mailto:security@stored.cloud")
}

func TestTrustHost_HeadCheckpoint(t *testing.T) {
	// Arrange: a signed log, translog_public ON, behind the top-level router.
	s, fl, _ := trustServer(t, prodHosts())
	require.NoError(t, fl.Set(context.Background(), flagTranslogPublic, "*", true, "test"))
	t.Cleanup(func() { _ = fl.Unset(context.Background(), flagTranslogPublic, "*") })
	srv := httptest.NewServer(TrustHostRouter(s.hosts, s.newTrustRouter(), s.router))
	t.Cleanup(srv.Close)
	do := func(method, path string) (*http.Response, []byte) {
		req, err := http.NewRequest(method, srv.URL+path, nil)
		require.NoError(t, err)
		req.Host = "trust.stored.cloud"
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		b, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		return resp, b
	}
	for _, p := range []string{"/translog/checkpoint", "/translog/tile/entries/000.p/1"} {
		get, body := do(http.MethodGet, p)
		require.Equal(t, http.StatusOK, get.StatusCode, p)
		require.NotEmpty(t, body)

		head, hbody := do(http.MethodHead, p)

		assert.Equal(t, http.StatusOK, head.StatusCode, p)
		assert.Empty(t, hbody, p)
		for _, k := range []string{"Content-Type", "Cache-Control", "Content-Length", "X-Content-Type-Options"} {
			assert.Equal(t, get.Header.Get(k), head.Header.Get(k), "%s %s", p, k)
		}
	}
	// A signed or presigned S3 request on the trust host is 404, never the S3 API.
	for _, signed := range []*http.Request{
		hostRequest(http.MethodGet, "trust.stored.cloud", "/translog/checkpoint", true, false),
		hostRequest(http.MethodGet, "trust.stored.cloud", "/translog/checkpoint?X-Amz-Signature=00&X-Amz-Credential=AKIA", false, false),
	} {
		w := httptest.NewRecorder()
		TrustHostRouter(s.hosts, s.newTrustRouter(), s.router).ServeHTTP(w, signed)
		assert.Equal(t, http.StatusNotFound, w.Code, signed.URL.String())
	}
	// Flag OFF: HEAD is the same 404 as GET.
	require.NoError(t, fl.Unset(context.Background(), flagTranslogPublic, "*"))
	head, _ := do(http.MethodHead, "/translog/checkpoint")
	assert.Equal(t, http.StatusNotFound, head.StatusCode)
	assert.Equal(t, "no-store", head.Header.Get("Cache-Control"))
}

func TestCanonicalHost_Unchanged(t *testing.T) {
	// The landing page on stored.cloud.
	h := fullServer(t)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, hostRequest(http.MethodGet, "stored.cloud", "/", false, true))
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Header().Get("Content-Type"), "text/html")

	// /translog/… there (and HEAD of it, RP41-6) is still the S3 API's.
	s, fl, _ := trustServer(t, prodHosts())
	require.NoError(t, fl.Set(context.Background(), flagTranslogPublic, "*", true, "test"))
	t.Cleanup(func() { _ = fl.Unset(context.Background(), flagTranslogPublic, "*") })
	top := TrustHostRouter(s.hosts, s.newTrustRouter(), s.router)
	for _, host := range []string{"stored.cloud", "s3.stored.cloud", "stored.ge"} {
		for _, m := range []string{http.MethodGet, http.MethodHead} {
			w := httptest.NewRecorder()
			top.ServeHTTP(w, hostRequest(m, host, "/translog/checkpoint", false, false))
			assert.Equal(t, http.StatusTeapot, w.Code, "%s %s", m, host)
		}
	}

	// A development origin: the trust router is not in the way at all.
	dev, _, _ := trustServer(t, hosts.Defaults(8000))
	devTop := TrustHostRouter(dev.hosts, dev.newTrustRouter(), dev.router)
	w = httptest.NewRecorder()
	devTop.ServeHTTP(w, hostRequest(http.MethodGet, "trust.stored.cloud", "/translog/checkpoint", true, false))
	assert.Equal(t, http.StatusTeapot, w.Code, "dev origin: the S3 API, as before")
}
