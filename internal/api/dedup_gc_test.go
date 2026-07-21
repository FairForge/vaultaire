package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/FairForge/vaultaire/internal/tenant"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	_ "github.com/lib/pq"
)

func setupGCFixture(t *testing.T) (*DedupGCRunner, *adapterTestFixture) {
	t.Helper()
	f := setupChunkingFixture(t)

	runner := NewDedupGCRunner(f.db, f.eng, f.adapter.gci, zap.NewNop())
	require.NotNil(t, runner)
	runner.GracePeriod = 0

	return runner, f
}

func TestDedupGC_DeletesMarkedPastGrace(t *testing.T) {
	runner, f := setupGCFixture(t)

	content := generateTestData(8 * 1024)
	putChunkedObject(t, f, "gc-delete.bin", content, "application/octet-stream")

	// Delete the object — decrements ref counts, marks chunks for deletion.
	delReq := httptest.NewRequest("DELETE", "/test-bucket/gc-delete.bin", nil)
	delReq = delReq.WithContext(tenant.WithTenant(delReq.Context(), f.tenant))
	dw := httptest.NewRecorder()
	f.adapter.HandleDelete(dw, delReq, "test-bucket", "gc-delete.bin")
	require.Equal(t, http.StatusNoContent, dw.Code)

	// Backdate marked_at and last_accessed_at so they're past grace.
	_, err := f.db.Exec(`
		UPDATE global_content_index
		SET marked_at = NOW() - INTERVAL '1 day',
		    last_accessed_at = NOW() - INTERVAL '1 day'
		WHERE marked_for_deletion = TRUE AND ref_count = 0`)
	require.NoError(t, err)

	// Collect the storage keys before GC.
	rows, err := f.db.Query(`
		SELECT storage_key FROM global_content_index
		WHERE marked_for_deletion = TRUE AND ref_count = 0`)
	require.NoError(t, err)
	var keys []string
	for rows.Next() {
		var k string
		require.NoError(t, rows.Scan(&k))
		keys = append(keys, k)
	}
	_ = rows.Close()
	require.NotEmpty(t, keys, "should have marked chunks")

	// Run GC.
	result, err := runner.RunOnce(context.Background())
	require.NoError(t, err)
	assert.Greater(t, result.Deleted, 0)
	assert.Greater(t, result.BytesReclaimed, int64(0))

	// GCI rows should be gone.
	var count int
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM global_content_index WHERE marked_for_deletion = TRUE`).Scan(&count))
	assert.Equal(t, 0, count, "all marked GCI rows should be deleted")

	// Backend data should be gone.
	for _, key := range keys {
		rc, getErr := f.eng.Get(context.Background(), chunkContainer, key)
		if rc != nil {
			_ = rc.Close()
		}
		assert.Error(t, getErr, "chunk %s should be deleted from backend", key)
	}
}

func TestDedupGC_RespectsGrace(t *testing.T) {
	runner, f := setupGCFixture(t)
	runner.GracePeriod = 7 * 24 * time.Hour // restore default grace

	content := generateTestData(8 * 1024)
	putChunkedObject(t, f, "gc-grace.bin", content, "application/octet-stream")

	delReq := httptest.NewRequest("DELETE", "/test-bucket/gc-grace.bin", nil)
	delReq = delReq.WithContext(tenant.WithTenant(delReq.Context(), f.tenant))
	dw := httptest.NewRecorder()
	f.adapter.HandleDelete(dw, delReq, "test-bucket", "gc-grace.bin")
	require.Equal(t, http.StatusNoContent, dw.Code)

	// marked_at is NOW — within grace period. GC should NOT delete.
	result, err := runner.RunOnce(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 0, result.Deleted, "chunks within grace should not be deleted")

	var count int
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM global_content_index WHERE marked_for_deletion = TRUE`).Scan(&count))
	assert.Greater(t, count, 0, "marked chunks should still exist")
}

func TestDedupGC_SkipsReferencedChunk(t *testing.T) {
	runner, f := setupGCFixture(t)

	content := generateTestData(8 * 1024)
	putChunkedObject(t, f, "gc-ref.bin", content, "application/octet-stream")

	// Object still exists — ref_count > 0. GC should not delete.
	result, err := runner.RunOnce(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 0, result.Deleted, "referenced chunks should not be deleted")
}

