package api

import (
	"database/sql"
	"encoding/xml"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/FairForge/vaultaire/internal/tenant"
	"github.com/FairForge/vaultaire/internal/usage"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	_ "github.com/lib/pq"
)

// TestCreateBucket_IdempotentReCreate_AtFreeTierLimit is the regression test for
// the load-test finding: a free-tier tenant AT its 1-bucket limit must still be
// able to re-create a bucket it already owns (S3 BucketAlreadyOwnedByYou), while
// a genuinely new bucket beyond the limit is still rejected.
func TestCreateBucket_IdempotentReCreate_AtFreeTierLimit(t *testing.T) {
	if testing.Short() {
		t.Skip("requires database")
	}
	db := testS3DB(t)
	defer func() { _ = db.Close() }()
	cleanupS3BucketData(t, db)
	defer cleanupS3BucketData(t, db)

	const tid = "test-s3-idem"
	_, err := db.Exec(`INSERT INTO tenants (id, name, email, access_key, secret_key) VALUES ($1,'Idem Co','idem@test.com','VK-idem','SK-idem') ON CONFLICT DO NOTHING`, tid)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO tenant_quotas (tenant_id, tier, storage_limit_bytes) VALUES ($1,'free',5368709120) ON CONFLICT (tenant_id) DO UPDATE SET tier='free'`, tid)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO buckets (tenant_id, name, visibility) VALUES ($1,'owned-bucket','private') ON CONFLICT DO NOTHING`, tid)
	require.NoError(t, err)

	s := s3ServerWithDB(t, db)
	s.quotaManager = usage.NewQuotaManager(db)
	defer func() { _ = os.RemoveAll("/tmp/vaultaire/" + tid) }()

	// Re-creating a bucket you already own must succeed even at the free-tier limit.
	w := httptest.NewRecorder()
	s.CreateBucket(w, withTenantCtx(httptest.NewRequest("PUT", "/owned-bucket", nil), tid))
	assert.Equal(t, http.StatusOK, w.Code, "re-creating an owned bucket at the limit must succeed, not 403")

	// A genuinely new bucket beyond the limit must still be rejected.
	w2 := httptest.NewRecorder()
	s.CreateBucket(w2, withTenantCtx(httptest.NewRequest("PUT", "/second-bucket", nil), tid))
	assert.Equal(t, http.StatusForbidden, w2.Code, "a new bucket beyond the free-tier limit must still 403")
}

func testS3DB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://viera@localhost:5432/vaultaire?sslmode=disable"
	}
	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	require.NoError(t, db.Ping())
	return db
}

func cleanupS3BucketData(t *testing.T, db *sql.DB) {
	t.Helper()
	_, _ = db.Exec(`DELETE FROM buckets WHERE tenant_id LIKE 'test-s3-%'`)
	_, _ = db.Exec(`DELETE FROM object_head_cache WHERE tenant_id LIKE 'test-s3-%'`)
	_, _ = db.Exec(`DELETE FROM tenant_quotas WHERE tenant_id LIKE 'test-s3-%'`)
	_, _ = db.Exec(`DELETE FROM tenants WHERE id LIKE 'test-s3-%'`)
}

func s3ServerWithDB(t *testing.T, db *sql.DB) *Server {
	t.Helper()
	return &Server{
		logger: zap.NewNop(),
		db:     db,
	}
}

func withTenantCtx(r *http.Request, tenantID string) *http.Request {
	t := &tenant.Tenant{ID: tenantID}
	ctx := tenant.WithTenant(r.Context(), t)
	return r.WithContext(ctx)
}

