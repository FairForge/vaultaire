// s3_review_d_test.go — regression tests for the second ultra-review batch
// (review PR D):
//
//	G1: SSE-C objects must NEVER enter the chunked path — chunking would chunk
//	    the ciphertext and stamp AES256-CE over the SSE-C marker, after which
//	    GET serves raw ciphertext with a 200 and no key check.
//	G3: a plain PUT / copy overwriting a chunked object must release the old
//	    chunk manifest in the same transaction and flip is_chunked, or the
//	    refs leak forever and GET reads the stale manifest.
package api

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/FairForge/vaultaire/internal/tenant"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSSEC_AboveChunkThreshold_NotChunked(t *testing.T) {
	f := setupEncryptedChunkingFixture(t) // chunkEncSvc set, threshold 1 KiB

	key := generateSSECKey(t)
	content := generateTestData(8 * 1024) // over the threshold

	req := httptest.NewRequest("PUT", "/test-bucket/ssec-big.bin", bytes.NewReader(content))
	req.ContentLength = int64(len(content))
	setSSECHeaders(t, req, key)
	req = req.WithContext(tenant.WithTenant(req.Context(), f.tenant))
	w := httptest.NewRecorder()
	f.adapter.HandlePut(w, req, "test-bucket", "ssec-big.bin")
	require.Equal(t, http.StatusOK, w.Code)

	// The object must be whole-object SSE-C — not chunked ciphertext with the
	// SSE-C marker overwritten by AES256-CE.
	var isChunked bool
	var algo string
	require.NoError(t, f.db.QueryRow(`
		SELECT is_chunked, COALESCE(encryption_algorithm, '') FROM object_head_cache
		WHERE tenant_id = $1 AND bucket = 'test-bucket' AND object_key = 'ssec-big.bin'`,
		f.tenantID).Scan(&isChunked, &algo))
	assert.False(t, isChunked, "SSE-C bodies are ciphertext — they must never be chunked")
	assert.Equal(t, "AES256-SSE-C", algo, "the SSE-C marker must survive")

	// GET with the key returns the plaintext.
	getReq := httptest.NewRequest("GET", "/test-bucket/ssec-big.bin", nil)
	setSSECHeaders(t, getReq, key)
	getReq = getReq.WithContext(tenant.WithTenant(getReq.Context(), f.tenant))
	gw := httptest.NewRecorder()
	f.adapter.HandleGet(gw, getReq, "test-bucket", "ssec-big.bin")
	require.Equal(t, http.StatusOK, gw.Code)
	assert.Equal(t, content, gw.Body.Bytes())

	// GET without the key must be refused — never raw ciphertext with a 200.
	noKeyReq := httptest.NewRequest("GET", "/test-bucket/ssec-big.bin", nil)
	noKeyReq = noKeyReq.WithContext(tenant.WithTenant(noKeyReq.Context(), f.tenant))
	nw := httptest.NewRecorder()
	f.adapter.HandleGet(nw, noKeyReq, "test-bucket", "ssec-big.bin")
	assert.Equal(t, http.StatusForbidden, nw.Code,
		"SSE-C object without the customer key must 403, not leak bytes")
}

func TestPlainPutOverChunked_ReleasesManifest(t *testing.T) {
	f := setupStringTenantChunkingFixture(t)

	// v1: chunked.
	big := generateTestData(8 * 1024)
	require.Equal(t, http.StatusOK, stringTenantPut(t, f, "shrink.bin", big).Code)

	var refCountBefore int
	require.NoError(t, f.db.QueryRow(`
		SELECT COUNT(*) FROM tenant_chunk_refs WHERE tenant_id::text = $1 AND object_key = 'shrink.bin'`,
		f.tenantID).Scan(&refCountBefore))
	require.Positive(t, refCountBefore)

	// v2: small plain overwrite (below the 1 KiB threshold → plain path).
	small := []byte("small plain overwrite")
	require.Equal(t, http.StatusOK, stringTenantPut(t, f, "shrink.bin", small).Code)

	// Old manifest fully released, flag flipped, GCI refs decremented to 0
	// (marked for GC).
	var refsAfter int
	require.NoError(t, f.db.QueryRow(`
		SELECT COUNT(*) FROM tenant_chunk_refs WHERE tenant_id::text = $1 AND object_key = 'shrink.bin'`,
		f.tenantID).Scan(&refsAfter))
	assert.Zero(t, refsAfter, "plain overwrite must release the old chunk manifest")

	var isChunked bool
	require.NoError(t, f.db.QueryRow(`
		SELECT is_chunked FROM object_head_cache
		WHERE tenant_id = $1 AND bucket = 'test-bucket' AND object_key = 'shrink.bin'`,
		f.tenantID).Scan(&isChunked))
	assert.False(t, isChunked)

	var metaRows int
	require.NoError(t, f.db.QueryRow(`
		SELECT COUNT(*) FROM object_metadata WHERE tenant_id::text = $1 AND object_key = 'shrink.bin'`,
		f.tenantID).Scan(&metaRows))
	assert.Zero(t, metaRows, "object_metadata for the chunked version must be gone")

	// The new content round-trips.
	gw := stringTenantGet(t, f, "shrink.bin")
	require.Equal(t, http.StatusOK, gw.Code)
	assert.Equal(t, small, gw.Body.Bytes())
}

func TestPlainCopyOverChunkedDest_ReleasesManifest(t *testing.T) {
	f := setupChunkedCopyFixture(t)

	// Chunked object sitting at the destination key.
	big := generateTestData(8 * 1024)
	f.seedChunked(t, "dest-key.bin", big)

	// A plain source, stored through the server path.
	plainSrc := testBytes(2048)
	require.Equal(t, 200, f.put(t, "plain-src.bin", plainSrc))

	// Plain copy ONTO the chunked object (same bucket).
	req := httptest.NewRequest("PUT", "/test-bucket/dest-key.bin", nil)
	req.Header.Set("x-amz-copy-source", "/test-bucket/plain-src.bin")
	req = req.WithContext(f.ctx(req.Context()))
	w := httptest.NewRecorder()
	f.server.handleCopyObject(w, req, f.s3Req("test-bucket", "dest-key.bin"))
	require.Equal(t, http.StatusOK, w.Code)

	var refs int
	require.NoError(t, f.db.QueryRow(`
		SELECT COUNT(*) FROM tenant_chunk_refs WHERE tenant_id::text = $1 AND object_key = 'dest-key.bin'`,
		f.tenantID).Scan(&refs))
	assert.Zero(t, refs, "plain copy over a chunked object must release its manifest")

	var isChunked bool
	require.NoError(t, f.db.QueryRow(`
		SELECT is_chunked FROM object_head_cache
		WHERE tenant_id = $1 AND bucket = 'test-bucket' AND object_key = 'dest-key.bin'`,
		f.tenantID).Scan(&isChunked))
	assert.False(t, isChunked, "is_chunked must flip on plain-copy overwrite")

	// GET returns the copied plain bytes — not stale chunked data.
	getReq := httptest.NewRequest("GET", "/test-bucket/dest-key.bin", nil)
	getReq = getReq.WithContext(tenant.WithTenant(getReq.Context(), f.tenant))
	gw := httptest.NewRecorder()
	f.adapter.HandleGet(gw, getReq, "test-bucket", "dest-key.bin")
	require.Equal(t, http.StatusOK, gw.Code)
	assert.Equal(t, plainSrc, gw.Body.Bytes())
}
