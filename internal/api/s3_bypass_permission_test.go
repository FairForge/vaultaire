package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	"github.com/aws/smithy-go/middleware"
	smithyhttp "github.com/aws/smithy-go/transport/http"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/auth"
	"github.com/FairForge/vaultaire/internal/config"
	"github.com/FairForge/vaultaire/internal/drivers"
	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/FairForge/vaultaire/internal/testutil"
)

// WP-R4-1 (Review R4-09 / R11-21): x-amz-bypass-governance-retention was the
// header alone — any key that could delete could bypass GOVERNANCE retention.
// The bypass is a privilege of the KEY: honoured only for a full-access key
// (`*`; the tenant's primary key is one) or a key that carries
// BypassGovernanceRetention, and only when the request's signature covers
// the header. Everything here goes through handleS3Request with real SigV4
// (aws-sdk-go-v2), because the thing under test is that the scope the
// authenticator found reaches the lock check.

const bypassPerm = "BypassGovernanceRetention"

type bypassFixture struct {
	t        *testing.T
	db       *sql.DB
	srv      *Server
	ts       *httptest.Server
	tempDir  string
	tenantID string
	userID   string
	bucket   string // unversioned
	vbucket  string // versioning Enabled
	rootAK   string
	rootSK   string
}

func setupBypassFixture(t *testing.T) *bypassFixture {
	t.Helper()
	db, err := sql.Open("postgres", testutil.DSN())
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	require.NoError(t, db.Ping())

	logger := zap.NewNop()
	tempDir, err := os.MkdirTemp("", "vaultaire-bypass-*")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(tempDir) })
	eng := engine.NewEngine(nil, logger, nil)
	eng.AddDriver("local", drivers.NewLocalDriver(tempDir, logger))
	eng.SetPrimary("local")

	id := uuid.New().String()
	f := &bypassFixture{t: t, db: db, tempDir: tempDir,
		tenantID: "byp-" + id[:8], userID: id, bucket: "locked", vbucket: "locked-versions",
		rootAK: "VKBYP" + strings.ToUpper(id[:8]), rootSK: "SK" + id}
	email := "bypass-" + id[:8] + "@test.local"
	f.exec(`INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, f.userID, email)
	f.exec(`INSERT INTO tenants (id, name, email, access_key, secret_key) VALUES ($1, 'Bypass Test', $2, $3, $4)`,
		f.tenantID, email, f.rootAK, f.rootSK)
	f.exec(`INSERT INTO buckets (tenant_id, name, visibility, object_lock_enabled) VALUES ($1, $2, 'private', TRUE)`, f.tenantID, f.bucket)
	f.exec(`INSERT INTO buckets (tenant_id, name, visibility, object_lock_enabled, versioning_status) VALUES ($1, $2, 'private', TRUE, 'Enabled')`, f.tenantID, f.vbucket)
	for _, b := range []string{f.bucket, f.vbucket} {
		require.NoError(t, os.MkdirAll(filepath.Join(tempDir, f.tenantID+"_"+b), 0o750))
	}

	f.srv = &Server{logger: logger, router: chi.NewRouter(), engine: eng, db: db, testMode: false,
		config: &config.Config{Server: config.ServerConfig{Port: 8000}}}
	f.ts = httptest.NewServer(http.HandlerFunc(f.srv.handleS3Request))
	t.Cleanup(func() {
		f.ts.Close()
		time.Sleep(50 * time.Millisecond) // emitEvent's webhook lookup runs detached
		for _, q := range []string{
			`DELETE FROM sts_tokens WHERE tenant_id = $1`,
			`DELETE FROM events WHERE tenant_id = $1`,
			`DELETE FROM object_locks WHERE tenant_id = $1`,
			`DELETE FROM object_versions WHERE tenant_id = $1`,
			`DELETE FROM multipart_uploads WHERE tenant_id = $1`,
			`DELETE FROM object_head_cache WHERE tenant_id = $1`,
			`DELETE FROM buckets WHERE tenant_id = $1`,
			`DELETE FROM tenants WHERE id = $1`,
		} {
			_, _ = db.Exec(q, f.tenantID)
		}
		_, _ = db.Exec(`DELETE FROM api_keys WHERE user_id = $1`, f.userID)
		_, _ = db.Exec(`DELETE FROM users WHERE id = $1`, f.userID)
	})
	return f
}

func (f *bypassFixture) exec(q string, args ...any) {
	f.t.Helper()
	_, err := f.db.Exec(q, args...)
	require.NoError(f.t, err, q)
}

// key creates a scoped API key of the fixture's user with exactly perms.
func (f *bypassFixture) key(perms ...string) (ak, sk string) {
	f.t.Helper()
	id := uuid.New().String()
	ak, sk = "VLT_"+strings.ToUpper(strings.ReplaceAll(id[:13], "-", "")), "sk-"+id
	pj, err := json.Marshal(perms)
	require.NoError(f.t, err)
	f.exec(`INSERT INTO api_keys (id, user_id, name, key_id, secret_hash, secret_key, permissions) VALUES ($1, $2, 'scoped', $3, 'h', $4, $5)`,
		id, f.userID, ak, sk, pj)
	return ak, sk
}

// writer is what a backup tool's key looks like: everything it needs to
// write, delete and manage retention — and nothing that says "bypass".
var writerPerms = []string{"GetObject", "HeadObject", "PutObject", "DeleteObject", "DeleteObjects", "ListObjects",
	"PutObjectRetention", "GetObjectRetention", "InitiateMultipartUpload", "UploadPart", "CompleteMultipartUpload"}

func (f *bypassFixture) client(ak, sk string) *s3.Client {
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

func (f *bypassFixture) root() *s3.Client { return f.client(f.rootAK, f.rootSK) }

// locked writes an object with the primary key and puts it under retention.
func (f *bypassFixture) locked(bucket, key, mode string) {
	f.t.Helper()
	_, err := f.root().PutObject(context.Background(), &s3.PutObjectInput{
		Bucket: aws.String(bucket), Key: aws.String(key), Body: strings.NewReader("retained bytes of " + key)})
	require.NoError(f.t, err)
	f.exec(`INSERT INTO object_locks (tenant_id, bucket, object_key, retention_mode, retain_until_date)
		VALUES ($1, $2, $3, $4, NOW() + INTERVAL '30 days')
		ON CONFLICT (tenant_id, bucket, object_key) DO UPDATE SET retention_mode = EXCLUDED.retention_mode, retain_until_date = EXCLUDED.retain_until_date`,
		f.tenantID, bucket, key, mode)
}

// intact: the object is exactly as it was locked — row, bytes, retention.
func (f *bypassFixture) intact(bucket, key string) bool {
	f.t.Helper()
	var n int
	require.NoError(f.t, f.db.QueryRow(`SELECT COUNT(*) FROM object_head_cache WHERE tenant_id=$1 AND bucket=$2 AND object_key=$3`, f.tenantID, bucket, key).Scan(&n))
	b, err := os.ReadFile(filepath.Join(f.tempDir, f.tenantID+"_"+bucket, key)) // #nosec G304 -- test fixture path
	var locks int
	require.NoError(f.t, f.db.QueryRow(`SELECT COUNT(*) FROM object_locks WHERE tenant_id=$1 AND bucket=$2 AND object_key=$3 AND retain_until_date > NOW() + INTERVAL '29 days'`, f.tenantID, bucket, key).Scan(&locks))
	return n == 1 && err == nil && string(b) == "retained bytes of "+key && locks == 1
}

func (f *bypassFixture) gone(bucket, key string) bool {
	var n int
	require.NoError(f.t, f.db.QueryRow(`SELECT COUNT(*) FROM object_head_cache WHERE tenant_id=$1 AND bucket=$2 AND object_key=$3`, f.tenantID, bucket, key).Scan(&n))
	return n == 0
}

func requireAccessDenied(t *testing.T, err error) {
	t.Helper()
	var ae smithy.APIError
	require.Error(t, err)
	require.True(t, errors.As(err, &ae), "%v", err)
	require.Equal(t, "AccessDenied", ae.ErrorCode(), "%v", err)
}

func del(c *s3.Client, bucket, key string, bypass bool) error {
	in := &s3.DeleteObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)}
	if bypass {
		in.BypassGovernanceRetention = aws.Bool(true)
	}
	_, err := c.DeleteObject(context.Background(), in)
	return err
}

func TestGovernanceBypass_ScopedKeyWithoutThePermissionIsRefused(t *testing.T) {
	// Arrange: a GOVERNANCE-retained object and a key that may delete.
	f := setupBypassFixture(t)
	f.locked(f.bucket, "ledger.pdf", "GOVERNANCE")
	c := f.client(f.key(writerPerms...))

	// Act: the header that used to be enough.
	err := del(c, f.bucket, "ledger.pdf", true)

	// Assert
	requireAccessDenied(t, err)
	assert.Contains(t, err.Error(), bypassPerm, "the refusal names the missing permission")
	assert.True(t, f.intact(f.bucket, "ledger.pdf"), "nothing changes")
}

func TestGovernanceBypass_KeyWithThePermissionDeletes(t *testing.T) {
	// Arrange
	f := setupBypassFixture(t)
	f.locked(f.bucket, "ledger.pdf", "GOVERNANCE")
	c := f.client(f.key(append([]string{bypassPerm}, writerPerms...)...))

	// Act + Assert: the permission alone is not a bypass — the header asks.
	requireAccessDenied(t, del(c, f.bucket, "ledger.pdf", false))
	require.True(t, f.intact(f.bucket, "ledger.pdf"))
	require.NoError(t, del(c, f.bucket, "ledger.pdf", true))
	assert.True(t, f.gone(f.bucket, "ledger.pdf"))
}

func TestGovernanceBypass_PrimaryKeyIsFullAccess(t *testing.T) {
	f := setupBypassFixture(t)
	f.locked(f.bucket, "ledger.pdf", "GOVERNANCE")

	requireAccessDenied(t, del(f.root(), f.bucket, "ledger.pdf", false))
	require.NoError(t, del(f.root(), f.bucket, "ledger.pdf", true))
	assert.True(t, f.gone(f.bucket, "ledger.pdf"))
}

func TestGovernanceBypass_EveryCallerHonoursThePermission(t *testing.T) {
	f := setupBypassFixture(t)
	ctx := context.Background()
	without := f.client(f.key(writerPerms...))
	with := f.client(f.key(append([]string{bypassPerm}, writerPerms...)...))

	t.Run("batch delete", func(t *testing.T) {
		f.locked(f.bucket, "batch.bin", "GOVERNANCE")
		in := func() *s3.DeleteObjectsInput {
			return &s3.DeleteObjectsInput{Bucket: aws.String(f.bucket), BypassGovernanceRetention: aws.Bool(true),
				Delete: &s3types.Delete{Objects: []s3types.ObjectIdentifier{{Key: aws.String("batch.bin")}}}}
		}
		out, err := without.DeleteObjects(ctx, in())
		require.NoError(t, err)
		require.Len(t, out.Errors, 1)
		assert.Equal(t, "AccessDenied", aws.ToString(out.Errors[0].Code))
		assert.True(t, f.intact(f.bucket, "batch.bin"))

		out, err = with.DeleteObjects(ctx, in())
		require.NoError(t, err)
		assert.Empty(t, out.Errors)
		assert.True(t, f.gone(f.bucket, "batch.bin"))
	})

	t.Run("delete marker on a versioned bucket", func(t *testing.T) {
		f.locked(f.vbucket, "marker.bin", "GOVERNANCE")
		requireAccessDenied(t, del(without, f.vbucket, "marker.bin", true))
		assert.True(t, f.intact(f.vbucket, "marker.bin"))
		require.NoError(t, del(with, f.vbucket, "marker.bin", true))
		assert.True(t, f.gone(f.vbucket, "marker.bin"))
	})

	t.Run("delete ?versionId", func(t *testing.T) {
		f.locked(f.vbucket, "version.bin", "GOVERNANCE")
		var vid string
		require.NoError(t, f.db.QueryRow(`SELECT version_id FROM object_versions WHERE tenant_id=$1 AND bucket=$2 AND object_key='version.bin' AND is_latest`, f.tenantID, f.vbucket).Scan(&vid))
		in := func() *s3.DeleteObjectInput {
			return &s3.DeleteObjectInput{Bucket: aws.String(f.vbucket), Key: aws.String("version.bin"),
				VersionId: aws.String(vid), BypassGovernanceRetention: aws.Bool(true)}
		}
		_, err := without.DeleteObject(ctx, in())
		requireAccessDenied(t, err)
		var n int
		require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM object_versions WHERE tenant_id=$1 AND bucket=$2 AND version_id=$3`, f.tenantID, f.vbucket, vid).Scan(&n))
		assert.Equal(t, 1, n, "the version row is still there")
		_, err = with.DeleteObject(ctx, in())
		require.NoError(t, err)
	})

	t.Run("PutObjectRetention shortening", func(t *testing.T) {
		f.locked(f.bucket, "shorten.bin", "GOVERNANCE")
		in := func() *s3.PutObjectRetentionInput {
			return &s3.PutObjectRetentionInput{Bucket: aws.String(f.bucket), Key: aws.String("shorten.bin"),
				BypassGovernanceRetention: aws.Bool(true),
				Retention: &s3types.ObjectLockRetention{Mode: s3types.ObjectLockRetentionModeGovernance,
					RetainUntilDate: aws.Time(time.Now().Add(time.Hour))}}
		}
		_, err := without.PutObjectRetention(ctx, in())
		requireAccessDenied(t, err)
		assert.True(t, f.intact(f.bucket, "shorten.bin"), "the retention date did not move")
		_, err = with.PutObjectRetention(ctx, in())
		require.NoError(t, err)
		var until time.Time
		require.NoError(t, f.db.QueryRow(`SELECT retain_until_date FROM object_locks WHERE tenant_id=$1 AND bucket=$2 AND object_key='shorten.bin'`, f.tenantID, f.bucket).Scan(&until))
		assert.WithinDuration(t, time.Now().Add(time.Hour), until, time.Minute)
	})

	// The SDK has no bypass field on the three writers; the header is set by
	// a build middleware so it is part of what the signer signs.
	bypassHeader := func(o *s3.Options) {
		o.APIOptions = append(o.APIOptions, smithyAddHeader("x-amz-bypass-governance-retention", "true"))
	}

	t.Run("overwrite by PUT", func(t *testing.T) {
		f.locked(f.bucket, "put.bin", "GOVERNANCE")
		in := func() *s3.PutObjectInput {
			return &s3.PutObjectInput{Bucket: aws.String(f.bucket), Key: aws.String("put.bin"), Body: strings.NewReader("replaced")}
		}
		_, err := without.PutObject(ctx, in(), bypassHeader)
		requireAccessDenied(t, err)
		assert.True(t, f.intact(f.bucket, "put.bin"))
		_, err = with.PutObject(ctx, in(), bypassHeader)
		require.NoError(t, err)
	})

	t.Run("overwrite by CopyObject", func(t *testing.T) {
		f.locked(f.bucket, "copy.bin", "GOVERNANCE")
		_, err := f.root().PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(f.bucket), Key: aws.String("copy-src.bin"), Body: strings.NewReader("source")})
		require.NoError(t, err)
		in := func() *s3.CopyObjectInput {
			return &s3.CopyObjectInput{Bucket: aws.String(f.bucket), Key: aws.String("copy.bin"), CopySource: aws.String(f.bucket + "/copy-src.bin")}
		}
		_, err = without.CopyObject(ctx, in(), bypassHeader)
		requireAccessDenied(t, err)
		assert.True(t, f.intact(f.bucket, "copy.bin"))
		_, err = with.CopyObject(ctx, in(), bypassHeader)
		require.NoError(t, err)
	})

	t.Run("overwrite by CompleteMultipartUpload", func(t *testing.T) {
		f.locked(f.bucket, "mp.bin", "GOVERNANCE")
		complete := func(c *s3.Client) error {
			up, err := c.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: aws.String(f.bucket), Key: aws.String("mp.bin")})
			require.NoError(t, err)
			t.Cleanup(func() { _ = os.RemoveAll(multipartDir(aws.ToString(up.UploadId))) })
			part, err := c.UploadPart(ctx, &s3.UploadPartInput{Bucket: aws.String(f.bucket), Key: aws.String("mp.bin"),
				UploadId: up.UploadId, PartNumber: aws.Int32(1), Body: strings.NewReader("multipart replacement")})
			require.NoError(t, err)
			_, err = c.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{Bucket: aws.String(f.bucket), Key: aws.String("mp.bin"),
				UploadId:        up.UploadId,
				MultipartUpload: &s3types.CompletedMultipartUpload{Parts: []s3types.CompletedPart{{PartNumber: aws.Int32(1), ETag: part.ETag}}}}, bypassHeader)
			return err
		}
		requireAccessDenied(t, complete(without))
		assert.True(t, f.intact(f.bucket, "mp.bin"))
		require.NoError(t, complete(with))
	})
}

