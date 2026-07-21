// s3_chunked_copy_test.go — review finding F4 (PR C): CopyObject returned 501
// for chunked sources, which after WP-C meant every object over the chunk
// threshold became un-copyable (breaking rclone renames / aws s3 mv on large
// objects, day one). A chunked copy is a manifest copy: the destination
// references the same chunks with incremented refcounts — no data moves.
package api

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/FairForge/vaultaire/internal/crypto"
	"github.com/FairForge/vaultaire/internal/tenant"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

type chunkedCopyFixture struct {
	*quotaAccountingFixture
	adapter *S3ToEngine
}

func setupChunkedCopyFixture(t *testing.T) *chunkedCopyFixture {
	t.Helper()
	f := setupQuotaAccountingFixture(t, 1<<30)
	gci := crypto.NewGlobalContentIndex(f.db)
	f.server.gci = gci

	adapter := NewS3ToEngine(f.server.engine, f.db, zap.NewNop())
	adapter.gci = gci
	adapter.chunkingThreshold = 1024

	tid := f.tenantID
	t.Cleanup(func() {
		_, _ = f.db.Exec("DELETE FROM tenant_chunk_refs WHERE tenant_id::text = $1", tid)
		_, _ = f.db.Exec(`DELETE FROM global_content_index g
			WHERE g.dedup_scope = $1
			   OR NOT EXISTS (
				SELECT 1 FROM tenant_chunk_refs r
				WHERE r.dedup_scope = g.dedup_scope AND r.plaintext_hash = g.plaintext_hash)`,
			tid)
		_, _ = f.db.Exec("DELETE FROM object_metadata WHERE tenant_id::text = $1", tid)
	})
	return &chunkedCopyFixture{quotaAccountingFixture: f, adapter: adapter}
}

// seedChunked stores content via the chunked adapter path (bypasses the
// server's quota reservation, so quota usage stays 0 until the copy).
func (f *chunkedCopyFixture) seedChunked(t *testing.T, key string, content []byte) string {
	t.Helper()
	req := httptest.NewRequest("PUT", "/test-bucket/"+key, bytes.NewReader(content))
	req.ContentLength = int64(len(content))
	req = req.WithContext(tenant.WithTenant(req.Context(), f.tenant))
	w := httptest.NewRecorder()
	f.adapter.HandlePut(w, req, "test-bucket", key)
	require.Equal(t, http.StatusOK, w.Code, "chunked seed PUT must succeed")

	var isChunked bool
	var etag string
	require.NoError(t, f.db.QueryRow(`
		SELECT is_chunked, etag FROM object_head_cache
		WHERE tenant_id = $1 AND bucket = 'test-bucket' AND object_key = $2`,
		f.tenantID, key).Scan(&isChunked, &etag))
	require.True(t, isChunked, "seed must be chunked for the test to mean anything")
	return etag
}

func (f *chunkedCopyFixture) copyObject(t *testing.T, srcKey, destBucket, destKey string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("PUT", "/"+destBucket+"/"+destKey, nil)
	req.Header.Set("x-amz-copy-source", "/test-bucket/"+srcKey)
	req = req.WithContext(f.ctx(req.Context()))
	w := httptest.NewRecorder()
	f.server.handleCopyObject(w, req, f.s3Req(destBucket, destKey))
	return w
}

