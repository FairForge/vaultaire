package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// signV4 signs r the way real S3 clients do (aws-sdk-go-v2 signer in S3 mode:
// single URI encoding, payload hash header included in the signature).
func signV4(t *testing.T, r *http.Request, accessKey, secret, region, payloadHash string, when time.Time) {
	t.Helper()
	r.Header.Set("X-Amz-Content-Sha256", payloadHash)
	signer := v4.NewSigner(func(o *v4.SignerOptions) {
		o.DisableURIPathEscaping = true // S3 semantics: sign the path as sent
	})
	creds := aws.Credentials{AccessKeyID: accessKey, SecretAccessKey: secret}
	err := signer.SignHTTP(context.Background(), creds, r, payloadHash, "s3", region, when)
	require.NoError(t, err)
}

func sha256Hex(body string) string {
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:])
}

const (
	testSecret = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
	testAK     = "AKIAIOSFODNN7EXAMPLE"
)

// verify parses the Authorization header off r and runs signature verification
// against the given secret — the seam ValidateRequest uses after key lookup.
func verify(t *testing.T, r *http.Request, secret string) error {
	t.Helper()
	a := NewAuth(nil, zap.NewNop())
	params, err := parseSigV4AuthHeader(r.Header.Get("Authorization"))
	if err != nil {
		return err
	}
	return a.verifySigV4(r, params, secret)
}

func TestVerifySigV4_AcceptsValidSignatures(t *testing.T) {
	now := time.Now().UTC()

	t.Run("GET root", func(t *testing.T) {
		r := httptest.NewRequest("GET", "http://stored.ge/", nil)
		signV4(t, r, testAK, testSecret, "us-east-1", sha256Hex(""), now)
		require.NoError(t, verify(t, r, testSecret))
	})

	t.Run("GET with query params needing AWS encoding", func(t *testing.T) {
		// Spaces, unicode, and slashes in query values exercise %20-style
		// encoding (url.QueryEscape's '+' would break this).
		r := httptest.NewRequest("GET",
			"http://stored.ge/my-bucket?list-type=2&prefix=my+folder%2F%C3%A9t%C3%A9&delimiter=%2F", nil)
		signV4(t, r, testAK, testSecret, "us-east-1", sha256Hex(""), now)
		require.NoError(t, verify(t, r, testSecret))
	})

	t.Run("PUT signed payload non-default region", func(t *testing.T) {
		// eu-west-1 catches a hardcoded us-east-1 in key derivation or scope.
		body := "hello vaultaire"
		r := httptest.NewRequest("PUT", "http://stored.ge/my-bucket/hello.txt",
			strings.NewReader(body))
		signV4(t, r, testAK, testSecret, "eu-west-1", sha256Hex(body), now)
		require.NoError(t, verify(t, r, testSecret))
	})

	t.Run("PUT UNSIGNED-PAYLOAD", func(t *testing.T) {
		r := httptest.NewRequest("PUT", "http://stored.ge/my-bucket/blob.bin",
			strings.NewReader("data"))
		signV4(t, r, testAK, testSecret, "us-east-1", "UNSIGNED-PAYLOAD", now)
		require.NoError(t, verify(t, r, testSecret))
	})

	t.Run("PUT streaming chunked payload marker", func(t *testing.T) {
		r := httptest.NewRequest("PUT", "http://stored.ge/my-bucket/big.bin",
			strings.NewReader("chunked-body-placeholder"))
		r.Header.Set("Content-Encoding", "aws-chunked")
		r.Header.Set("X-Amz-Decoded-Content-Length", "24")
		signV4(t, r, testAK, testSecret, "us-east-1", "STREAMING-AWS4-HMAC-SHA256-PAYLOAD", now)
		require.NoError(t, verify(t, r, testSecret))
	})

	t.Run("path with encoded specials", func(t *testing.T) {
		r := httptest.NewRequest("GET",
			"http://stored.ge/my-bucket/my%20file%20%281%29%2Bplus.txt", nil)
		signV4(t, r, testAK, testSecret, "us-east-1", sha256Hex(""), now)
		require.NoError(t, verify(t, r, testSecret))
	})

	t.Run("extra unsigned header does not break signature", func(t *testing.T) {
		r := httptest.NewRequest("GET", "http://stored.ge/my-bucket", nil)
		signV4(t, r, testAK, testSecret, "us-east-1", sha256Hex(""), now)
		// Proxies (HAProxy/Cloudflare) add headers after signing.
		r.Header.Set("X-Forwarded-For", "203.0.113.9")
		r.Header.Set("CF-Connecting-IP", "203.0.113.9")
		require.NoError(t, verify(t, r, testSecret))
	})
}

