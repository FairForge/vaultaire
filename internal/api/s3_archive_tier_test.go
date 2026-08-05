package api

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/FairForge/vaultaire/internal/drivers"
	"github.com/FairForge/vaultaire/internal/tenant"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// archiveFixture: chunking fixture + a registered "geyser" driver (local disk
// stand-in — routing is what's under test, not the Geyser wire) + a bucket row
// with tier_preference='archive'.
func archiveFixture(t *testing.T) (*adapterTestFixture, string) {
	t.Helper()
	f := setupChunkingFixture(t)

	geyserDir, err := os.MkdirTemp("", "vaultaire-geyser-stub-*")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(geyserDir) })
	f.eng.AddDriver("geyser", drivers.NewLocalDriver(geyserDir, zap.NewNop()))
	require.NoError(t, os.MkdirAll(filepath.Join(geyserDir, f.tenant.NamespaceContainer("test-bucket")), 0750))

	_, err = f.db.Exec(`
		INSERT INTO buckets (tenant_id, name, tier_preference)
		VALUES ($1, 'test-bucket', 'archive')
		ON CONFLICT (tenant_id, name) DO UPDATE SET tier_preference = 'archive'`,
		f.tenantID)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = f.db.Exec(`DELETE FROM buckets WHERE tenant_id = $1 AND name = 'test-bucket'`, f.tenantID)
	})
	return f, geyserDir
}

func chunkRefCount(t *testing.T, f *adapterTestFixture, key string) int {
	t.Helper()
	tenantUUID, err := uuid.Parse(f.tenantID)
	require.NoError(t, err)
	var n int
	require.NoError(t, f.db.QueryRow(`
		SELECT COUNT(*) FROM tenant_chunk_refs
		WHERE tenant_id = $1 AND bucket_name = 'test-bucket' AND object_key = $2`,
		tenantUUID, key).Scan(&n))
	return n
}

// TestHandlePut_ArchiveTierSkipsChunking: above-threshold objects in an
// archive-tier bucket must take the PLAIN path and land whole on geyser —
// chunk blobs always land on the engine primary via the shared _global
// container, which silently gave archive objects hot-tier COGS and made
// "it's on tape" false for exactly the >64 MB media files the tier is sold
// for. Deliberate trade: archive objects skip dedup.
func TestHandlePut_ArchiveTierSkipsChunking(t *testing.T) {
	f, _ := archiveFixture(t)

	content := generateTestData(8 * 1024) // above the 1 KB fixture chunk threshold
	req := httptest.NewRequest("PUT", "/test-bucket/master.mov", bytes.NewReader(content))
	req.ContentLength = int64(len(content))
	req = req.WithContext(tenant.WithTenant(req.Context(), f.tenant))
	w := httptest.NewRecorder()
	f.adapter.HandlePut(w, req, "test-bucket", "master.mov")
	require.Equal(t, http.StatusOK, w.Code)

	backend, chunked := headCacheBackendAndChunked(t, f, "master.mov")
	assert.Equal(t, "geyser", backend, "archive tier must store whole objects on geyser")
	assert.False(t, chunked, "archive-tier objects must not chunk")
	assert.Zero(t, chunkRefCount(t, f, "master.mov"),
		"archive-tier objects must leave no chunk refs")

	// And the object must round-trip.
	getReq := httptest.NewRequest("GET", "/test-bucket/master.mov", nil)
	getReq = getReq.WithContext(tenant.WithTenant(getReq.Context(), f.tenant))
	gw := httptest.NewRecorder()
	f.adapter.HandleGet(gw, getReq, "test-bucket", "master.mov")
	require.Equal(t, http.StatusOK, gw.Code)
	assert.Equal(t, content, gw.Body.Bytes())
}

// TestHandlePut_GlacierHeaderSkipsChunking: an explicit x-amz-storage-class
// header must gate chunking exactly like the bucket tier — GLACIER and
// DEEP_ARCHIVE resolve before the chunking decision.
func TestHandlePut_GlacierHeaderSkipsChunking(t *testing.T) {
	for _, class := range []string{"GLACIER", "DEEP_ARCHIVE"} {
		t.Run(class, func(t *testing.T) {
			f := setupChunkingFixture(t)

			geyserDir, err := os.MkdirTemp("", "vaultaire-geyser-stub-*")
			require.NoError(t, err)
			t.Cleanup(func() { _ = os.RemoveAll(geyserDir) })
			f.eng.AddDriver("geyser", drivers.NewLocalDriver(geyserDir, zap.NewNop()))
			require.NoError(t, os.MkdirAll(
				filepath.Join(geyserDir, f.tenant.NamespaceContainer("test-bucket")), 0750))

			content := generateTestData(8 * 1024) // above the 1 KB fixture threshold
			req := httptest.NewRequest("PUT", "/test-bucket/archive.bin", bytes.NewReader(content))
			req.ContentLength = int64(len(content))
			req.Header.Set("x-amz-storage-class", class)
			req = req.WithContext(tenant.WithTenant(req.Context(), f.tenant))
			w := httptest.NewRecorder()
			f.adapter.HandlePut(w, req, "test-bucket", "archive.bin")
			require.Equal(t, http.StatusOK, w.Code)

			backend, chunked := headCacheBackendAndChunked(t, f, "archive.bin")
			assert.Equal(t, "geyser", backend, "%s objects must store whole on geyser", class)
			assert.False(t, chunked)
			assert.Zero(t, chunkRefCount(t, f, "archive.bin"))
		})
	}
}
