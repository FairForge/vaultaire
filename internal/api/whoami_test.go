package api

// GET /api/v1/whoami (docs/STATUS.md queue item 3, 2026-10-06; plan 42.7):
// what a key is, told to the holder of the key. An edge gateway that only
// has a customer's S3 key (never a JWT) needs to know the tenant to partition
// its cache per tenant instead of per key, and whether the key is primary,
// scoped or a temporary STS token. Signed with SigV4 like any S3 request;
// never a secret in the answer.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/FairForge/vaultaire/internal/auth"
	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type whoamiAnswer struct {
	Object      string   `json:"object"`
	TenantID    string   `json:"tenant_id"`
	KeyID       string   `json:"key_id"`
	KeyType     string   `json:"key_type"`
	KeyName     string   `json:"key_name"`
	Permissions []string `json:"permissions"`
	BucketScope []string `json:"bucket_scope"`
	IPAllowlist []string `json:"ip_allowlist"`
	ExpiresAt   *string  `json:"expires_at"`
	Temporary   bool     `json:"temporary"`
	RequestID   string   `json:"request_id"`
}

// whoami signs GET /api/v1/whoami with the pair and returns the response.
func whoami(t *testing.T, ts *httptest.Server, ak, sk string, mutate func(*http.Request)) (*http.Response, []byte) {
	t.Helper()
	ctx := context.Background()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/api/v1/whoami", nil)
	require.NoError(t, err)
	req.Header.Set("X-Amz-Content-Sha256", "UNSIGNED-PAYLOAD")
	if ak != "" {
		signer := v4.NewSigner(func(o *v4.SignerOptions) { o.DisableURIPathEscaping = true })
		require.NoError(t, signer.SignHTTP(ctx, aws.Credentials{AccessKeyID: ak, SecretAccessKey: sk}, req, "UNSIGNED-PAYLOAD", "s3", "us-east-1", time.Now().UTC()))
	}
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

func TestWhoami_PrimaryScopedAndSTS(t *testing.T) {
	if testing.Short() {
		t.Skip("DB-backed")
	}
	f := setupBypassFixture(t)
	ts := httptest.NewServer(http.HandlerFunc(f.srv.handleWhoami))
	t.Cleanup(ts.Close)

	// the primary pair
	resp, body := whoami(t, ts, f.rootAK, f.rootSK, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	assert.Equal(t, "application/json", resp.Header.Get("Content-Type"))
	assert.Equal(t, "no-store", resp.Header.Get("Cache-Control"))
	var a whoamiAnswer
	require.NoError(t, json.Unmarshal(body, &a))
	assert.Equal(t, "whoami", a.Object)
	assert.Equal(t, f.tenantID, a.TenantID)
	assert.Equal(t, f.rootAK, a.KeyID)
	assert.Equal(t, "primary", a.KeyType)
	assert.Equal(t, "primary", a.KeyName)
	assert.Equal(t, []string{"*"}, a.Permissions)
	assert.Equal(t, []string{}, a.BucketScope, "never null")
	assert.Equal(t, []string{}, a.IPAllowlist, "never null")
	assert.Nil(t, a.ExpiresAt)
	assert.False(t, a.Temporary)
	assert.NotEmpty(t, a.RequestID)
	assert.NotContains(t, string(body), f.rootSK, "never a secret")

	// a scoped key
	ak, sk := f.key("GetObject", "ListObjects")
	resp, body = whoami(t, ts, ak, sk, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	require.NoError(t, json.Unmarshal(body, &a))
	assert.Equal(t, f.tenantID, a.TenantID)
	assert.Equal(t, ak, a.KeyID)
	assert.Equal(t, "scoped", a.KeyType)
	assert.Equal(t, "scoped", a.KeyName)
	assert.ElementsMatch(t, []string{"GetObject", "ListObjects"}, a.Permissions)
	assert.NotContains(t, string(body), sk)

	// an STS token of the primary: temporary, its own expiry, the parent's scope narrowed
	tok, err := auth.GenerateSTSToken(context.Background(), f.db, f.tenantID, f.rootAK,
		&auth.KeyScope{Permissions: []string{"*"}}, auth.STSRequest{Permissions: []string{"GetObject"}, BucketScope: []string{f.bucket}, TTL: 600})
	require.NoError(t, err)
	resp, body = whoami(t, ts, tok.AccessKey, tok.SecretKey, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	require.NoError(t, json.Unmarshal(body, &a))
	assert.Equal(t, f.tenantID, a.TenantID)
	assert.Equal(t, tok.AccessKey, a.KeyID)
	assert.Equal(t, "sts", a.KeyType)
	assert.True(t, a.Temporary)
	assert.Equal(t, []string{"GetObject"}, a.Permissions)
	assert.Equal(t, []string{f.bucket}, a.BucketScope)
	require.NotNil(t, a.ExpiresAt)
	exp, err := time.Parse(time.RFC3339, *a.ExpiresAt)
	require.NoError(t, err)
	assert.WithinDuration(t, time.Now().Add(600*time.Second), exp, 30*time.Second)
	assert.NotContains(t, string(body), tok.SecretKey)
}

func TestWhoami_Refusals(t *testing.T) {
	if testing.Short() {
		t.Skip("DB-backed")
	}
	f := setupBypassFixture(t)
	ts := httptest.NewServer(http.HandlerFunc(f.srv.handleWhoami))
	t.Cleanup(ts.Close)

	envelope := func(body []byte) (string, string) {
		var e struct {
			Error struct {
				Type string `json:"type"`
				Code string `json:"code"`
			} `json:"error"`
		}
		_ = json.Unmarshal(body, &e)
		return e.Error.Type, e.Error.Code
	}

	// unsigned
	resp, body := whoami(t, ts, "", "", nil)
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode, string(body))
	typ, code := envelope(body)
	assert.Equal(t, ErrTypeAuthentication, typ)
	assert.Equal(t, "invalid_credentials", code)

	// a wrong secret
	resp, body = whoami(t, ts, f.rootAK, "not-the-secret", nil)
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode, string(body))
	_, code = envelope(body)
	assert.Equal(t, "signature_mismatch", code)

	// a revoked key: known and dead (the S3 layer says InvalidAccessKeyId)
	ak, sk := f.key("GetObject")
	f.exec(`UPDATE api_keys SET revoked_at = NOW() WHERE key_id = $1`, ak)
	resp, body = whoami(t, ts, ak, sk, nil)
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode, string(body))
	_, code = envelope(body)
	assert.Equal(t, "key_revoked", code)

	// an expired key
	ak, sk = f.key("GetObject")
	f.exec(`UPDATE api_keys SET expires_at = NOW() - INTERVAL '1 hour' WHERE key_id = $1`, ak)
	resp, body = whoami(t, ts, ak, sk, nil)
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode, string(body))
	_, code = envelope(body)
	assert.Equal(t, "key_expired", code)

	// an IP-restricted key from the wrong address
	ak, sk = f.key("GetObject")
	f.exec(`UPDATE api_keys SET ip_allowlist = '{203.0.113.0/24}' WHERE key_id = $1`, ak)
	resp, body = whoami(t, ts, ak, sk, nil)
	assert.Equal(t, http.StatusForbidden, resp.StatusCode, string(body))
	typ, code = envelope(body)
	assert.Equal(t, ErrTypePermission, typ)
	assert.Equal(t, "ip_denied", code)

	// a suspended tenant
	f.exec(`UPDATE tenants SET suspended_at = NOW() WHERE id = $1`, f.tenantID)
	t.Cleanup(func() { f.exec(`UPDATE tenants SET suspended_at = NULL WHERE id = $1`, f.tenantID) })
	resp, body = whoami(t, ts, f.rootAK, f.rootSK, nil)
	assert.Equal(t, http.StatusForbidden, resp.StatusCode, string(body))
	_, code = envelope(body)
	assert.Equal(t, "account_suspended", code)
}

// The route is on the production router, documented, and answers only GET.
func TestWhoami_RouteIsRegistered(t *testing.T) {
	f := setupBypassFixture(t)
	f.srv.registerWhoamiRoute()
	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		r := httptest.NewRequest(m, "/api/v1/whoami", nil)
		w := httptest.NewRecorder()
		f.srv.router.ServeHTTP(w, r)
		assert.Equal(t, http.StatusMethodNotAllowed, w.Code, m)
	}
	r := httptest.NewRequest(http.MethodGet, "/api/v1/whoami", nil)
	w := httptest.NewRecorder()
	f.srv.router.ServeHTTP(w, r)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}