func TestCreateBucket_PersistsToBucketsTable(t *testing.T) {
	if testing.Short() {
		t.Skip("requires database")
	}
	db := testS3DB(t)
	defer func() { _ = db.Close() }()
	cleanupS3BucketData(t, db)
	defer cleanupS3BucketData(t, db)

	_, err := db.Exec(`INSERT INTO tenants (id, name, email, access_key, secret_key) VALUES ('test-s3-1', 'Test Co', 'tests3-1@test.com', 'VK-s31', 'SK-s31') ON CONFLICT DO NOTHING`)
	require.NoError(t, err)

	s := s3ServerWithDB(t, db)
	defer func() { _ = os.RemoveAll("/tmp/vaultaire/test-s3-1") }()

	req := httptest.NewRequest("PUT", "/my-test-bucket", nil)
	req = withTenantCtx(req, "test-s3-1")
	w := httptest.NewRecorder()

	s.CreateBucket(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	var vis string
	err = db.QueryRow(`SELECT visibility FROM buckets WHERE tenant_id = 'test-s3-1' AND name = 'my-test-bucket'`).Scan(&vis)
	require.NoError(t, err)
	assert.Equal(t, "private", vis)
}

func TestCreateBucket_DuplicateBucket_Succeeds(t *testing.T) {
	if testing.Short() {
		t.Skip("requires database")
	}
	db := testS3DB(t)
	defer func() { _ = db.Close() }()
	cleanupS3BucketData(t, db)
	defer cleanupS3BucketData(t, db)

	_, err := db.Exec(`INSERT INTO tenants (id, name, email, access_key, secret_key) VALUES ('test-s3-2', 'Test Co 2', 'tests3-2@test.com', 'VK-s32', 'SK-s32') ON CONFLICT DO NOTHING`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO buckets (tenant_id, name, visibility) VALUES ('test-s3-2', 'existing-bucket', 'private') ON CONFLICT DO NOTHING`)
	require.NoError(t, err)

	s := s3ServerWithDB(t, db)
	defer func() { _ = os.RemoveAll("/tmp/vaultaire/test-s3-2") }()

	req := httptest.NewRequest("PUT", "/existing-bucket", nil)
	req = withTenantCtx(req, "test-s3-2")
	w := httptest.NewRecorder()

	s.CreateBucket(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
}

func TestCreateBucket_NoDB_StillWorks(t *testing.T) {
	s := &Server{logger: zap.NewNop(), db: nil}
	defer func() { _ = os.RemoveAll("/tmp/vaultaire/default/no-db-bucket") }()

	req := httptest.NewRequest("PUT", "/no-db-bucket", nil)
	w := httptest.NewRecorder()

	s.CreateBucket(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
}

func TestListBuckets_ReadsFromDB(t *testing.T) {
	if testing.Short() {
		t.Skip("requires database")
	}
	db := testS3DB(t)
	defer func() { _ = db.Close() }()
	cleanupS3BucketData(t, db)
	defer cleanupS3BucketData(t, db)

	_, err := db.Exec(`INSERT INTO tenants (id, name, email, access_key, secret_key) VALUES ('test-s3-3', 'Test Co 3', 'tests3-3@test.com', 'VK-s33', 'SK-s33') ON CONFLICT DO NOTHING`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO buckets (tenant_id, name) VALUES ('test-s3-3', 'alpha-bucket'), ('test-s3-3', 'beta-bucket') ON CONFLICT DO NOTHING`)
	require.NoError(t, err)

	s := s3ServerWithDB(t, db)

	req := httptest.NewRequest("GET", "/", nil)
	req = withTenantCtx(req, "test-s3-3")
	w := httptest.NewRecorder()

	s.ListBuckets(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Header().Get("Content-Type"), "application/xml")

	var resp ListBucketsResponse
	err = xml.Unmarshal(w.Body.Bytes(), &resp)
	require.NoError(t, err)
	assert.Len(t, resp.Buckets.Bucket, 2)
	assert.Equal(t, "alpha-bucket", resp.Buckets.Bucket[0].Name)
	assert.Equal(t, "beta-bucket", resp.Buckets.Bucket[1].Name)
}

func TestCreateBucket_InvalidName_Rejected(t *testing.T) {
	s := &Server{logger: zap.NewNop(), db: nil}

	invalidNames := []string{
		"../etc",
		"..",
		"../../tmp/pwned",
		"UPPERCASE",
		"a",
		"has%20spaces",
		strings.Repeat("a", 64),
		"-leading-dash",
		"trailing-dash-",
	}

	for _, name := range invalidNames {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest("PUT", "/"+name, nil)
			w := httptest.NewRecorder()
			s.CreateBucket(w, req)
			assert.Equal(t, http.StatusBadRequest, w.Code, "bucket name %q should be rejected", name)
			assert.Contains(t, w.Body.String(), "InvalidBucketName")
		})
	}
}

