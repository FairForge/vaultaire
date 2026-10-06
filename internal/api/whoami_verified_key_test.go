package api

// Post-merge review of #604: whoami reported the key id from
// auth.AccessKeyFromRequest, a metrics helper that PREFERS the Authorization
// header over X-Amz-Credential. A valid presigned whoami URL sent with a junk
// `Authorization: AWS4-HMAC-SHA256 Credential=<other key>/…` header answered
// 200 with the OTHER key's id, name and type — the header was never verified.
// The id reported is now the one the chosen auth path verified, a request
// carrying both mechanisms is refused (AWS's rule), and the per-key failure
// metric is attributed to the credential of the path actually attempted.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/FairForge/vaultaire/internal/auth"
	"github.com/google/uuid"
	promtest "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// whoamiPresigned sends GET /api/v1/whoami presigned with the pair.
func whoamiPresigned(t *testing.T, ts *httptest.Server, ak, sk string, mutate func(*http.Request)) (*http.Response, []byte) {
	t.Helper()
	u, err := url.Parse(ts.URL)
	require.NoError(t, err)
	q := signPresignedURL(http.MethodGet, "/api/v1/whoami", u.Host, ak, sk, 300, time.Now().UTC())
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, ts.URL+"/api/v1/whoami?"+q.Encode(), nil)
	require.NoError(t, err)
	if mutate != nil {
		mutate(req)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp, b
}

