package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/FairForge/vaultaire/internal/crypto"
	"github.com/FairForge/vaultaire/internal/flags"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// markBucketSSE flags the fixture's test bucket as SSE-by-default (the state
// every new bucket is in once a master key exists) and cleans up after.
func markBucketSSE(t *testing.T, f *adapterTestFixture) {
	t.Helper()
	_, err := f.db.Exec(`INSERT INTO buckets (tenant_id, name, sse_enabled)
		VALUES ($1,$2,TRUE)
		ON CONFLICT (tenant_id, name) DO UPDATE SET sse_enabled = TRUE`,
		f.tenantID, "test-bucket")
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = f.db.Exec(`DELETE FROM buckets WHERE tenant_id=$1 AND name=$2`, f.tenantID, "test-bucket")
		_, _ = f.db.Exec(`DELETE FROM tenant_encryption_keys WHERE tenant_id=$1`, f.tenantID)
	})
}

func headRowEncryption(t *testing.T, f *adapterTestFixture, key string) (algo string, chunked bool) {
	t.Helper()
	require.NoError(t, f.db.QueryRow(`
		SELECT COALESCE(encryption_algorithm,''), is_chunked FROM object_head_cache
		WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3`,
		f.tenantID, "test-bucket", key).Scan(&algo, &chunked))
	return algo, chunked
}

// R8-02: two first-stores of the same (scope, hash) both miss the lookup and
// both encode their chunk BEFORE taking the advisory lock. The second one must
// notice the row the first committed and take a reference on it — never
// overwrite the blob with bytes the row's compression/ciphertext metadata does
// not describe (which made every object sharing the chunk unreadable, and with
// encryption on produced the R8-01 nonce-reuse pair).
func TestStoreChunkLocked_ReusesExistingRowUnderLock(t *testing.T) {
	f := setupChunkingFixture(t)
	ctx := context.Background()
	scope := f.tenantID
	chunk := []byte("shared chunk stored twice concurrently")
	sum := sha256.Sum256(chunk)
	hash := hex.EncodeToString(sum[:])
	storageKey := "_chunks/" + scope + "/" + hash

	firstCT := "1111111111111111111111111111111111111111111111111111111111111111"
	require.NoError(t, f.adapter.gci.InsertChunk(ctx, &crypto.GCIEntry{
		DedupScope: scope, PlaintextHash: hash, BackendID: "local", StorageKey: storageKey,
		SizeBytes: int64(len(chunk)), Encrypted: true, CiphertextHash: &firstCT, RefCount: 1,
	}))

	secondCT := "2222222222222222222222222222222222222222222222222222222222222222"
	res, err := f.adapter.storeChunkLocked(ctx, scope, storageKey, []byte("second writer's different encoding"),
		&crypto.GCIEntry{
			DedupScope: scope, PlaintextHash: hash, StorageKey: storageKey,
			SizeBytes: int64(len(chunk)), Encrypted: true, CiphertextHash: &secondCT, RefCount: 1,
		})
	require.NoError(t, err)

	assert.True(t, res.reused, "the second writer must take a reference, not store")
	assert.Equal(t, "local", res.backend)
	require.NotNil(t, res.ciphertextHash)
	assert.Equal(t, firstCT, *res.ciphertextHash, "the manifest must carry the STORED blob's hash")

	_, statErr := os.Stat(filepath.Join(f.tempDir, chunkContainer, storageKey))
	assert.True(t, os.IsNotExist(statErr), "the second writer must not write (overwrite) the blob")

	var refCount int
	var rowCT string
	require.NoError(t, f.db.QueryRow(`SELECT ref_count, ciphertext_hash FROM global_content_index
		WHERE dedup_scope = $1 AND plaintext_hash = $2`, scope, hash).Scan(&refCount, &rowCT))
	assert.Equal(t, 2, refCount)
	assert.Equal(t, firstCT, rowCT, "row metadata stays the first writer's")
}

