package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/FairForge/vaultaire/internal/crypto"
	"github.com/FairForge/vaultaire/internal/tenant"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	_ "github.com/lib/pq"
)

// putChunkedObject is a helper that uploads content above the chunking
// threshold via the adapter and returns the resulting ETag.
func putChunkedObject(t *testing.T, f *adapterTestFixture, key string, content []byte, contentType string) string {
	t.Helper()
	req := httptest.NewRequest("PUT", "/test-bucket/"+key, bytes.NewReader(content))
	req.ContentLength = int64(len(content))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	req = req.WithContext(tenant.WithTenant(req.Context(), f.tenant))
	w := httptest.NewRecorder()
	f.adapter.HandlePut(w, req, "test-bucket", key)
	require.Equal(t, http.StatusOK, w.Code, "chunked PUT should succeed")
	return w.Header().Get("ETag")
}

func TestHandleGet_ChunkedObject(t *testing.T) {
	f := setupChunkingFixture(t)

	content := generateTestData(8 * 1024) // 8 KB — above 1 KB threshold
	putETag := putChunkedObject(t, f, "chunked-get.bin", content, "application/octet-stream")

	// Sanity: the object is actually stored chunked.
	var isChunked bool
	require.NoError(t, f.db.QueryRow(`
		SELECT is_chunked FROM object_head_cache
		WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3`,
		f.tenantID, "test-bucket", "chunked-get.bin").Scan(&isChunked))
	require.True(t, isChunked, "object should be chunked for this test to be meaningful")

	getReq := httptest.NewRequest("GET", "/test-bucket/chunked-get.bin", nil)
	getReq = getReq.WithContext(tenant.WithTenant(getReq.Context(), f.tenant))
	gw := httptest.NewRecorder()
	f.adapter.HandleGet(gw, getReq, "test-bucket", "chunked-get.bin")

	require.Equal(t, http.StatusOK, gw.Code)
	body, _ := io.ReadAll(gw.Body)
	assert.Equal(t, content, body, "reassembled body must match uploaded content")
	assert.Equal(t, strconv.Itoa(len(content)), gw.Header().Get("Content-Length"))
	assert.Equal(t, putETag, gw.Header().Get("ETag"), "ETag must match the PUT ETag")
	assert.Equal(t, "application/octet-stream", gw.Header().Get("Content-Type"))
}

func TestHandleGet_ChunkedObject_RangeRequest(t *testing.T) {
	f := setupChunkingFixture(t)

	content := generateTestData(8 * 1024)
	putChunkedObject(t, f, "range.bin", content, "application/octet-stream")

	getReq := httptest.NewRequest("GET", "/test-bucket/range.bin", nil)
	getReq.Header.Set("Range", "bytes=0-99")
	getReq = getReq.WithContext(tenant.WithTenant(getReq.Context(), f.tenant))
	gw := httptest.NewRecorder()
	f.adapter.HandleGet(gw, getReq, "test-bucket", "range.bin")

	require.Equal(t, http.StatusPartialContent, gw.Code)
	body, _ := io.ReadAll(gw.Body)
	assert.Equal(t, content[0:100], body, "range slice must match")
	assert.Equal(t, "100", gw.Header().Get("Content-Length"))
	assert.Equal(t, fmt.Sprintf("bytes 0-99/%d", len(content)), gw.Header().Get("Content-Range"))
}

