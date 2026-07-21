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
	"strings"
	"testing"
	"time"

	"github.com/FairForge/vaultaire/internal/crypto"
	"github.com/FairForge/vaultaire/internal/engine"
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

// putChunkedAs uploads content for an explicit tenant (not just f.tenant), used
// by the cross-tenant dedup test.
func putChunkedAs(t *testing.T, f *adapterTestFixture, tn *tenant.Tenant, bucket, key string, content []byte) {
	t.Helper()
	req := httptest.NewRequest("PUT", "/"+bucket+"/"+key, bytes.NewReader(content))
	req.ContentLength = int64(len(content))
	req = req.WithContext(tenant.WithTenant(req.Context(), tn))
	w := httptest.NewRecorder()
	f.adapter.HandlePut(w, req, bucket, key)
	require.Equal(t, http.StatusOK, w.Code)
}

func getChunkedAs(t *testing.T, f *adapterTestFixture, tn *tenant.Tenant, bucket, key string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", "/"+bucket+"/"+key, nil)
	req = req.WithContext(tenant.WithTenant(req.Context(), tn))
	w := httptest.NewRecorder()
	f.adapter.HandleGet(w, req, bucket, key)
	return w
}

// TestHandleGet_ChunkedObject_MultiChunk forces an object large enough to split
// into multiple chunks (>16 MB max chunk size) and verifies that reassembly is
// byte-identical and in chunk_index order — the core of handleChunkedGet that
// the 8 KB tests (single chunk) never exercise.
func TestHandleGet_ChunkedObject_MultiChunk(t *testing.T) {
	f := setupChunkingFixture(t)

	content := generateTestData(20 * 1024 * 1024) // 20 MB → multiple chunks (16 MB max)
	putETag := putChunkedObject(t, f, "multi.bin", content, "application/octet-stream")

	tenantUUID, err := uuid.Parse(f.tenantID)
	require.NoError(t, err)
	var chunkCount int
	require.NoError(t, f.db.QueryRow(`
		SELECT COUNT(*) FROM tenant_chunk_refs
		WHERE tenant_id = $1 AND bucket_name = $2 AND object_key = $3`,
		tenantUUID, "test-bucket", "multi.bin").Scan(&chunkCount))
	require.GreaterOrEqual(t, chunkCount, 2, "20 MB object must split into multiple chunks for this test to be meaningful")

	gw := getChunkedAs(t, f, f.tenant, "test-bucket", "multi.bin")
	require.Equal(t, http.StatusOK, gw.Code)
	require.Equal(t, content, gw.Body.Bytes(), "multi-chunk reassembly must be byte-identical and in order")
	assert.Equal(t, putETag, gw.Header().Get("ETag"))

	// Range straddling a chunk boundary (~1 MB).
	rangeReq := httptest.NewRequest("GET", "/test-bucket/multi.bin", nil)
	rangeReq.Header.Set("Range", "bytes=1048570-1048580")
	rangeReq = rangeReq.WithContext(tenant.WithTenant(rangeReq.Context(), f.tenant))
	rw := httptest.NewRecorder()
	f.adapter.HandleGet(rw, rangeReq, "test-bucket", "multi.bin")
	require.Equal(t, http.StatusPartialContent, rw.Code)
	assert.Equal(t, content[1048570:1048581], rw.Body.Bytes())
}

// TestHandleGet_ChunkedDedup_CrossBucket verifies that an object which dedups
// against a chunk first stored under a *different bucket* (same tenant) is still
// retrievable. Chunks must live in a shared store, not the per-bucket namespace.
func TestHandleGet_ChunkedDedup_CrossBucket(t *testing.T) {
	f := setupChunkingFixture(t)
	content := generateTestData(8 * 1024)

	bucket2 := "second-bucket"
	require.NoError(t, os.MkdirAll(filepath.Join(f.tempDir, f.tenant.NamespaceContainer(bucket2)), 0755))
	t.Cleanup(func() {
		_, _ = f.db.Exec("DELETE FROM object_head_cache WHERE tenant_id = $1 AND bucket = $2", f.tenantID, bucket2)
		tenantUUID, _ := uuid.Parse(f.tenantID)
		_, _ = f.db.Exec("DELETE FROM tenant_chunk_refs WHERE tenant_id = $1 AND bucket_name = $2", tenantUUID, bucket2)
		_, _ = f.db.Exec("DELETE FROM object_metadata WHERE tenant_id = $1 AND bucket_name = $2", tenantUUID, bucket2)
	})

	putChunkedObject(t, f, "obj1", content, "application/octet-stream") // test-bucket
	putChunkedAs(t, f, f.tenant, bucket2, "obj2", content)              // second-bucket, dedup hit

	gw := getChunkedAs(t, f, f.tenant, bucket2, "obj2")
	require.Equal(t, http.StatusOK, gw.Code, "cross-bucket dedup GET must return the object")
	assert.Equal(t, content, gw.Body.Bytes())
}

