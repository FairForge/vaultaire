package compatibility

import (
	"bytes"
	"crypto/md5" // #nosec G501 — S3 spec requires MD5 for ETags
	"database/sql"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/FairForge/vaultaire/internal/api"
	"github.com/FairForge/vaultaire/internal/drivers"
	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/FairForge/vaultaire/internal/tenant"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	_ "github.com/lib/pq"
)

type compatFixture struct {
	server   *api.Server
	adapter  *api.S3ToEngine
	db       *sql.DB
	eng      *engine.CoreEngine
	tenantID string
	tenant   *tenant.Tenant
	tempDir  string
	bucket   string
}

func setupCompatFixture(t *testing.T) *compatFixture {
	t.Helper()

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set — skipping integration test")
	}
	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	require.NoError(t, db.Ping())

	ensureCompatTables(t, db)

	logger := zap.NewNop()

	tempDir, err := os.MkdirTemp("", "vaultaire-compat-test-*")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(tempDir) })

	eng := engine.NewEngine(nil, logger, nil)
	driver := drivers.NewLocalDriver(tempDir, logger)
	eng.AddDriver("local", driver)
	eng.SetPrimary("local")

	tenantID := fmt.Sprintf("compat-%s-%d", t.Name(), os.Getpid())
	bucket := "compat-bucket"
	email := fmt.Sprintf("compat-%s-%d@test.local", t.Name(), os.Getpid())

	// Clean up any leftovers from a prior run with the same tenant ID.
	_, _ = db.Exec("DELETE FROM multipart_parts WHERE upload_id IN (SELECT upload_id FROM multipart_uploads WHERE tenant_id = $1)", tenantID)
	_, _ = db.Exec("DELETE FROM multipart_uploads WHERE tenant_id = $1", tenantID)
	_, _ = db.Exec("DELETE FROM object_versions WHERE tenant_id = $1", tenantID)
	_, _ = db.Exec("DELETE FROM object_head_cache WHERE tenant_id = $1", tenantID)
	_, _ = db.Exec("DELETE FROM buckets WHERE tenant_id = $1", tenantID)
	_, _ = db.Exec("DELETE FROM tenants WHERE id = $1", tenantID)

	_, err = db.Exec(`
		INSERT INTO tenants (id, name, email, access_key, secret_key)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (id) DO NOTHING
	`, tenantID, "Compat Test", email, "AK-"+tenantID, "SK-"+tenantID)
	require.NoError(t, err)

	_, err = db.Exec(`
		INSERT INTO buckets (tenant_id, name, visibility, versioning_status)
		VALUES ($1, $2, 'private', 'disabled')
		ON CONFLICT (tenant_id, name) DO UPDATE SET versioning_status = 'disabled'
	`, tenantID, bucket)
	require.NoError(t, err)

	t.Cleanup(func() {
		_, _ = db.Exec("DELETE FROM multipart_parts WHERE upload_id IN (SELECT upload_id FROM multipart_uploads WHERE tenant_id = $1)", tenantID)
		_, _ = db.Exec("DELETE FROM multipart_uploads WHERE tenant_id = $1", tenantID)
		_, _ = db.Exec("DELETE FROM object_versions WHERE tenant_id = $1", tenantID)
		_, _ = db.Exec("DELETE FROM object_head_cache WHERE tenant_id = $1", tenantID)
		_, _ = db.Exec("DELETE FROM buckets WHERE tenant_id = $1", tenantID)
		_, _ = db.Exec("DELETE FROM tenants WHERE id = $1", tenantID)
	})

	tn := &tenant.Tenant{
		ID:        tenantID,
		Namespace: "tenant/" + tenantID + "/",
	}

	container := tn.NamespaceContainer(bucket)
	require.NoError(t, os.MkdirAll(filepath.Join(tempDir, container), 0755))

	srv := api.NewTestServer(eng, db, logger)

	return &compatFixture{
		server:   srv,
		adapter:  api.NewS3ToEngine(eng, db, logger),
		db:       db,
		eng:      eng,
		tenantID: tenantID,
		tenant:   tn,
		tempDir:  tempDir,
		bucket:   bucket,
	}
}

func (f *compatFixture) put(t *testing.T, key, contentType string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("PUT", "/"+f.bucket+"/"+key, bytes.NewReader(body))
	req.Header.Set("Content-Type", contentType)
	req.ContentLength = int64(len(body))
	ctx := tenant.WithTenant(req.Context(), f.tenant)
	req = req.WithContext(ctx)
	w := httptest.NewRecorder()
	f.adapter.HandlePut(w, req, f.bucket, key)
	return w
}

