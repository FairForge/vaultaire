// s3_chunking_string_tenant_test.go — WP-C (item 1.12): chunking must work for
// REAL tenants. Registration mints string IDs ("tenant-<hex>"), but the chunk
// tables used UUID tenant_id columns and the adapter silently skipped the
// chunked path whenever uuid.Parse(t.ID) failed — so no real customer ever got
// chunking, dedup, or compression (prod had zero is_chunked objects).
//
// These tests drive the full chunked lifecycle with a non-UUID tenant ID:
// red before WP-C (PUT silently falls through to the plain path).
package api

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/FairForge/vaultaire/internal/crypto"
	"github.com/FairForge/vaultaire/internal/tenant"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setupStringTenantChunkingFixture wires GCI + a low threshold onto the plain
// adapter fixture, whose tenant ID is a non-UUID string — the same shape
// registration produces.
func setupStringTenantChunkingFixture(t *testing.T) *adapterTestFixture {
	t.Helper()
	f := setupAdapterFixture(t)
	f.adapter.gci = crypto.NewGlobalContentIndex(f.db)
	f.adapter.chunkingThreshold = 1024

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
	return f
}

func stringTenantPut(t *testing.T, f *adapterTestFixture, key string, content []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("PUT", "/test-bucket/"+key, bytes.NewReader(content))
	req.ContentLength = int64(len(content))
	req = req.WithContext(tenant.WithTenant(req.Context(), f.tenant))
	w := httptest.NewRecorder()
	f.adapter.HandlePut(w, req, "test-bucket", key)
	return w
}

func stringTenantGet(t *testing.T, f *adapterTestFixture, key string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", "/test-bucket/"+key, nil)
	req = req.WithContext(tenant.WithTenant(req.Context(), f.tenant))
	w := httptest.NewRecorder()
	f.adapter.HandleGet(w, req, "test-bucket", key)
	return w
}

func TestChunking_StringTenantID_PutGetRoundtrip(t *testing.T) {
	f := setupStringTenantChunkingFixture(t)
	_, parseErr := uuid.Parse(f.tenantID)
	require.Error(t, parseErr,
		"fixture tenant ID must NOT be a UUID — that is the whole point of this test")

	content := generateTestData(8 * 1024) // above the 1 KiB test threshold

	w := stringTenantPut(t, f, "string-chunked.bin", content)
	require.Equal(t, http.StatusOK, w.Code)

	// The object must actually be chunked — not silently stored plain.
	var isChunked bool
	require.NoError(t, f.db.QueryRow(`
		SELECT is_chunked FROM object_head_cache
		WHERE tenant_id = $1 AND bucket = 'test-bucket' AND object_key = 'string-chunked.bin'`,
		f.tenantID).Scan(&isChunked))
	assert.True(t, isChunked,
		"a real (string-ID) tenant's over-threshold PUT must take the chunked path")

	var refs int
	require.NoError(t, f.db.QueryRow(
		`SELECT COUNT(*) FROM tenant_chunk_refs WHERE tenant_id::text = $1`,
		f.tenantID).Scan(&refs))
	assert.Positive(t, refs, "chunk manifest rows must exist for the string-ID tenant")

	gw := stringTenantGet(t, f, "string-chunked.bin")
	require.Equal(t, http.StatusOK, gw.Code)
	assert.Equal(t, content, gw.Body.Bytes(), "chunked GET must return byte-identical content")
}

func TestChunking_StringTenantID_DedupAndDelete(t *testing.T) {
	f := setupStringTenantChunkingFixture(t)

	content := generateTestData(8 * 1024)
	require.Equal(t, http.StatusOK, stringTenantPut(t, f, "dedup-a.bin", content).Code)
	require.Equal(t, http.StatusOK, stringTenantPut(t, f, "dedup-b.bin", content).Code)

	// Identical content re-uploaded must dedup: ref_count 2, not new rows.
	var maxRef int
	require.NoError(t, f.db.QueryRow(`
		SELECT MAX(gci.ref_count) FROM global_content_index gci
		INNER JOIN tenant_chunk_refs tcr
			ON tcr.plaintext_hash = gci.plaintext_hash AND tcr.dedup_scope = gci.dedup_scope
		WHERE tcr.tenant_id::text = $1`, f.tenantID).Scan(&maxRef))
	assert.GreaterOrEqual(t, maxRef, 2, "same-tenant identical upload must dedup")

	// DELETE must take the chunked path (decrement refs), not the plain path.
	req := httptest.NewRequest("DELETE", "/test-bucket/dedup-a.bin", nil)
	req = req.WithContext(tenant.WithTenant(req.Context(), f.tenant))
	w := httptest.NewRecorder()
	f.adapter.HandleDelete(w, req, "test-bucket", "dedup-a.bin")
	require.Equal(t, http.StatusNoContent, w.Code)

	var refsA int
	require.NoError(t, f.db.QueryRow(
		`SELECT COUNT(*) FROM tenant_chunk_refs WHERE tenant_id::text = $1 AND object_key = 'dedup-a.bin'`,
		f.tenantID).Scan(&refsA))
	assert.Zero(t, refsA, "deleted object's chunk refs must be removed")

	// The surviving copy still round-trips.
	gw := stringTenantGet(t, f, "dedup-b.bin")
	require.Equal(t, http.StatusOK, gw.Code)
	assert.Equal(t, content, gw.Body.Bytes())
}