// TestHandleGet_ChunkedDedup_CrossTenant verifies the GCI's headline capability:
// global dedup across tenants. Tenant B's object dedups against a chunk first
// uploaded by tenant A, and B can still GET it.
func TestHandleGet_ChunkedDedup_CrossTenant(t *testing.T) {
	f := setupChunkingFixture(t)
	content := generateTestData(8 * 1024)

	// Tenant A (the fixture tenant) uploads first.
	putChunkedObject(t, f, "objA", content, "application/octet-stream")

	// Tenant B shares the same db + engine + data dir.
	bUUID := uuid.New()
	_, err := f.db.Exec(`
		INSERT INTO tenants (id, name, email, access_key, secret_key)
		VALUES ($1, $2, $3, $4, $5) ON CONFLICT (id) DO NOTHING`,
		bUUID.String(), "Chunk Tenant B", "chunk-b@test.local", "AK-"+bUUID.String()[:8], "SK-"+bUUID.String()[:8])
	require.NoError(t, err)
	tB := &tenant.Tenant{ID: bUUID.String(), Namespace: "tenant/" + bUUID.String() + "/"}
	require.NoError(t, os.MkdirAll(filepath.Join(f.tempDir, tB.NamespaceContainer("test-bucket")), 0755))
	t.Cleanup(func() {
		_, _ = f.db.Exec("DELETE FROM tenant_chunk_refs WHERE tenant_id = $1", bUUID)
		_, _ = f.db.Exec("DELETE FROM object_metadata WHERE tenant_id = $1", bUUID)
		_, _ = f.db.Exec("DELETE FROM object_head_cache WHERE tenant_id = $1", bUUID.String())
		_, _ = f.db.Exec("DELETE FROM tenants WHERE id = $1", bUUID.String())
	})

	// Tenant B uploads identical content — must dedup against A's chunk.
	putChunkedAs(t, f, tB, "test-bucket", "objB", content)

	// Confirm a real dedup happened (ref_count == 2 across A + B).
	var maxRef int
	require.NoError(t, f.db.QueryRow(`
		SELECT MAX(gci.ref_count) FROM global_content_index gci
		INNER JOIN tenant_chunk_refs tcr ON tcr.plaintext_hash = gci.plaintext_hash
		WHERE tcr.tenant_id = $1 AND tcr.bucket_name = $2 AND tcr.object_key = $3`,
		bUUID, "test-bucket", "objB").Scan(&maxRef))
	require.Equal(t, 2, maxRef, "tenant B's chunk must be deduplicated against tenant A's (ref_count=2)")

	// Tenant B GETs — must reassemble from the globally-stored chunk.
	gw := getChunkedAs(t, f, tB, "test-bucket", "objB")
	require.Equal(t, http.StatusOK, gw.Code, "cross-tenant dedup GET must return the object")
	assert.Equal(t, content, gw.Body.Bytes())
}

// TestHandleGet_ChunkedObject_MultiChunkRanges exercises range requests that
// span chunk boundaries on a multi-chunk (20 MB) object — the streaming range
// path that selects only overlapping chunks via chunk_offset.
func TestHandleGet_ChunkedObject_MultiChunkRanges(t *testing.T) {
	f := setupChunkingFixture(t)
	content := generateTestData(20 * 1024 * 1024)
	putChunkedObject(t, f, "ranges.bin", content, "application/octet-stream")
	total := len(content)

	cases := []struct {
		name   string
		hdr    string
		lo, hi int // [lo, hi)
	}{
		{"spans-multiple-chunks", "bytes=500000-15000000", 500000, 15000001},
		{"suffix", "bytes=-1048576", total - 1048576, total},
		{"open-ended", "bytes=18000000-", 18000000, total},
		{"single-byte", "bytes=10000000-10000000", 10000000, 10000001},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/test-bucket/ranges.bin", nil)
			req.Header.Set("Range", tc.hdr)
			req = req.WithContext(tenant.WithTenant(req.Context(), f.tenant))
			w := httptest.NewRecorder()
			f.adapter.HandleGet(w, req, "test-bucket", "ranges.bin")

			require.Equal(t, http.StatusPartialContent, w.Code)
			assert.Equal(t, content[tc.lo:tc.hi], w.Body.Bytes(), "range slice must be exact")
			assert.Equal(t, strconv.Itoa(tc.hi-tc.lo), w.Header().Get("Content-Length"))
			assert.Equal(t, fmt.Sprintf("bytes %d-%d/%d", tc.lo, tc.hi-1, total), w.Header().Get("Content-Range"))
		})
	}
}

// TestHandleGet_ChunkedObject_CorruptChunk verifies read-time integrity: if a
// stored chunk's bytes no longer hash to its plaintext_hash, the corrupt data
// must NOT be served (200), and the failure is surfaced as 500 (the object
// exists but is corrupt) rather than falling through to 404.
func TestHandleGet_ChunkedObject_CorruptChunk(t *testing.T) {
	f := setupChunkingFixture(t)
	content := generateTestData(8 * 1024) // single chunk
	putChunkedObject(t, f, "corrupt.bin", content, "application/octet-stream")

	tenantUUID, err := uuid.Parse(f.tenantID)
	require.NoError(t, err)
	var hashHex string
	require.NoError(t, f.db.QueryRow(`
		SELECT plaintext_hash FROM tenant_chunk_refs
		WHERE tenant_id = $1 AND bucket_name = $2 AND object_key = $3
		ORDER BY chunk_index LIMIT 1`,
		tenantUUID, "test-bucket", "corrupt.bin").Scan(&hashHex))

	// Overwrite the chunk's bytes in the shared global container on disk.
	chunkPath := filepath.Join(f.tempDir, "_global", "_chunks", hashHex)
	require.NoError(t, os.WriteFile(chunkPath, []byte("corrupted chunk payload — wrong bytes"), 0600))

	gw := getChunkedAs(t, f, f.tenant, "test-bucket", "corrupt.bin")
	require.Equal(t, http.StatusInternalServerError, gw.Code, "corrupt chunk must not be served as 200")
	assert.NotEqual(t, content, gw.Body.Bytes(), "original/correct bytes must not be served")
}

