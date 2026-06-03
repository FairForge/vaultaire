package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/FairForge/vaultaire/internal/crypto"
	"github.com/FairForge/vaultaire/internal/tenant"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	_ "github.com/lib/pq"
)

func setupChunkingFixture(t *testing.T) *adapterTestFixture {
	t.Helper()
	f := setupAdapterFixture(t)

	f.adapter.gci = crypto.NewGlobalContentIndex(f.db)
	// Use a low threshold (1 KB) so tests don't need 64 MB of data.
	f.adapter.chunkingThreshold = 1024

	t.Cleanup(func() {
		_, _ = f.db.Exec("DELETE FROM tenant_chunk_refs WHERE tenant_id = $1::text::uuid", f.tenantID)
		_, _ = f.db.Exec("DELETE FROM object_metadata WHERE tenant_id = $1::text::uuid", f.tenantID)
		_, _ = f.db.Exec("DELETE FROM global_content_index WHERE plaintext_hash LIKE 'test_%' OR plaintext_hash IN (SELECT plaintext_hash FROM tenant_chunk_refs WHERE tenant_id = $1::text::uuid)", f.tenantID)
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
