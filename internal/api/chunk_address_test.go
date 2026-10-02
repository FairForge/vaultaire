package api

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/FairForge/vaultaire/internal/common"
	"github.com/FairForge/vaultaire/internal/tenant"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// WP-R8-7 (found by WP-R10-3c, 2026-10-02) — SKIPPED: it is the red test of a
// work package that is not built. Remove the Skip to see it fail.
//
// The chunk container is meant to be shared and tenant-independent
// (chunkContainer's comment). On a fixed-bucket backend it is not: the driver
// keys every object `t-<tenant>/<container>/<artifact>` with the tenant from
// the request context, and S3 requests carry it (s3.go sets
// common.TenantIDKey). So a chunk is stored under the prefix of whoever
// uploaded it first, and
//
//  1. a second tenant whose upload dedups against it gets 200 on PUT and 500
//     on GET — its read looks under its own prefix, where nothing was stored;
//  2. dedup GC, whose job context carries no tenant, deletes the GCI row and
//     then "deletes" the blob at `t-default/_global/…`: the real blob stays
//     under the uploader's prefix for ever — after an ordinary DELETE and
//     after an account erasure alike (the erasure sweep leaves `_global`
//     alone on purpose and counts what it saw: chunk_blobs_left).
//
// Every other test in this package runs on the local driver, which keys by
// container only — `_global` is one directory there, so none of them sees it.
// Prod's primary is iDrive (fixed bucket). Prod today: one chunked object,
// one tenant, 127 chunks, no hash shared by two tenants, dedup GC has never
// deleted a row.
func TestChunkBlobs_HaveOneAddressOnAFixedBucketBackend(t *testing.T) {
	t.Skip("WP-R8-7: chunk blobs are keyed under the uploading tenant's prefix on fixed-bucket backends — red until they have one address")

	// Arrange: a fixed-bucket primary, two tenants, the request context the
	// way s3.go builds it.
	f := setupChunkingFixture(t)
	fixed := &fixedBucketDriver{dir: t.TempDir()}
	f.eng.AddDriver("fixed", fixed)
	f.eng.SetPrimary("fixed")
	a := f.tenant
	bID := uuid.New().String()
	b := &tenant.Tenant{ID: bID, Namespace: "tenant/" + bID + "/"}
	_, err := f.db.Exec(`INSERT INTO tenants (id, name, email, access_key, secret_key) VALUES ($1, 'B', $2, $3, $4)`,
		bID, "chunk-b-"+bID[:8]+"@test.local", "AKB"+bID[:8], "SKB"+bID[:8])
	require.NoError(t, err)
	t.Cleanup(func() {
		cleanupTenantChunkRows(f.db, bID, bID)
		_, _ = f.db.Exec("DELETE FROM object_head_cache WHERE tenant_id = $1", bID)
		_, _ = f.db.Exec("DELETE FROM tenants WHERE id = $1", bID)
	})
	reqCtx := func(tn *tenant.Tenant) context.Context {
		return common.WithTenantID(tenant.WithTenant(context.Background(), tn), tn.ID)
	}
	do := func(method string, tn *tenant.Tenant, key string, body []byte) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "/test-bucket/"+key, bytes.NewReader(body)).WithContext(reqCtx(tn))
		req.ContentLength = int64(len(body))
		w := httptest.NewRecorder()
		switch method {
		case http.MethodPut:
			f.adapter.HandlePut(w, req, "test-bucket", key)
		case http.MethodGet:
			f.adapter.HandleGet(w, req, "test-bucket", key)
		default:
			f.adapter.HandleDelete(w, req, "test-bucket", key)
		}
		return w
	}
	blobs := func() int {
		n := 0
		_ = filepath.Walk(fixed.dir, func(_ string, info os.FileInfo, err error) error {
			if err == nil && !info.IsDir() {
				n++
			}
			return nil
		})
		return n
	}
	content := generateTestData(8 * 1024)

	// Act + Assert 1: the second tenant can read what its PUT said was stored.
	require.Equal(t, http.StatusOK, do(http.MethodPut, a, "a.bin", content).Code)
	require.Equal(t, http.StatusOK, do(http.MethodPut, b, "b.bin", content).Code)
	got := do(http.MethodGet, b, "b.bin", nil)
	require.Equal(t, http.StatusOK, got.Code, "2026-10-02: 500 — the chunk is under the first uploader's prefix")
	assert.Equal(t, content, got.Body.Bytes())

	// Act + Assert 2: once nothing references the chunks, GC removes the bytes.
	chunks := tenantChunks(t, f.db, a.ID, "a.bin")
	require.NotEmpty(t, chunks)
	require.Equal(t, http.StatusNoContent, do(http.MethodDelete, a, "a.bin", nil).Code)
	require.Equal(t, http.StatusNoContent, do(http.MethodDelete, b, "b.bin", nil).Code)
	gc := NewDedupGCRunner(f.db, f.eng, f.adapter.gci, zap.NewNop())
	for _, c := range chunks {
		_, err := gc.sweepOne(context.Background(), c.scope, c.hash, "fixed", c.storageKey)
		require.NoError(t, err)
	}
	assert.Zero(t, blobs(), "2026-10-02: the GCI rows are gone and the blob is still under t-<uploader>/_global/")
}