func TestGovernanceBypass_STSTokensOnlyNarrow(t *testing.T) {
	f := setupBypassFixture(t)
	ctx := context.Background()
	mint := func(parent []string, requested ...string) *s3.Client {
		tok, err := auth.GenerateSTSToken(ctx, f.db, f.tenantID, "parent", &auth.KeyScope{Permissions: parent}, auth.STSRequest{Permissions: requested})
		require.NoError(t, err)
		return f.client(tok.AccessKey, tok.SecretKey)
	}
	withPerm := append([]string{bypassPerm}, writerPerms...)

	for _, tc := range []struct {
		name      string
		parent    []string
		requested []string
		bypasses  bool
	}{
		{"a parent without the permission cannot hand it out", writerPerms, []string{"DeleteObject", bypassPerm}, false},
		{"a parent with it: the token that asks keeps it", withPerm, []string{"DeleteObject", bypassPerm}, true},
		{"a parent with it: the token that does not ask loses it", withPerm, []string{"DeleteObject"}, false},
		{"a parent with it: an unscoped token loses it", withPerm, nil, false},
		{"the account's own authority: an unscoped token does not bypass", []string{"*"}, nil, false},
		{"the account's own authority: a token asking for * does not bypass", []string{"*"}, []string{"*"}, false},
		{"the account's own authority: the token that asks keeps it", []string{"*"}, []string{"DeleteObject", bypassPerm}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			key := "sts-" + uuid.New().String()[:8]
			f.locked(f.bucket, key, "GOVERNANCE")
			err := del(mint(tc.parent, tc.requested...), f.bucket, key, true)
			if tc.bypasses {
				require.NoError(t, err)
				assert.True(t, f.gone(f.bucket, key))
				return
			}
			requireAccessDenied(t, err)
			assert.True(t, f.intact(f.bucket, key))
		})
	}
}

