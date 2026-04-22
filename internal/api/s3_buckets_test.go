package api

import (
	"database/sql"
	"encoding/xml"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/FairForge/vaultaire/internal/tenant"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	_ "github.com/lib/pq"
)

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