// TestChunkedOverwrite_FewerChunks_ReplacesManifest verifies that overwriting a
// chunked object with a smaller one fully replaces the manifest — stale
// higher-index chunk refs from the previous version must not linger and corrupt
// the GET.
func TestChunkedOverwrite_FewerChunks_ReplacesManifest(t *testing.T) {
	f := setupChunkingFixture(t)
	key := "overwrite.bin"

	big := generateTestData(20 * 1024 * 1024) // many chunks
	putChunkedObject(t, f, key, big, "application/octet-stream")

	small := generateTestData(8 * 1024) // single chunk, still > 1KB threshold
	putChunkedObject(t, f, key, small, "application/octet-stream")

	gw := getChunkedAs(t, f, f.tenant, "test-bucket", key)
	require.Equal(t, http.StatusOK, gw.Code)
	require.Equal(t, small, gw.Body.Bytes(),
		"overwrite must replace the manifest, not append stale chunks from the previous version")

	// The new manifest must contain exactly the new object's chunk count.
	tenantUUID, err := uuid.Parse(f.tenantID)
	require.NoError(t, err)
	var refCount int
	require.NoError(t, f.db.QueryRow(`
		SELECT COUNT(*) FROM tenant_chunk_refs
		WHERE tenant_id = $1 AND bucket_name = $2 AND object_key = $3`,
		tenantUUID, "test-bucket", key).Scan(&refCount))
	assert.Equal(t, 1, refCount, "8 KB object should leave exactly one chunk ref")
}

// TestChunkedOverwrite_ReleasesOldChunkRefs verifies that overwriting a chunked
// object decrements the previous version's chunk references (no ref-count leak).
func TestChunkedOverwrite_ReleasesOldChunkRefs(t *testing.T) {
	f := setupChunkingFixture(t)
	key := "release.bin"

	contentA := generateTestData(8 * 1024)
	putChunkedObject(t, f, key, contentA, "application/octet-stream")

	tenantUUID, err := uuid.Parse(f.tenantID)
	require.NoError(t, err)
	var oldHash string
	require.NoError(t, f.db.QueryRow(`
		SELECT plaintext_hash FROM tenant_chunk_refs
		WHERE tenant_id = $1 AND bucket_name = $2 AND object_key = $3 ORDER BY chunk_index LIMIT 1`,
		tenantUUID, "test-bucket", key).Scan(&oldHash))

	// Overwrite with different content (different chunk hash).
	contentB := generateTestData(8 * 1024)
	putChunkedObject(t, f, key, contentB, "application/octet-stream")

	// The old version's chunk should have been released (ref_count 0 → marked).
	var refCount int
	var marked bool
	require.NoError(t, f.db.QueryRow(`
		SELECT ref_count, marked_for_deletion FROM global_content_index WHERE plaintext_hash = $1`,
		oldHash).Scan(&refCount, &marked))
	assert.Equal(t, 0, refCount, "overwritten object's old chunk must be released, not leaked")
	assert.True(t, marked, "released chunk (ref_count 0) should be marked_for_deletion")

	// New content still round-trips.
	gw := getChunkedAs(t, f, f.tenant, "test-bucket", key)
	require.Equal(t, http.StatusOK, gw.Code)
	assert.Equal(t, contentB, gw.Body.Bytes())
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
		// Reclaim GCI rows this fixture created: its tenant-scoped (encrypted)
		// rows, plus any now-orphaned rows (global chunks whose only refs were
		// just deleted). Keeps table-wide GC assertions from seeing leaked rows.
		_, _ = f.db.Exec(`DELETE FROM global_content_index g
			WHERE g.dedup_scope = $1
			   OR NOT EXISTS (
				SELECT 1 FROM tenant_chunk_refs r
				WHERE r.dedup_scope = g.dedup_scope AND r.plaintext_hash = g.plaintext_hash)`,
			tenantUUID)
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
	result, err := gci.LookupChunk(ctx, crypto.GlobalDedupScope, hash)
	require.NoError(t, err)
	assert.True(t, result.Exists)
	assert.Equal(t, "local", result.Entry.BackendID)

	// Increment + decrement
	incRows, incErr := gci.IncrementRef(ctx, crypto.GlobalDedupScope, hash)
	require.NoError(t, incErr)
	require.Equal(t, int64(1), incRows)
	newCount, err := gci.DecrementRef(ctx, crypto.GlobalDedupScope, hash)
	require.NoError(t, err)
	assert.Equal(t, 1, newCount)

	// Decrement to 0
	newCount, err = gci.DecrementRef(ctx, crypto.GlobalDedupScope, hash)
	require.NoError(t, err)
	assert.Equal(t, 0, newCount)

	// Verify marked for deletion
	var marked bool
	require.NoError(t, f.db.QueryRow(
		"SELECT marked_for_deletion FROM global_content_index WHERE plaintext_hash = $1",
		hash).Scan(&marked))
	assert.True(t, marked)

	// Tenant chunk ref + object metadata (string tenant IDs since WP-C)
	tenantID := uuid.New().String()
	require.NoError(t, gci.AddTenantChunkRef(ctx, &crypto.TenantChunkRef{
		TenantID:             tenantID,
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
		TenantID:     tenantID,
		BucketName:   "test-bucket",
		ObjectKey:    "test-key",
		TotalSize:    4096,
		ChunkCount:   1,
		ContentType:  &contentType,
		LogicalSize:  4096,
		PhysicalSize: &physSize,
		DedupRatio:   &dedupRatio,
	}))

	meta, err := gci.GetObjectMetadata(ctx, tenantID, "test-bucket", "test-key")
	require.NoError(t, err)
	require.NotNil(t, meta)
	assert.Equal(t, int64(4096), meta.TotalSize)

	// Dedup stats
	stats, err := gci.GetTenantDedupStats(ctx, tenantID)
	require.NoError(t, err)
	assert.Equal(t, int64(4096), stats.BytesLogical)

	// Cleanup
	t.Cleanup(func() {
		_, _ = f.db.Exec("DELETE FROM tenant_chunk_refs WHERE tenant_id = $1", tenantID)
		_, _ = f.db.Exec("DELETE FROM object_metadata WHERE tenant_id = $1", tenantID)
		_, _ = f.db.Exec("DELETE FROM global_content_index WHERE plaintext_hash = $1", hash)
	})
}