// R8-03: with the `chunking` kill-switch off, an SSE-by-default bucket must
// still encrypt an object above the chunk threshold — whole-object SSE-S3 —
// instead of skipping SSE "because the chunk path will encrypt it" and then
// not chunking either (live: 70 MiB stored as plaintext).
func TestHandlePut_ChunkingFlagOff_SSEBucketStillEncrypts(t *testing.T) {
	f := setupEncryptedChunkingFixture(t)
	svc := withChunkingFlag(t, f)
	sse, err := crypto.NewSSEService(f.db, testSSEMasterKey)
	require.NoError(t, err)
	f.adapter.sseService = sse
	markBucketSSE(t, f)

	// Sanity: flag on → chunked + per-chunk encryption, no whole-object SSE.
	putChunkedObject(t, f, "flag-on.bin", generateTestData(8*1024), "application/octet-stream")
	algo, chunked := headRowEncryption(t, f, "flag-on.bin")
	require.True(t, chunked)
	require.Equal(t, "AES256-CE", algo)

	require.NoError(t, svc.Set(context.Background(), flagChunking, flags.GlobalTenant, false, "test"))
	content := generateTestData(8 * 1024)
	req := httptest.NewRequest("PUT", "/test-bucket/flag-off.bin", bytes.NewReader(content))
	req.ContentLength = int64(len(content))
	req = req.WithContext(s3Ctx(req.Context(), f.tenant))
	w := httptest.NewRecorder()
	f.adapter.HandlePut(w, req, "test-bucket", "flag-off.bin")
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "AES256", w.Header().Get("x-amz-server-side-encryption"))

	algo, chunked = headRowEncryption(t, f, "flag-off.bin")
	assert.False(t, chunked)
	assert.Equal(t, crypto.SSEAlgorithm, algo, "flag off must fall back to whole-object SSE-S3, never plaintext")

	gw := getChunkedObject(t, f, "flag-off.bin")
	require.Equal(t, http.StatusOK, gw.Code)
	body, _ := io.ReadAll(gw.Body)
	assert.Equal(t, content, body)
}

// R8-03 (b): flag off + SSE bucket + object above the whole-object SSE cap →
// the encryption-required guard must fire (413), exactly as when no chunk
// encryption service exists at all.
func TestHandlePut_ChunkingFlagOff_SSEBucketOversize_Rejected(t *testing.T) {
	f := setupEncryptedChunkingFixture(t)
	svc := withChunkingFlag(t, f)
	sse, err := crypto.NewSSEService(f.db, testSSEMasterKey)
	require.NoError(t, err)
	f.adapter.sseService = sse
	markBucketSSE(t, f)

	// Flag on: the chunk path takes oversize objects (no 413 from the guard).
	w := oversizePut(t, f, "big-flag-on.bin", nil)
	assert.NotEqual(t, http.StatusRequestEntityTooLarge, w.Code,
		"with chunk encryption available the guard must not fire")

	require.NoError(t, svc.Set(context.Background(), flagChunking, flags.GlobalTenant, false, "test"))
	w = oversizePut(t, f, "big-flag-off.bin", nil)
	assert.Equal(t, http.StatusRequestEntityTooLarge, w.Code,
		"flag off: an oversize object that cannot be encrypted must be rejected, not stored plaintext")
}

// R8-04: a GET of an SSE-S3 object on a process with no SSE service (master
// key unset/mistyped) must fail, never stream the ciphertext as a 200 with the
// plaintext Content-Length (live: 41,943,040 bytes of garbage, status 200).
func TestHandleGet_SSEObjectWithoutService_Fails(t *testing.T) {
	f := setupAdapterFixture(t)
	sse, err := crypto.NewSSEService(f.db, testSSEMasterKey)
	require.NoError(t, err)
	f.adapter.sseService = sse
	t.Cleanup(func() {
		_, _ = f.db.Exec(`DELETE FROM tenant_encryption_keys WHERE tenant_id=$1`, f.tenantID)
	})

	plaintext := []byte("customer bytes that must never come back as ciphertext")
	req := httptest.NewRequest("PUT", "/test-bucket/sse.bin", bytes.NewReader(plaintext))
	req.ContentLength = int64(len(plaintext))
	req.Header.Set("x-amz-server-side-encryption", "AES256")
	req = req.WithContext(s3Ctx(req.Context(), f.tenant))
	w := httptest.NewRecorder()
	f.adapter.HandlePut(w, req, "test-bucket", "sse.bin")
	require.Equal(t, http.StatusOK, w.Code)
	algo, _ := headRowEncryption(t, f, "sse.bin")
	require.Equal(t, crypto.SSEAlgorithm, algo)

	// The key is gone from this process.
	f.adapter.sseService = nil

	gw := getChunkedObject(t, f, "sse.bin")
	assert.Equal(t, http.StatusInternalServerError, gw.Code)
	body, _ := io.ReadAll(gw.Body)
	assert.NotEqual(t, plaintext, body)
	assert.False(t, bytes.HasPrefix(body, []byte{0x01}), "the SSE-S3 blob (version byte 0x01) must not be served")
}