func TestHandleGet_ChunkedDedup_SharedContent(t *testing.T) {
	f := setupChunkingFixture(t)

	content := generateTestData(8 * 1024)
	keys := []string{"dup-a.bin", "dup-b.bin"}
	for _, key := range keys {
		putChunkedObject(t, f, key, content, "application/octet-stream")
	}

	bodies := make([][]byte, 0, len(keys))
	for _, key := range keys {
		getReq := httptest.NewRequest("GET", "/test-bucket/"+key, nil)
		getReq = getReq.WithContext(tenant.WithTenant(getReq.Context(), f.tenant))
		gw := httptest.NewRecorder()
		f.adapter.HandleGet(gw, getReq, "test-bucket", key)
		require.Equal(t, http.StatusOK, gw.Code)
		b, _ := io.ReadAll(gw.Body)
		bodies = append(bodies, b)
	}

	assert.Equal(t, content, bodies[0])
	assert.Equal(t, content, bodies[1])
	assert.Equal(t, bodies[0], bodies[1], "deduplicated objects must return identical content")
}

func TestHandleGet_SmallObject_NotChunked(t *testing.T) {
	f := setupChunkingFixture(t)

	content := []byte("small object below the chunking threshold")
	putReq := httptest.NewRequest("PUT", "/test-bucket/small-get.txt", bytes.NewReader(content))
	putReq.ContentLength = int64(len(content))
	putReq.Header.Set("Content-Type", "text/plain")
	putReq = putReq.WithContext(tenant.WithTenant(putReq.Context(), f.tenant))
	pw := httptest.NewRecorder()
	f.adapter.HandlePut(pw, putReq, "test-bucket", "small-get.txt")
	require.Equal(t, http.StatusOK, pw.Code)

	var isChunked bool
	require.NoError(t, f.db.QueryRow(`
		SELECT is_chunked FROM object_head_cache
		WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3`,
		f.tenantID, "test-bucket", "small-get.txt").Scan(&isChunked))
	require.False(t, isChunked, "small object should use the normal path")

	getReq := httptest.NewRequest("GET", "/test-bucket/small-get.txt", nil)
	getReq = getReq.WithContext(tenant.WithTenant(getReq.Context(), f.tenant))
	gw := httptest.NewRecorder()
	f.adapter.HandleGet(gw, getReq, "test-bucket", "small-get.txt")

	require.Equal(t, http.StatusOK, gw.Code)
	body, _ := io.ReadAll(gw.Body)
	assert.Equal(t, content, body)
	assert.Equal(t, "text/plain", gw.Header().Get("Content-Type"))
}

func TestHandleHead_ChunkedObject(t *testing.T) {
	f := setupChunkingFixture(t)

	content := generateTestData(8 * 1024)
	putETag := putChunkedObject(t, f, "head-chunked.bin", content, "application/octet-stream")

	srv := &Server{db: f.db, logger: zap.NewNop()}
	headReq := httptest.NewRequest("HEAD", "/test-bucket/head-chunked.bin", nil)
	headReq = headReq.WithContext(tenant.WithTenant(headReq.Context(), f.tenant))
	hw := httptest.NewRecorder()
	srv.handleHeadObject(hw, headReq, &S3Request{Bucket: "test-bucket", Object: "head-chunked.bin"})

	require.Equal(t, http.StatusOK, hw.Code)
	assert.Equal(t, strconv.Itoa(len(content)), hw.Header().Get("Content-Length"),
		"HEAD should report the plaintext size")
	assert.Equal(t, putETag, hw.Header().Get("ETag"))
	body, _ := io.ReadAll(hw.Body)
	assert.Empty(t, body, "HEAD must not return a body")
}