func TestVerifySigV4_RejectsBadSignatures(t *testing.T) {
	now := time.Now().UTC()

	t.Run("wrong secret", func(t *testing.T) {
		r := httptest.NewRequest("GET", "http://stored.ge/my-bucket", nil)
		signV4(t, r, testAK, "not-the-real-secret", "us-east-1", sha256Hex(""), now)
		err := verify(t, r, testSecret)
		require.Error(t, err)
		assert.True(t, errors.Is(err, ErrSignatureMismatch), "want ErrSignatureMismatch, got %v", err)
	})

	t.Run("tampered signature", func(t *testing.T) {
		r := httptest.NewRequest("GET", "http://stored.ge/my-bucket", nil)
		signV4(t, r, testAK, testSecret, "us-east-1", sha256Hex(""), now)
		auth := r.Header.Get("Authorization")
		r.Header.Set("Authorization", auth[:len(auth)-8]+"deadbeef")
		err := verify(t, r, testSecret)
		require.Error(t, err)
		assert.True(t, errors.Is(err, ErrSignatureMismatch))
	})

	t.Run("tampered query after signing", func(t *testing.T) {
		r := httptest.NewRequest("GET", "http://stored.ge/my-bucket?prefix=alpha", nil)
		signV4(t, r, testAK, testSecret, "us-east-1", sha256Hex(""), now)
		r.URL.RawQuery = "prefix=beta"
		err := verify(t, r, testSecret)
		require.Error(t, err)
		assert.True(t, errors.Is(err, ErrSignatureMismatch))
	})

	t.Run("tampered path after signing", func(t *testing.T) {
		r := httptest.NewRequest("DELETE", "http://stored.ge/my-bucket/a.txt", nil)
		signV4(t, r, testAK, testSecret, "us-east-1", sha256Hex(""), now)
		r.URL.Path = "/my-bucket/b.txt"
		r.URL.RawPath = ""
		err := verify(t, r, testSecret)
		require.Error(t, err)
		assert.True(t, errors.Is(err, ErrSignatureMismatch))
	})

	t.Run("tampered payload hash after signing", func(t *testing.T) {
		body := "hello"
		r := httptest.NewRequest("PUT", "http://stored.ge/my-bucket/a.txt", strings.NewReader(body))
		signV4(t, r, testAK, testSecret, "us-east-1", sha256Hex(body), now)
		r.Header.Set("X-Amz-Content-Sha256", sha256Hex("evil"))
		err := verify(t, r, testSecret)
		require.Error(t, err)
		assert.True(t, errors.Is(err, ErrSignatureMismatch))
	})

	t.Run("stale X-Amz-Date", func(t *testing.T) {
		r := httptest.NewRequest("GET", "http://stored.ge/my-bucket", nil)
		signV4(t, r, testAK, testSecret, "us-east-1", sha256Hex(""), now.Add(-25*time.Minute))
		err := verify(t, r, testSecret)
		require.Error(t, err)
		assert.True(t, errors.Is(err, ErrRequestTimeSkewed), "want ErrRequestTimeSkewed, got %v", err)
	})

	t.Run("future X-Amz-Date", func(t *testing.T) {
		r := httptest.NewRequest("GET", "http://stored.ge/my-bucket", nil)
		signV4(t, r, testAK, testSecret, "us-east-1", sha256Hex(""), now.Add(25*time.Minute))
		err := verify(t, r, testSecret)
		require.Error(t, err)
		assert.True(t, errors.Is(err, ErrRequestTimeSkewed))
	})

	t.Run("credential scope date disagrees with X-Amz-Date", func(t *testing.T) {
		r := httptest.NewRequest("GET", "http://stored.ge/my-bucket", nil)
		signV4(t, r, testAK, testSecret, "us-east-1", sha256Hex(""), now)
		// Rewrite the scope date inside Credential=AK/DATE/region/s3/aws4_request.
		authz := r.Header.Get("Authorization")
		today := now.Format("20060102")
		r.Header.Set("Authorization", strings.Replace(authz, "/"+today+"/", "/19700101/", 1))
		err := verify(t, r, testSecret)
		require.Error(t, err)
	})
}