func (f *compatFixture) get(t *testing.T, key string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", "/"+f.bucket+"/"+key, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	ctx := tenant.WithTenant(req.Context(), f.tenant)
	req = req.WithContext(ctx)
	w := httptest.NewRecorder()
	f.adapter.HandleGet(w, req, f.bucket, key)
	return w
}

func (f *compatFixture) head(t *testing.T, key string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("HEAD", "/"+f.bucket+"/"+key, nil)
	ctx := tenant.WithTenant(req.Context(), f.tenant)
	req = req.WithContext(ctx)
	w := httptest.NewRecorder()
	f.server.HandleHeadObject(w, req, f.bucket, key)
	return w
}

func (f *compatFixture) del(t *testing.T, key string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("DELETE", "/"+f.bucket+"/"+key, nil)
	ctx := tenant.WithTenant(req.Context(), f.tenant)
	req = req.WithContext(ctx)
	w := httptest.NewRecorder()
	f.adapter.HandleDelete(w, req, f.bucket, key)
	return w
}

func (f *compatFixture) listV2(t *testing.T, params map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	url := "/" + f.bucket + "?list-type=2"
	for k, v := range params {
		url += "&" + k + "=" + v
	}
	req := httptest.NewRequest("GET", url, nil)
	ctx := tenant.WithTenant(req.Context(), f.tenant)
	req = req.WithContext(ctx)
	w := httptest.NewRecorder()
	f.adapter.HandleListV2(w, req, f.bucket)
	return w
}

func md5hex(data []byte) string {
	h := md5.Sum(data) // #nosec G401
	return hex.EncodeToString(h[:])
}

func ensureCompatTables(t *testing.T, db *sql.DB) {
	t.Helper()
	ddl := []string{
		`CREATE TABLE IF NOT EXISTS multipart_uploads (
			upload_id TEXT NOT NULL PRIMARY KEY,
			tenant_id TEXT NOT NULL,
			bucket TEXT NOT NULL,
			object_key TEXT NOT NULL,
			status TEXT NOT NULL DEFAULT 'active',
			created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
		)`,
		`CREATE TABLE IF NOT EXISTS multipart_parts (
			upload_id TEXT NOT NULL REFERENCES multipart_uploads(upload_id) ON DELETE CASCADE,
			part_number INT NOT NULL,
			etag TEXT NOT NULL,
			size_bytes BIGINT NOT NULL,
			created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
			PRIMARY KEY (upload_id, part_number)
		)`,
	}
	for _, stmt := range ddl {
		_, err := db.Exec(stmt)
		require.NoError(t, err)
	}
}

// --- Test groups ---

func TestCompat_BasicCRUD(t *testing.T) {
	f := setupCompatFixture(t)

	key := "compat/basic-crud.txt"
	body := []byte("hello from rclone compatibility test")
	expectedETag := md5hex(body)

	// PUT
	w := f.put(t, key, "text/plain", body)
	require.Equal(t, http.StatusOK, w.Code, "PUT should succeed")
	putETag := w.Header().Get("ETag")
	assert.Contains(t, putETag, expectedETag, "PUT ETag should contain MD5 of body")

	// GET — verify body
	w = f.get(t, key, nil)
	require.Equal(t, http.StatusOK, w.Code, "GET should succeed")
	assert.Equal(t, body, w.Body.Bytes(), "GET body should match PUT body")

	// HEAD — verify metadata
	w = f.head(t, key)
	require.Equal(t, http.StatusOK, w.Code, "HEAD should succeed")
	assert.Equal(t, strconv.Itoa(len(body)), w.Header().Get("Content-Length"),
		"HEAD Content-Length should match body size")
	headETag := w.Header().Get("ETag")
	assert.Contains(t, headETag, expectedETag, "HEAD ETag should match")
	assert.Equal(t, "text/plain", w.Header().Get("Content-Type"),
		"HEAD Content-Type should match PUT")

	// DELETE
	w = f.del(t, key)
	require.Equal(t, http.StatusNoContent, w.Code, "DELETE should return 204")

	// GET after DELETE → 404
	w = f.get(t, key, nil)
	assert.Equal(t, http.StatusNotFound, w.Code, "GET after DELETE should return 404")
}