// TestHandlePut_ChunkedUpload_LazyReader drives HandlePut with an io.LimitReader
// over a repeating pattern — the TEST never holds the whole object in a single
// buffer. Verifies the streaming upload path doesn't materialise the whole body.
func TestHandlePut_ChunkedUpload_LazyReader(t *testing.T) {
	f := setupChunkingFixture(t)

	// patternReader repeats a small seed indefinitely; wrapping it in
	// io.LimitReader gives a stream of known bytes with no large allocation.
	seed := make([]byte, 4096)
	_, _ = rand.Read(seed)
	objSize := int64(32 * 1024) // 32 KB — well above the 1 KB threshold
	body := io.LimitReader(&patternReader{seed: seed}, objSize)

	req := httptest.NewRequest("PUT", "/test-bucket/lazy.bin", body)
	req.ContentLength = objSize
	req.Header.Set("Content-Type", "application/octet-stream")
	req = req.WithContext(tenant.WithTenant(req.Context(), f.tenant))
	w := httptest.NewRecorder()
	f.adapter.HandlePut(w, req, "test-bucket", "lazy.bin")

	require.Equal(t, http.StatusOK, w.Code, "streaming chunked PUT from lazy reader must succeed")
	assert.NotEmpty(t, w.Header().Get("ETag"))

	var isChunked bool
	require.NoError(t, f.db.QueryRow(`
		SELECT is_chunked FROM object_head_cache
		WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3`,
		f.tenantID, "test-bucket", "lazy.bin").Scan(&isChunked))
	require.True(t, isChunked)

	// GET and verify content hash matches what LimitReader would produce.
	expected := makeLimitedPattern(seed, int(objSize))
	gw := getChunkedAs(t, f, f.tenant, "test-bucket", "lazy.bin")
	require.Equal(t, http.StatusOK, gw.Code)
	assert.Equal(t, expected, gw.Body.Bytes(), "GET must return the exact lazy-reader bytes")
}

// patternReader repeats seed forever — for use with io.LimitReader.
type patternReader struct {
	seed []byte
	pos  int
}

func (r *patternReader) Read(p []byte) (int, error) {
	n := 0
	for n < len(p) {
		copied := copy(p[n:], r.seed[r.pos:])
		n += copied
		r.pos = (r.pos + copied) % len(r.seed)
	}
	return n, nil
}

func makeLimitedPattern(seed []byte, size int) []byte {
	out := make([]byte, size)
	for i := 0; i < size; {
		i += copy(out[i:], seed)
	}
	return out
}