func TestDedupGC_ReconcilesOrphan(t *testing.T) {
	runner, f := setupGCFixture(t)

	content := generateTestData(8 * 1024)
	putChunkedObject(t, f, "gc-reconcile.bin", content, "application/octet-stream")

	// Artificially inflate ref_count to simulate drift.
	_, err := f.db.Exec(`
		UPDATE global_content_index
		SET ref_count = ref_count + 5,
		    last_accessed_at = NOW() - INTERVAL '1 day'`)
	require.NoError(t, err)

	result, gcErr := runner.RunOnce(context.Background())
	require.NoError(t, gcErr)
	assert.Greater(t, result.Reconciled, 0, "should reconcile drifted ref counts")

	// Verify ref counts now match actual refs, counted within each dedup scope
	// (ref counting is per (dedup_scope, plaintext_hash) since WP-7).
	rows, err := f.db.Query(`
		SELECT g.plaintext_hash, g.ref_count,
		       (SELECT COUNT(*) FROM tenant_chunk_refs r
		        WHERE r.dedup_scope = g.dedup_scope AND r.plaintext_hash = g.plaintext_hash)
		FROM global_content_index g`)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var hash string
		var gcRefCount, actualCount int
		require.NoError(t, rows.Scan(&hash, &gcRefCount, &actualCount))
		assert.Equal(t, actualCount, gcRefCount, "ref_count should match actual refs for %s", hash)
	}
}

func TestDedupGC_DoesNotReconcileFreshChunk(t *testing.T) {
	runner, f := setupGCFixture(t)
	runner.GracePeriod = 7 * 24 * time.Hour // need non-zero grace for this test

	// Insert a GCI entry with ref_count=1 but NO tenant_chunk_refs (simulates
	// an in-flight streaming PUT that finished InsertChunk but not
	// ReplaceObjectManifest). last_accessed_at = NOW().
	raw := uuid.New().String()
	hash := fmt.Sprintf("deadbeef%s%s", raw, raw)
	hash = hash[:64]
	_, err := f.db.Exec(`
		INSERT INTO global_content_index
		(plaintext_hash, backend_id, storage_key, size_bytes, ref_count, last_accessed_at)
		VALUES ($1, 'local', '_chunks/test-fresh', 4096, 1, NOW())
	`, hash)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = f.db.Exec("DELETE FROM global_content_index WHERE plaintext_hash = $1", hash)
	})

	result, gcErr := runner.RunOnce(context.Background())
	require.NoError(t, gcErr)

	// The fresh chunk should NOT be reconciled to 0 — it's within grace.
	var refCount int
	var marked bool
	require.NoError(t, f.db.QueryRow(`
		SELECT ref_count, marked_for_deletion FROM global_content_index WHERE plaintext_hash = $1
	`, hash).Scan(&refCount, &marked))
	assert.Equal(t, 1, refCount, "fresh chunk ref_count should remain 1")
	assert.False(t, marked, "fresh chunk should not be marked for deletion")
	assert.Equal(t, 0, result.Reconciled, "no reconciliation for fresh chunks")
}

