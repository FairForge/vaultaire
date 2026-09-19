package api

// A1 conformance fixes (WP-VG1 gap report, 2026-09-18): wire-shape gaps found
// by running the versitygw integration suite against a local server. Each test
// here pins one of the confirmed divergences from AWS behavior.

import (
	"context"
	"database/sql"
	"encoding/xml"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/FairForge/vaultaire/internal/tenant"
)

// --- operation routing -------------------------------------------------

func TestDetermineOperation_ACLSubresource(t *testing.T) {
	p := NewS3Parser(zap.NewNop())

	cases := []struct {
		method, path, want string
	}{
		{"GET", "/bkt?acl", "GetBucketAcl"},
		{"PUT", "/bkt?acl", "PutBucketAcl"},
		{"GET", "/bkt/obj?acl", "GetObjectAcl"},
		{"PUT", "/bkt/obj?acl", "PutObjectAcl"},
	}
	for _, tc := range cases {
		r := httptest.NewRequest(tc.method, tc.path, nil)
		req, err := p.ParseRequest(r)
		require.NoError(t, err, "%s %s", tc.method, tc.path)
		assert.Equal(t, tc.want, req.Operation, "%s %s", tc.method, tc.path)
	}
}

// --- fixture ------------------------------------------------------------

type a1Fixture struct {
	server   *Server
	db       *sql.DB
	tenantID string
	tenant   *tenant.Tenant
	bucket   string
	object   string
}

func setupA1Fixture(t *testing.T) *a1Fixture {
	t.Helper()

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set — skipping integration test")
	}
	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	require.NoError(t, db.Ping())

	logger := zap.NewNop()
	eng := engine.NewEngine(nil, logger, nil)

	tenantID := fmt.Sprintf("a1-%d", os.Getpid())
	bucket := "a1-bucket"
	object := "a1-doc.txt"
	email := fmt.Sprintf("a1-%d@test.local", os.Getpid())

	_, err = db.Exec(`
		INSERT INTO tenants (id, name, email, access_key, secret_key)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (id) DO NOTHING
	`, tenantID, "A1 Conformance Test", email, "AK-"+tenantID, "SK-"+tenantID)
	require.NoError(t, err)

	_, err = db.Exec(`
		INSERT INTO buckets (tenant_id, name, visibility)
		VALUES ($1, $2, 'private')
		ON CONFLICT (tenant_id, name) DO NOTHING
	`, tenantID, bucket)
	require.NoError(t, err)

	_, err = db.Exec(`
		INSERT INTO object_head_cache (tenant_id, bucket, object_key, size_bytes, etag, content_type, backend_name)
		VALUES ($1, $2, $3, 5, 'abc123', 'text/plain', 'local')
		ON CONFLICT (tenant_id, bucket, object_key) DO NOTHING
	`, tenantID, bucket, object)
	require.NoError(t, err)

	t.Cleanup(func() {
		_, _ = db.Exec("DELETE FROM object_head_cache WHERE tenant_id = $1", tenantID)
		_, _ = db.Exec("DELETE FROM buckets WHERE tenant_id = $1", tenantID)
		_, _ = db.Exec("DELETE FROM tenants WHERE id = $1", tenantID)
	})

	tn := &tenant.Tenant{
		ID:        tenantID,
		Namespace: "tenant/" + tenantID + "/",
	}

	srv := &Server{
		logger:   logger,
		router:   chi.NewRouter(),
		engine:   eng,
		db:       db,
		testMode: true,
	}

	return &a1Fixture{
		server:   srv,
		db:       db,
		tenantID: tenantID,
		tenant:   tn,
		bucket:   bucket,
		object:   object,
	}
}

func (f *a1Fixture) ctx() context.Context {
	return tenant.WithTenant(context.Background(), f.tenant)
}

// --- CreateBucket Location header ----------------------------------------

func TestCreateBucket_LocationHeader(t *testing.T) {
	f := setupA1Fixture(t)

	r := httptest.NewRequest("PUT", "/a1-loc-bucket", nil).WithContext(f.ctx())
	w := httptest.NewRecorder()
	f.server.CreateBucket(w, r)
	t.Cleanup(func() {
		_, _ = f.db.Exec("DELETE FROM buckets WHERE tenant_id = $1 AND name = 'a1-loc-bucket'", f.tenantID)
	})

	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "/a1-loc-bucket", w.Header().Get("Location"))
}

// --- GetBucketAcl / GetObjectAcl -----------------------------------------