// TestHandlePut_ChunkedUpload_BackendFailureReturns5xx verifies that when the
// backend engine.Put fails mid-stream, HandlePut returns 5xx and does NOT
// create a 0-byte object_head_cache row (the old fallthrough bug).
func TestHandlePut_ChunkedUpload_BackendFailureReturns5xx(t *testing.T) {
	f := setupChunkingFixture(t)

	// Replace the primary driver with one that always fails on Put.
	f.eng.AddDriver("local", &failingPutDriver{})
	f.eng.SetPrimary("local")

	content := generateTestData(8 * 1024)
	req := httptest.NewRequest("PUT", "/test-bucket/fail.bin", bytes.NewReader(content))
	req.ContentLength = int64(len(content))
	req = req.WithContext(tenant.WithTenant(req.Context(), f.tenant))
	w := httptest.NewRecorder()
	f.adapter.HandlePut(w, req, "test-bucket", "fail.bin")

	require.GreaterOrEqual(t, w.Code, 500, "backend failure must yield 5xx, not 200")

	// Must NOT have created a 0-byte or any object_head_cache row.
	var count int
	require.NoError(t, f.db.QueryRow(`
		SELECT COUNT(*) FROM object_head_cache
		WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3`,
		f.tenantID, "test-bucket", "fail.bin").Scan(&count))
	assert.Equal(t, 0, count, "failed chunked upload must not leave a cache row")
}

// failingPutDriver wraps engine.Driver with a Put that always errors.
type failingPutDriver struct{}

func (d *failingPutDriver) Name() string { return "failing" }
func (d *failingPutDriver) Get(_ context.Context, _, _ string) (io.ReadCloser, error) {
	return nil, fmt.Errorf("not implemented")
}
func (d *failingPutDriver) Put(_ context.Context, _, _ string, _ io.Reader, _ ...engine.PutOption) error {
	return fmt.Errorf("injected backend failure")
}
func (d *failingPutDriver) Delete(_ context.Context, _, _ string) error           { return nil }
func (d *failingPutDriver) List(_ context.Context, _, _ string) ([]string, error) { return nil, nil }
func (d *failingPutDriver) Exists(_ context.Context, _, _ string) (bool, error)   { return false, nil }
func (d *failingPutDriver) HealthCheck(_ context.Context) error                   { return nil }

// --- Phase 9: Compression tests ---

func TestHandlePut_CompressesTextChunks(t *testing.T) {
	f := setupChunkingFixture(t)

	// Highly compressible text data above chunking threshold
	content := []byte(strings.Repeat("The quick brown fox jumps over the lazy dog. ", 200))
	putChunkedObject(t, f, "compressible.txt", content, "text/plain")

	tenantUUID, err := uuid.Parse(f.tenantID)
	require.NoError(t, err)

	rows, err := f.db.Query(`
		SELECT g.compressed_size, g.compression_algo, g.size_bytes
		FROM tenant_chunk_refs r
		JOIN global_content_index g ON g.plaintext_hash = r.plaintext_hash
		WHERE r.tenant_id = $1 AND r.bucket_name = $2 AND r.object_key = $3`,
		tenantUUID, "test-bucket", "compressible.txt")
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()

	var found int
	for rows.Next() {
		var compressedSize *int64
		var algo *string
		var sizeBytes int64
		require.NoError(t, rows.Scan(&compressedSize, &algo, &sizeBytes))
		found++
		require.NotNil(t, compressedSize, "compressed_size should be set for compressible text")
		require.NotNil(t, algo, "compression_algo should be set")
		assert.Equal(t, "zstd", *algo)
		assert.Less(t, *compressedSize, sizeBytes, "compressed size should be smaller than plaintext")
	}
	require.NoError(t, rows.Err())
	assert.Greater(t, found, 0, "should have at least one chunk")
}

func TestHandlePut_SkipsCompressionForImages(t *testing.T) {
	f := setupChunkingFixture(t)

	// JPEG magic bytes + random data (incompressible)
	content := make([]byte, 4*1024)
	content[0] = 0xFF
	content[1] = 0xD8
	content[2] = 0xFF
	content[3] = 0xE0
	_, _ = rand.Read(content[4:])
	putChunkedObject(t, f, "photo.jpg", content, "image/jpeg")

	tenantUUID, err := uuid.Parse(f.tenantID)
	require.NoError(t, err)

	rows, err := f.db.Query(`
		SELECT g.compressed_size, g.compression_algo
		FROM tenant_chunk_refs r
		JOIN global_content_index g ON g.plaintext_hash = r.plaintext_hash
		WHERE r.tenant_id = $1 AND r.bucket_name = $2 AND r.object_key = $3`,
		tenantUUID, "test-bucket", "photo.jpg")
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var compressedSize *int64
		var algo *string
		require.NoError(t, rows.Scan(&compressedSize, &algo))
		assert.Nil(t, compressedSize, "compressed_size should be NULL for image content")
		assert.Nil(t, algo, "compression_algo should be NULL for image content")
	}
	require.NoError(t, rows.Err())
}

func TestHandleGet_DecompressesCompressedChunks(t *testing.T) {
	f := setupChunkingFixture(t)

	content := []byte(strings.Repeat("Compression test payload for decompression verification. ", 200))
	putChunkedObject(t, f, "compressed-get.txt", content, "text/plain")

	getReq := httptest.NewRequest("GET", "/test-bucket/compressed-get.txt", nil)
	getReq = getReq.WithContext(tenant.WithTenant(getReq.Context(), f.tenant))
	gw := httptest.NewRecorder()
	f.adapter.HandleGet(gw, getReq, "test-bucket", "compressed-get.txt")

	require.Equal(t, http.StatusOK, gw.Code)
	body, _ := io.ReadAll(gw.Body)
	assert.Equal(t, content, body, "decompressed body must match original plaintext")
}

