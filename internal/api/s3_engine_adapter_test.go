package api

import (
	"bytes"
	"context"
	"crypto/md5" // #nosec G401 — S3 spec requires MD5
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/FairForge/vaultaire/internal/crypto"
	"github.com/FairForge/vaultaire/internal/drivers"
	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/FairForge/vaultaire/internal/tenant"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	_ "github.com/lib/pq"
)

type adapterTestFixture struct {
	adapter  *S3ToEngine
	db       *sql.DB
	eng      *engine.CoreEngine
	tenantID string
	tenant   *tenant.Tenant
	tempDir  string
}

func setupAdapterFixture(t *testing.T) *adapterTestFixture {
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

	tempDir, err := os.MkdirTemp("", "vaultaire-adapter-test-*")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(tempDir) })

	eng := engine.NewEngine(nil, logger, nil)
	driver := drivers.NewLocalDriver(tempDir, logger)
	eng.AddDriver("local", driver)
	eng.SetPrimary("local")

	tenantID := "adapter-test-" + t.Name()

	_, err = db.Exec(`
		INSERT INTO tenants (id, name, email, access_key, secret_key)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (id) DO NOTHING
	`, tenantID, "Adapter Test", "adapter@test.local", "AK-"+tenantID, "SK-"+tenantID)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = db.Exec("DELETE FROM object_head_cache WHERE tenant_id = $1", tenantID)
		_, _ = db.Exec("DELETE FROM tenants WHERE id = $1", tenantID)
	})

	tn := &tenant.Tenant{
		ID:        tenantID,
		Namespace: "tenant/" + tenantID + "/",
	}

	container := tn.NamespaceContainer("test-bucket")
	require.NoError(t, os.MkdirAll(filepath.Join(tempDir, container), 0755))

	return &adapterTestFixture{
		adapter:  NewS3ToEngine(eng, db, logger),
		db:       db,
		eng:      eng,
		tenantID: tenantID,
		tenant:   tn,
		tempDir:  tempDir,
	}
}

func TestHandleGet_ContentTypeFromCache(t *testing.T) {
	f := setupAdapterFixture(t)

	content := []byte("fake video content for testing")
	container := f.tenant.NamespaceContainer("test-bucket")
	_, err := f.eng.Put(context.Background(), container, "video.mp4", bytes.NewReader(content))
	require.NoError(t, err)

	_, err = f.db.Exec(`
		INSERT INTO object_head_cache (tenant_id, bucket, object_key, size_bytes, etag, content_type)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (tenant_id, bucket, object_key) DO UPDATE SET
			size_bytes = $4, etag = $5, content_type = $6
	`, f.tenantID, "test-bucket", "video.mp4", len(content), "abc123", "video/mp4")
	require.NoError(t, err)

	req := httptest.NewRequest("GET", "/test-bucket/video.mp4", nil)
	ctx := tenant.WithTenant(req.Context(), f.tenant)
	req = req.WithContext(ctx)

	w := httptest.NewRecorder()
	f.adapter.HandleGet(w, req, "test-bucket", "video.mp4")

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "video/mp4", w.Header().Get("Content-Type"))

	body, _ := io.ReadAll(w.Body)
	assert.Equal(t, content, body)
}

func TestHandleGet_ContentTypeFallsBackToExtension(t *testing.T) {
	f := setupAdapterFixture(t)

	content := []byte("some json data")
	container := f.tenant.NamespaceContainer("test-bucket")
	_, err := f.eng.Put(context.Background(), container, "data.json", bytes.NewReader(content))
	require.NoError(t, err)

	req := httptest.NewRequest("GET", "/test-bucket/data.json", nil)
	ctx := tenant.WithTenant(req.Context(), f.tenant)
	req = req.WithContext(ctx)

	w := httptest.NewRecorder()
	f.adapter.HandleGet(w, req, "test-bucket", "data.json")

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "application/json", w.Header().Get("Content-Type"))
}

