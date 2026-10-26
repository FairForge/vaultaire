package auth

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestValidateDescribed_SameVerdictAsValidateRequest (WP-EDGE-1 Part A):
// the one verifier's two entrances agree on every shape — a valid SigV4
// header, a SigV4A header, SigV2, a presigned query, a tampered Host, a
// wrong secret, a skewed date, the key's IP allow-list.
func TestValidateDescribed_SameVerdictAsValidateRequest(t *testing.T) {
	a, ak, sk := setupSigV4Tenant(t)
	ctx := context.Background()

	describe := func(r *http.Request) RequestDescriptor {
		h := http.Header{}
		for k, v := range r.Header {
			h[strings.ToLower(k)] = v // the Worker lower-cases every name
		}
		return RequestDescriptor{Method: r.Method, Host: r.Host, Path: r.URL.EscapedPath(), RawQuery: r.URL.RawQuery, Headers: h, ContentLength: r.ContentLength}
	}
	both := func(r *http.Request, clientIP string) (string, error, string, error) {
		d := describe(r)
		liveTenant, _, liveErr := a.ValidateRequest(r)
		descTenant, _, descErr := a.ValidateDescribed(ctx, d, clientIP)
		return liveTenant, liveErr, descTenant, descErr
	}
	newPut := func() *http.Request {
		r, _ := http.NewRequest(http.MethodPut, "https://s3.stored.test/bkt/a%2Fb.txt?x-id=PutObject", strings.NewReader("hello"))
		r.ContentLength = 5
		r.Header.Set("Content-Length", "5")
		r.Header.Set("Content-Type", "text/plain")
		r.RemoteAddr = "203.0.113.4:1"
		return r
	}

	t.Run("valid SigV4 header", func(t *testing.T) {
		r := newPut()
		signV4(t, r, ak, sk, "us-east-1", sha256Hex("hello"), time.Now())
		lt, le, dt, de := both(r, "203.0.113.4")
		require.NoError(t, le)
		require.NoError(t, de)
		assert.Equal(t, lt, dt)
		assert.Equal(t, "sigv4-test-tenant", dt)
	})
	t.Run("wrong secret", func(t *testing.T) {
		r := newPut()
		signV4(t, r, ak, "not-the-secret", "us-east-1", sha256Hex("hello"), time.Now())
		_, le, _, de := both(r, "203.0.113.4")
		assert.ErrorIs(t, le, ErrSignatureMismatch)
		assert.ErrorIs(t, de, ErrSignatureMismatch)
	})
	t.Run("tampered host", func(t *testing.T) {
		r := newPut()
		signV4(t, r, ak, sk, "us-east-1", sha256Hex("hello"), time.Now())
		r.Host = "evil.stored.test"
		_, le, _, de := both(r, "203.0.113.4")
		assert.ErrorIs(t, le, ErrSignatureMismatch)
		assert.ErrorIs(t, de, ErrSignatureMismatch)
	})
	t.Run("skewed date", func(t *testing.T) {
		r := newPut()
		signV4(t, r, ak, sk, "us-east-1", sha256Hex("hello"), time.Now().Add(-2*time.Hour))
		_, le, _, de := both(r, "203.0.113.4")
		assert.ErrorIs(t, le, ErrRequestTimeSkewed)
		assert.ErrorIs(t, de, ErrRequestTimeSkewed)
	})
	t.Run("SigV4A header", func(t *testing.T) {
		r := newPut()
		r.Header.Set("Authorization", "AWS4-ECDSA-P256-SHA256 Credential="+ak+"/20260101/us-east-1/s3/aws4_request, SignedHeaders=host, Signature=00")
		_, le, _, de := both(r, "203.0.113.4")
		require.Error(t, le)
		require.Error(t, de)
		assert.Equal(t, le.Error(), de.Error())
	})
	t.Run("SigV2 header", func(t *testing.T) {
		r := newPut()
		r.Header.Set("Authorization", "AWS "+ak+":c2lnbmF0dXJl")
		_, le, _, de := both(r, "203.0.113.4")
		assert.ErrorIs(t, le, ErrSignatureMismatch)
		assert.ErrorIs(t, de, ErrSignatureMismatch)
	})
	t.Run("presigned query is not verified by description", func(t *testing.T) {
		r := newPut()
		q := url.Values{"X-Amz-Algorithm": {"AWS4-HMAC-SHA256"}, "X-Amz-Credential": {ak + "/20260101/us-east-1/s3/aws4_request"}, "X-Amz-Signature": {"00"}}
		r.URL.RawQuery = q.Encode()
		_, _, de := a.ValidateDescribed(ctx, describe(r), "203.0.113.4")
		assert.ErrorIs(t, de, ErrSignatureMismatch)
	})
	t.Run("the key's IP allow-list", func(t *testing.T) {
		_, err := a.db.Exec(`UPDATE api_keys SET ip_allowlist = '{10.0.0.0/8}' WHERE key_id = $1`, ak)
		require.NoError(t, err)
		t.Cleanup(func() { _, _ = a.db.Exec(`UPDATE api_keys SET ip_allowlist = '{}' WHERE key_id = $1`, ak) })
		r := newPut()
		signV4(t, r, ak, sk, "us-east-1", sha256Hex("hello"), time.Now())
		_, le, _, de := both(r, "203.0.113.4")
		assert.ErrorIs(t, le, ErrIPDenied)
		assert.ErrorIs(t, de, ErrIPDenied)
		r2 := newPut()
		r2.RemoteAddr = "10.1.2.3:1"
		signV4(t, r2, ak, sk, "us-east-1", sha256Hex("hello"), time.Now())
		_, le2, _, de2 := both(r2, "10.1.2.3")
		assert.NoError(t, le2)
		assert.NoError(t, de2)
	})
	t.Run("the described entrance never reads a body", func(t *testing.T) {
		r := newPut()
		signV4(t, r, ak, sk, "us-east-1", sha256Hex("hello"), time.Now())
		d := describe(r)
		_, _, err := a.ValidateDescribed(ctx, d, "203.0.113.4")
		require.NoError(t, err)
		claim := d.Headers[strings.ToLower("X-Amz-Content-Sha256")] // the Worker's lower-cased name, not a canonical key
		assert.Equal(t, []string{sha256Hex("hello")}, claim, "the claim rides in the headers")
	})
}