func TestGovernanceBypass_PresignedURLCarriesTheKeysScope(t *testing.T) {
	f := setupBypassFixture(t)
	ctx := context.Background()
	presign := func(c *s3.Client, key string, bypass bool) (*http.Request, error) {
		in := &s3.DeleteObjectInput{Bucket: aws.String(f.bucket), Key: aws.String(key)}
		if bypass {
			in.BypassGovernanceRetention = aws.Bool(true)
		}
		p, err := s3.NewPresignClient(c).PresignDeleteObject(ctx, in, s3.WithPresignExpires(5*time.Minute))
		if err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, p.Method, p.URL, nil)
		if err != nil {
			return nil, err
		}
		for k, v := range p.SignedHeader {
			if !strings.EqualFold(k, "host") {
				req.Header[k] = v
			}
		}
		return req, nil
	}
	do := func(req *http.Request) (int, string) {
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}

	t.Run("signed by a key without the permission", func(t *testing.T) {
		f.locked(f.bucket, "p1.bin", "GOVERNANCE")
		req, err := presign(f.client(f.key(writerPerms...)), "p1.bin", true)
		require.NoError(t, err)
		code, body := do(req)
		assert.Equal(t, http.StatusForbidden, code, body)
		assert.True(t, f.intact(f.bucket, "p1.bin"))
	})

	t.Run("signed by a key with the permission", func(t *testing.T) {
		f.locked(f.bucket, "p2.bin", "GOVERNANCE")
		req, err := presign(f.client(f.key(append([]string{bypassPerm}, writerPerms...)...)), "p2.bin", true)
		require.NoError(t, err)
		code, body := do(req)
		assert.Equal(t, http.StatusNoContent, code, body)
		assert.True(t, f.gone(f.bucket, "p2.bin"))
	})

	t.Run("the holder of a URL cannot add the bypass the signer did not sign", func(t *testing.T) {
		// A full-access key signs a plain DELETE URL and hands it out. The
		// signature covers `host` only, so the header rides along unsigned —
		// and the signature still verifies.
		f.locked(f.bucket, "p3.bin", "GOVERNANCE")
		req, err := presign(f.root(), "p3.bin", false)
		require.NoError(t, err)
		req.Header.Set("x-amz-bypass-governance-retention", "true")
		code, body := do(req)
		assert.Equal(t, http.StatusForbidden, code, body)
		assert.True(t, f.intact(f.bucket, "p3.bin"), "the bypass the signer never asked for was honoured")
	})
}