func TestHandleGet_RangeRequest(t *testing.T) {
	f := setupAdapterFixture(t)

	content := []byte("ABCDEFGHIJKLMNOPQRSTUVWXYZ")
	container := f.tenant.NamespaceContainer("test-bucket")
	_, err := f.eng.Put(context.Background(), container, "alphabet.txt", bytes.NewReader(content))
	require.NoError(t, err)

	_, err = f.db.Exec(`
		INSERT INTO object_head_cache (tenant_id, bucket, object_key, size_bytes, etag, content_type)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (tenant_id, bucket, object_key) DO UPDATE SET
			size_bytes = $4, etag = $5, content_type = $6
	`, f.tenantID, "test-bucket", "alphabet.txt", len(content), "alpha123", "text/plain")
	require.NoError(t, err)

	req := httptest.NewRequest("GET", "/test-bucket/alphabet.txt", nil)
	req.Header.Set("Range", "bytes=0-4")
	ctx := tenant.WithTenant(req.Context(), f.tenant)
	req = req.WithContext(ctx)

	w := httptest.NewRecorder()
	f.adapter.HandleGet(w, req, "test-bucket", "alphabet.txt")

	assert.Equal(t, http.StatusPartialContent, w.Code)
	assert.Equal(t, "bytes 0-4/26", w.Header().Get("Content-Range"))
	assert.Equal(t, "5", w.Header().Get("Content-Length"))

	body, _ := io.ReadAll(w.Body)
	assert.Equal(t, "ABCDE", string(body))
}

func TestHandleGet_RangeRequest_Unsatisfiable(t *testing.T) {
	f := setupAdapterFixture(t)

	content := []byte("short")
	container := f.tenant.NamespaceContainer("test-bucket")
	_, err := f.eng.Put(context.Background(), container, "small.txt", bytes.NewReader(content))
	require.NoError(t, err)

	_, err = f.db.Exec(`
		INSERT INTO object_head_cache (tenant_id, bucket, object_key, size_bytes, etag, content_type)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (tenant_id, bucket, object_key) DO UPDATE SET
			size_bytes = $4, etag = $5, content_type = $6
	`, f.tenantID, "test-bucket", "small.txt", len(content), "small123", "text/plain")
	require.NoError(t, err)

	req := httptest.NewRequest("GET", "/test-bucket/small.txt", nil)
	req.Header.Set("Range", "bytes=100-200")
	ctx := tenant.WithTenant(req.Context(), f.tenant)
	req = req.WithContext(ctx)

	w := httptest.NewRecorder()
	f.adapter.HandleGet(w, req, "test-bucket", "small.txt")

	assert.Equal(t, http.StatusRequestedRangeNotSatisfiable, w.Code)
}

func TestHandleGet_RangeIgnoredWithoutCache(t *testing.T) {
	f := setupAdapterFixture(t)

	content := []byte("no cache content")
	container := f.tenant.NamespaceContainer("test-bucket")
	_, err := f.eng.Put(context.Background(), container, "nocache.txt", bytes.NewReader(content))
	require.NoError(t, err)

	req := httptest.NewRequest("GET", "/test-bucket/nocache.txt", nil)
	req.Header.Set("Range", "bytes=0-4")
	ctx := tenant.WithTenant(req.Context(), f.tenant)
	req = req.WithContext(ctx)

	w := httptest.NewRecorder()
	f.adapter.HandleGet(w, req, "test-bucket", "nocache.txt")

	assert.Equal(t, http.StatusOK, w.Code, "without cache, range is ignored and full content returned")

	body, _ := io.ReadAll(w.Body)
	assert.Equal(t, content, body)
}

func TestHandleGet_ContentLengthSetFromCache(t *testing.T) {
	f := setupAdapterFixture(t)

	content := []byte("content with known size")
	container := f.tenant.NamespaceContainer("test-bucket")
	_, err := f.eng.Put(context.Background(), container, "sized.txt", bytes.NewReader(content))
	require.NoError(t, err)

	_, err = f.db.Exec(`
		INSERT INTO object_head_cache (tenant_id, bucket, object_key, size_bytes, etag, content_type)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (tenant_id, bucket, object_key) DO UPDATE SET
			size_bytes = $4, etag = $5, content_type = $6
	`, f.tenantID, "test-bucket", "sized.txt", len(content), "sized123", "text/plain")
	require.NoError(t, err)

	req := httptest.NewRequest("GET", "/test-bucket/sized.txt", nil)
	ctx := tenant.WithTenant(req.Context(), f.tenant)
	req = req.WithContext(ctx)

	w := httptest.NewRecorder()
	f.adapter.HandleGet(w, req, "test-bucket", "sized.txt")

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "23", w.Header().Get("Content-Length"))
}

func generateSSECKey(t *testing.T) []byte {
	t.Helper()
	key := make([]byte, 32)
	_, err := rand.Read(key)
	require.NoError(t, err)
	return key
}

