package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/FairForge/vaultaire/internal/account"
	"github.com/FairForge/vaultaire/internal/auth"
	"github.com/FairForge/vaultaire/internal/config"
	"github.com/FairForge/vaultaire/internal/drivers"
	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/FairForge/vaultaire/internal/testutil"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/go-chi/chi/v5"
	promtest "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// WP-R5-14 on the S3 entry points: a rotated primary pair is dead on the
// header-auth path (403 InvalidAccessKeyId, counted against a KNOWN key),
// on a presigned URL minted before the rotation, and for the STS tokens it
// parented; every surface that signs for the tenant uses the new row.
// Everything goes through handleS3Request with real SigV4 (aws-sdk-go-v2)
// on an account registered through the auth service, on its own tenant.

type lifecycleFixture struct {
	t      *testing.T
	db     *sql.DB
	svc    *auth.AuthService
	srv    *Server
	ts     *httptest.Server
	user   *auth.User
	tenant *auth.Tenant
	key    *auth.APIKey
	bucket string
}

func setupLifecycleFixture(t *testing.T) *lifecycleFixture {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping database test")
	}
	db, err := sql.Open("postgres", testutil.DSN())
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Ping(); err != nil {
		t.Skipf("test database unreachable (%v) — create it with `make test-db`", err)
	}
	logger := zap.NewNop()
	tempDir := t.TempDir()
	eng := engine.NewEngine(nil, logger, nil)
	eng.AddDriver("local", drivers.NewLocalDriver(tempDir, logger))
	eng.SetPrimary("local")

	svc := auth.NewAuthService(nil, db)
	email := fmt.Sprintf("lifecycle-%s@test.local", auth.GenerateID()[:8])
	user, tenant, key, err := svc.CreateUserWithTenant(context.Background(), email, "a-long-password", "Lifecycle Co")
	require.NoError(t, err)

	f := &lifecycleFixture{t: t, db: db, svc: svc, user: user, tenant: tenant, key: key, bucket: "lifecycle"}
	f.exec(`INSERT INTO buckets (tenant_id, name, visibility) VALUES ($1, $2, 'private')`, tenant.ID, f.bucket)
	require.NoError(t, os.MkdirAll(filepath.Join(tempDir, tenant.ID+"_"+f.bucket), 0o750))

	f.srv = &Server{logger: logger, router: chi.NewRouter(), engine: eng, db: db, testMode: false, auth: svc,
		config: &config.Config{Server: config.ServerConfig{Port: 8000}}}
	f.ts = httptest.NewServer(http.HandlerFunc(f.srv.handleS3Request))
	t.Cleanup(func() {
		f.ts.Close()
		time.Sleep(50 * time.Millisecond) // emitEvent's webhook lookup runs detached
		ctx := context.Background()
		for _, r := range account.Deleted {
			switch r.Key {
			case account.ByTenant:
				_, _ = db.ExecContext(ctx, r.SQL, tenant.ID)
			case account.ByUser:
				_, _ = db.ExecContext(ctx, r.SQL, user.ID)
			case account.ByEmail:
				_, _ = db.ExecContext(ctx, r.SQL, email)
			}
		}
		_, _ = db.ExecContext(ctx, `DELETE FROM audit_logs WHERE tenant_id = $1`, tenant.ID)
	})
	return f
}

func (f *lifecycleFixture) exec(q string, args ...any) {
	f.t.Helper()
	_, err := f.db.Exec(q, args...)
	require.NoError(f.t, err, q)
}

func (f *lifecycleFixture) client(ak, sk string) *s3.Client {
	return s3.New(s3.Options{
		Region:                     "us-east-1",
		Credentials:                credentials.NewStaticCredentialsProvider(ak, sk, ""),
		BaseEndpoint:               aws.String(f.ts.URL),
		UsePathStyle:               true,
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
		ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired,
		RetryMaxAttempts:           1,
	})
}

// put writes an object with the pair and returns the S3 error code ("" on success).
func (f *lifecycleFixture) put(ak, sk, key string) string {
	f.t.Helper()
	_, err := f.client(ak, sk).PutObject(context.Background(), &s3.PutObjectInput{
		Bucket: aws.String(f.bucket), Key: aws.String(key), Body: strings.NewReader("bytes of " + key)})
	if err == nil {
		return ""
	}
	return s3ErrorCode(err)
}

func s3ErrorCode(err error) string {
	msg := err.Error()
	for _, code := range []string{ErrInvalidAccessKeyId, ErrSignatureDoesNotMatch, ErrAccessDenied, ErrExpiredPresignedRequest} {
		if strings.Contains(msg, code) {
			return code
		}
	}
	return msg
}