func TestGetBucketAcl_ReturnsOwnerFullControl(t *testing.T) {
	f := setupA1Fixture(t)

	s3Req := &S3Request{Bucket: f.bucket, TenantID: f.tenantID}
	r := httptest.NewRequest("GET", "/"+f.bucket+"?acl", nil).WithContext(f.ctx())
	w := httptest.NewRecorder()
	f.server.handleGetBucketAcl(w, r, s3Req)

	require.Equal(t, http.StatusOK, w.Code)
	body := w.Body.String()
	assert.Contains(t, body, "<AccessControlPolicy")
	assert.Contains(t, body, "<ID>"+f.tenantID+"</ID>")
	assert.Contains(t, body, "<Permission>FULL_CONTROL</Permission>")
	assert.Contains(t, body, `xsi:type="CanonicalUser"`)

	var policy struct {
		Owner struct {
			ID string `xml:"ID"`
		} `xml:"Owner"`
		Grants []struct {
			Permission string `xml:"Permission"`
		} `xml:"AccessControlList>Grant"`
	}
	require.NoError(t, xml.Unmarshal(w.Body.Bytes(), &policy))
	assert.Equal(t, f.tenantID, policy.Owner.ID)
	require.Len(t, policy.Grants, 1)
	assert.Equal(t, "FULL_CONTROL", policy.Grants[0].Permission)
}

func TestGetBucketAcl_MissingBucket(t *testing.T) {
	f := setupA1Fixture(t)

	s3Req := &S3Request{Bucket: "no-such-bkt", TenantID: f.tenantID}
	r := httptest.NewRequest("GET", "/no-such-bkt?acl", nil).WithContext(f.ctx())
	w := httptest.NewRecorder()
	f.server.handleGetBucketAcl(w, r, s3Req)

	assert.Equal(t, http.StatusNotFound, w.Code)
	assert.Contains(t, w.Body.String(), "NoSuchBucket")
}

func TestGetObjectAcl_ReturnsOwnerFullControl(t *testing.T) {
	f := setupA1Fixture(t)

	s3Req := &S3Request{Bucket: f.bucket, Object: f.object, TenantID: f.tenantID}
	r := httptest.NewRequest("GET", "/"+f.bucket+"/"+f.object+"?acl", nil).WithContext(f.ctx())
	w := httptest.NewRecorder()
	f.server.handleGetObjectAcl(w, r, s3Req)

	require.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "<Permission>FULL_CONTROL</Permission>")
}

func TestGetObjectAcl_MissingKey(t *testing.T) {
	f := setupA1Fixture(t)

	s3Req := &S3Request{Bucket: f.bucket, Object: "nope.txt", TenantID: f.tenantID}
	r := httptest.NewRequest("GET", "/"+f.bucket+"/nope.txt?acl", nil).WithContext(f.ctx())
	w := httptest.NewRecorder()
	f.server.handleGetObjectAcl(w, r, s3Req)

	assert.Equal(t, http.StatusNotFound, w.Code)
	assert.Contains(t, w.Body.String(), "NoSuchKey")
}

// --- PutBucketAcl / PutObjectAcl (owner-enforced model) -------------------

func TestPutBucketAcl_CannedPrivateAccepted(t *testing.T) {
	f := setupA1Fixture(t)

	for _, canned := range []string{"", "private", "bucket-owner-full-control"} {
		s3Req := &S3Request{Bucket: f.bucket, TenantID: f.tenantID}
		r := httptest.NewRequest("PUT", "/"+f.bucket+"?acl", nil).WithContext(f.ctx())
		if canned != "" {
			r.Header.Set("x-amz-acl", canned)
		}
		w := httptest.NewRecorder()
		f.server.handlePutBucketAcl(w, r, s3Req)
		assert.Equal(t, http.StatusOK, w.Code, "canned=%q", canned)
	}
}

func TestPutBucketAcl_PublicReadRejected(t *testing.T) {
	f := setupA1Fixture(t)

	s3Req := &S3Request{Bucket: f.bucket, TenantID: f.tenantID}
	r := httptest.NewRequest("PUT", "/"+f.bucket+"?acl", nil).WithContext(f.ctx())
	r.Header.Set("x-amz-acl", "public-read")
	w := httptest.NewRecorder()
	f.server.handlePutBucketAcl(w, r, s3Req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "AccessControlListNotSupported")
}

func TestPutObjectAcl_DoesNotOverwriteObject(t *testing.T) {
	// Before this fix, PUT /{bucket}/{key}?acl fell through to PutObject and
	// REPLACED the object's bytes with the ACL XML body. The routing test
	// above pins the dispatch; this pins the non-destructive behavior at the
	// handler level: the head-cache row must be untouched after a PutObjectAcl.
	f := setupA1Fixture(t)

	s3Req := &S3Request{Bucket: f.bucket, Object: f.object, TenantID: f.tenantID}
	body := strings.NewReader("<AccessControlPolicy></AccessControlPolicy>")
	r := httptest.NewRequest("PUT", "/"+f.bucket+"/"+f.object+"?acl", body).WithContext(f.ctx())
	w := httptest.NewRecorder()
	f.server.handlePutObjectAcl(w, r, s3Req)
	assert.Equal(t, http.StatusOK, w.Code)

	var size int64
	var etag string
	require.NoError(t, f.db.QueryRow(
		`SELECT size_bytes, etag FROM object_head_cache WHERE tenant_id=$1 AND bucket=$2 AND object_key=$3`,
		f.tenantID, f.bucket, f.object).Scan(&size, &etag))
	assert.Equal(t, int64(5), size)
	assert.Equal(t, "abc123", etag)
}