func TestCompat_MultipartUpload(t *testing.T) {
	f := setupCompatFixture(t)

	key := "compat/multipart.bin"
	partSize := 6 * 1024 * 1024 // 6MB per part (exceeds 5MB minimum)
	part1Data := bytes.Repeat([]byte("A"), partSize)
	part2Data := bytes.Repeat([]byte("B"), partSize)

	// Initiate
	initReq := httptest.NewRequest("POST", "/"+f.bucket+"/"+key+"?uploads", nil)
	ctx := tenant.WithTenant(initReq.Context(), f.tenant)
	initReq = initReq.WithContext(ctx)
	initW := httptest.NewRecorder()
	f.server.HandleInitiateMultipartUpload(initW, initReq, f.bucket, key)
	require.Equal(t, http.StatusOK, initW.Code, "InitiateMultipartUpload should succeed")

	var initResult struct {
		XMLName  xml.Name `xml:"InitiateMultipartUploadResult"`
		Bucket   string   `xml:"Bucket"`
		Key      string   `xml:"Key"`
		UploadID string   `xml:"UploadId"`
	}
	require.NoError(t, xml.Unmarshal(initW.Body.Bytes(), &initResult))
	uploadID := initResult.UploadID
	require.NotEmpty(t, uploadID, "upload ID should not be empty")

	// UploadPart 1
	part1Req := httptest.NewRequest("PUT",
		"/"+f.bucket+"/"+key+"?partNumber=1&uploadId="+uploadID,
		bytes.NewReader(part1Data))
	ctx = tenant.WithTenant(part1Req.Context(), f.tenant)
	part1Req = part1Req.WithContext(ctx)
	part1W := httptest.NewRecorder()
	f.server.HandleUploadPart(part1W, part1Req, f.bucket, key)
	require.Equal(t, http.StatusOK, part1W.Code, "UploadPart 1 should succeed")
	part1ETag := part1W.Header().Get("ETag")
	require.NotEmpty(t, part1ETag)

	// UploadPart 2
	part2Req := httptest.NewRequest("PUT",
		"/"+f.bucket+"/"+key+"?partNumber=2&uploadId="+uploadID,
		bytes.NewReader(part2Data))
	ctx = tenant.WithTenant(part2Req.Context(), f.tenant)
	part2Req = part2Req.WithContext(ctx)
	part2W := httptest.NewRecorder()
	f.server.HandleUploadPart(part2W, part2Req, f.bucket, key)
	require.Equal(t, http.StatusOK, part2W.Code, "UploadPart 2 should succeed")
	part2ETag := part2W.Header().Get("ETag")
	require.NotEmpty(t, part2ETag)

	// CompleteMultipartUpload
	completeBody := fmt.Sprintf(`<CompleteMultipartUpload>
		<Part><PartNumber>1</PartNumber><ETag>%s</ETag></Part>
		<Part><PartNumber>2</PartNumber><ETag>%s</ETag></Part>
	</CompleteMultipartUpload>`, part1ETag, part2ETag)

	completeReq := httptest.NewRequest("POST",
		"/"+f.bucket+"/"+key+"?uploadId="+uploadID,
		strings.NewReader(completeBody))
	ctx = tenant.WithTenant(completeReq.Context(), f.tenant)
	completeReq = completeReq.WithContext(ctx)
	completeW := httptest.NewRecorder()
	f.server.HandleCompleteMultipartUpload(completeW, completeReq, f.bucket, key)
	require.Equal(t, http.StatusOK, completeW.Code, "CompleteMultipartUpload should succeed")

	var completeResult struct {
		XMLName xml.Name `xml:"CompleteMultipartUploadResult"`
		ETag    string   `xml:"ETag"`
	}
	require.NoError(t, xml.Unmarshal(completeW.Body.Bytes(), &completeResult))
	assert.NotEmpty(t, completeResult.ETag, "complete should return an ETag")
	assert.Contains(t, completeResult.ETag, "-2", "multipart ETag should end with -2")

	// GET assembled object — verify concatenated body
	w := f.get(t, key, nil)
	require.Equal(t, http.StatusOK, w.Code)
	expected := append(part1Data, part2Data...)
	assert.Equal(t, len(expected), w.Body.Len(),
		"assembled object size should equal sum of parts")
	assert.Equal(t, expected, w.Body.Bytes(),
		"assembled object should be part1 + part2")

	// Cleanup
	f.del(t, key)
}