func TestHandleGet_RangeOnCompressedChunks(t *testing.T) {
	f := setupChunkingFixture(t)

	content := []byte(strings.Repeat("Range request on compressed data works correctly. ", 200))
	putChunkedObject(t, f, "compressed-range.txt", content, "text/plain")

	getReq := httptest.NewRequest("GET", "/test-bucket/compressed-range.txt", nil)
	getReq.Header.Set("Range", "bytes=50-149")
	getReq = getReq.WithContext(tenant.WithTenant(getReq.Context(), f.tenant))
	gw := httptest.NewRecorder()
	f.adapter.HandleGet(gw, getReq, "test-bucket", "compressed-range.txt")

	require.Equal(t, http.StatusPartialContent, gw.Code)
	body, _ := io.ReadAll(gw.Body)
	assert.Equal(t, content[50:150], body, "range slice must match original plaintext range")
	assert.Equal(t, "100", gw.Header().Get("Content-Length"))
}

func TestHandlePut_CompressionExpansionSkipped(t *testing.T) {
	f := setupChunkingFixture(t)

	// Purely random data — compression would expand it
	content := generateTestData(4 * 1024)
	putChunkedObject(t, f, "random.bin", content, "application/octet-stream")

	tenantUUID, err := uuid.Parse(f.tenantID)
	require.NoError(t, err)

	rows, err := f.db.Query(`
		SELECT g.compressed_size, g.compression_algo, g.size_bytes
		FROM tenant_chunk_refs r
		JOIN global_content_index g ON g.plaintext_hash = r.plaintext_hash
		WHERE r.tenant_id = $1 AND r.bucket_name = $2 AND r.object_key = $3`,
		tenantUUID, "test-bucket", "random.bin")
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var compressedSize *int64
		var algo *string
		var sizeBytes int64
		require.NoError(t, rows.Scan(&compressedSize, &algo, &sizeBytes))
		// Random data should NOT be compressed (expansion skipped)
		assert.Nil(t, compressedSize, "compressed_size should be NULL for incompressible data")
		assert.Nil(t, algo, "compression_algo should be NULL for incompressible data")
	}
	require.NoError(t, rows.Err())
}

// setupEncryptedChunkingFixture creates a chunking fixture with per-chunk
// convergent encryption enabled (chunkEncSvc set).
func setupEncryptedChunkingFixture(t *testing.T) *adapterTestFixture {
	t.Helper()
	f := setupChunkingFixture(t)

	masterHex := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	km, err := crypto.NewKeyManager(&crypto.KeyManagerConfig{
		MasterKeyHex:  masterHex,
		CacheMaxAge:   1 * time.Hour,
		EnableCaching: true,
	})
	require.NoError(t, err)
	f.adapter.chunkEncSvc = crypto.NewChunkEncryptionService(km)
	return f
}

// TestChunkedEncryption_SkipsWholeObjectSSE guards the WP-7 fix that makes
// whole-object SSE-S3 and per-chunk convergent encryption mutually exclusive.
//
// Before the fix, a chunk-sized object on an SSE-enabled path was encrypted
// TWICE: SSE-S3 wrapped the whole object (+1117 bytes, and non-determin-
// istically), then the result was chunked and per-chunk encrypted. GET peeled
// only the per-chunk layer and returned SSE ciphertext (silent corruption),
// and the non-deterministic SSE layer defeated chunk dedup entirely.
func TestChunkedEncryption_SkipsWholeObjectSSE(t *testing.T) {
	f := setupChunkingFixture(t) // chunkThreshold = 1024

	masterHex := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	sse, err := crypto.NewSSEService(f.db, masterHex)
	require.NoError(t, err)
	f.adapter.sseService = sse
	km, err := crypto.NewKeyManager(&crypto.KeyManagerConfig{
		MasterKeyHex: masterHex, CacheMaxAge: time.Hour, EnableCaching: true,
	})
	require.NoError(t, err)
	f.adapter.chunkEncSvc = crypto.NewChunkEncryptionService(km)

	content := generateTestData(8 * 1024) // > 1 KB threshold → chunks

	// Explicit SSE-S3 header: this is exactly the case that used to double-encrypt.
	req := httptest.NewRequest("PUT", "/test-bucket/sse-and-chunk.bin", bytes.NewReader(content))
	req.ContentLength = int64(len(content))
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("x-amz-server-side-encryption", "AES256")
	req = req.WithContext(tenant.WithTenant(req.Context(), f.tenant))
	w := httptest.NewRecorder()
	f.adapter.HandlePut(w, req, "test-bucket", "sse-and-chunk.bin")
	require.Equal(t, http.StatusOK, w.Code)

	// Stored via the chunked per-chunk path, NOT whole-object SSE-S3.
	var isChunked bool
	var encAlgo string
	var storedSize int64
	require.NoError(t, f.db.QueryRow(`
		SELECT is_chunked, encryption_algorithm, size_bytes FROM object_head_cache
		WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3`,
		f.tenantID, "test-bucket", "sse-and-chunk.bin").Scan(&isChunked, &encAlgo, &storedSize))
	assert.True(t, isChunked, "large SSE object must take the chunked path")
	assert.Equal(t, "AES256-CE", encAlgo, "encryption must be per-chunk, not whole-object SSE-S3")
	assert.Equal(t, int64(len(content)), storedSize,
		"size must be plaintext size — a +1117 delta means SSE-S3 also wrapped it")

	// The decisive check: GET must return the original plaintext, not ciphertext.
	getReq := httptest.NewRequest("GET", "/test-bucket/sse-and-chunk.bin", nil)
	getReq = getReq.WithContext(tenant.WithTenant(getReq.Context(), f.tenant))
	gw := httptest.NewRecorder()
	f.adapter.HandleGet(gw, getReq, "test-bucket", "sse-and-chunk.bin")
	require.Equal(t, http.StatusOK, gw.Code)
	body, _ := io.ReadAll(gw.Body)
	assert.Equal(t, content, body, "round-trip must yield the original plaintext (no double-encryption)")
}