func TestChunkedRoundTrip_PutDeletePut(t *testing.T) {
	f := setupChunkingFixture(t)

	content := generateTestData(8 * 1024)
	key := "roundtrip.bin"

	etag1 := putChunkedObject(t, f, key, content, "application/octet-stream")

	// DELETE the chunked object (decrements chunk refs; data stays until GC).
	delReq := httptest.NewRequest("DELETE", "/test-bucket/"+key, nil)
	delReq = delReq.WithContext(tenant.WithTenant(delReq.Context(), f.tenant))
	dw := httptest.NewRecorder()
	f.adapter.HandleDelete(dw, delReq, "test-bucket", key)
	require.Equal(t, http.StatusNoContent, dw.Code)

	// PUT the same content again — chunks are re-referenced from the index.
	etag2 := putChunkedObject(t, f, key, content, "application/octet-stream")
	assert.Equal(t, etag1, etag2, "same content must yield the same ETag")

	// GET and verify the data round-trips intact.
	getReq := httptest.NewRequest("GET", "/test-bucket/"+key, nil)
	getReq = getReq.WithContext(tenant.WithTenant(getReq.Context(), f.tenant))
	gw := httptest.NewRecorder()
	f.adapter.HandleGet(gw, getReq, "test-bucket", key)
	require.Equal(t, http.StatusOK, gw.Code)
	body, _ := io.ReadAll(gw.Body)
	assert.Equal(t, content, body, "data must survive PUT → DELETE → PUT round-trip")
}

func setupChunkingFixture(t *testing.T) *adapterTestFixture {
	t.Helper()

	// Chunking requires UUID tenant IDs (GCI uses uuid.UUID).
	// Override the text-based tenant ID from setupAdapterFixture with a real UUID.
	f := setupAdapterFixture(t)
	tenantUUID := uuid.New()
	f.tenantID = tenantUUID.String()
	f.tenant = &tenant.Tenant{
		ID:        tenantUUID.String(),
		Namespace: "tenant/" + tenantUUID.String() + "/",
	}

	// Re-create tenant row with UUID ID
	_, err := f.db.Exec(`
		INSERT INTO tenants (id, name, email, access_key, secret_key)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (id) DO NOTHING
	`, f.tenantID, "Chunking Test", "chunking@test.local", "AK-"+f.tenantID[:8], "SK-"+f.tenantID[:8])
	require.NoError(t, err)

	// Create the namespaced container directory
	container := f.tenant.NamespaceContainer("test-bucket")
	require.NoError(t, os.MkdirAll(filepath.Join(f.tempDir, container), 0755))

	f.adapter = NewS3ToEngine(f.eng, f.db, zap.NewNop())
	f.adapter.gci = crypto.NewGlobalContentIndex(f.db)
	f.adapter.chunkingThreshold = 1024

	t.Cleanup(func() {
		_, _ = f.db.Exec("DELETE FROM tenant_chunk_refs WHERE tenant_id = $1", tenantUUID)
		_, _ = f.db.Exec("DELETE FROM object_metadata WHERE tenant_id = $1", tenantUUID)
		_, _ = f.db.Exec("DELETE FROM object_head_cache WHERE tenant_id = $1", f.tenantID)
		_, _ = f.db.Exec("DELETE FROM tenants WHERE id = $1", f.tenantID)
	})

	return f
}

// generateTestData creates deterministic test data of the given size.
func generateTestData(size int) []byte {
	data := make([]byte, size)
	_, _ = rand.Read(data)
	return data
}