func whoamiErrCode(t *testing.T, body []byte) (string, string) {
	t.Helper()
	var e struct {
		Error struct {
			Type      string `json:"type"`
			Code      string `json:"code"`
			Param     string `json:"param"`
			RequestID string `json:"request_id"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(body, &e), string(body))
	assert.Empty(t, e.Error.Param, "the 5th argument is param, never the request id")
	return e.Error.Type, e.Error.Code
}

func junkAuthHeader(ak string) string {
	return "AWS4-HMAC-SHA256 Credential=" + ak + "/20261006/us-east-1/s3/aws4_request, SignedHeaders=host, Signature=00"
}

func TestWhoami_PresignedReportsThePresignedKey(t *testing.T) {
	if testing.Short() {
		t.Skip("DB-backed")
	}
	f := setupBypassFixture(t)
	ts := httptest.NewServer(http.HandlerFunc(f.srv.handleWhoami))
	t.Cleanup(ts.Close)

	ak, sk := f.key("GetObject")
	resp, body := whoamiPresigned(t, ts, ak, sk, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	var a whoamiAnswer
	require.NoError(t, json.Unmarshal(body, &a))
	assert.Equal(t, f.tenantID, a.TenantID)
	assert.Equal(t, ak, a.KeyID)
	assert.Equal(t, "scoped", a.KeyType)
	assert.Equal(t, "scoped", a.KeyName)
}

func TestWhoami_PresignedPlusAuthorizationHeaderIsRefused(t *testing.T) {
	if testing.Short() {
		t.Skip("DB-backed")
	}
	f := setupBypassFixture(t)
	ts := httptest.NewServer(http.HandlerFunc(f.srv.handleWhoami))
	t.Cleanup(ts.Close)

	ak, sk := f.key("GetObject")
	resp, body := whoamiPresigned(t, ts, ak, sk, func(r *http.Request) {
		r.Header.Set("Authorization", junkAuthHeader(f.rootAK))
	})
	require.Equal(t, http.StatusBadRequest, resp.StatusCode, string(body))
	typ, code := whoamiErrCode(t, body)
	assert.Equal(t, ErrTypeInvalidRequest, typ)
	assert.Equal(t, "invalid_request", code)
	for _, leak := range []string{f.rootAK, ak, f.tenantID, "primary", `"key_id"`} {
		assert.NotContains(t, string(body), leak, "no key data on a mixed-auth request")
	}

	// a header-signed request that also carries presign query parameters
	resp, body = whoami(t, ts, f.rootAK, f.rootSK, func(r *http.Request) {
		q := r.URL.Query()
		q.Set("X-Amz-Credential", ak+"/20261006/us-east-1/s3/aws4_request")
		r.URL.RawQuery = q.Encode()
	})
	require.Equal(t, http.StatusBadRequest, resp.StatusCode, string(body))
	_, code = whoamiErrCode(t, body)
	assert.Equal(t, "invalid_request", code)
}

func TestWhoami_STSExpiredAndDeadParent(t *testing.T) {
	if testing.Short() {
		t.Skip("DB-backed")
	}
	f := setupBypassFixture(t)
	ts := httptest.NewServer(http.HandlerFunc(f.srv.handleWhoami))
	t.Cleanup(ts.Close)
	ctx := context.Background()

	// an expired STS token: key_expired on both paths
	tok, err := auth.GenerateSTSToken(ctx, f.db, f.tenantID, f.rootAK,
		&auth.KeyScope{Permissions: []string{"*"}}, auth.STSRequest{Permissions: []string{"GetObject"}, TTL: 900})
	require.NoError(t, err)
	f.exec(`UPDATE sts_tokens SET expires_at = NOW() - INTERVAL '1 minute' WHERE access_key = $1`, tok.AccessKey)
	resp, body := whoami(t, ts, tok.AccessKey, tok.SecretKey, nil)
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode, string(body))
	_, code := whoamiErrCode(t, body)
	assert.Equal(t, "key_expired", code, "header path")
	resp, body = whoamiPresigned(t, ts, tok.AccessKey, tok.SecretKey, nil)
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode, string(body))
	_, code = whoamiErrCode(t, body)
	assert.Equal(t, "key_expired", code, "presigned path")

	// an STS token whose parent key is revoked: refused on both paths
	pak, _ := f.key("GetObject")
	tok, err = auth.GenerateSTSToken(ctx, f.db, f.tenantID, pak,
		&auth.KeyScope{Permissions: []string{"GetObject"}}, auth.STSRequest{Permissions: []string{"GetObject"}, TTL: 900})
	require.NoError(t, err)
	f.exec(`UPDATE api_keys SET revoked_at = NOW() WHERE key_id = $1`, pak)
	resp, body = whoami(t, ts, tok.AccessKey, tok.SecretKey, nil)
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode, string(body))
	_, code = whoamiErrCode(t, body)
	assert.Equal(t, "key_revoked", code)
	resp, body = whoamiPresigned(t, ts, tok.AccessKey, tok.SecretKey, nil)
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode, string(body))
	_, code = whoamiErrCode(t, body)
	assert.Equal(t, "key_revoked", code)
}

func TestWhoami_UnknownKey(t *testing.T) {
	if testing.Short() {
		t.Skip("DB-backed")
	}
	f := setupBypassFixture(t)
	ts := httptest.NewServer(http.HandlerFunc(f.srv.handleWhoami))
	t.Cleanup(ts.Close)

	ghost := "VKGHOST" + strings.ToUpper(uuid.NewString()[:8])
	resp, body := whoami(t, ts, ghost, "whatever", nil)
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode, string(body))
	_, code := whoamiErrCode(t, body)
	assert.Equal(t, "invalid_credentials", code)
	resp, body = whoamiPresigned(t, ts, ghost, "whatever", nil)
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode, string(body))
	_, code = whoamiErrCode(t, body)
	assert.Equal(t, "invalid_credentials", code)
}

// The body's request_id is the response's X-Request-Id, through the
// production middleware chain; the route sits behind the JSON API limiter.
func TestWhoami_RequestIDAndRateLimitHeaders(t *testing.T) {
	if testing.Short() {
		t.Skip("DB-backed")
	}
	f := setupBypassFixture(t)
	f.srv.router.Use(f.srv.requestIDMiddleware)
	f.srv.registerWhoamiRoute()
	ts := httptest.NewServer(f.srv.router)
	t.Cleanup(ts.Close)

	resp, body := whoami(t, ts, f.rootAK, f.rootSK, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	var a whoamiAnswer
	require.NoError(t, json.Unmarshal(body, &a))
	require.NotEmpty(t, resp.Header.Get("X-Request-Id"))
	assert.Equal(t, resp.Header.Get("X-Request-Id"), a.RequestID)
	assert.NotEmpty(t, resp.Header.Get("X-RateLimit-Limit"), "behind the JSON API limiter")

	resp, body = whoami(t, ts, f.rootAK, "wrong", nil)
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode, string(body))
	var e struct {
		Error struct {
			RequestID string `json:"request_id"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(body, &e))
	assert.Equal(t, resp.Header.Get("X-Request-Id"), e.Error.RequestID)

	// without the middleware (the handler mounted bare) body and header still agree
	bare := httptest.NewServer(http.HandlerFunc(f.srv.handleWhoami))
	t.Cleanup(bare.Close)
	resp, body = whoami(t, bare, f.rootAK, f.rootSK, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	require.NoError(t, json.Unmarshal(body, &a))
	assert.NotEmpty(t, a.RequestID)
	assert.Equal(t, resp.Header.Get("X-Request-Id"), a.RequestID)
}

// The S3 path: a failed presigned request that also carries an Authorization
// header naming another key is attributed to the X-Amz-Credential key — the
// credential the presigned verifier actually tried.
func TestS3PresignFailure_AttributedToTheQueryCredential(t *testing.T) {
	s, mock, cleanup := newMgmtTestServer(t)
	defer cleanup()
	s.router.HandleFunc("/*", s.handleS3Request)

	queryKey := "VKquery" + uuid.NewString()[:8]
	headerKey := "VKheader" + uuid.NewString()[:8]
	mock.ExpectQuery(`SELECT EXISTS\(SELECT 1 FROM api_keys WHERE key_id`).
		WithArgs(queryKey).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))

	stale := time.Now().UTC().Add(-2 * time.Hour)
	q := signPresignedURL("GET", "/b/k", "localhost:8000", queryKey, "whatever", 60, stale)
	req := httptest.NewRequest(http.MethodGet, "/b/k?"+q.Encode(), nil)
	req.Host = "localhost:8000"
	req.Header.Set("Authorization", junkAuthHeader(headerKey))

	rec := httptest.NewRecorder()
	s.router.ServeHTTP(rec, req)
	require.Equal(t, http.StatusForbidden, rec.Code)
	assert.Equal(t, float64(1), promtest.ToFloat64(authFailuresByKey.WithLabelValues(accessKeyHash(queryKey))))
	assert.Equal(t, float64(0), promtest.ToFloat64(authFailuresByKey.WithLabelValues(accessKeyHash(headerKey))),
		"the header credential was never attempted")
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestAttemptedAccessKey_FollowsTheAuthPath(t *testing.T) {
	q := url.Values{}
	q.Set("X-Amz-Algorithm", "AWS4-HMAC-SHA256")
	q.Set("X-Amz-Credential", "QKEY/20261006/us-east-1/s3/aws4_request")
	r := httptest.NewRequest(http.MethodGet, "/api/v1/whoami?"+q.Encode(), nil)
	r.Header.Set("Authorization", junkAuthHeader("HKEY"))
	assert.Equal(t, "QKEY", attemptedAccessKey(r), "presigned path → X-Amz-Credential")
	assert.Equal(t, "HKEY", auth.AccessKeyFromRequest(r), "the generic helper is unchanged")

	r = httptest.NewRequest(http.MethodGet, "/b/k?X-Amz-Credential=QKEY%2F20261006%2Fus-east-1%2Fs3%2Faws4_request", nil)
	r.Header.Set("Authorization", junkAuthHeader("HKEY"))
	assert.Equal(t, "HKEY", attemptedAccessKey(r), "header path → Authorization")

	r = httptest.NewRequest(http.MethodGet, "/b/k?X-Amz-Credential=QKEY%2F20261006%2Fus-east-1%2Fs3%2Faws4_request", nil)
	assert.Equal(t, "", attemptedAccessKey(r), "header path with no header attempted no credential")
}