func TestChunkedCopy_ManifestCopyRoundtrip(t *testing.T) {
	f := setupChunkedCopyFixture(t)

	content := generateTestData(8 * 1024)
	srcETag := f.seedChunked(t, "src.bin", content)

	w := f.copyObject(t, "src.bin", "dest-bucket", "copy.bin")
	require.Equal(t, http.StatusOK, w.Code, "chunked copy must succeed, not 501")
	assert.Contains(t, w.Body.String(), srcETag, "copy result must carry the source ETag")

	// Destination is chunked, right size, refs shared with incremented counts.
	var destChunked bool
	var destSize int64
	require.NoError(t, f.db.QueryRow(`
		SELECT is_chunked, size_bytes FROM object_head_cache
		WHERE tenant_id = $1 AND bucket = 'dest-bucket' AND object_key = 'copy.bin'`,
		f.tenantID).Scan(&destChunked, &destSize))
	assert.True(t, destChunked)
	assert.Equal(t, int64(len(content)), destSize)

	var minRef int
	require.NoError(t, f.db.QueryRow(`
		SELECT MIN(g.ref_count) FROM global_content_index g
		JOIN tenant_chunk_refs r ON r.plaintext_hash = g.plaintext_hash AND r.dedup_scope = g.dedup_scope
		WHERE r.tenant_id::text = $1 AND r.object_key = 'copy.bin'`,
		f.tenantID).Scan(&minRef))
	assert.GreaterOrEqual(t, minRef, 2, "each chunk must have gained a reference")

	// Destination GET is byte-identical.
	getReq := httptest.NewRequest("GET", "/dest-bucket/copy.bin", nil)
	getReq = getReq.WithContext(tenant.WithTenant(getReq.Context(), f.tenant))
	gw := httptest.NewRecorder()
	f.adapter.HandleGet(gw, getReq, "dest-bucket", "copy.bin")
	require.Equal(t, http.StatusOK, gw.Code)
	assert.Equal(t, content, gw.Body.Bytes())

	// Copy billed the destination's logical bytes (seed bypassed quota).
	assert.Equal(t, int64(len(content)), f.used(t),
		"chunked copy must reserve+settle the destination's logical size")

	// Deleting the source must not orphan the copy.
	delReq := httptest.NewRequest("DELETE", "/test-bucket/src.bin", nil)
	delReq = delReq.WithContext(tenant.WithTenant(delReq.Context(), f.tenant))
	dw := httptest.NewRecorder()
	f.adapter.HandleDelete(dw, delReq, "test-bucket", "src.bin")
	require.Equal(t, http.StatusNoContent, dw.Code)

	gw2 := httptest.NewRecorder()
	getReq2 := httptest.NewRequest("GET", "/dest-bucket/copy.bin", nil)
	getReq2 = getReq2.WithContext(tenant.WithTenant(getReq2.Context(), f.tenant))
	f.adapter.HandleGet(gw2, getReq2, "dest-bucket", "copy.bin")
	require.Equal(t, http.StatusOK, gw2.Code)
	assert.Equal(t, content, gw2.Body.Bytes(),
		"copy must survive source deletion — its refs are independent")
}

func TestChunkedCopy_QuotaExceededRejected(t *testing.T) {
	f := setupChunkedCopyFixture(t)

	content := generateTestData(8 * 1024)
	f.seedChunked(t, "big-src.bin", content)

	// Shrink the limit below the copy's size.
	_, err := f.db.Exec(`UPDATE tenant_quotas SET storage_limit_bytes = 4096 WHERE tenant_id = $1`,
		f.tenantID)
	require.NoError(t, err)

	w := f.copyObject(t, "big-src.bin", "dest-bucket", "over.bin")
	assert.Equal(t, http.StatusForbidden, w.Code, "over-quota chunked copy must be rejected")
	assert.Equal(t, int64(0), f.used(t), "rejected copy must not leak reserved bytes")
}

func TestChunkedCopy_VersionedDestinationRefused(t *testing.T) {
	f := setupChunkedCopyFixture(t)

	content := generateTestData(8 * 1024)
	f.seedChunked(t, "vsrc.bin", content)

	_, err := f.db.Exec(`
		INSERT INTO buckets (tenant_id, name, versioning_status)
		VALUES ($1, 'dest-bucket', 'Enabled')
		ON CONFLICT (tenant_id, name) DO UPDATE SET versioning_status = 'Enabled'`,
		f.tenantID)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = f.db.Exec(`DELETE FROM buckets WHERE tenant_id = $1 AND name = 'dest-bucket'`, f.tenantID)
	})

	w := f.copyObject(t, "vsrc.bin", "dest-bucket", "vcopy.bin")
	assert.Equal(t, http.StatusNotImplemented, w.Code,
		"chunked copy into a versioned bucket must be refused (manifests aren't version-aware)")

	var n int
	require.NoError(t, f.db.QueryRow(
		`SELECT COUNT(*) FROM tenant_chunk_refs WHERE tenant_id::text = $1 AND object_key = 'vcopy.bin'`,
		f.tenantID).Scan(&n))
	assert.Zero(t, n, "refused copy must leave no manifest rows")
}