func TestCompat_ListObjects(t *testing.T) {
	f := setupCompatFixture(t)

	// PUT 5 objects with a common prefix
	prefix := "compat-prefix/"
	keys := []string{
		prefix + "alpha.txt",
		prefix + "bravo.txt",
		prefix + "charlie.txt",
		prefix + "delta.txt",
		prefix + "echo.txt",
	}
	for _, k := range keys {
		w := f.put(t, k, "text/plain", []byte("data-"+k))
		require.Equal(t, http.StatusOK, w.Code)
	}
	t.Cleanup(func() {
		for _, k := range keys {
			f.del(t, k)
		}
	})

	// ListObjectsV2 — all 5 should appear
	w := f.listV2(t, map[string]string{"prefix": prefix})
	require.Equal(t, http.StatusOK, w.Code)

	var listResult api.ListBucketV2Result
	require.NoError(t, xml.Unmarshal(w.Body.Bytes(), &listResult))
	assert.Equal(t, 5, len(listResult.Contents),
		"all 5 objects should be listed")

	returnedKeys := make([]string, len(listResult.Contents))
	for i, c := range listResult.Contents {
		returnedKeys[i] = c.Key
	}
	for _, k := range keys {
		assert.Contains(t, returnedKeys, k, "key %s should be in listing", k)
	}

	// PUT objects at a different prefix
	otherKey := "other-prefix/file.txt"
	w = f.put(t, otherKey, "text/plain", []byte("other"))
	require.Equal(t, http.StatusOK, w.Code)
	t.Cleanup(func() { f.del(t, otherKey) })

	// ListObjectsV2 with prefix filter — only the 5
	w = f.listV2(t, map[string]string{"prefix": prefix})
	require.Equal(t, http.StatusOK, w.Code)
	var filteredResult api.ListBucketV2Result
	require.NoError(t, xml.Unmarshal(w.Body.Bytes(), &filteredResult))
	assert.Equal(t, 5, len(filteredResult.Contents),
		"prefix filter should exclude other-prefix/")

	// ListObjectsV2 with max-keys=2 — verify truncation
	w = f.listV2(t, map[string]string{"prefix": prefix, "max-keys": "2"})
	require.Equal(t, http.StatusOK, w.Code)
	var truncResult api.ListBucketV2Result
	require.NoError(t, xml.Unmarshal(w.Body.Bytes(), &truncResult))
	assert.Equal(t, 2, len(truncResult.Contents),
		"max-keys=2 should return exactly 2 objects")
	assert.True(t, truncResult.IsTruncated,
		"listing should be truncated when max-keys < total objects")
	assert.NotEmpty(t, truncResult.NextContinuationToken,
		"truncated listing should include NextContinuationToken")
}

func TestCompat_CopyObject(t *testing.T) {
	f := setupCompatFixture(t)

	srcKey := "compat/copy-source.txt"
	dstKey := "compat/copy-dest.txt"
	body := []byte("copy me via x-amz-copy-source")

	// PUT source
	w := f.put(t, srcKey, "text/plain", body)
	require.Equal(t, http.StatusOK, w.Code)
	t.Cleanup(func() {
		f.del(t, srcKey)
		f.del(t, dstKey)
	})

	// CopyObject: PUT dest with x-amz-copy-source
	copyReq := httptest.NewRequest("PUT", "/"+f.bucket+"/"+dstKey, nil)
	copyReq.Header.Set("x-amz-copy-source", "/"+f.bucket+"/"+srcKey)
	ctx := tenant.WithTenant(copyReq.Context(), f.tenant)
	copyReq = copyReq.WithContext(ctx)
	copyW := httptest.NewRecorder()
	f.server.HandleCopyObject(copyW, copyReq, f.bucket, dstKey)
	require.Equal(t, http.StatusOK, copyW.Code, "CopyObject should succeed")

	// Verify CopyObjectResult XML
	var copyResult struct {
		XMLName xml.Name `xml:"CopyObjectResult"`
		ETag    string   `xml:"ETag"`
	}
	require.NoError(t, xml.Unmarshal(copyW.Body.Bytes(), &copyResult))
	assert.NotEmpty(t, copyResult.ETag, "CopyObjectResult should have an ETag")

	// GET destination — verify body matches source
	w = f.get(t, dstKey, nil)
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, body, w.Body.Bytes(), "copied object body should match source")

	// Source should still exist
	w = f.get(t, srcKey, nil)
	require.Equal(t, http.StatusOK, w.Code, "source should still exist after copy")
	assert.Equal(t, body, w.Body.Bytes())
}