func TestDeleteBucket_InvalidName_Rejected(t *testing.T) {
	s := &Server{logger: zap.NewNop(), db: nil}

	invalidNames := []string{
		"../etc",
		"..",
		"../../tmp/pwned",
		"UPPERCASE",
	}

	for _, name := range invalidNames {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest("DELETE", "/"+name, nil)
			w := httptest.NewRecorder()
			s.DeleteBucket(w, req)
			assert.Equal(t, http.StatusBadRequest, w.Code, "bucket name %q should be rejected", name)
			assert.Contains(t, w.Body.String(), "InvalidBucketName")
		})
	}
}

func TestCreateBucket_ValidNames(t *testing.T) {
	s := &Server{logger: zap.NewNop(), db: nil}

	validNames := []string{
		"my-bucket",
		"data.2026",
		"abc",
		"a-b",
		"my.long.bucket.name.with.dots",
	}

	for _, name := range validNames {
		t.Run(name, func(t *testing.T) {
			defer func() { _ = os.RemoveAll("/tmp/vaultaire/default/" + name) }()
			req := httptest.NewRequest("PUT", "/"+name, nil)
			w := httptest.NewRecorder()
			s.CreateBucket(w, req)
			assert.Equal(t, http.StatusOK, w.Code, "bucket name %q should be accepted", name)
		})
	}
}

func TestCreateBucket_WithRegionHeader(t *testing.T) {
	if testing.Short() {
		t.Skip("requires database")
	}
	db := testS3DB(t)
	defer func() { _ = db.Close() }()
	cleanupS3BucketData(t, db)
	defer cleanupS3BucketData(t, db)

	_, err := db.Exec(`INSERT INTO tenants (id, name, email, access_key, secret_key) VALUES ('test-s3-r1', 'Region Co', 'r1@test.com', 'VK-r1', 'SK-r1') ON CONFLICT DO NOTHING`)
	require.NoError(t, err)

	s := s3ServerWithDB(t, db)
	defer func() { _ = os.RemoveAll("/tmp/vaultaire/test-s3-r1") }()

	req := httptest.NewRequest("PUT", "/eu-region-bucket", nil)
	req.Header.Set("X-Stored-Region", "eu-west-1")
	req = withTenantCtx(req, "test-s3-r1")
	w := httptest.NewRecorder()

	s.CreateBucket(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "eu-west-1", w.Header().Get("x-amz-bucket-region"))

	var region string
	err = db.QueryRow(`SELECT region FROM buckets WHERE tenant_id = 'test-s3-r1' AND name = 'eu-region-bucket'`).Scan(&region)
	require.NoError(t, err)
	assert.Equal(t, "eu-west-1", region)
}

func TestCreateBucket_InvalidRegion(t *testing.T) {
	s := &Server{logger: zap.NewNop(), db: nil}
	defer func() { _ = os.RemoveAll("/tmp/vaultaire/default/bad-region-bucket") }()

	req := httptest.NewRequest("PUT", "/bad-region-bucket", nil)
	req.Header.Set("X-Stored-Region", "ap-southeast-1")
	w := httptest.NewRecorder()

	s.CreateBucket(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "InvalidLocationConstraint")
}