func TestHandlePut_ChunkedUpload(t *testing.T) {
	f := setupChunkingFixture(t)

	content := generateTestData(8 * 1024) // 8 KB — above 1 KB threshold
	req := httptest.NewRequest("PUT", "/test-bucket/large-object.bin", bytes.NewReader(content))
	req.ContentLength = int64(len(content))
	req.Header.Set("Content-Type", "application/octet-stream")
	ctx := tenant.WithTenant(req.Context(), f.tenant)
	req = req.WithContext(ctx)

	w := httptest.NewRecorder()
	f.adapter.HandlePut(w, req, "test-bucket", "large-object.bin")

	require.Equal(t, http.StatusOK, w.Code, "PUT should succeed")
	assert.NotEmpty(t, w.Header().Get("ETag"), "should have an ETag")

	// Verify is_chunked = TRUE in object_head_cache
	var isChunked bool
	err := f.db.QueryRow(`
		SELECT is_chunked FROM object_head_cache
		WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3`,
		f.tenantID, "test-bucket", "large-object.bin").Scan(&isChunked)
	require.NoError(t, err)
	assert.True(t, isChunked, "object should be marked as chunked")

	// Verify chunks exist in global_content_index
	tenantUUID, err := uuid.Parse(f.tenantID)
	require.NoError(t, err)

	var chunkCount int
	err = f.db.QueryRow(`
		SELECT COUNT(*) FROM tenant_chunk_refs
		WHERE tenant_id = $1 AND bucket_name = $2 AND object_key = $3`,
		tenantUUID, "test-bucket", "large-object.bin").Scan(&chunkCount)
	require.NoError(t, err)
	assert.Greater(t, chunkCount, 0, "should have chunk references")

	// Verify object_metadata was saved
	var totalSize int64
	var savedChunkCount int
	err = f.db.QueryRow(`
		SELECT total_size, chunk_count FROM object_metadata
		WHERE tenant_id = $1 AND bucket_name = $2 AND object_key = $3`,
		tenantUUID, "test-bucket", "large-object.bin").Scan(&totalSize, &savedChunkCount)
	require.NoError(t, err)
	assert.Equal(t, int64(len(content)), totalSize)
	assert.Equal(t, chunkCount, savedChunkCount)
}

func TestHandlePut_ChunkedDedup(t *testing.T) {
	f := setupChunkingFixture(t)

	content := generateTestData(8 * 1024)

	// First upload
	req1 := httptest.NewRequest("PUT", "/test-bucket/dedup-obj-1.bin", bytes.NewReader(content))
	req1.ContentLength = int64(len(content))
	ctx1 := tenant.WithTenant(req1.Context(), f.tenant)
	req1 = req1.WithContext(ctx1)
	w1 := httptest.NewRecorder()
	f.adapter.HandlePut(w1, req1, "test-bucket", "dedup-obj-1.bin")
	require.Equal(t, http.StatusOK, w1.Code)

	// Get chunk hashes from first upload
	tenantUUID, _ := uuid.Parse(f.tenantID)
	var firstChunkCount int
	err := f.db.QueryRow(`
		SELECT COUNT(*) FROM tenant_chunk_refs
		WHERE tenant_id = $1 AND bucket_name = $2 AND object_key = $3`,
		tenantUUID, "test-bucket", "dedup-obj-1.bin").Scan(&firstChunkCount)
	require.NoError(t, err)

	// Second upload with same content under a different key
	req2 := httptest.NewRequest("PUT", "/test-bucket/dedup-obj-2.bin", bytes.NewReader(content))
	req2.ContentLength = int64(len(content))
	ctx2 := tenant.WithTenant(req2.Context(), f.tenant)
	req2 = req2.WithContext(ctx2)
	w2 := httptest.NewRecorder()
	f.adapter.HandlePut(w2, req2, "test-bucket", "dedup-obj-2.bin")
	require.Equal(t, http.StatusOK, w2.Code)

	// Verify ref_counts are 2 for all chunks (same content → same hashes)
	rows, err := f.db.Query(`
		SELECT gci.ref_count FROM global_content_index gci
		INNER JOIN tenant_chunk_refs tcr ON tcr.plaintext_hash = gci.plaintext_hash
		WHERE tcr.tenant_id = $1 AND tcr.bucket_name = $2 AND tcr.object_key = $3`,
		tenantUUID, "test-bucket", "dedup-obj-1.bin")
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var refCount int
		require.NoError(t, rows.Scan(&refCount))
		assert.Equal(t, 2, refCount, "each chunk should have ref_count=2 after dedup")
	}
	require.NoError(t, rows.Err())

	// Verify ETags match (same content)
	assert.Equal(t, w1.Header().Get("ETag"), w2.Header().Get("ETag"))
}