func TestGovernanceBypass_PresignedURLHolderCannotAddTheFlagToTheQuery(t *testing.T) {
	// The query form of the flag is honoured because the signature covers
	// every query parameter — so adding it must break the signature.
	f := setupBypassFixture(t)
	f.locked(f.bucket, "q.bin", "GOVERNANCE")
	p, err := s3.NewPresignClient(f.root()).PresignDeleteObject(context.Background(),
		&s3.DeleteObjectInput{Bucket: aws.String(f.bucket), Key: aws.String("q.bin")}, s3.WithPresignExpires(5*time.Minute))
	require.NoError(t, err)
	req, err := http.NewRequestWithContext(context.Background(), http.MethodDelete, p.URL+"&x-amz-bypass-governance-retention=true", nil)
	require.NoError(t, err)

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	assert.Contains(t, string(body), "SignatureDoesNotMatch")
	assert.True(t, f.intact(f.bucket, "q.bin"))
}

func TestGovernanceBypass_TheHeaderOnAnUnlockedObjectChangesNothing(t *testing.T) {
	// A client that always sends the header (the AWS console does) with a
	// key that may not bypass still deletes what is not retained.
	f := setupBypassFixture(t)
	_, err := f.root().PutObject(context.Background(), &s3.PutObjectInput{Bucket: aws.String(f.bucket), Key: aws.String("plain.bin"), Body: strings.NewReader("x")})
	require.NoError(t, err)

	err = del(f.client(f.key(writerPerms...)), f.bucket, "plain.bin", true)

	require.NoError(t, err)
	assert.True(t, f.gone(f.bucket, "plain.bin"))
}