// --- Content-Encoding ------------------------------------------------------

func TestRequestContentEncoding_StripsAWSChunked(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		{"gzip", "gzip"},
		{"aws-chunked", ""},
		{"aws-chunked,gzip", "gzip"},
		{"aws-chunked, gzip", "gzip"},
		{"AWS-CHUNKED,br", "br"},
		{"gzip, aws-chunked", "gzip"},
	}
	for _, tc := range cases {
		r := httptest.NewRequest("PUT", "/b/k", nil)
		if tc.in != "" {
			r.Header.Set("Content-Encoding", tc.in)
		}
		assert.Equal(t, tc.want, requestContentEncoding(r), "in=%q", tc.in)
	}
}

func TestHeadObject_ReturnsContentEncoding(t *testing.T) {
	f := setupA1Fixture(t)

	_, err := f.db.Exec(`
		UPDATE object_head_cache SET content_encoding = 'gzip', content_language = 'eng',
			cache_control = 'max-age=60', http_expires = 'Sat, 19 Sep 2026 05:31:53 GMT',
			website_redirect_location = '/elsewhere'
		WHERE tenant_id=$1 AND bucket=$2 AND object_key=$3`,
		f.tenantID, f.bucket, f.object)
	require.NoError(t, err)

	s3Req := &S3Request{Bucket: f.bucket, Object: f.object, TenantID: f.tenantID}
	r := httptest.NewRequest("HEAD", "/"+f.bucket+"/"+f.object, nil).WithContext(f.ctx())
	w := httptest.NewRecorder()
	f.server.handleHeadObject(w, r, s3Req)

	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "gzip", w.Header().Get("Content-Encoding"))
	assert.Equal(t, "eng", w.Header().Get("Content-Language"))
	assert.Equal(t, "max-age=60", w.Header().Get("Cache-Control"))
	assert.Equal(t, "Sat, 19 Sep 2026 05:31:53 GMT", w.Header().Get("Expires"))
	assert.Equal(t, "/elsewhere", w.Header().Get("x-amz-website-redirect-location"))
}

func TestRequestContentLanguage_DropsControlChars(t *testing.T) {
	r := httptest.NewRequest("PUT", "/b/k", nil)
	r.Header.Set("Content-Language", "eng")
	assert.Equal(t, "eng", requestContentLanguage(r))
	r.Header.Set("Content-Language", "en\x00g")
	assert.Equal(t, "", requestContentLanguage(r))
}

// --- ListObjects storage class from backend --------------------------------

func TestListObjectsV2_StorageClassFromBackend(t *testing.T) {
	f := setupA1Fixture(t)

	_, err := f.db.Exec(`
		INSERT INTO object_head_cache (tenant_id, bucket, object_key, size_bytes, etag, content_type, backend_name)
		VALUES
			($1, $2, 'sc-glacier', 3, 'e1', 'text/plain', 'geyser'),
			($1, $2, 'sc-idrive', 3, 'e2', 'text/plain', 'idrive')
		ON CONFLICT (tenant_id, bucket, object_key) DO UPDATE SET backend_name = EXCLUDED.backend_name`,
		f.tenantID, f.bucket)
	require.NoError(t, err)

	adapter := NewS3ToEngine(f.server.engine, f.db, f.server.logger)
	r := httptest.NewRequest("GET", "/"+f.bucket+"?list-type=2", nil).WithContext(f.ctx())
	w := httptest.NewRecorder()
	adapter.HandleListV2(w, r, f.bucket)

	require.Equal(t, http.StatusOK, w.Code)
	var result ListBucketV2Result
	require.NoError(t, xml.Unmarshal(w.Body.Bytes(), &result))

	classes := map[string]string{}
	for _, e := range result.Contents {
		classes[e.Key] = e.StorageClass
	}
	assert.Equal(t, "GLACIER", classes["sc-glacier"])
	assert.Equal(t, "STANDARD", classes["sc-idrive"])
	// The fixture object rides backend 'local' → REDUCED_REDUNDANCY, matching
	// what HEAD reports for the same object (the versitygw sweep caught the
	// two paths disagreeing).
	assert.Equal(t, "REDUCED_REDUNDANCY", classes[f.object])
}