func TestS3Auth_RotatedPrimaryPairIsDeadAndKnown(t *testing.T) {
	f := setupLifecycleFixture(t)
	ctx := context.Background()
	oldAK, oldSK := f.key.Key, f.key.Secret
	require.Equal(t, "", f.put(oldAK, oldSK, "before.txt"), "the pair registration handed out works")

	beforeRevoked := promtest.ToFloat64(authFailures.WithLabelValues("revoked", "true"))
	beforeByKey := promtest.ToFloat64(authFailuresByKey.WithLabelValues(accessKeyHash(oldAK)))

	// Act: rotate through the service (what the dashboard, the user API
	// and the management API call).
	newKey, err := f.svc.RotateAPIKey(ctx, f.user.ID, f.key.ID)
	require.NoError(t, err)

	// Assert: the old pair answers 403 InvalidAccessKeyId, counted as a
	// failure against a KNOWN key (the rule pages on a burst of these).
	assert.Equal(t, ErrInvalidAccessKeyId, f.put(oldAK, oldSK, "after-old.txt"))
	assert.Equal(t, beforeRevoked+1, promtest.ToFloat64(authFailures.WithLabelValues("revoked", "true")))
	assert.Equal(t, beforeByKey+1, promtest.ToFloat64(authFailuresByKey.WithLabelValues(accessKeyHash(oldAK))),
		"the dead key gets its per-key series: it exists and someone is still using it")

	// The new pair works, and is what the tenants mirror says.
	assert.Equal(t, "", f.put(newKey.Key, newKey.Secret, "after-new.txt"))
	var mirror string
	require.NoError(t, f.db.QueryRow(`SELECT access_key FROM tenants WHERE id = $1`, f.tenant.ID).Scan(&mirror))
	assert.Equal(t, newKey.Key, mirror)
}

func TestPresign_URLMintedBeforeTheRotationIsDead(t *testing.T) {
	f := setupLifecycleFixture(t)
	ctx := context.Background()
	require.Equal(t, "", f.put(f.key.Key, f.key.Secret, "doc.txt"))

	// The presigned-URL route signs on the live primary row.
	get := func() (string, int) {
		req := httptest.NewRequest("GET", "/api/v1/presign?bucket="+f.bucket+"&key=doc.txt&method=GET&expires=600", nil)
		req = req.WithContext(context.WithValue(req.Context(), tenantIDKey, f.tenant.ID))
		rr := httptest.NewRecorder()
		f.srv.handleGetPresignedURL(rr, req)
		require.Equal(t, 200, rr.Code, rr.Body.String())
		var resp map[string]string
		require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
		return resp["url"], rr.Code
	}
	link, _ := get()
	u, err := url.Parse(link)
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(u.Query().Get("X-Amz-Credential"), f.key.Key+"/"))

	// The URL is signed for the public host; serve it through the handler
	// with that Host, as the export test does.
	fetch := func(link string) (int, string) {
		u, err := url.Parse(link)
		require.NoError(t, err)
		req := httptest.NewRequest("GET", u.RequestURI(), nil)
		req.Host = u.Host
		rr := httptest.NewRecorder()
		f.srv.handleS3Request(rr, req)
		return rr.Code, rr.Body.String()
	}
	code, body := fetch(link)
	require.Equal(t, 200, code, body)
	assert.Equal(t, "bytes of doc.txt", body)

	newKey, err := f.svc.RotateAPIKey(ctx, f.user.ID, f.key.ID)
	require.NoError(t, err)

	code, body = fetch(link)
	assert.Equal(t, 403, code)
	assert.Contains(t, body, ErrInvalidAccessKeyId, "a presigned URL of the old pair is dead")

	link2, _ := get()
	u2, err := url.Parse(link2)
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(u2.Query().Get("X-Amz-Credential"), newKey.Key+"/"), "a URL minted after the rotation is on the new pair")
	code, body = fetch(link2)
	assert.Equal(t, 200, code, body)
}

// The STS route's default parent is the primary ROW, so a rotation of the
// primary kills the tokens minted from it (WP-R5-5).
func TestSTSRoute_DefaultParentIsThePrimaryRowAndDiesWithIt(t *testing.T) {
	f := setupLifecycleFixture(t)
	ctx := context.Background()

	mint := func() (int, map[string]any) {
		req := httptest.NewRequest("POST", "/api/v1/sts/token", strings.NewReader(`{"permissions":["PutObject"],"ttl":600}`))
		c := context.WithValue(req.Context(), userIDKey, f.user.ID)
		c = context.WithValue(c, tenantIDKey, f.tenant.ID)
		rr := httptest.NewRecorder()
		f.srv.handleSTSCreateToken(rr, req.WithContext(c))
		var resp map[string]any
		_ = json.Unmarshal(rr.Body.Bytes(), &resp)
		return rr.Code, resp
	}
	code, resp := mint()
	require.Equal(t, 201, code, resp)
	ak, _ := resp["access_key"].(string)
	sk, _ := resp["secret_key"].(string)
	var parent string
	require.NoError(t, f.db.QueryRow(`SELECT parent_key_id FROM sts_tokens WHERE access_key = $1`, ak).Scan(&parent))
	assert.Equal(t, f.key.Key, parent, "the default parent is the primary key id, never a synthetic name")
	require.Equal(t, "", f.put(ak, sk, "by-token.txt"))

	_, err := f.svc.RotateAPIKey(ctx, f.user.ID, f.key.ID)
	require.NoError(t, err)

	assert.Equal(t, ErrInvalidAccessKeyId, f.put(ak, sk, "by-token-after.txt"), "the token died with its parent")
	code, resp = mint()
	require.Equal(t, 201, code, resp)
	ak2, _ := resp["access_key"].(string)
	require.NoError(t, f.db.QueryRow(`SELECT parent_key_id FROM sts_tokens WHERE access_key = $1`, ak2).Scan(&parent))
	assert.NotEqual(t, f.key.Key, parent, "a token minted after the rotation is parented by the new primary")
}

