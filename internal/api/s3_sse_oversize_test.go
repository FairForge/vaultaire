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
)

// 64 hex chars = 32-byte SSE-S3 master key.
const testSSEMasterKey = "abababababababababababababababababababababababababababababababab"

// oversizePut issues a PUT whose declared Content-Length exceeds the SSE size
// cap while sending only a tiny body. The reject-guard runs before the body is
// read, so this exercises the size gate without allocating 256+ MiB.
func oversizePut(t *testing.T, f *adapterTestFixture, key string, mutate func(*http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("PUT", "/test-bucket/"+key, bytes.NewReader([]byte("tiny")))
	req.ContentLength = crypto.MaxEncryptableSize + 1 // just over the cap
	if mutate != nil {
		mutate(req)
	}
	req = req.WithContext(tenant.WithTenant(req.Context(), f.tenant))
	w := httptest.NewRecorder()
	f.adapter.HandlePut(w, req, "test-bucket", key)
	return w
}

// TestHandlePut_SSEHeaderOversize_Rejected: an explicit SSE-S3 request for an
// object larger than the encryptable cap must be rejected, never stored plaintext.
func TestHandlePut_SSEHeaderOversize_Rejected(t *testing.T) {
	f := setupChunkingFixture(t)
	svc, err := crypto.NewSSEService(f.db, testSSEMasterKey)
	require.NoError(t, err)
	f.adapter.sseService = svc

	w := oversizePut(t, f, "sse-header-big.bin", func(r *http.Request) {
		r.Header.Set("x-amz-server-side-encryption", "AES256")
	})

	require.Equal(t, http.StatusRequestEntityTooLarge, w.Code, "oversize SSE-S3 object must be rejected (EntityTooLarge)")

	// And it must NOT have been stored.
	var count int
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM object_head_cache
		WHERE tenant_id=$1 AND bucket=$2 AND object_key=$3`,
		f.tenantID, "test-bucket", "sse-header-big.bin").Scan(&count))
	assert.Equal(t, 0, count, "rejected object must not be persisted")
}

// TestHandlePut_SSEBucketOversize_Rejected: a bucket that defaults to SSE must
// reject oversize objects rather than silently storing them unencrypted.
func TestHandlePut_SSEBucketOversize_Rejected(t *testing.T) {
	f := setupChunkingFixture(t)
	svc, err := crypto.NewSSEService(f.db, testSSEMasterKey)
	require.NoError(t, err)
	f.adapter.sseService = svc

	// Mark the bucket SSE-enabled.
	_, err = f.db.Exec(`INSERT INTO buckets (tenant_id, name, sse_enabled)
		VALUES ($1,$2,TRUE)
		ON CONFLICT (tenant_id, name) DO UPDATE SET sse_enabled = TRUE`,
		f.tenantID, "test-bucket")
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = f.db.Exec(`DELETE FROM buckets WHERE tenant_id=$1 AND name=$2`, f.tenantID, "test-bucket")
	})

	w := oversizePut(t, f, "sse-bucket-big.bin", nil)
	require.Equal(t, http.StatusRequestEntityTooLarge, w.Code, "oversize object in SSE bucket must be rejected")
}

// TestHandlePut_NoSSEOversize_NotRejected: large objects WITHOUT encryption must
// still be accepted (the guard only fires when encryption is required).
func TestHandlePut_NoSSEOversize_NotRejected(t *testing.T) {
	f := setupChunkingFixture(t)
	svc, err := crypto.NewSSEService(f.db, testSSEMasterKey)
	require.NoError(t, err)
	f.adapter.sseService = svc // service present, but no SSE requested for this bucket/object

	w := oversizePut(t, f, "plain-big.bin", nil)
	assert.NotEqual(t, http.StatusRequestEntityTooLarge, w.Code,
		"large non-encrypted object must not be rejected by the SSE guard")
}
