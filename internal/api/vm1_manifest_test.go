package api

// WP-VM-1 (Stage 1a Prompt 7), Parts A and B: a chunked object's manifest
// has an address, as a whole blob does, and a displaced manifest is retired
// — kept while a head or version row names it, released by the collector
// after the grace — never released at the flip. "Before" = these tests on
// the Part 0 branch (3f73b30), in a separate worktree.

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/FairForge/vaultaire/internal/crypto"
	"github.com/FairForge/vaultaire/internal/engine"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// body is n bytes distinct per seed AND per fixture (the tenant id leads
// it): the plaintext content index is shared by every test on the
// database, and a chunk another test stored would dedup-hit a blob that
// test's driver held.
func (f *mpChunkFixture) body(n int, seed byte) []byte {
	b := mpBody(n, seed)
	copy(b, f.tenantID)
	return b
}

// putBody PUTs body to test-bucket/key through the server path, and has
// the content-index rows of its chunks removed with the test.
func (f *mpChunkFixture) putBody(t *testing.T, key string, body []byte) (int, string) {
	t.Helper()
	w := doS3Request(f.server, f.tenant, "PUT", "/test-bucket/"+key, bytes.NewReader(body))
	if w.Code == http.StatusOK {
		f.forgetChunksOf(t, key)
	}
	return w.Code, w.Header().Get("x-amz-version-id")
}

// forgetChunksOf removes, after the test, the content-index rows of the
// chunks key's manifest references now (bounded to them: packages share
// the database).
func (f *mpChunkFixture) forgetChunksOf(t *testing.T, key string) {
	t.Helper()
	rows, err := f.db.Query(`SELECT DISTINCT r.dedup_scope, r.plaintext_hash FROM tenant_chunk_refs r
		JOIN object_head_cache h ON h.tenant_id = r.tenant_id::text AND h.bucket = r.bucket_name
		 AND COALESCE(h.storage_key, h.object_key) = r.object_key
		WHERE h.tenant_id = $1 AND h.bucket = 'test-bucket' AND h.object_key = $2 AND h.is_chunked`, f.tenantID, key)
	require.NoError(t, err)
	type scoped struct{ scope, hash string }
	var hs []scoped
	for rows.Next() {
		var h scoped
		require.NoError(t, rows.Scan(&h.scope, &h.hash))
		hs = append(hs, h)
	}
	require.NoError(t, rows.Err())
	_ = rows.Close()
	t.Cleanup(func() {
		for _, h := range hs {
			_, _ = f.db.Exec(`DELETE FROM global_content_index WHERE dedup_scope = $1 AND plaintext_hash = $2`, h.scope, h.hash)
		}
	})
}

// head reads the key's head row: chunked, its address ("" = none).
func (f *mpChunkFixture) head(t *testing.T, key string) (chunked bool, storageKey string) {
	t.Helper()
	require.NoError(t, f.db.QueryRow(`SELECT is_chunked, COALESCE(storage_key, '') FROM object_head_cache
		WHERE tenant_id = $1 AND bucket = 'test-bucket' AND object_key = $2`, f.tenantID, key).Scan(&chunked, &storageKey))
	return chunked, storageKey
}

// manifestRows counts the manifest rows at object_key = at (refs, metadata).
func (f *mpChunkFixture) manifestRows(t *testing.T, at string) (refs, meta int) {
	t.Helper()
	require.NoError(t, f.db.QueryRow(`SELECT
		(SELECT COUNT(*) FROM tenant_chunk_refs WHERE tenant_id::text = $1 AND bucket_name = 'test-bucket' AND object_key = $2),
		(SELECT COUNT(*) FROM object_metadata WHERE tenant_id::text = $1 AND bucket_name = 'test-bucket' AND object_key = $2)`,
		f.tenantID, at).Scan(&refs, &meta))
	return refs, meta
}