func TestParseSigV4AuthHeader(t *testing.T) {
	t.Run("well-formed", func(t *testing.T) {
		h := "AWS4-HMAC-SHA256 Credential=AKID/20260716/us-east-1/s3/aws4_request, " +
			"SignedHeaders=host;x-amz-content-sha256;x-amz-date, Signature=abc123"
		p, err := parseSigV4AuthHeader(h)
		require.NoError(t, err)
		assert.Equal(t, "AKID", p.AccessKey)
		assert.Equal(t, "20260716", p.Date)
		assert.Equal(t, "us-east-1", p.Region)
		assert.Equal(t, "s3", p.Service)
		assert.Equal(t, "host;x-amz-content-sha256;x-amz-date", p.SignedHeaders)
		assert.Equal(t, "abc123", p.Signature)
	})

	t.Run("no space after commas", func(t *testing.T) {
		h := "AWS4-HMAC-SHA256 Credential=AKID/20260716/eu-west-1/s3/aws4_request," +
			"SignedHeaders=host;x-amz-date,Signature=abc123"
		p, err := parseSigV4AuthHeader(h)
		require.NoError(t, err)
		assert.Equal(t, "AKID", p.AccessKey)
		assert.Equal(t, "eu-west-1", p.Region)
	})

	t.Run("malformed credential scope", func(t *testing.T) {
		h := "AWS4-HMAC-SHA256 Credential=AKID/garbage, SignedHeaders=host, Signature=abc"
		_, err := parseSigV4AuthHeader(h)
		require.Error(t, err)
	})

	t.Run("missing signature", func(t *testing.T) {
		h := "AWS4-HMAC-SHA256 Credential=AKID/20260716/us-east-1/s3/aws4_request, SignedHeaders=host"
		_, err := parseSigV4AuthHeader(h)
		require.Error(t, err)
	})
}

// --- Integration: full ValidateRequest path against a real DB ---

func setupSigV4Tenant(t *testing.T) (*Auth, string, string) {
	t.Helper()
	db := setupTestDB(t)
	t.Cleanup(func() { _ = db.Close() })

	const ak, sk = "VKSIGV4TESTKEY", "SKsigv4integrationsecret"
	_, err := db.Exec(`INSERT INTO tenants (id, name, email, access_key, secret_key)
		VALUES ('sigv4-test-tenant', 'SigV4 Test', 'sigv4@stored.ge', $1, $2)
		ON CONFLICT (id) DO UPDATE SET access_key = $1, secret_key = $2`, ak, sk)
	require.NoError(t, err)

	return NewAuth(db, zap.NewNop()), ak, sk
}