func setSSECHeaders(t *testing.T, req *http.Request, key []byte) {
	t.Helper()
	req.Header.Set("x-amz-server-side-encryption-customer-algorithm", "AES256")
	req.Header.Set("x-amz-server-side-encryption-customer-key", base64.StdEncoding.EncodeToString(key))
	digest := md5.Sum(key) // #nosec G401
	req.Header.Set("x-amz-server-side-encryption-customer-key-MD5", base64.StdEncoding.EncodeToString(digest[:]))
}

func TestPutObject_SSEC_Encrypts(t *testing.T) {
	f := setupAdapterFixture(t)
	key := generateSSECKey(t)
	content := []byte("SSE-C encrypted content for testing")

	req := httptest.NewRequest("PUT", "/test-bucket/ssec-obj.txt", bytes.NewReader(content))
	req.ContentLength = int64(len(content))
	req.Header.Set("Content-Type", "text/plain")
	setSSECHeaders(t, req, key)
	ctx := tenant.WithTenant(req.Context(), f.tenant)
	req = req.WithContext(ctx)

	w := httptest.NewRecorder()
	f.adapter.HandlePut(w, req, "test-bucket", "ssec-obj.txt")

	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "AES256", w.Header().Get("x-amz-server-side-encryption-customer-algorithm"))
	assert.Empty(t, w.Header().Get("x-amz-server-side-encryption"))

	var encAlgo string
	err := f.db.QueryRow(`
		SELECT COALESCE(encryption_algorithm, '')
		FROM object_head_cache
		WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3`,
		f.tenantID, "test-bucket", "ssec-obj.txt").Scan(&encAlgo)
	require.NoError(t, err)
	assert.Equal(t, crypto.SSECAlgorithm, encAlgo)

	var storedSize int64
	err = f.db.QueryRow(`
		SELECT size_bytes FROM object_head_cache
		WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3`,
		f.tenantID, "test-bucket", "ssec-obj.txt").Scan(&storedSize)
	require.NoError(t, err)
	assert.Equal(t, int64(len(content)), storedSize, "cache stores plaintext size")
}

func TestGetObject_SSEC_RequiresKey(t *testing.T) {
	f := setupAdapterFixture(t)
	key := generateSSECKey(t)
	content := []byte("requires key to read")

	putReq := httptest.NewRequest("PUT", "/test-bucket/ssec-need-key.txt", bytes.NewReader(content))
	putReq.ContentLength = int64(len(content))
	setSSECHeaders(t, putReq, key)
	ctx := tenant.WithTenant(putReq.Context(), f.tenant)
	putReq = putReq.WithContext(ctx)
	pw := httptest.NewRecorder()
	f.adapter.HandlePut(pw, putReq, "test-bucket", "ssec-need-key.txt")
	require.Equal(t, http.StatusOK, pw.Code)

	getReq := httptest.NewRequest("GET", "/test-bucket/ssec-need-key.txt", nil)
	ctx = tenant.WithTenant(getReq.Context(), f.tenant)
	getReq = getReq.WithContext(ctx)
	gw := httptest.NewRecorder()
	f.adapter.HandleGet(gw, getReq, "test-bucket", "ssec-need-key.txt")

	assert.Equal(t, http.StatusForbidden, gw.Code)
	assert.Contains(t, gw.Body.String(), "SSE-C")
}

func TestGetObject_SSEC_WrongKey(t *testing.T) {
	f := setupAdapterFixture(t)
	key := generateSSECKey(t)
	wrongKey := generateSSECKey(t)
	content := []byte("wrong key test data")

	putReq := httptest.NewRequest("PUT", "/test-bucket/ssec-wrong.txt", bytes.NewReader(content))
	putReq.ContentLength = int64(len(content))
	setSSECHeaders(t, putReq, key)
	ctx := tenant.WithTenant(putReq.Context(), f.tenant)
	putReq = putReq.WithContext(ctx)
	pw := httptest.NewRecorder()
	f.adapter.HandlePut(pw, putReq, "test-bucket", "ssec-wrong.txt")
	require.Equal(t, http.StatusOK, pw.Code)

	getReq := httptest.NewRequest("GET", "/test-bucket/ssec-wrong.txt", nil)
	setSSECHeaders(t, getReq, wrongKey)
	ctx = tenant.WithTenant(getReq.Context(), f.tenant)
	getReq = getReq.WithContext(ctx)
	gw := httptest.NewRecorder()
	f.adapter.HandleGet(gw, getReq, "test-bucket", "ssec-wrong.txt")

	assert.Equal(t, http.StatusForbidden, gw.Code)
	assert.Contains(t, gw.Body.String(), "does not match")
}