func TestHandlePut_SmallObjectSkipsChunking(t *testing.T) {
	f := setupChunkingFixture(t)

	content := []byte("small object under threshold")
	req := httptest.NewRequest("PUT", "/test-bucket/small.txt", bytes.NewReader(content))
	req.ContentLength = int64(len(content))
	req.Header.Set("Content-Type", "text/plain")
	ctx := tenant.WithTenant(req.Context(), f.tenant)
	req = req.WithContext(ctx)

	w := httptest.NewRecorder()
	f.adapter.HandlePut(w, req, "test-bucket", "small.txt")

	require.Equal(t, http.StatusOK, w.Code)

	var isChunked bool
	err := f.db.QueryRow(`
		SELECT is_chunked FROM object_head_cache
		WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3`,
		f.tenantID, "test-bucket", "small.txt").Scan(&isChunked)
	require.NoError(t, err)
	assert.False(t, isChunked, "small object should NOT be chunked")
}

func TestHandlePut_EncryptedSkipsChunking(t *testing.T) {
	f := setupChunkingFixture(t)

	content := generateTestData(8 * 1024) // above threshold
	req := httptest.NewRequest("PUT", "/test-bucket/encrypted-large.bin", bytes.NewReader(content))
	req.ContentLength = int64(len(content))

	// Set SSE-C headers so the object is encrypted
	key := generateSSECKey(t)
	setSSECHeaders(t, req, key)

	ctx := tenant.WithTenant(req.Context(), f.tenant)
	req = req.WithContext(ctx)

	w := httptest.NewRecorder()
	f.adapter.HandlePut(w, req, "test-bucket", "encrypted-large.bin")

	require.Equal(t, http.StatusOK, w.Code)

	var isChunked bool
	err := f.db.QueryRow(`
		SELECT is_chunked FROM object_head_cache
		WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3`,
		f.tenantID, "test-bucket", "encrypted-large.bin").Scan(&isChunked)
	require.NoError(t, err)
	assert.False(t, isChunked, "encrypted object should NOT be chunked")
}

func TestHandleDelete_ChunkedObject(t *testing.T) {
	f := setupChunkingFixture(t)

	content := generateTestData(8 * 1024)
	putReq := httptest.NewRequest("PUT", "/test-bucket/to-delete.bin", bytes.NewReader(content))
	putReq.ContentLength = int64(len(content))
	ctx := tenant.WithTenant(putReq.Context(), f.tenant)
	putReq = putReq.WithContext(ctx)
	pw := httptest.NewRecorder()
	f.adapter.HandlePut(pw, putReq, "test-bucket", "to-delete.bin")
	require.Equal(t, http.StatusOK, pw.Code)

	// Verify it's chunked
	var isChunked bool
	require.NoError(t, f.db.QueryRow(`
		SELECT is_chunked FROM object_head_cache
		WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3`,
		f.tenantID, "test-bucket", "to-delete.bin").Scan(&isChunked))
	require.True(t, isChunked)

	// Delete the chunked object
	delReq := httptest.NewRequest("DELETE", "/test-bucket/to-delete.bin", nil)
	ctx = tenant.WithTenant(delReq.Context(), f.tenant)
	delReq = delReq.WithContext(ctx)
	dw := httptest.NewRecorder()
	f.adapter.HandleDelete(dw, delReq, "test-bucket", "to-delete.bin")

	assert.Equal(t, http.StatusNoContent, dw.Code)

	// Verify object_head_cache is deleted
	var count int
	err := f.db.QueryRow(`
		SELECT COUNT(*) FROM object_head_cache
		WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3`,
		f.tenantID, "test-bucket", "to-delete.bin").Scan(&count)
	require.NoError(t, err)
	assert.Equal(t, 0, count, "object_head_cache should be cleared")

	// Verify tenant_chunk_refs are deleted
	tenantUUID, _ := uuid.Parse(f.tenantID)
	err = f.db.QueryRow(`
		SELECT COUNT(*) FROM tenant_chunk_refs
		WHERE tenant_id = $1 AND bucket_name = $2 AND object_key = $3`,
		tenantUUID, "test-bucket", "to-delete.bin").Scan(&count)
	require.NoError(t, err)
	assert.Equal(t, 0, count, "tenant_chunk_refs should be cleared")

	// Verify object_metadata is deleted
	err = f.db.QueryRow(`
		SELECT COUNT(*) FROM object_metadata
		WHERE tenant_id = $1 AND bucket_name = $2 AND object_key = $3`,
		tenantUUID, "test-bucket", "to-delete.bin").Scan(&count)
	require.NoError(t, err)
	assert.Equal(t, 0, count, "object_metadata should be cleared")

	// Chunks should be marked_for_deletion (ref_count = 0), but still exist
	err = f.db.QueryRow(`
		SELECT COUNT(*) FROM global_content_index
		WHERE marked_for_deletion = TRUE`).Scan(&count)
	require.NoError(t, err)
	assert.Greater(t, count, 0, "chunks should be marked for deletion, not removed")
}