// A scoped key with an IP allowlist: a token asking for a wider or disjoint
// restriction is refused, one asking for none inherits the parent's.
func TestSTSRoute_IPRestrictionCannotBeDropped(t *testing.T) {
	f := setupLifecycleFixture(t)
	ctx := context.Background()
	parent, err := f.svc.GenerateAPIKey(ctx, f.user.ID, "office", &auth.KeyCreateOptions{
		Permissions: []string{"PutObject"}, IPAllowlist: []string{"203.0.113.0/24"}})
	require.NoError(t, err)

	mint := func(body string) (int, map[string]any) {
		req := httptest.NewRequest("POST", "/api/v1/sts/token", strings.NewReader(body))
		c := context.WithValue(req.Context(), userIDKey, f.user.ID)
		c = context.WithValue(c, tenantIDKey, f.tenant.ID)
		rr := httptest.NewRecorder()
		f.srv.handleSTSCreateToken(rr, req.WithContext(c))
		var resp map[string]any
		_ = json.Unmarshal(rr.Body.Bytes(), &resp)
		return rr.Code, resp
	}
	code, resp := mint(`{"parent_key_id":"` + parent.ID + `"}`)
	require.Equal(t, 201, code, resp)
	var ips []byte
	require.NoError(t, f.db.QueryRow(`SELECT ip_restrict::text FROM sts_tokens WHERE access_key = $1`, resp["access_key"]).Scan(&ips))
	assert.Equal(t, "{203.0.113.0/24}", string(ips), "a token asking for nothing inherits the parent's allowlist")

	code, resp = mint(`{"parent_key_id":"` + parent.ID + `","ip_restrict":["0.0.0.0/0"]}`)
	assert.Equal(t, 400, code, resp)
	assert.Equal(t, "scope_error", resp["error"].(map[string]any)["code"])

	code, resp = mint(`{"parent_key_id":"` + parent.ID + `","ip_restrict":["203.0.113.7"]}`)
	require.Equal(t, 201, code, resp)
	require.NoError(t, f.db.QueryRow(`SELECT ip_restrict::text FROM sts_tokens WHERE access_key = $1`, resp["access_key"]).Scan(&ips))
	assert.Equal(t, "{203.0.113.7}", string(ips))
}

// The user API and the management API refuse to revoke the primary with a
// 409 and a reason, never a 500.
func TestKeyAPIs_PrimaryRevokeIs409(t *testing.T) {
	f := setupLifecycleFixture(t)

	req := httptest.NewRequest("DELETE", "/api/v1/user/apikeys/"+f.key.ID, nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("keyId", f.key.ID)
	c := context.WithValue(req.Context(), chi.RouteCtxKey, rctx)
	c = context.WithValue(c, userIDKey, f.user.ID)
	rr := httptest.NewRecorder()
	f.srv.handleDeleteUserAPIKey(rr, req.WithContext(c))
	assert.Equal(t, http.StatusConflict, rr.Code, rr.Body.String())
	assert.Contains(t, rr.Body.String(), "rotate")

	req = httptest.NewRequest("DELETE", "/api/v1/manage/keys/"+f.key.ID, nil)
	rctx = chi.NewRouteContext()
	rctx.URLParams.Add("id", f.key.ID)
	c = context.WithValue(req.Context(), chi.RouteCtxKey, rctx)
	c = context.WithValue(c, userIDKey, f.user.ID)
	c = context.WithValue(c, tenantIDKey, f.tenant.ID)
	rr = httptest.NewRecorder()
	f.srv.handleMgmtDeleteKey(rr, req.WithContext(c))
	assert.Equal(t, http.StatusConflict, rr.Code, rr.Body.String())
	assert.Contains(t, rr.Body.String(), "primary_key")

	var revoked sql.NullTime
	require.NoError(t, f.db.QueryRow(`SELECT revoked_at FROM api_keys WHERE id = $1`, f.key.ID).Scan(&revoked))
	assert.False(t, revoked.Valid, "the primary is still live")
}