func TestValidateRequest_SigV4Enforcement(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping database test")
	}
	now := time.Now().UTC()

	t.Run("correctly signed request authenticates", func(t *testing.T) {
		a, ak, sk := setupSigV4Tenant(t)
		r := httptest.NewRequest("GET", "http://stored.ge/some-bucket?list-type=2", nil)
		signV4(t, r, ak, sk, "us-east-1", sha256Hex(""), now)

		tenantID, scope, err := a.ValidateRequest(r)
		require.NoError(t, err)
		assert.Equal(t, "sigv4-test-tenant", tenantID)
		require.NotNil(t, scope)
	})

	t.Run("valid key with wrong signature is rejected", func(t *testing.T) {
		a, ak, _ := setupSigV4Tenant(t)
		r := httptest.NewRequest("GET", "http://stored.ge/some-bucket", nil)
		signV4(t, r, ak, "attacker-guessed-secret", "us-east-1", sha256Hex(""), now)

		_, _, err := a.ValidateRequest(r)
		require.Error(t, err)
		assert.True(t, errors.Is(err, ErrSignatureMismatch), "want ErrSignatureMismatch, got %v", err)
	})

	t.Run("legacy AWS basic format is rejected when enforcing", func(t *testing.T) {
		a, ak, _ := setupSigV4Tenant(t)
		r := httptest.NewRequest("GET", "http://stored.ge/some-bucket", nil)
		r.Header.Set("Authorization", "AWS "+ak+":bogus-sigv2-signature")

		_, _, err := a.ValidateRequest(r)
		require.Error(t, err, "key-ID-only auth must not authenticate under SigV4 enforcement")
	})

	t.Run("AWSAccessKeyId query fallback is rejected when enforcing", func(t *testing.T) {
		a, ak, _ := setupSigV4Tenant(t)
		r := httptest.NewRequest("GET", "http://stored.ge/some-bucket?AWSAccessKeyId="+ak, nil)

		_, _, err := a.ValidateRequest(r)
		require.Error(t, err, "bare access key ID in query must not authenticate under SigV4 enforcement")
	})

	t.Run("SIGV4_ENFORCE=false falls back to key-existence auth", func(t *testing.T) {
		t.Setenv("SIGV4_ENFORCE", "false")
		a, ak, _ := setupSigV4Tenant(t)
		r := httptest.NewRequest("GET", "http://stored.ge/some-bucket", nil)
		signV4(t, r, ak, "wrong-secret-but-flag-off", "us-east-1", sha256Hex(""), now)

		tenantID, _, err := a.ValidateRequest(r)
		require.NoError(t, err)
		assert.Equal(t, "sigv4-test-tenant", tenantID)
	})

	t.Run("unknown access key still rejected", func(t *testing.T) {
		a, _, _ := setupSigV4Tenant(t)
		r := httptest.NewRequest("GET", "http://stored.ge/some-bucket", nil)
		signV4(t, r, "VKNOSUCHKEY", "whatever", "us-east-1", sha256Hex(""), now)

		_, _, err := a.ValidateRequest(r)
		require.Error(t, err)
	})

	t.Run("scoped key without stored secret fails with actionable error", func(t *testing.T) {
		// Legacy api_keys rows carry only a bcrypt secret_hash (secret_key
		// NULL) — their signature can never be verified. They must fail
		// closed with a message telling the operator to regenerate the key.
		db := setupTestDB(t)
		t.Cleanup(func() { _ = db.Close() })
		a := NewAuth(db, zap.NewNop())

		const ak = "VLT_legacy_hashonly"
		var userID string
		require.NoError(t, db.QueryRow(`INSERT INTO users (email, password_hash)
			VALUES ('sigv4-legacy@stored.ge', 'x') RETURNING id`).Scan(&userID))
		_, err := db.Exec(`INSERT INTO tenants (id, name, email, access_key, secret_key)
			VALUES ('sigv4-legacy-tenant', 'Legacy', 'sigv4-legacy@stored.ge', 'VKLEGACYPRIMARY', 'primary-secret')
			ON CONFLICT (id) DO NOTHING`)
		require.NoError(t, err)
		_, err = db.Exec(`INSERT INTO api_keys (user_id, name, key_id, secret_hash)
			VALUES ($1, 'legacy', $2, 'bcrypt-hash-only') ON CONFLICT (key_id) DO NOTHING`, userID, ak)
		require.NoError(t, err)
		t.Cleanup(func() {
			_, _ = db.Exec(`DELETE FROM api_keys WHERE key_id = $1`, ak)
			_, _ = db.Exec(`DELETE FROM tenants WHERE id = 'sigv4-legacy-tenant'`)
			_, _ = db.Exec(`DELETE FROM users WHERE id = $1`, userID)
		})

		r := httptest.NewRequest("GET", "http://stored.ge/some-bucket", nil)
		signV4(t, r, ak, "the-real-secret-the-user-still-has", "us-east-1", sha256Hex(""), now)

		_, _, err = a.ValidateRequest(r)
		require.Error(t, err)
		assert.True(t, errors.Is(err, ErrSignatureMismatch))
		assert.Contains(t, err.Error(), "regenerate", "error must tell the operator how to fix the key")
	})
}
