package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/FairForge/vaultaire/internal/auth"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	promtest "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Stage 1a Prompt 42 PR B (plan 43.7; docs/surfaces/TRUST.md item 12;
// contract rules 4 and 5): S3 secrets wrapped under the tenant's key. Every
// test registers its own account through the auth service on a server with a
// tenant key store, and signs with aws-sdk-go-v2 through handleS3Request.

// wrappedRow reads a credential row's wrapped form.
func (f *lifecycleFixture) wrappedRow(table, idCol, keyID string) (wrapped []byte, version *int, plaintext *string) {
	f.t.Helper()
	require.NoError(f.t, f.db.QueryRow(`SELECT secret_wrapped, secret_key_version, secret_key FROM `+table+` WHERE `+idCol+` = $1`,
		keyID).Scan(&wrapped, &version, &plaintext))
	return wrapped, version, plaintext
}

// nullPlaintext is what a dump without the plaintext column holds (PR C's
// drop): every reader must sign from the wrapped form alone.
func (f *lifecycleFixture) nullPlaintext() {
	f.t.Helper()
	f.exec(`UPDATE api_keys SET secret_key = NULL WHERE tenant_id = $1`, f.tenant.ID)
	f.exec(`UPDATE sts_tokens SET secret_key = NULL WHERE tenant_id = $1`, f.tenant.ID)
	f.exec(`UPDATE tenants SET secret_key = NULL WHERE id = $1`, f.tenant.ID)
}

func (f *lifecycleFixture) get(ak, sk, token, key string) (string, string) {
	f.t.Helper()
	c := s3.New(s3.Options{
		Region:                     "us-east-1",
		Credentials:                credentials.NewStaticCredentialsProvider(ak, sk, token),
		BaseEndpoint:               aws.String(f.ts.URL),
		UsePathStyle:               true,
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
		ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired,
		RetryMaxAttempts:           1,
	})
	out, err := c.GetObject(context.Background(), &s3.GetObjectInput{Bucket: aws.String(f.bucket), Key: aws.String(key)})
	if err != nil {
		return "", s3ErrorCode(err)
	}
	defer func() { _ = out.Body.Close() }()
	b, err := io.ReadAll(out.Body)
	require.NoError(f.t, err)
	return string(b), ""
}

func TestLookupCredential_WrappedRowSignsAWSRequest(t *testing.T) {
	f := setupLifecycleFixtureWith(t, keyTestMasterA)
	ctx := context.Background()

	// Registration wrapped the primary pair under the tenant's v1.
	wrapped, version, plaintext := f.wrappedRow("api_keys", "key_id", f.key.Key)
	require.NotEmpty(t, wrapped, "registration wraps the primary secret")
	require.NotNil(t, version)
	assert.Equal(t, 1, *version)
	require.NotNil(t, plaintext, "PR B still writes the plaintext (expand)")
	assert.NotContains(t, string(wrapped), f.key.Secret)

	// A key made later and a rotation's successor are wrapped too.
	extra, err := f.svc.GenerateAPIKey(ctx, f.user.ID, "extra", nil)
	require.NoError(t, err)
	w, _, _ := f.wrappedRow("api_keys", "key_id", extra.Key)
	assert.NotEmpty(t, w, "GenerateAPIKey wraps")

	// The plaintext columns gone (the drop, or a dump restored without
	// them): the wrapped form alone signs a real PUT and GET.
	f.nullPlaintext()
	require.Equal(t, "", f.put(f.key.Key, f.key.Secret, "wrapped.txt"))
	body, code := f.get(f.key.Key, f.key.Secret, "", "wrapped.txt")
	require.Equal(t, "", code)
	assert.Equal(t, "bytes of wrapped.txt", body)
	assert.Equal(t, "", f.put(extra.Key, extra.Secret, "extra.txt"))

	// The presigned-URL route signs on the wrapped primary (PrimaryPair),
	// and the URL verifies (verifyPresignedURL → LookupCredential).
	req := httptest.NewRequest("GET", "/api/v1/presign?bucket="+f.bucket+"&key=wrapped.txt&method=GET&expires=600", nil)
	req = req.WithContext(context.WithValue(req.Context(), tenantIDKey, f.tenant.ID))
	rr := httptest.NewRecorder()
	f.srv.handleGetPresignedURL(rr, req)
	require.Equal(t, 200, rr.Code, rr.Body.String())
	var resp map[string]string
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	u, err := url.Parse(resp["url"])
	require.NoError(t, err)
	preq := httptest.NewRequest("GET", u.RequestURI(), nil)
	preq.Host = u.Host
	prr := httptest.NewRecorder()
	f.srv.handleS3Request(prr, preq)
	require.Equal(t, 200, prr.Code, prr.Body.String())
	assert.Equal(t, "bytes of wrapped.txt", prr.Body.String())

	// The rotation's successor is wrapped and signs with no plaintext either.
	newKey, err := f.svc.RotateAPIKey(ctx, f.user.ID, f.key.ID)
	require.NoError(t, err)
	w, _, _ = f.wrappedRow("api_keys", "key_id", newKey.Key)
	assert.NotEmpty(t, w, "RotateAPIKey wraps the successor")
	f.nullPlaintext()
	assert.Equal(t, "", f.put(newKey.Key, newKey.Secret, "rotated.txt"))
	assert.Equal(t, ErrInvalidAccessKeyId, f.put(f.key.Key, f.key.Secret, "old.txt"), "the rotated pair stays dead")
}

func TestLookupCredential_UnwrapFailureIsOursNeverPlaintext(t *testing.T) {
	f := setupLifecycleFixtureWith(t, keyTestMasterA)
	require.Equal(t, "", f.put(f.key.Key, f.key.Secret, "before.txt"))
	failures := func() float64 {
		return promtest.ToFloat64(auth.CredentialUnwrapFailures.WithLabelValues("api_keys"))
	}

	// 1. A damaged wrapped form: the plaintext column is still there and
	// correct — and is NOT used. The answer is ours (500), counted.
	before := failures()
	f.exec(`UPDATE api_keys SET secret_wrapped = set_byte(secret_wrapped, 20, get_byte(secret_wrapped, 20) # 1) WHERE key_id = $1`, f.key.Key)
	assert.Equal(t, ErrInternalError, f.put(f.key.Key, f.key.Secret, "damaged.txt"))
	assert.Equal(t, before+1, failures())

	// 2. A wrapped form moved from another row (another key of the same
	// tenant): the AAD names the key id, it does not open.
	extra, err := f.svc.GenerateAPIKey(context.Background(), f.user.ID, "extra", nil)
	require.NoError(t, err)
	f.exec(`UPDATE api_keys SET secret_wrapped = (SELECT secret_wrapped FROM api_keys WHERE key_id = $2),
	                            secret_key = (SELECT secret_key FROM api_keys WHERE key_id = $2)
	         WHERE key_id = $1`, f.key.Key, extra.Key)
	before = failures()
	assert.Equal(t, ErrInternalError, f.put(f.key.Key, extra.Secret, "moved.txt"), "another row's wrap with its own plaintext beside it")
	assert.Equal(t, before+1, failures())

	// 3. A server without the master key meets a wrapped row: ours, never
	// the plaintext beside it.
	ok := extra // untouched: its own wrap, its own plaintext
	f.srv.tenantKeys = nil
	before = failures()
	assert.Equal(t, ErrInternalError, f.put(ok.Key, ok.Secret, "nokey.txt"))
	assert.Equal(t, before+1, failures())

	// 4. The presigned-URL route: a primary that does not open is a 500,
	// not "credentials not found".
	req := httptest.NewRequest("GET", "/api/v1/presign?bucket="+f.bucket+"&key=before.txt&method=GET&expires=600", nil)
	req = req.WithContext(context.WithValue(req.Context(), tenantIDKey, f.tenant.ID))
	rr := httptest.NewRecorder()
	f.srv.handleGetPresignedURL(rr, req)
	assert.Equal(t, http.StatusInternalServerError, rr.Code, rr.Body.String())

	// 5. A row with no wrapped form still signs from the plaintext (a row
	// written before 108, or by a server without a master key).
	f.srv.tenantKeys = testTenantKeys(t, f.db, keyTestMasterA)
	require.NoError(t, f.svc.RevokeAPIKey(context.Background(), f.user.ID, extra.ID)) // the plan's key cap
	plain, err := f.svcWithoutKeys().GenerateAPIKey(context.Background(), f.user.ID, "plain", nil)
	require.NoError(t, err)
	w, _, _ := f.wrappedRow("api_keys", "key_id", plain.Key)
	require.Empty(t, w)
	assert.Equal(t, "", f.put(plain.Key, plain.Secret, "plain.txt"))
}

// svcWithoutKeys is a second auth service on the fixture's database with no
// key store (the previous build, or a server without a master key).
func (f *lifecycleFixture) svcWithoutKeys() *auth.AuthService {
	f.t.Helper()
	svc := auth.NewAuthService(nil, f.db)
	require.NoError(f.t, svc.LoadFromDB(context.Background()))
	return svc
}

func TestLookupCredential_STSWrappedAndParentChecks(t *testing.T) {
	f := setupLifecycleFixtureWith(t, keyTestMasterA)
	ctx := context.Background()

	mint := func() map[string]any {
		req := httptest.NewRequest("POST", "/api/v1/sts/token", strings.NewReader(`{"ttl":600}`))
		c := context.WithValue(req.Context(), userIDKey, f.user.ID)
		c = context.WithValue(c, tenantIDKey, f.tenant.ID)
		rr := httptest.NewRecorder()
		f.srv.handleSTSCreateToken(rr, req.WithContext(c))
		require.Equal(t, http.StatusCreated, rr.Code, rr.Body.String())
		var resp map[string]any
		require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
		return resp
	}
	tok := mint()
	// The ASIA pair signs like any key (no session token: the lookup is by
	// the access key id).
	ak, sk, st := tok["access_key"].(string), tok["secret_key"].(string), ""

	wrapped, version, _ := f.wrappedRow("sts_tokens", "access_key", ak)
	require.NotEmpty(t, wrapped, "the STS route wraps the token's secret")
	require.NotNil(t, version)

	f.nullPlaintext()
	require.Equal(t, "", f.put(f.key.Key, f.key.Secret, "sts.txt"))
	body, code := f.get(ak, sk, st, "sts.txt")
	require.Equal(t, "", code, "a wrapped STS secret signs with no plaintext")
	assert.Equal(t, "bytes of sts.txt", body)

	// A damaged wrap is ours (500), counted on its own table.
	before := promtest.ToFloat64(auth.CredentialUnwrapFailures.WithLabelValues("sts_tokens"))
	f.exec(`UPDATE sts_tokens SET secret_wrapped = set_byte(secret_wrapped, 20, get_byte(secret_wrapped, 20) # 1) WHERE access_key = $1`, ak)
	_, code = f.get(ak, sk, st, "sts.txt")
	assert.Equal(t, ErrInternalError, code)
	assert.Equal(t, before+1, promtest.ToFloat64(auth.CredentialUnwrapFailures.WithLabelValues("sts_tokens")))

	// The parent check comes first: a token of a rotated parent is dead
	// (InvalidAccessKeyId), wrapped or not.
	tok2 := mint()
	_, err := f.svc.RotateAPIKey(ctx, f.user.ID, f.key.ID)
	require.NoError(t, err)
	_, code = f.get(tok2["access_key"].(string), tok2["secret_key"].(string), "", "sts.txt")
	assert.Equal(t, ErrInvalidAccessKeyId, code)
}