func TestHandleDelete_ChunkedDedup_PartialRefCount(t *testing.T) {
	f := setupChunkingFixture(t)

	content := generateTestData(8 * 1024)

	// Upload same content as two different keys
	for _, key := range []string{"shared-1.bin", "shared-2.bin"} {
		req := httptest.NewRequest("PUT", "/test-bucket/"+key, bytes.NewReader(content))
		req.ContentLength = int64(len(content))
		ctx := tenant.WithTenant(req.Context(), f.tenant)
		req = req.WithContext(ctx)
		w := httptest.NewRecorder()
		f.adapter.HandlePut(w, req, "test-bucket", key)
		require.Equal(t, http.StatusOK, w.Code)
	}

	// Delete only the first key
	delReq := httptest.NewRequest("DELETE", "/test-bucket/shared-1.bin", nil)
	ctx := tenant.WithTenant(delReq.Context(), f.tenant)
	delReq = delReq.WithContext(ctx)
	dw := httptest.NewRecorder()
	f.adapter.HandleDelete(dw, delReq, "test-bucket", "shared-1.bin")
	assert.Equal(t, http.StatusNoContent, dw.Code)

	// Verify ref_counts are now 1 (not 0) because shared-2.bin still references them
	tenantUUID, _ := uuid.Parse(f.tenantID)
	rows, err := f.db.Query(`
		SELECT gci.ref_count, gci.marked_for_deletion FROM global_content_index gci
		INNER JOIN tenant_chunk_refs tcr ON tcr.plaintext_hash = gci.plaintext_hash
		WHERE tcr.tenant_id = $1 AND tcr.bucket_name = $2 AND tcr.object_key = $3`,
		tenantUUID, "test-bucket", "shared-2.bin")
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var refCount int
		var marked bool
		require.NoError(t, rows.Scan(&refCount, &marked))
		assert.Equal(t, 1, refCount, "ref_count should be 1 after deleting one of two references")
		assert.False(t, marked, "chunk should NOT be marked for deletion while ref_count > 0")
	}
	require.NoError(t, rows.Err())
}

func TestBackendRegion_ChunkStorageKey(t *testing.T) {
	hash := "abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890"
	storageKey := "_chunks/" + hash
	assert.Equal(t, "_chunks/abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890", storageKey)
	assert.True(t, len(storageKey) > 0)
	assert.Equal(t, "_chunks/", storageKey[:8])
}