func TestCompat_BucketOperations(t *testing.T) {
	f := setupCompatFixture(t)

	newBucket := "compat-ops-test"

	// CreateBucket
	createReq := httptest.NewRequest("PUT", "/"+newBucket, nil)
	ctx := tenant.WithTenant(createReq.Context(), f.tenant)
	createReq = createReq.WithContext(ctx)
	createW := httptest.NewRecorder()
	f.server.CreateBucket(createW, createReq)
	require.Equal(t, http.StatusOK, createW.Code, "CreateBucket should succeed")

	// ListBuckets — verify it appears
	listReq := httptest.NewRequest("GET", "/", nil)
	ctx = tenant.WithTenant(listReq.Context(), f.tenant)
	listReq = listReq.WithContext(ctx)
	listW := httptest.NewRecorder()
	f.server.ListBuckets(listW, listReq)
	require.Equal(t, http.StatusOK, listW.Code, "ListBuckets should succeed")

	var listResult api.ListBucketsResponse
	require.NoError(t, xml.Unmarshal(listW.Body.Bytes(), &listResult))
	found := false
	for _, b := range listResult.Buckets.Bucket {
		if b.Name == newBucket {
			found = true
			break
		}
	}
	assert.True(t, found, "new bucket should appear in ListBuckets")

	// DeleteBucket
	delReq := httptest.NewRequest("DELETE", "/"+newBucket, nil)
	ctx = tenant.WithTenant(delReq.Context(), f.tenant)
	delReq = delReq.WithContext(ctx)
	delW := httptest.NewRecorder()
	f.server.DeleteBucket(delW, delReq)
	require.Equal(t, http.StatusNoContent, delW.Code, "DeleteBucket should return 204")
}

func TestCompat_ConditionalRequests(t *testing.T) {
	f := setupCompatFixture(t)

	key := "compat/conditional.txt"
	body := []byte("conditional request test body")
	expectedETag := md5hex(body)

	// PUT
	w := f.put(t, key, "text/plain", body)
	require.Equal(t, http.StatusOK, w.Code)
	t.Cleanup(func() { f.del(t, key) })

	// HEAD — verify ETag is quoted (rclone parses this strictly)
	w = f.head(t, key)
	require.Equal(t, http.StatusOK, w.Code)
	headETag := w.Header().Get("ETag")
	assert.True(t, strings.HasPrefix(headETag, `"`), "ETag must start with quote")
	assert.True(t, strings.HasSuffix(headETag, `"`), "ETag must end with quote")
	assert.Contains(t, headETag, expectedETag, "ETag should contain the MD5 hash")

	// GET with If-None-Match (matching ETag) → 304
	w = f.get(t, key, map[string]string{"If-None-Match": headETag})
	assert.Equal(t, http.StatusNotModified, w.Code,
		"GET with matching If-None-Match should return 304")

	// GET with If-None-Match (different ETag) → 200 with body
	w = f.get(t, key, map[string]string{"If-None-Match": `"0000000000000000"`})
	require.Equal(t, http.StatusOK, w.Code,
		"GET with non-matching If-None-Match should return 200")
	assert.Equal(t, body, w.Body.Bytes(), "body should be returned on 200")
}

func TestCompat_RangeRequests(t *testing.T) {
	f := setupCompatFixture(t)

	key := "compat/range.bin"
	body := make([]byte, 10240) // 10KB
	for i := range body {
		body[i] = byte(i % 256)
	}

	// PUT
	w := f.put(t, key, "application/octet-stream", body)
	require.Equal(t, http.StatusOK, w.Code)
	t.Cleanup(func() { f.del(t, key) })

	// Range: bytes=0-999 → 206 + exactly 1000 bytes
	w = f.get(t, key, map[string]string{"Range": "bytes=0-999"})
	assert.Equal(t, http.StatusPartialContent, w.Code,
		"range request should return 206")
	assert.Equal(t, 1000, w.Body.Len(),
		"range bytes=0-999 should return exactly 1000 bytes")
	assert.Equal(t, body[:1000], w.Body.Bytes(),
		"range content should match expected byte slice")
	assert.Contains(t, w.Header().Get("Content-Range"), "bytes 0-999/10240",
		"Content-Range header should be set correctly")

	// Range: bytes=1000-1999 → correct middle slice
	w = f.get(t, key, map[string]string{"Range": "bytes=1000-1999"})
	assert.Equal(t, http.StatusPartialContent, w.Code)
	assert.Equal(t, 1000, w.Body.Len())
	assert.Equal(t, body[1000:2000], w.Body.Bytes(),
		"second range should return the correct byte slice")
	assert.Contains(t, w.Header().Get("Content-Range"), "bytes 1000-1999/10240")
}