// R8-07 (R2-16): on a versioning-enabled bucket the delete-marker branch
// removed the head row of a chunked object and never released its manifest —
// refs and GCI counts leaked forever, the chunks unsweepable.
func TestHandleDelete_DeleteMarkerReleasesChunkedManifest(t *testing.T) {
	f := setupChunkingFixture(t)
	ctx := context.Background()

	putChunkedObject(t, f, "versioned-later.bin", generateTestData(8*1024), "application/octet-stream")
	before := tenantChunks(t, f.db, f.tenantID, "versioned-later.bin")
	require.NotEmpty(t, before)

	// Versioning enabled AFTER the object was chunked (chunking is gated off
	// on versioned buckets, so this is the only way a chunked object meets
	// the marker branch).
	_, err := f.db.Exec(`INSERT INTO buckets (tenant_id, name, versioning_status)
		VALUES ($1,$2,'Enabled')
		ON CONFLICT (tenant_id, name) DO UPDATE SET versioning_status = 'Enabled'`,
		f.tenantID, "test-bucket")
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = f.db.Exec(`DELETE FROM object_versions WHERE tenant_id=$1`, f.tenantID)
		_, _ = f.db.Exec(`DELETE FROM buckets WHERE tenant_id=$1 AND name=$2`, f.tenantID, "test-bucket")
	})

	req := httptest.NewRequest("DELETE", "/test-bucket/versioned-later.bin", nil)
	req = req.WithContext(s3Ctx(req.Context(), f.tenant))
	w := httptest.NewRecorder()
	f.adapter.HandleDelete(w, req, "test-bucket", "versioned-later.bin")
	require.Equal(t, http.StatusNoContent, w.Code)
	require.Equal(t, "true", w.Header().Get("x-amz-delete-marker"))

	assert.Empty(t, tenantChunks(t, f.db, f.tenantID, "versioned-later.bin"),
		"the delete marker must release the chunked manifest")
	for _, c := range before {
		var refCount int
		var marked bool
		require.NoError(t, f.db.QueryRow(`SELECT ref_count, marked_for_deletion FROM global_content_index
			WHERE dedup_scope = $1 AND plaintext_hash = $2`, c.scope, c.hash).Scan(&refCount, &marked))
		assert.Equal(t, 0, refCount, "chunk %s must have its reference released", c.hash[:8])
		assert.True(t, marked)
	}
	var metaRows int
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM object_metadata
		WHERE tenant_id = $1 AND object_key = $2`, f.tenantID, "versioned-later.bin").Scan(&metaRows))
	assert.Equal(t, 0, metaRows)
	_ = ctx
}

// R8-08: the plain chunked DELETE released the manifest in one transaction and
// removed the head row in another; both must go together (a crash between
// them left a chunked head row with no manifest: HEAD 200, GET 500, billed).
// Observable contract: after DELETE nothing of the object remains.
func TestHandleDelete_ChunkedReleasesManifestAndHeadRow(t *testing.T) {
	f := setupChunkingFixture(t)
	putChunkedObject(t, f, "gone.bin", generateTestData(8*1024), "application/octet-stream")
	before := tenantChunks(t, f.db, f.tenantID, "gone.bin")
	require.NotEmpty(t, before)

	deleteChunkedObject(t, f, "gone.bin")

	assert.Empty(t, tenantChunks(t, f.db, f.tenantID, "gone.bin"))
	var headRows int
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM object_head_cache
		WHERE tenant_id = $1 AND object_key = $2`, f.tenantID, "gone.bin").Scan(&headRows))
	assert.Equal(t, 0, headRows)
	for _, c := range before {
		var refCount int
		require.NoError(t, f.db.QueryRow(`SELECT ref_count FROM global_content_index
			WHERE dedup_scope = $1 AND plaintext_hash = $2`, c.scope, c.hash).Scan(&refCount))
		assert.Equal(t, 0, refCount)
	}
	gw := getChunkedObject(t, f, "gone.bin")
	assert.Equal(t, http.StatusNotFound, gw.Code)
}

// R8-20 (R2-20): the displaced head row's backend is what the stale
// whole-object blob delete after a chunked PUT has to hint; atomicHeadUpsert
// must return it.
func TestAtomicHeadUpsert_ReturnsDisplacedBackend(t *testing.T) {
	f := setupAdapterFixture(t)
	ctx := context.Background()
	_, err := f.db.Exec(`INSERT INTO object_head_cache
		(tenant_id, bucket, object_key, size_bytes, etag, content_type, backend_name, floor, updated_at)
		VALUES ($1, 'test-bucket', 'displaced.bin', 123, 'e', 'text/plain', 'lyve', 'standard', NOW())`, f.tenantID)
	require.NoError(t, err)

	displaced, err := atomicHeadUpsert(ctx, f.db, f.tenantID, "test-bucket", "displaced.bin", func(*sql.Tx) error { return nil })
	require.NoError(t, err)
	assert.Equal(t, int64(123), displaced.Size)
	assert.Equal(t, "lyve", displaced.Backend)
}