func TestCreateBucket_DefaultRegion(t *testing.T) {
	if testing.Short() {
		t.Skip("requires database")
	}
	db := testS3DB(t)
	defer func() { _ = db.Close() }()
	cleanupS3BucketData(t, db)
	defer cleanupS3BucketData(t, db)

	_, err := db.Exec(`INSERT INTO tenants (id, name, email, access_key, secret_key) VALUES ('test-s3-r2', 'Default Co', 'r2@test.com', 'VK-r2', 'SK-r2') ON CONFLICT DO NOTHING`)
	require.NoError(t, err)

	s := s3ServerWithDB(t, db)
	defer func() { _ = os.RemoveAll("/tmp/vaultaire/test-s3-r2") }()

	req := httptest.NewRequest("PUT", "/default-region-bucket", nil)
	req = withTenantCtx(req, "test-s3-r2")
	w := httptest.NewRecorder()

	s.CreateBucket(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	var region string
	err = db.QueryRow(`SELECT region FROM buckets WHERE tenant_id = 'test-s3-r2' AND name = 'default-region-bucket'`).Scan(&region)
	require.NoError(t, err)
	assert.Equal(t, "us-west-1", region)
}

func TestCreateBucket_XMLLocationConstraint(t *testing.T) {
	if testing.Short() {
		t.Skip("requires database")
	}
	db := testS3DB(t)
	defer func() { _ = db.Close() }()
	cleanupS3BucketData(t, db)
	defer cleanupS3BucketData(t, db)

	_, err := db.Exec(`INSERT INTO tenants (id, name, email, access_key, secret_key) VALUES ('test-s3-r3', 'XML Co', 'r3@test.com', 'VK-r3', 'SK-r3') ON CONFLICT DO NOTHING`)
	require.NoError(t, err)

	s := s3ServerWithDB(t, db)
	defer func() { _ = os.RemoveAll("/tmp/vaultaire/test-s3-r3") }()

	body := `<CreateBucketConfiguration><LocationConstraint>eu-central-2</LocationConstraint></CreateBucketConfiguration>`
	req := httptest.NewRequest("PUT", "/xml-region-bucket", strings.NewReader(body))
	req.Header.Set("Content-Length", "100")
	req = withTenantCtx(req, "test-s3-r3")
	w := httptest.NewRecorder()

	s.CreateBucket(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	var region string
	err = db.QueryRow(`SELECT region FROM buckets WHERE tenant_id = 'test-s3-r3' AND name = 'xml-region-bucket'`).Scan(&region)
	require.NoError(t, err)
	assert.Equal(t, "eu-central-2", region)
}

func TestGetBucketLocation(t *testing.T) {
	if testing.Short() {
		t.Skip("requires database")
	}
	db := testS3DB(t)
	defer func() { _ = db.Close() }()
	cleanupS3BucketData(t, db)
	defer cleanupS3BucketData(t, db)

	_, err := db.Exec(`INSERT INTO tenants (id, name, email, access_key, secret_key) VALUES ('test-s3-r4', 'Loc Co', 'r4@test.com', 'VK-r4', 'SK-r4') ON CONFLICT DO NOTHING`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO buckets (tenant_id, name, region) VALUES ('test-s3-r4', 'located-bucket', 'eu-south-1') ON CONFLICT DO NOTHING`)
	require.NoError(t, err)

	s := s3ServerWithDB(t, db)

	req := httptest.NewRequest("GET", "/located-bucket?location", nil)
	req = withTenantCtx(req, "test-s3-r4")
	w := httptest.NewRecorder()

	s3Req := &S3Request{Bucket: "located-bucket", Operation: "GetBucketLocation"}
	s.handleGetBucketLocation(w, req, s3Req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Header().Get("Content-Type"), "application/xml")
	assert.Equal(t, "eu-south-1", w.Header().Get("x-amz-bucket-region"))

	var resp LocationConstraintResponse
	err = xml.Unmarshal(w.Body.Bytes(), &resp)
	require.NoError(t, err)
	assert.Equal(t, "eu-south-1", resp.Location)
}

func TestListBuckets_NoDB_FallsBackToFilesystem(t *testing.T) {
	s := &Server{logger: zap.NewNop(), db: nil}

	tenantID := "test-s3-fs"
	basePath := "/tmp/vaultaire/" + tenantID
	defer func() { _ = os.RemoveAll(basePath) }()
	require.NoError(t, os.MkdirAll(basePath+"/fs-bucket-1", 0755))
	require.NoError(t, os.MkdirAll(basePath+"/fs-bucket-2", 0755))

	req := httptest.NewRequest("GET", "/", nil)
	req = withTenantCtx(req, tenantID)
	w := httptest.NewRecorder()

	s.ListBuckets(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	var resp ListBucketsResponse
	err := xml.Unmarshal(w.Body.Bytes(), &resp)
	require.NoError(t, err)
	assert.Len(t, resp.Buckets.Bucket, 2)
}