func TestHandlePut_ChunkedWithEncryption(t *testing.T) {
	f := setupEncryptedChunkingFixture(t)

	content := generateTestData(8 * 1024) // 8 KB — above 1 KB threshold
	req := httptest.NewRequest("PUT", "/test-bucket/encrypted-chunked.bin", bytes.NewReader(content))
	req.ContentLength = int64(len(content))
	req.Header.Set("Content-Type", "application/octet-stream")
	req = req.WithContext(tenant.WithTenant(req.Context(), f.tenant))

	w := httptest.NewRecorder()
	f.adapter.HandlePut(w, req, "test-bucket", "encrypted-chunked.bin")
	require.Equal(t, http.StatusOK, w.Code)
	assert.NotEmpty(t, w.Header().Get("ETag"))

	// Verify is_chunked = TRUE and encryption_algorithm = 'AES256-CE'
	var isChunked bool
	var encAlgo string
	err := f.db.QueryRow(`
		SELECT is_chunked, encryption_algorithm FROM object_head_cache
		WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3`,
		f.tenantID, "test-bucket", "encrypted-chunked.bin").Scan(&isChunked, &encAlgo)
	require.NoError(t, err)
	assert.True(t, isChunked)
	assert.Equal(t, "AES256-CE", encAlgo)

	// Verify GCI entries are marked encrypted
	tenantUUID, _ := uuid.Parse(f.tenantID)
	rows, err := f.db.Query(`
		SELECT gci.encrypted, gci.encryption_algo FROM global_content_index gci
		INNER JOIN tenant_chunk_refs tcr ON tcr.plaintext_hash = gci.plaintext_hash
		WHERE tcr.tenant_id = $1 AND tcr.bucket_name = $2 AND tcr.object_key = $3`,
		tenantUUID, "test-bucket", "encrypted-chunked.bin")
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var encrypted bool
		var algo *string
		require.NoError(t, rows.Scan(&encrypted, &algo))
		assert.True(t, encrypted, "GCI entry must be marked encrypted")
		require.NotNil(t, algo)
		assert.Equal(t, "AES256-CE", *algo)
	}
	require.NoError(t, rows.Err())

	// Verify tenant_chunk_refs have ciphertext_hash set
	var refCount int
	err = f.db.QueryRow(`
		SELECT COUNT(*) FROM tenant_chunk_refs
		WHERE tenant_id = $1 AND bucket_name = $2 AND object_key = $3 AND ciphertext_hash IS NOT NULL`,
		tenantUUID, "test-bucket", "encrypted-chunked.bin").Scan(&refCount)
	require.NoError(t, err)
	assert.Greater(t, refCount, 0, "all chunk refs should have ciphertext_hash")

	// Verify raw stored data is NOT plaintext
	refs, err := f.adapter.gci.GetObjectChunks(context.Background(), f.tenantID, "test-bucket", "encrypted-chunked.bin")
	require.NoError(t, err)
	require.NotEmpty(t, refs)

	// Encrypted chunks live in the tenant's dedup scope, not the global one.
	require.Equal(t, f.tenantID, refs[0].DedupScope, "encrypted chunk must be tenant-scoped")
	lookup, err := f.adapter.gci.LookupChunk(context.Background(), refs[0].DedupScope, refs[0].PlaintextHash)
	require.NoError(t, err)
	require.NotNil(t, lookup.Entry)

	raw, err := f.eng.Get(context.Background(), "_global", lookup.Entry.StorageKey)
	require.NoError(t, err)
	rawBytes, _ := io.ReadAll(raw)
	_ = raw.Close()
	assert.NotEqual(t, content, rawBytes[:len(content)], "stored data must be encrypted, not plaintext")
}

func TestHandleGet_ChunkedEncryptedRoundTrip(t *testing.T) {
	f := setupEncryptedChunkingFixture(t)

	content := generateTestData(8 * 1024)
	putETag := putChunkedObject(t, f, "enc-roundtrip.bin", content, "application/octet-stream")

	getReq := httptest.NewRequest("GET", "/test-bucket/enc-roundtrip.bin", nil)
	getReq = getReq.WithContext(tenant.WithTenant(getReq.Context(), f.tenant))
	gw := httptest.NewRecorder()
	f.adapter.HandleGet(gw, getReq, "test-bucket", "enc-roundtrip.bin")

	require.Equal(t, http.StatusOK, gw.Code)
	body, _ := io.ReadAll(gw.Body)
	assert.Equal(t, content, body, "decrypted body must match original plaintext")
	assert.Equal(t, putETag, gw.Header().Get("ETag"))
}