func TestGovernanceBypass_UnsignedHeaderOnASignedRequestIsIgnored(t *testing.T) {
	// Arrange: a valid header-signed DELETE by a full-access key that does
	// NOT ask for the bypass; something between the client and us adds the
	// header afterwards (SignedHeaders does not list it, the signature holds).
	f := setupBypassFixture(t)
	f.locked(f.bucket, "mitm.bin", "GOVERNANCE")
	addAfterSigning := func(o *s3.Options) {
		o.HTTPClient = roundTripFunc(func(r *http.Request) (*http.Response, error) {
			r.Header.Set("x-amz-bypass-governance-retention", "true")
			return http.DefaultTransport.RoundTrip(r)
		})
	}

	// Act
	_, err := f.root().DeleteObject(context.Background(), &s3.DeleteObjectInput{Bucket: aws.String(f.bucket), Key: aws.String("mitm.bin")}, addAfterSigning)

	// Assert
	requireAccessDenied(t, err)
	assert.True(t, f.intact(f.bucket, "mitm.bin"))
}

func TestComplianceRetention_NothingBypassesIt(t *testing.T) {
	// Arrange: the most privileged caller there is — the primary key, with
	// the bypass header, signed.
	f := setupBypassFixture(t)
	ctx := context.Background()
	c := f.root()
	bypassHeader := func(o *s3.Options) {
		o.APIOptions = append(o.APIOptions, smithyAddHeader("x-amz-bypass-governance-retention", "true"))
	}
	f.locked(f.bucket, "worm.bin", "COMPLIANCE")
	f.locked(f.vbucket, "worm.bin", "COMPLIANCE")

	// Act + Assert: every flavour is refused and nothing moves.
	requireAccessDenied(t, del(c, f.bucket, "worm.bin", true))
	requireAccessDenied(t, del(c, f.vbucket, "worm.bin", true))
	out, err := c.DeleteObjects(ctx, &s3.DeleteObjectsInput{Bucket: aws.String(f.bucket), BypassGovernanceRetention: aws.Bool(true),
		Delete: &s3types.Delete{Objects: []s3types.ObjectIdentifier{{Key: aws.String("worm.bin")}}}})
	require.NoError(t, err)
	require.Len(t, out.Errors, 1)
	_, err = c.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(f.bucket), Key: aws.String("worm.bin"), Body: strings.NewReader("x")}, bypassHeader)
	requireAccessDenied(t, err)
	_, err = c.PutObjectRetention(ctx, &s3.PutObjectRetentionInput{Bucket: aws.String(f.bucket), Key: aws.String("worm.bin"),
		BypassGovernanceRetention: aws.Bool(true),
		Retention:                 &s3types.ObjectLockRetention{Mode: s3types.ObjectLockRetentionModeCompliance, RetainUntilDate: aws.Time(time.Now().Add(time.Hour))}})
	requireAccessDenied(t, err)
	_, err = c.PutObjectRetention(ctx, &s3.PutObjectRetentionInput{Bucket: aws.String(f.bucket), Key: aws.String("worm.bin"),
		BypassGovernanceRetention: aws.Bool(true),
		Retention:                 &s3types.ObjectLockRetention{Mode: s3types.ObjectLockRetentionModeGovernance, RetainUntilDate: aws.Time(time.Now().Add(60 * 24 * time.Hour))}})
	requireAccessDenied(t, err)
	assert.True(t, f.intact(f.bucket, "worm.bin"))
	assert.True(t, f.intact(f.vbucket, "worm.bin"))
	var mode string
	require.NoError(t, f.db.QueryRow(`SELECT retention_mode FROM object_locks WHERE tenant_id=$1 AND bucket=$2 AND object_key='worm.bin'`, f.tenantID, f.bucket).Scan(&mode))
	assert.Equal(t, "COMPLIANCE", mode)
}