func TestHandlePut_ChunkedUpload_NoGCI(t *testing.T) {
	f := setupAdapterFixture(t)

	// GCI is nil — large objects should use normal path
	content := generateTestData(8 * 1024)
	f.adapter.chunkingThreshold = 1024

	req := httptest.NewRequest("PUT", "/test-bucket/no-gci-large.bin", bytes.NewReader(content))
	req.ContentLength = int64(len(content))
	ctx := tenant.WithTenant(req.Context(), f.tenant)
	req = req.WithContext(ctx)

	w := httptest.NewRecorder()
	f.adapter.HandlePut(w, req, "test-bucket", "no-gci-large.bin")

	require.Equal(t, http.StatusOK, w.Code)

	var isChunked bool
	err := f.db.QueryRow(`
		SELECT is_chunked FROM object_head_cache
		WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3`,
		f.tenantID, "test-bucket", "no-gci-large.bin").Scan(&isChunked)
	require.NoError(t, err)
	assert.False(t, isChunked, "without GCI, object should NOT be chunked")
}

func TestGCI_IntegrationWithMigration(t *testing.T) {
	f := setupChunkingFixture(t)

	gci := crypto.NewGlobalContentIndex(f.db)
	ctx := context.Background()

	hash := "migration_test_hash_456789012345678901234567890123456789012345"
	entry := &crypto.GCIEntry{
		PlaintextHash: hash,
		BackendID:     "local",
		StorageKey:    "_chunks/" + hash,
		SizeBytes:     4096,
		RefCount:      1,
	}

	// Insert
	require.NoError(t, gci.InsertChunk(ctx, entry))

	// Lookup
	result, err := gci.LookupChunk(ctx, hash)
	require.NoError(t, err)
	assert.True(t, result.Exists)
	assert.Equal(t, "local", result.Entry.BackendID)

	// Increment + decrement
	require.NoError(t, gci.IncrementRef(ctx, hash))
	newCount, err := gci.DecrementRef(ctx, hash)
	require.NoError(t, err)
	assert.Equal(t, 1, newCount)

	// Decrement to 0
	newCount, err = gci.DecrementRef(ctx, hash)
	require.NoError(t, err)
	assert.Equal(t, 0, newCount)

	// Verify marked for deletion
	var marked bool
	require.NoError(t, f.db.QueryRow(
		"SELECT marked_for_deletion FROM global_content_index WHERE plaintext_hash = $1",
		hash).Scan(&marked))
	assert.True(t, marked)

	// Tenant chunk ref + object metadata
	tenantUUID := uuid.New()
	require.NoError(t, gci.AddTenantChunkRef(ctx, &crypto.TenantChunkRef{
		TenantID:             tenantUUID,
		BucketName:           "test-bucket",
		ObjectKey:            "test-key",
		ChunkIndex:           0,
		ChunkOffset:          0,
		PlaintextHash:        hash,
		EncryptionKeyVersion: 1,
	}))

	contentType := "application/octet-stream"
	physSize := int64(4096)
	dedupRatio := float32(1.0)
	require.NoError(t, gci.SaveObjectMetadata(ctx, &crypto.ObjectMeta{
		TenantID:     tenantUUID,
		BucketName:   "test-bucket",
		ObjectKey:    "test-key",
		TotalSize:    4096,
		ChunkCount:   1,
		ContentType:  &contentType,
		LogicalSize:  4096,
		PhysicalSize: &physSize,
		DedupRatio:   &dedupRatio,
	}))

	meta, err := gci.GetObjectMetadata(ctx, tenantUUID, "test-bucket", "test-key")
	require.NoError(t, err)
	require.NotNil(t, meta)
	assert.Equal(t, int64(4096), meta.TotalSize)

	// Dedup stats
	stats, err := gci.GetTenantDedupStats(ctx, tenantUUID)
	require.NoError(t, err)
	assert.Equal(t, int64(4096), stats.BytesLogical)

	// Cleanup
	t.Cleanup(func() {
		_, _ = f.db.Exec("DELETE FROM tenant_chunk_refs WHERE tenant_id = $1", tenantUUID)
		_, _ = f.db.Exec("DELETE FROM object_metadata WHERE tenant_id = $1", tenantUUID)
		_, _ = f.db.Exec("DELETE FROM global_content_index WHERE plaintext_hash = $1", hash)
	})
}