func TestHandleGet_EncryptedCompressedRoundTrip(t *testing.T) {
	f := setupEncryptedChunkingFixture(t)

	// Use compressible data (repeating pattern) to exercise compress+encrypt path
	content := bytes.Repeat([]byte("compressible data pattern! "), 400)
	putETag := putChunkedObject(t, f, "comp-enc.bin", content, "text/plain")

	getReq := httptest.NewRequest("GET", "/test-bucket/comp-enc.bin", nil)
	getReq = getReq.WithContext(tenant.WithTenant(getReq.Context(), f.tenant))
	gw := httptest.NewRecorder()
	f.adapter.HandleGet(gw, getReq, "test-bucket", "comp-enc.bin")

	require.Equal(t, http.StatusOK, gw.Code)
	body, _ := io.ReadAll(gw.Body)
	assert.Equal(t, content, body, "decrypt+decompress must yield original content")
	assert.Equal(t, putETag, gw.Header().Get("ETag"))

	// Verify that compression was applied (stored size < plaintext)
	tenantUUID, _ := uuid.Parse(f.tenantID)
	var physicalSize *int64
	_ = f.db.QueryRow(`
		SELECT physical_size FROM object_metadata
		WHERE tenant_id = $1 AND bucket_name = $2 AND object_key = $3`,
		tenantUUID, "test-bucket", "comp-enc.bin").Scan(&physicalSize)
	// Physical includes encryption overhead but compression should still reduce size
	// for highly compressible data
	if physicalSize != nil {
		assert.Less(t, *physicalSize, int64(len(content)),
			"compressed+encrypted size should still be less than plaintext for compressible data")
	}
}

func TestHandlePut_ChunkedEncryptedDedup(t *testing.T) {
	f := setupEncryptedChunkingFixture(t)

	content := generateTestData(8 * 1024)

	// Upload same content twice under different keys
	putChunkedObject(t, f, "enc-dedup-1.bin", content, "application/octet-stream")
	putChunkedObject(t, f, "enc-dedup-2.bin", content, "application/octet-stream")

	// Verify dedup: ref_count should be 2 for all chunks (same tenant → same ciphertext)
	tenantUUID, _ := uuid.Parse(f.tenantID)
	rows, err := f.db.Query(`
		SELECT gci.ref_count FROM global_content_index gci
		INNER JOIN tenant_chunk_refs tcr ON tcr.plaintext_hash = gci.plaintext_hash
		WHERE tcr.tenant_id = $1 AND tcr.bucket_name = $2 AND tcr.object_key = $3`,
		tenantUUID, "test-bucket", "enc-dedup-1.bin")
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var refCount int
		require.NoError(t, rows.Scan(&refCount))
		assert.Equal(t, 2, refCount, "same-tenant dedup must increment ref_count")
	}
	require.NoError(t, rows.Err())

	// Both objects must be retrievable
	for _, key := range []string{"enc-dedup-1.bin", "enc-dedup-2.bin"} {
		getReq := httptest.NewRequest("GET", "/test-bucket/"+key, nil)
		getReq = getReq.WithContext(tenant.WithTenant(getReq.Context(), f.tenant))
		gw := httptest.NewRecorder()
		f.adapter.HandleGet(gw, getReq, "test-bucket", key)
		require.Equal(t, http.StatusOK, gw.Code)
		body, _ := io.ReadAll(gw.Body)
		assert.Equal(t, content, body, "deduped encrypted object %s must be retrievable", key)
	}
}

// A chunk's stored blob is compressed (or not) based on the FIRST upload's
// Content-Type. A later dedup hit on the same chunk must record the ciphertext
// hash of that stored blob — not one recomputed under the current request's
// Content-Type, which can differ and make the second object unreadable.
func TestHandleGet_EncryptedDedup_DifferentContentType(t *testing.T) {
	f := setupEncryptedChunkingFixture(t)

	// Compressible content: text/plain compresses on first store;
	// application/pdf is on the compression skip-list, so a recomputed hash
	// would describe a blob that was never stored.
	content := bytes.Repeat([]byte("dedup across content types! "), 400)
	putChunkedObject(t, f, "ct-first.txt", content, "text/plain")
	putChunkedObject(t, f, "ct-second.pdf", content, "application/pdf")

	for _, key := range []string{"ct-first.txt", "ct-second.pdf"} {
		getReq := httptest.NewRequest("GET", "/test-bucket/"+key, nil)
		getReq = getReq.WithContext(tenant.WithTenant(getReq.Context(), f.tenant))
		gw := httptest.NewRecorder()
		f.adapter.HandleGet(gw, getReq, "test-bucket", key)
		require.Equal(t, http.StatusOK, gw.Code,
			"deduped object %s must be readable regardless of upload Content-Type", key)
		body, _ := io.ReadAll(gw.Body)
		assert.Equal(t, content, body, "object %s must round-trip", key)
	}
}