func TestIsObjectLockBypass_NoScopeInTheContextIsNoBypass(t *testing.T) {
	// A handler reached without the authenticator having put a scope in the
	// context (a new entry point, a test that calls a handler directly) must
	// fail closed: the header alone is never a bypass again.
	r := httptest.NewRequest("DELETE", "/b/k", nil)
	r.Header.Set("x-amz-bypass-governance-retention", "true")
	assert.False(t, isObjectLockBypass(r))

	full := r.WithContext(auth.WithKeyScope(r.Context(), &auth.KeyScope{Permissions: []string{"*"}}))
	assert.True(t, isObjectLockBypass(full))

	scoped := r.WithContext(auth.WithKeyScope(r.Context(), &auth.KeyScope{Permissions: []string{"DeleteObject"}}))
	assert.False(t, isObjectLockBypass(scoped))

	noHeader := httptest.NewRequest("DELETE", "/b/k", nil)
	noHeader = noHeader.WithContext(auth.WithKeyScope(noHeader.Context(), &auth.KeyScope{Permissions: []string{"*"}}))
	assert.False(t, isObjectLockBypass(noHeader), "the permission without the header asks for nothing")
}

// The two JSON APIs that create a key validate against the one permission
// table and store what was asked (the dashboard form is the third place:
// internal/dashboard, TestAPIKeysPage_OffersTheGovernanceBypassPermission).
func TestKeyAPIs_CreateAKeyWithTheBypassPermission(t *testing.T) {
	stored := func(t *testing.T, svc *auth.AuthService, userID, id string) []string {
		t.Helper()
		keys, err := svc.ListAPIKeys(context.Background(), userID)
		require.NoError(t, err)
		for _, k := range keys {
			if k.ID == id {
				return k.Permissions
			}
		}
		t.Fatal("created key not found")
		return nil
	}
	want := []string{"DeleteObject", bypassPerm}
	body := `{"name":"retention-admin","permissions":["DeleteObject","` + bypassPerm + `"]}`

	t.Run("user API", func(t *testing.T) {
		svc := auth.NewAuthService(nil, nil)
		user, _, _, err := svc.CreateUserWithTenant(context.Background(), "bypass-user@stored.ge", "Str0ngPassw0rd!", "x")
		require.NoError(t, err)
		s := &Server{logger: zap.NewNop(), auth: svc, config: &config.Config{Server: config.ServerConfig{Port: 8000}}}
		req := httptest.NewRequest(http.MethodPost, "/api/v1/user/apikeys", strings.NewReader(body))
		req = req.WithContext(context.WithValue(req.Context(), userIDKey, user.ID))
		w := httptest.NewRecorder()

		s.handleCreateUserAPIKey(w, req)

		require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
		var resp struct {
			ID string `json:"id"`
		}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
		assert.Equal(t, want, stored(t, svc, user.ID, resp.ID))
	})

	t.Run("management API", func(t *testing.T) {
		svc := auth.NewAuthService(nil, nil)
		user, _, _, err := svc.CreateUserWithTenant(context.Background(), "bypass-mgmt@stored.ge", "Str0ngPassw0rd!", "x")
		require.NoError(t, err)
		s := &Server{logger: zap.NewNop(), auth: svc, config: &config.Config{}, quotaManager: &stubQuotaManager{}}
		req := httptest.NewRequest(http.MethodPost, "/api/v1/manage/keys", strings.NewReader(body))
		ctx := context.WithValue(req.Context(), userIDKey, user.ID)
		req = req.WithContext(context.WithValue(ctx, tenantIDKey, "test-tenant"))
		w := httptest.NewRecorder()

		s.handleMgmtCreateKey(w, req)

		require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
		var resp struct {
			ID string `json:"id"`
		}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
		assert.Equal(t, want, stored(t, svc, user.ID, resp.ID))

		// Rotation keeps the scope: the replacement is not a wider key.
		rotated, err := svc.RotateAPIKey(context.Background(), user.ID, resp.ID)
		require.NoError(t, err)
		assert.Equal(t, want, rotated.Permissions)
	})
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) Do(r *http.Request) (*http.Response, error) { return f(r) }

// smithyAddHeader sets a request header in the SDK's build step — before
// the signer runs, so the header is part of SignedHeaders.
func smithyAddHeader(name, value string) func(*middleware.Stack) error {
	return smithyhttp.AddHeaderValue(name, value)
}