func TestGetObject_SSEC_CorrectKey(t *testing.T) {
	f := setupAdapterFixture(t)
	key := generateSSECKey(t)
	content := []byte("round-trip SSE-C data — should come back identical")

	putReq := httptest.NewRequest("PUT", "/test-bucket/ssec-roundtrip.txt", bytes.NewReader(content))
	putReq.ContentLength = int64(len(content))
	putReq.Header.Set("Content-Type", "text/plain")
	setSSECHeaders(t, putReq, key)
	ctx := tenant.WithTenant(putReq.Context(), f.tenant)
	putReq = putReq.WithContext(ctx)
	pw := httptest.NewRecorder()
	f.adapter.HandlePut(pw, putReq, "test-bucket", "ssec-roundtrip.txt")
	require.Equal(t, http.StatusOK, pw.Code)

	getReq := httptest.NewRequest("GET", "/test-bucket/ssec-roundtrip.txt", nil)
	setSSECHeaders(t, getReq, key)
	ctx = tenant.WithTenant(getReq.Context(), f.tenant)
	getReq = getReq.WithContext(ctx)
	gw := httptest.NewRecorder()
	f.adapter.HandleGet(gw, getReq, "test-bucket", "ssec-roundtrip.txt")

	require.Equal(t, http.StatusOK, gw.Code)
	body, _ := io.ReadAll(gw.Body)
	assert.Equal(t, content, body)
	assert.Equal(t, "AES256", gw.Header().Get("x-amz-server-side-encryption-customer-algorithm"))
}

func TestHeadObject_SSEC_RequiresKey(t *testing.T) {
	f := setupAdapterFixture(t)

	_, err := f.db.Exec(`
		INSERT INTO object_head_cache
			(tenant_id, bucket, object_key, size_bytes, etag, content_type, encryption_algorithm)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (tenant_id, bucket, object_key) DO UPDATE SET
			encryption_algorithm = $7
	`, f.tenantID, "test-bucket", "ssec-head.txt", 100, "abc123", "text/plain", crypto.SSECAlgorithm)
	require.NoError(t, err)

	srv := &Server{db: f.db, logger: zap.NewNop()}
	req := httptest.NewRequest("HEAD", "/test-bucket/ssec-head.txt", nil)
	ctx := tenant.WithTenant(req.Context(), f.tenant)
	req = req.WithContext(ctx)
	s3Req := &S3Request{Bucket: "test-bucket", Object: "ssec-head.txt"}

	w := httptest.NewRecorder()
	srv.handleHeadObject(w, req, s3Req)

	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestHeadObject_SSEC_WithKey(t *testing.T) {
	f := setupAdapterFixture(t)
	key := generateSSECKey(t)

	_, err := f.db.Exec(`
		INSERT INTO object_head_cache
			(tenant_id, bucket, object_key, size_bytes, etag, content_type, encryption_algorithm)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (tenant_id, bucket, object_key) DO UPDATE SET
			encryption_algorithm = $7
	`, f.tenantID, "test-bucket", "ssec-head-ok.txt", 100, "def456", "text/plain", crypto.SSECAlgorithm)
	require.NoError(t, err)

	srv := &Server{db: f.db, logger: zap.NewNop()}
	req := httptest.NewRequest("HEAD", "/test-bucket/ssec-head-ok.txt", nil)
	setSSECHeaders(t, req, key)
	ctx := tenant.WithTenant(req.Context(), f.tenant)
	req = req.WithContext(ctx)
	s3Req := &S3Request{Bucket: "test-bucket", Object: "ssec-head-ok.txt"}

	w := httptest.NewRecorder()
	srv.handleHeadObject(w, req, s3Req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "AES256", w.Header().Get("x-amz-server-side-encryption-customer-algorithm"))
	assert.Empty(t, w.Header().Get("x-amz-server-side-encryption"))
}

func TestPutObject_SSEC_And_SSES3_MutuallyExclusive(t *testing.T) {
	f := setupAdapterFixture(t)
	key := generateSSECKey(t)
	content := []byte("conflict test")

	req := httptest.NewRequest("PUT", "/test-bucket/conflict.txt", bytes.NewReader(content))
	req.ContentLength = int64(len(content))
	setSSECHeaders(t, req, key)
	req.Header.Set("x-amz-server-side-encryption", "AES256")
	ctx := tenant.WithTenant(req.Context(), f.tenant)
	req = req.WithContext(ctx)

	w := httptest.NewRecorder()
	f.adapter.HandlePut(w, req, "test-bucket", "conflict.txt")

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "simultaneously")
}
