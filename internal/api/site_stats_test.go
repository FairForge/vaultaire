package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/FairForge/vaultaire/internal/sitestats"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

const browserUA = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/152.0.0.0 Safari/537.36"

func statsServer(t *testing.T) *Server {
	t.Helper()
	return &Server{logger: zap.NewNop(), siteStats: sitestats.New(nil)}
}

func htmlHandler(status int) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(status)
		_, _ = w.Write([]byte("<html></html>"))
	})
}

func TestSiteStatsMiddleware_CountsRenderedPublicPagesOnly(t *testing.T) {
	s := statsServer(t)
	h := s.siteStatsMiddleware(htmlHandler(http.StatusOK))
	get := func(path, ua string) {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("User-Agent", ua)
		h.ServeHTTP(httptest.NewRecorder(), req)
	}

	get("/", browserUA)
	get("/docs/rclone", browserUA)
	get("/", "curl/8.7.1")         // a tool, not a visit
	get("/health", browserUA)      // not a public page
	get("/api/v1/user", browserUA) // not a public page
	get("/dashboard/buckets", browserUA)
	keys, visitors := s.siteStats.Pending()
	assert.Equal(t, 2, keys, "two pages counted, one row each")
	assert.Equal(t, 1, visitors, "the same browser on two pages is one visitor")

	// S3 ListBuckets on "/" is the API.
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("User-Agent", browserUA)
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=x")
	h.ServeHTTP(httptest.NewRecorder(), req)
	// HEAD and non-200 are not views either.
	req = httptest.NewRequest(http.MethodHead, "/", nil)
	req.Header.Set("User-Agent", browserUA)
	h.ServeHTTP(httptest.NewRecorder(), req)
	s.siteStatsMiddleware(htmlHandler(http.StatusNotFound)).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/docs/nope", nil))
	keys, _ = s.siteStats.Pending()
	assert.Equal(t, 2, keys)
}

func TestSiteStatsMiddleware_IgnoresNonHTMLAndPassesThrough(t *testing.T) {
	s := statsServer(t)
	jsonH := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	})
	req := httptest.NewRequest(http.MethodGet, "/status", nil)
	req.Header.Set("User-Agent", browserUA)
	w := httptest.NewRecorder()
	s.siteStatsMiddleware(jsonH).ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, `{}`, w.Body.String())
	keys, _ := s.siteStats.Pending()
	assert.Zero(t, keys)

	// nil collector: plain pass-through
	n := &Server{logger: zap.NewNop()}
	w = httptest.NewRecorder()
	n.siteStatsMiddleware(htmlHandler(http.StatusOK)).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestHandlePing_CountsAllowedEventsOnly(t *testing.T) {
	s := statsServer(t)
	post := func(body, ua string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/ping", strings.NewReader(body))
		req.RemoteAddr = "203.0.113.77:1"
		req.Header.Set("User-Agent", ua)
		w := httptest.NewRecorder()
		s.handlePing(w, req)
		return w
	}
	w := post(`{"event":"builder.edit","referrer":"https://lowendtalk.com/x","utm_source":"let"}`, browserUA)
	assert.Equal(t, http.StatusNoContent, w.Code)
	assert.Equal(t, "no-store", w.Header().Get("Cache-Control"))
	assert.Equal(t, http.StatusNoContent, post(`{"event":"not.a.thing"}`, browserUA).Code, "unknown events are ignored, not rejected")
	assert.Equal(t, http.StatusNoContent, post(`{"event":"builder.edit"}`, "curl/8.7.1").Code)
	assert.Equal(t, http.StatusBadRequest, post(`not json`, browserUA).Code)
	keys, _ := s.siteStats.Pending()
	assert.Equal(t, 1, keys, "only the allowed event from a browser was counted")
}

func TestHandlePing_RateLimited(t *testing.T) {
	s := statsServer(t)
	var last int
	for i := 0; i < 241; i++ {
		req := httptest.NewRequest(http.MethodPost, "/api/ping", strings.NewReader(`{"event":"copy"}`))
		req.RemoteAddr = "203.0.113.200:1"
		req.Header.Set("User-Agent", browserUA)
		w := httptest.NewRecorder()
		s.handlePing(w, req)
		last = w.Code
	}
	require.Equal(t, http.StatusTooManyRequests, last)
}
