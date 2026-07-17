package api

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/FairForge/vaultaire/internal/tenant"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// addTenant creates a second tenant sharing the same adapter/engine/gci/
// chunkEncSvc as f, so both tenants dedup against the same Global Content
// Index. Returns the new tenant.
func addTenant(t *testing.T, f *adapterTestFixture) *tenant.Tenant {
	t.Helper()
	tid := uuid.New()
	_, err := f.db.Exec(`
		INSERT INTO tenants (id, name, email, access_key, secret_key)
		VALUES ($1, $2, $3, $4, $5) ON CONFLICT (id) DO NOTHING`,
		tid.String(), "Second Tenant", "second-"+tid.String()[:8]+"@test.local",
		"AK-"+tid.String()[:8], "SK-"+tid.String()[:8])
	require.NoError(t, err)

	tn := &tenant.Tenant{ID: tid.String(), Namespace: "tenant/" + tid.String() + "/"}
	require.NoError(t, os.MkdirAll(filepath.Join(f.tempDir, tn.NamespaceContainer("test-bucket")), 0755))

	t.Cleanup(func() {
		_, _ = f.db.Exec("DELETE FROM tenant_chunk_refs WHERE tenant_id = $1", tid)
		_, _ = f.db.Exec("DELETE FROM object_metadata WHERE tenant_id = $1", tid)
		_, _ = f.db.Exec("DELETE FROM object_head_cache WHERE tenant_id = $1", tid.String())
		_, _ = f.db.Exec("DELETE FROM tenants WHERE id = $1", tid.String())
	})
	return tn
}

func putAs(t *testing.T, f *adapterTestFixture, tn *tenant.Tenant, key string, content []byte) string {
	t.Helper()
	req := httptest.NewRequest("PUT", "/test-bucket/"+key, bytes.NewReader(content))
	req.ContentLength = int64(len(content))
	req.Header.Set("Content-Type", "application/octet-stream")
	req = req.WithContext(tenant.WithTenant(req.Context(), tn))
	w := httptest.NewRecorder()
	f.adapter.HandlePut(w, req, "test-bucket", key)
	require.Equal(t, http.StatusOK, w.Code, "PUT should succeed for tenant %s", tn.ID)
	return w.Header().Get("ETag")
}

func getAs(t *testing.T, f *adapterTestFixture, tn *tenant.Tenant, key string) (int, []byte) {
	t.Helper()
	req := httptest.NewRequest("GET", "/test-bucket/"+key, nil)
	req = req.WithContext(tenant.WithTenant(req.Context(), tn))
	w := httptest.NewRecorder()
	f.adapter.HandleGet(w, req, "test-bucket", key)
	body, _ := io.ReadAll(w.Body)
	return w.Code, body
}

// TestCrossTenantEncryptedDedup_Isolation is the CR-7b regression test.
//
// With the deterministic chunking polynomial (WP-7 step 1), two tenants
// uploading identical plaintext produce identical plaintext chunk hashes.
// Under global-by-plaintext-hash dedup, tenant B's manifest would point at
// the chunk tenant A stored ENCRYPTED with A's key — so B's GET would fetch
// A's ciphertext and fail GCM decryption (404-after-200 / data loss).
//
// Tenant-scoped encrypted dedup must give B its own bytes back.
func TestCrossTenantEncryptedDedup_Isolation(t *testing.T) {
	f := setupEncryptedChunkingFixture(t)
	tenantA := f.tenant
	tenantB := addTenant(t, f)

	content := generateTestData(8 * 1024)

	etagA := putAs(t, f, tenantA, "shared.bin", content)
	etagB := putAs(t, f, tenantB, "shared.bin", content)
	assert.Equal(t, etagA, etagB, "plaintext ETag is identical for identical content")

	// The crux: tenant B must get ITS OWN plaintext back, not a GCM failure.
	codeB, bodyB := getAs(t, f, tenantB, "shared.bin")
	require.Equal(t, http.StatusOK, codeB, "tenant B GET must not 404/500 on shared content")
	assert.Equal(t, content, bodyB, "tenant B must receive its own bytes, not tenant A's ciphertext")

	// Tenant A must still round-trip too.
	codeA, bodyA := getAs(t, f, tenantA, "shared.bin")
	require.Equal(t, http.StatusOK, codeA)
	assert.Equal(t, content, bodyA)

	// Isolation invariant: identical plaintext under encryption must be stored
	// as TWO physically distinct chunk sets — one per tenant scope — never one
	// shared row. (Cross-tenant dedup of encrypted data is deliberately off.)
	aUUID, _ := uuid.Parse(tenantA.ID)
	bUUID, _ := uuid.Parse(tenantB.ID)
	refsA, err := f.adapter.gci.GetObjectChunks(context.Background(), aUUID, "test-bucket", "shared.bin")
	require.NoError(t, err)
	refsB, err := f.adapter.gci.GetObjectChunks(context.Background(), bUUID, "test-bucket", "shared.bin")
	require.NoError(t, err)
	require.NotEmpty(t, refsA)
	require.Len(t, refsB, len(refsA))

	// Every GCI row backing these chunks must have ref_count 1 (per-tenant),
	// never 2 — a count of 2 would mean the two tenants collapsed onto one row.
	for _, ref := range refsA {
		var count int
		require.NoError(t, f.db.QueryRow(`
			SELECT COUNT(*) FROM global_content_index
			WHERE plaintext_hash = $1`, ref.PlaintextHash).Scan(&count))
		assert.Equal(t, 2, count,
			"encrypted chunk %s must exist as two scoped rows (one per tenant)", ref.PlaintextHash[:12])
	}
}
