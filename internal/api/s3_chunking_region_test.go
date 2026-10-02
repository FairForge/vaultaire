package api

// A bucket pinned to a region promises where its bytes live (WP-R7-1): the
// plain PUT, multipart complete and CopyObject place through the region's
// driver or refuse (R3-08). The chunked path placed through the engine's
// primary — an object above the chunk threshold in an eu-west-1 bucket had
// its chunks in Dallas, and with no driver for the region the whole-object
// path refused the PUT while the chunked path accepted it. Found in the
// plan-driver review of WP-R8-7 (#556). A region-pinned bucket is never
// chunked: whole objects through the region's driver, on every entry point.

import (
	"bytes"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func pinBucketRegion(t *testing.T, db *sql.DB, tenantID, bucket, region string) {
	t.Helper()
	_, err := db.Exec(`
		INSERT INTO buckets (tenant_id, name, visibility, region) VALUES ($1, $2, 'private', $3)
		ON CONFLICT (tenant_id, name) DO UPDATE SET region = EXCLUDED.region`, tenantID, bucket, region)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM buckets WHERE tenant_id = $1 AND name = $2`, tenantID, bucket) })
}

func TestHandlePut_RegionPinnedBucketIsNeverChunked(t *testing.T) {
	f := setupChunkingFixture(t)
	pinBucketRegion(t, f.db, f.tenantID, "test-bucket", "eu-west-1")
	content := generateTestData(8 * 1024) // above the fixture's 1 KiB threshold

	put := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest("PUT", "/test-bucket/eu.bin", bytes.NewReader(content))
		req.ContentLength = int64(len(content))
		req = req.WithContext(s3Ctx(req.Context(), f.tenant))
		w := httptest.NewRecorder()
		f.adapter.HandlePut(w, req, "test-bucket", "eu.bin")
		return w
	}
	noChunks := func(t *testing.T) {
		t.Helper()
		var refs int
		require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM tenant_chunk_refs WHERE tenant_id = $1`, f.tenantID).Scan(&refs))
		assert.Zero(t, refs, "a region-pinned object has no chunk manifest")
		for _, b := range f.fixed.blobs() {
			assert.False(t, strings.Contains(b, "/"+chunkContainer+"/"), "a chunk blob on the primary: %s", b)
		}
	}

	t.Run("no driver for the region: refused, nothing on the primary", func(t *testing.T) {
		w := put()
		assert.Equal(t, http.StatusServiceUnavailable, w.Code, w.Body.String())
		noChunks(t)
		assert.Empty(t, f.fixed.blobs(), "a region-pinned object must never land on the primary")
	})

	eu := &fixedBucketDriver{dir: t.TempDir()}
	f.eng.AddDriver("idrive-eu-west-1", eu)

	t.Run("driver present: one whole object in the region, the row says so", func(t *testing.T) {
		w := put()
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		var isChunked bool
		var backend string
		require.NoError(t, f.db.QueryRow(`
			SELECT is_chunked, COALESCE(backend_name, '') FROM object_head_cache
			 WHERE tenant_id = $1 AND bucket = 'test-bucket' AND object_key = 'eu.bin'`, f.tenantID).Scan(&isChunked, &backend))
		assert.False(t, isChunked, "stored whole")
		assert.Equal(t, "idrive-eu-west-1", backend)
		noChunks(t)
		assert.Equal(t, []string{"t-" + f.tenantID + "/" + f.tenant.NamespaceContainer("test-bucket") + "/eu.bin"}, eu.blobs())
		assert.Empty(t, f.fixed.blobs(), "nothing on the primary")
	})
}

func TestChunkedCopy_RegionPinnedDestination(t *testing.T) {
	f := setupChunkedCopyFixture(t)
	f.seedChunked(t, "src.bin", generateTestData(8*1024))
	pinBucketRegion(t, f.db, f.tenantID, "dest-bucket", "eu-west-1")
	nothingInDest := func(t *testing.T) {
		t.Helper()
		var n int
		require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM object_head_cache WHERE tenant_id = $1 AND bucket = 'dest-bucket'`, f.tenantID).Scan(&n))
		assert.Zero(t, n, "no manifest copy lands in a region-pinned bucket")
	}

	t.Run("no driver for the region: refused like every other write", func(t *testing.T) {
		w := f.copyObject(t, "src.bin", "dest-bucket", "eu-copy.bin")
		assert.Equal(t, http.StatusServiceUnavailable, w.Code, w.Body.String())
		nothingInDest(t)
	})

	f.server.engine.AddDriver("idrive-eu-west-1", &fixedBucketDriver{dir: t.TempDir()})

	t.Run("driver present: a manifest copy would leave the chunks on the primary — not implemented, nothing written", func(t *testing.T) {
		w := f.copyObject(t, "src.bin", "dest-bucket", "eu-copy.bin")
		assert.Equal(t, http.StatusNotImplemented, w.Code, w.Body.String())
		nothingInDest(t)
	})
}