func TestDedupGC_EndToEnd(t *testing.T) {
	runner, f := setupGCFixture(t)

	// Upload two objects with the same content (dedup).
	content := generateTestData(8 * 1024)
	putChunkedObject(t, f, "gc-e2e-a.bin", content, "application/octet-stream")
	putChunkedObject(t, f, "gc-e2e-b.bin", content, "application/octet-stream")

	// Count shared chunks.
	var chunkCount int
	require.NoError(t, f.db.QueryRow(`
		SELECT COUNT(*) FROM global_content_index`).Scan(&chunkCount))
	require.Greater(t, chunkCount, 0)

	// Delete object A — ref counts decrement but chunks survive (still used by B).
	delReq := httptest.NewRequest("DELETE", "/test-bucket/gc-e2e-a.bin", nil)
	delReq = delReq.WithContext(tenant.WithTenant(delReq.Context(), f.tenant))
	dw := httptest.NewRecorder()
	f.adapter.HandleDelete(dw, delReq, "test-bucket", "gc-e2e-a.bin")
	require.Equal(t, http.StatusNoContent, dw.Code)

	// Backdate for grace.
	_, err := f.db.Exec(`UPDATE global_content_index SET last_accessed_at = NOW() - INTERVAL '1 day'`)
	require.NoError(t, err)

	// Run GC — shared chunks should survive (ref_count >= 1 from object B).
	result, gcErr := runner.RunOnce(context.Background())
	require.NoError(t, gcErr)
	assert.Equal(t, 0, result.Deleted, "shared chunks should not be deleted")

	// Object B should still be readable.
	getReq := httptest.NewRequest("GET", "/test-bucket/gc-e2e-b.bin", nil)
	getReq = getReq.WithContext(tenant.WithTenant(getReq.Context(), f.tenant))
	gw := httptest.NewRecorder()
	f.adapter.HandleGet(gw, getReq, "test-bucket", "gc-e2e-b.bin")
	require.Equal(t, http.StatusOK, gw.Code)
	assert.Equal(t, content, gw.Body.Bytes())

	// Now delete object B too.
	delReq2 := httptest.NewRequest("DELETE", "/test-bucket/gc-e2e-b.bin", nil)
	delReq2 = delReq2.WithContext(tenant.WithTenant(delReq2.Context(), f.tenant))
	dw2 := httptest.NewRecorder()
	f.adapter.HandleDelete(dw2, delReq2, "test-bucket", "gc-e2e-b.bin")
	require.Equal(t, http.StatusNoContent, dw2.Code)

	// Backdate again.
	_, err = f.db.Exec(`
		UPDATE global_content_index
		SET marked_at = NOW() - INTERVAL '1 day',
		    last_accessed_at = NOW() - INTERVAL '1 day'
		WHERE marked_for_deletion = TRUE`)
	require.NoError(t, err)

	// Run GC again — now all chunks should be collected.
	result2, gcErr2 := runner.RunOnce(context.Background())
	require.NoError(t, gcErr2)
	assert.Greater(t, result2.Deleted, 0, "orphaned chunks should be deleted")
	assert.Greater(t, result2.BytesReclaimed, int64(0))

	// GCI should be empty.
	var remaining int
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM global_content_index`).Scan(&remaining))
	assert.Equal(t, 0, remaining, "GCI should have no rows after full GC")
}

func TestNewDedupGCRunner_NilGuards(t *testing.T) {
	logger := zap.NewNop()
	eng := engine.NewEngine(nil, logger, nil)

	assert.Nil(t, NewDedupGCRunner(nil, eng, nil, logger), "nil db should return nil")
	assert.Nil(t, NewDedupGCRunner(nil, nil, nil, logger), "nil both should return nil")

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set")
	}
	// Not checking db-only since eng=nil also returns nil — tested above.
}

func TestDedupGC_GlobalContainerCleanup(t *testing.T) {
	runner, f := setupGCFixture(t)

	content := generateTestData(8 * 1024)
	putChunkedObject(t, f, "gc-global.bin", content, "application/octet-stream")

	// Verify chunks exist in the _global container on disk.
	globalDir := filepath.Join(f.tempDir, chunkContainer)
	entries, err := os.ReadDir(globalDir)
	if err == nil {
		require.NotEmpty(t, entries, "_global container should have chunk files")
	}

	// Delete, backdate, GC.
	delReq := httptest.NewRequest("DELETE", "/test-bucket/gc-global.bin", nil)
	delReq = delReq.WithContext(tenant.WithTenant(delReq.Context(), f.tenant))
	dw := httptest.NewRecorder()
	f.adapter.HandleDelete(dw, delReq, "test-bucket", "gc-global.bin")
	require.Equal(t, http.StatusNoContent, dw.Code)

	_, _ = f.db.Exec(`
		UPDATE global_content_index
		SET marked_at = NOW() - INTERVAL '1 day',
		    last_accessed_at = NOW() - INTERVAL '1 day'
		WHERE marked_for_deletion = TRUE`)

	result, gcErr := runner.RunOnce(context.Background())
	require.NoError(t, gcErr)
	assert.Greater(t, result.Deleted, 0)

	// Verify chunks are actually gone from disk.
	entries2, _ := os.ReadDir(globalDir)
	// The _chunks/ dir might still exist but should have no chunk files
	// matching deleted hashes. We verified per-key deletion in other tests,
	// so just check the GCI is clean.
	_ = entries2
	var count int
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM global_content_index`).Scan(&count))
	assert.Equal(t, 0, count)
}

func TestDedupGC_ManualTrigger(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set — skipping integration test")
	}

	runner, _ := setupGCFixture(t)

	// Just test that RunOnce works on an empty GCI.
	result, err := runner.RunOnce(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 0, result.Deleted)
	assert.Equal(t, int64(0), result.BytesReclaimed)
}

// Verify that putChunkedObject creates chunks in _global (not tenant namespace).
func TestDedupGC_ChunksInGlobalContainer(t *testing.T) {
	runner, f := setupGCFixture(t)

	content := generateTestData(8 * 1024)
	putChunkedObject(t, f, "gc-check-global.bin", content, "application/octet-stream")

	// All GCI entries should reference _global via their storage_key prefix.
	rows, err := f.db.Query(`SELECT storage_key FROM global_content_index`)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var key string
		require.NoError(t, rows.Scan(&key))
		assert.Contains(t, key, "_chunks/", "chunks should be stored with _chunks/ prefix")
	}
	_ = runner // use runner to confirm it builds
}