// retiredManifests counts retired_blobs rows of the tenant on the chunked
// sentinel.
func (f *mpChunkFixture) retiredManifests(t *testing.T) int {
	t.Helper()
	var n int
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM retired_blobs WHERE tenant_id = $1 AND backend_id = $2`,
		f.tenantID, chunkedBackendSentinel).Scan(&n))
	return n
}

// collectAfterGrace runs the collector for the fixture's tenant as if the
// grace had passed.
func (f *mpChunkFixture) collectAfterGrace(t *testing.T) RetiredBlobsResult {
	t.Helper()
	c := newTestRetiredCollector(f)
	c.now = func() time.Time { return time.Now().Add(retiredBlobGrace + time.Hour) }
	res, err := c.RunOnce(context.Background())
	require.NoError(t, err)
	return res
}

func newTestRetiredCollector(f *mpChunkFixture) *RetiredBlobCollector {
	c := NewRetiredBlobCollector(f.db, f.server.engine, f.server.gci, zap.NewNop())
	c.onlyTenant = f.tenantID
	c.JobName = "test_retired_vm1_" + f.tenantID[:8]
	return c
}

func (f *mpChunkFixture) getBytes(t *testing.T, key, vid string) []byte {
	t.Helper()
	w := f.getV(t, "GET", key, vid)
	require.Equal(t, http.StatusOK, w.Code, "GET %s?versionId=%s: %s", key, vid, w.Body.String())
	return w.Body.Bytes()
}

func TestVM1_AChunkedWritePutsItsManifestAtItsOwnAddress(t *testing.T) {
	// Before: the manifest was keyed by the object key (storage_key NULL),
	// so a second chunked write of the key replaced it in place.
	f := setupMPChunkFixture(t, false)
	body := f.body(4096, 1)
	code, _ := f.putBody(t, "obj.bin", body)
	require.Equal(t, http.StatusOK, code)

	chunked, sk := f.head(t, "obj.bin")
	require.True(t, chunked)
	assert.True(t, isBlobAddressOf("obj.bin", sk), "the head row names the manifest's address: %q", sk)
	refs, meta := f.manifestRows(t, sk)
	assert.Equal(t, 1, meta, "the manifest is at its address")
	assert.Positive(t, refs)
	refs, meta = f.manifestRows(t, "obj.bin")
	assert.Zero(t, refs+meta, "nothing at the bare key")
	assert.Equal(t, body, f.getBytes(t, "obj.bin", ""))
}

func TestVM1_AChunkedOverwriteRetiresTheDisplacedManifest_TheCollectorReleasesIt(t *testing.T) {
	// Before: the overwrite's transaction released the old manifest (refs
	// deleted, counts decremented): a GET that had resolved it a moment
	// before lost its chunks to the next dedup GC.
	f := setupMPChunkFixture(t, false)
	a, b := f.body(4096, 1), f.body(4096, 2)
	require.Equal(t, http.StatusOK, mustCode(f.putBody(t, "obj.bin", a)))
	_, oldAddr := f.head(t, "obj.bin")
	require.Equal(t, http.StatusOK, mustCode(f.putBody(t, "obj.bin", b)))

	refs, meta := f.manifestRows(t, oldAddr)
	assert.Equal(t, 1, meta, "the displaced manifest is kept for the grace")
	assert.Positive(t, refs)
	assert.Equal(t, 1, f.retiredManifests(t))
	assert.Equal(t, b, f.getBytes(t, "obj.bin", ""))
	rd, err := f.server.writeAdapter().openChunkedObject(context.Background(), f.tenantID, "test-bucket", "obj.bin", oldAddr, int64(len(a)))
	require.NoError(t, err, "a reader that resolved the old row still reads it")
	got := new(bytes.Buffer)
	_, err = got.ReadFrom(rd)
	require.NoError(t, err)
	_ = rd.Close()
	assert.Equal(t, a, got.Bytes())
	f.ledgerAgrees(t, "after the overwrite")

	res := f.collectAfterGrace(t)

	assert.Equal(t, 1, res.Deleted)
	refs, meta = f.manifestRows(t, oldAddr)
	assert.Zero(t, refs+meta, "the collector released the manifest")
	assert.Zero(t, f.retiredManifests(t))
	assert.Equal(t, b, f.getBytes(t, "obj.bin", ""))
	f.ledgerAgrees(t, "after the collector")
}

func TestVM1_AChunkedDeleteRetiresItsManifest(t *testing.T) {
	f := setupMPChunkFixture(t, false)
	require.Equal(t, http.StatusOK, mustCode(f.putBody(t, "obj.bin", f.body(4096, 1))))
	_, addr := f.head(t, "obj.bin")

	require.Equal(t, http.StatusNoContent, doS3Request(f.server, f.tenant, "DELETE", "/test-bucket/obj.bin", nil).Code)

	assert.Zero(t, f.used(t), "released with the row")
	refs, _ := f.manifestRows(t, addr)
	assert.Positive(t, refs, "kept for the grace")
	assert.Equal(t, 1, f.retiredManifests(t))
	f.collectAfterGrace(t)
	refs, _ = f.manifestRows(t, addr)
	assert.Zero(t, refs)
	f.ledgerAgrees(t, "after the collector")
}

func TestVM1_PM3_APreVersioningChunkedObjectIsKeptAsTheNullVersionByAnOverwrite(t *testing.T) {
	// PM-3 (WP-R2-1 post-merge): the first Enabled overwrite released the
	// manifest of an object chunked before versioning was enabled; AWS keeps
	// it as the null version. Before: GET ?versionId=null 404.
	f := setupMPChunkFixture(t, false)
	a := f.body(4096, 1)
	require.Equal(t, http.StatusOK, mustCode(f.putBody(t, "obj.bin", a)))
	f.versioning(t, "Enabled")

	code, vid := f.putBody(t, "obj.bin", []byte("small whole overwrite"))
	require.Equal(t, http.StatusOK, code)
	require.NotEmpty(t, vid)

	assert.Equal(t, a, f.getBytes(t, "obj.bin", "null"), "the pre-versioning object is the null version")
	assert.Equal(t, []byte("small whole overwrite"), f.getBytes(t, "obj.bin", ""))
	assert.Equal(t, int64(4096+21), f.used(t), "the null version stays billed (C2)")
	f.ledgerAgrees(t, "after the overwrite")
	assert.Zero(t, f.retiredManifests(t), "the null version names the manifest: nothing retired")
	res := f.collectAfterGrace(t)
	assert.Zero(t, res.Deleted)
	assert.Equal(t, a, f.getBytes(t, "obj.bin", "null"))

	// DELETE ?versionId=null: the version goes, its bytes are released, its
	// manifest retired then released.
	f.delVersion(t, "obj.bin", "null")
	assert.Equal(t, int64(21), f.used(t))
	f.ledgerAgrees(t, "after DELETE ?versionId=null")
	assert.Equal(t, 1, f.retiredManifests(t))
	f.collectAfterGrace(t)
	assert.Zero(t, f.retiredManifests(t))
}

func TestVM1_PM3_APreVersioningChunkedObjectIsKeptBehindADeleteMarker(t *testing.T) {
	f := setupMPChunkFixture(t, false)
	a := f.body(4096, 1)
	require.Equal(t, http.StatusOK, mustCode(f.putBody(t, "obj.bin", a)))
	f.versioning(t, "Enabled")

	w := doS3Request(f.server, f.tenant, "DELETE", "/test-bucket/obj.bin", nil)
	require.Equal(t, http.StatusNoContent, w.Code)
	assert.Equal(t, "true", w.Header().Get("x-amz-delete-marker"))

	assert.Equal(t, http.StatusNotFound, f.getV(t, "GET", "obj.bin", "").Code)
	assert.Equal(t, a, f.getBytes(t, "obj.bin", "null"))
	assert.Equal(t, int64(4096), f.used(t), "the null version keeps its bill behind the marker")
	f.ledgerAgrees(t, "behind the marker")
	f.collectAfterGrace(t)
	assert.Equal(t, a, f.getBytes(t, "obj.bin", "null"), "nothing collected while the version names it")
}

func TestVM1_ASuspendedOverwriteOfAPreVersioningChunkedObjectReleasesIt(t *testing.T) {
	// Suspended: the write replaces the null version (AWS), so the displaced
	// chunked object is retired and released — through the collector.
	f := setupMPChunkFixture(t, false)
	require.Equal(t, http.StatusOK, mustCode(f.putBody(t, "obj.bin", f.body(4096, 1))))
	_, addr := f.head(t, "obj.bin")
	f.versioning(t, "Suspended")

	code, vid := f.putBody(t, "obj.bin", []byte("whole"))
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, "null", vid)

	assert.Equal(t, int64(5), f.used(t))
	f.ledgerAgrees(t, "after the Suspended overwrite")
	assert.Equal(t, 1, f.retiredManifests(t))
	f.collectAfterGrace(t)
	refs, _ := f.manifestRows(t, addr)
	assert.Zero(t, refs)
}

func TestVM1_ALegacyByKeyManifestStillReadsOverwritesAndDeletes(t *testing.T) {
	// A manifest written before 092 is keyed by the object key and its head
	// row names no address: it reads by key, and an overwrite or a delete
	// retires it by key.
	f := setupMPChunkFixture(t, false)
	a := f.body(4096, 1)
	plantLegacy := func(key string, body []byte) {
		require.Equal(t, http.StatusOK, mustCode(f.putBody(t, key, body)))
		_, sk := f.head(t, key)
		if sk == "" {
			return // the build under test writes by key already
		}
		for _, q := range []string{
			`UPDATE tenant_chunk_refs SET object_key = $3 WHERE tenant_id::text = $1 AND bucket_name = 'test-bucket' AND object_key = $2`,
			`UPDATE object_metadata SET object_key = $3 WHERE tenant_id::text = $1 AND bucket_name = 'test-bucket' AND object_key = $2`,
		} {
			_, err := f.db.Exec(q, f.tenantID, sk, key)
			require.NoError(t, err)
		}
		_, err := f.db.Exec(`UPDATE object_head_cache SET storage_key = NULL WHERE tenant_id = $1 AND object_key = $2`, f.tenantID, key)
		require.NoError(t, err)
	}
	plantLegacy("old.bin", a)
	plantLegacy("gone.bin", f.body(3000, 3))

	assert.Equal(t, a, f.getBytes(t, "old.bin", ""), "a legacy manifest reads by key")
	require.Equal(t, http.StatusOK, mustCode(f.putBody(t, "old.bin", f.body(4096, 2))))
	require.Equal(t, http.StatusNoContent, doS3Request(f.server, f.tenant, "DELETE", "/test-bucket/gone.bin", nil).Code)
	assert.Equal(t, f.body(4096, 2), f.getBytes(t, "old.bin", ""))
	f.ledgerAgrees(t, "after the overwrite and the delete")

	f.collectAfterGrace(t)
	for _, k := range []string{"old.bin", "gone.bin"} {
		refs, meta := f.manifestRows(t, k)
		assert.Zero(t, refs+meta, "the legacy manifest of %s is released by key", k)
	}
	assert.Equal(t, f.body(4096, 2), f.getBytes(t, "old.bin", ""))
}

func TestVM1_AManifestNoRowNamesIsFoundAndReleased(t *testing.T) {
	// The two-slot window: the previous build's overwrite or delete of an
	// addressed chunked object releases nothing (it looks the manifest up by
	// key) and drops the head row. The collector's orphan pass finds the
	// manifest no row names and releases it after the grace.
	f := setupMPChunkFixture(t, false)
	require.Equal(t, http.StatusOK, mustCode(f.putBody(t, "obj.bin", f.body(4096, 1))))
	_, addr := f.head(t, "obj.bin")
	require.True(t, isBlobAddressOf("obj.bin", addr), "an addressed manifest: %q", addr)
	_, err := f.db.Exec(`DELETE FROM object_head_cache WHERE tenant_id = $1 AND object_key = 'obj.bin'`, f.tenantID) // the old slot's delete
	require.NoError(t, err)
	_, err = f.db.Exec(`UPDATE object_metadata SET created_at = NOW() - INTERVAL '2 days', updated_at = NOW() - INTERVAL '2 days'
		WHERE tenant_id::text = $1 AND object_key = $2`, f.tenantID, addr)
	require.NoError(t, err)

	f.collectAfterGrace(t) // finds and queues it
	f.collectAfterGrace(t) // releases it

	refs, meta := f.manifestRows(t, addr)
	assert.Zero(t, refs+meta)
}

func mustCode(code int, _ string) int { return code }

// releaseRetiredManifests runs the collector over tenantID's retired rows
// as if their grace had passed: what a delete or an overwrite of a chunked
// object releases since WP-VM-1, after the grace. Tests of dedup GC and the
// chunk move start where the collector ends.
func releaseRetiredManifests(t *testing.T, db *sql.DB, eng engine.Engine, gci *crypto.GlobalContentIndex, tenantID string) {
	t.Helper()
	c := NewRetiredBlobCollector(db, eng, gci, zap.NewNop())
	require.NotNil(t, c)
	c.onlyTenant = tenantID
	c.JobName = "test_retired_release_" + tenantID[:8]
	c.now = func() time.Time { return time.Now().Add(retiredBlobGrace + time.Hour) }
	_, err := c.RunOnce(context.Background())
	require.NoError(t, err)
}

// manifestKeyOf is where key's manifest is stored: the address its chunked
// head row names (WP-VM-1), the key itself for one written before 092 or a
// key with no chunked head row.
func manifestKeyOf(t *testing.T, db *sql.DB, tenantID, bucket, key string) string {
	t.Helper()
	var at string
	err := db.QueryRow(`SELECT COALESCE(storage_key, object_key) FROM object_head_cache
		WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3 AND is_chunked`, tenantID, bucket, key).Scan(&at)
	if errors.Is(err, sql.ErrNoRows) {
		return key
	}
	require.NoError(t, err)
	return at
}
