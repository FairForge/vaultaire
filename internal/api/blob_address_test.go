package api

import (
	"bytes"
	"context"
	"crypto/md5" // #nosec G505 -- S3 ETag semantics
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/FairForge/vaultaire/internal/common"
	"github.com/FairForge/vaultaire/internal/crypto"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// WP-R2-1 (Stage 1a Prompt 2B): every reader of whole-object bytes honours
// the blob's own address — object_head_cache.storage_key, NULL = <key>.
// No writer sets one yet (2C), so each test sets it by SQL, with a STALE
// blob left at <key>: a reader that still reads <key> serves the wrong bytes.

func etagOf(b []byte) string {
	sum := md5.Sum(b) // #nosec G401 -- S3 ETag semantics
	return hex.EncodeToString(sum[:])
}

// pointRowAt makes the head row of key describe body stored at storageKey —
// what a 2C write leaves behind.
func pointRowAt(t *testing.T, f *adapterTestFixture, bucket, key, storageKey string, body []byte) {
	t.Helper()
	res, err := f.db.Exec(`UPDATE object_head_cache SET storage_key = $4, size_bytes = $5, etag = $6
		WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3`,
		f.tenantID, bucket, key, storageKey, len(body), etagOf(body))
	require.NoError(t, err)
	n, _ := res.RowsAffected()
	require.Equal(t, int64(1), n)
}

func cleanupBlobRows(t *testing.T, f *adapterTestFixture) {
	t.Helper()
	t.Cleanup(func() {
		_, _ = f.db.Exec(`DELETE FROM retired_blobs WHERE tenant_id = $1`, f.tenantID)
		_, _ = f.db.Exec(`DELETE FROM object_versions WHERE tenant_id = $1`, f.tenantID)
		_, _ = f.db.Exec(`DELETE FROM buckets WHERE tenant_id = $1`, f.tenantID)
	})
}

func putWhole(t *testing.T, f *adapterTestFixture, key string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("PUT", "/test-bucket/"+key, bytes.NewReader(body))
	req.ContentLength = int64(len(body))
	req = req.WithContext(s3Ctx(req.Context(), f.tenant))
	w := httptest.NewRecorder()
	f.adapter.HandlePut(w, req, "test-bucket", key)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	return w
}

func getWith(t *testing.T, f *adapterTestFixture, target, key string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", target, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	req = req.WithContext(s3Ctx(req.Context(), f.tenant))
	w := httptest.NewRecorder()
	f.adapter.HandleGet(w, req, "test-bucket", key)
	return w
}

func tctxOf(f *adapterTestFixture) context.Context {
	return common.WithTenantID(context.Background(), f.tenantID)
}

func TestGet_ServesTheRowsStorageKeyNotTheObjectKey(t *testing.T) {
	// Arrange: prod's key shape (fixed bucket); the row points at x@v1, the
	// blob at x is a stale earlier body.
	f := setupChunkingFixture(t)
	cleanupBlobRows(t, f)
	stale, fresh := []byte("stale bytes at the object key"), []byte("fresh bytes at the storage key!")
	putWhole(t, f, "x", stale)
	container := f.tenant.NamespaceContainer("test-bucket")
	require.NoError(t, f.fixed.Put(tctxOf(f), container, "x@v1", bytes.NewReader(fresh)))
	pointRowAt(t, f, "test-bucket", "x", "x@v1", fresh)

	// Act
	w := getWith(t, f, "/test-bucket/x", "x", nil)

	// Assert
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, fresh, w.Body.Bytes(), "the bytes the row names, never the blob at <key>")
	assert.Equal(t, `"`+etagOf(fresh)+`"`, w.Header().Get("ETag"))
}

func TestGet_Range_ServesTheRowsStorageKey(t *testing.T) {
	f := setupChunkingFixture(t)
	cleanupBlobRows(t, f)
	stale, fresh := []byte("SSSSSSSSSSSSSSSSSSSSSSSS"), []byte("FFFFFFFFFFFFFFFFFFFFFFFF")
	putWhole(t, f, "r", stale)
	container := f.tenant.NamespaceContainer("test-bucket")
	require.NoError(t, f.fixed.Put(tctxOf(f), container, "r@v1", bytes.NewReader(fresh)))
	pointRowAt(t, f, "test-bucket", "r", "r@v1", fresh)

	w := getWith(t, f, "/test-bucket/r", "r", map[string]string{"Range": "bytes=2-9"})

	require.Equal(t, http.StatusPartialContent, w.Code, w.Body.String())
	assert.Equal(t, fresh[2:10], w.Body.Bytes())
}

func TestParallelGet_ServesTheRowsStorageKey(t *testing.T) {
	f := setupParallelFixture(t, true)
	cleanupBlobRows(t, f.adapterTestFixture)
	stale, fresh := randomBytes(2<<20+11), bytes.Repeat([]byte("F"), 2<<20+11)
	f.put(t, "big.bin", stale)
	container := f.tenant.NamespaceContainer("test-bucket")
	require.NoError(t, f.drv.Put(tctxOf(f.adapterTestFixture), container, "big.bin@v1", bytes.NewReader(fresh)))
	pointRowAt(t, f.adapterTestFixture, "test-bucket", "big.bin", "big.bin@v1", fresh)

	w := f.get(t, "big.bin", nil)
	require.Equal(t, http.StatusOK, w.Code)
	assert.True(t, bytes.Equal(fresh, w.Body.Bytes()), "the parallel ranges read the storage key")
	assert.GreaterOrEqual(t, f.drv.ranges.Load(), int32(2), "served by the parallel path")

	w = f.get(t, "big.bin", map[string]string{"Range": "bytes=1048576-2097151"})
	require.Equal(t, http.StatusPartialContent, w.Code)
	assert.True(t, bytes.Equal(fresh[1<<20:2<<20], w.Body.Bytes()), "a parallel Range reads the storage key")
}

// --- versions -------------------------------------------------------------------

// versionedAdapter makes test-bucket versioning-Enabled on the adapter fixture.
func versionedAdapter(t *testing.T, f *adapterTestFixture) {
	t.Helper()
	cleanupBlobRows(t, f)
	_, err := f.db.Exec(`INSERT INTO buckets (tenant_id, name, visibility, versioning_status)
		VALUES ($1, 'test-bucket', 'private', 'Enabled')
		ON CONFLICT (tenant_id, name) DO UPDATE SET versioning_status = 'Enabled'`, f.tenantID)
	require.NoError(t, err)
}

// keepVersionBytes copies body to storageKey and records it on the version
// row — what 2C's write leaves for every version.
func keepVersionBytes(t *testing.T, f *adapterTestFixture, key, versionID, storageKey string, body []byte, encAlgo string) {
	t.Helper()
	container := f.tenant.NamespaceContainer("test-bucket")
	_, err := f.eng.Put(tctxOf(f), container, storageKey, bytes.NewReader(body))
	require.NoError(t, err)
	res, err := f.db.Exec(`UPDATE object_versions SET storage_key = $5, encryption_algorithm = NULLIF($6, '')
		WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3 AND version_id = $4`,
		f.tenantID, "test-bucket", key, versionID, storageKey, encAlgo)
	require.NoError(t, err)
	n, _ := res.RowsAffected()
	require.Equal(t, int64(1), n)
}

func TestGetVersion_NonCurrentWithBytes_Served(t *testing.T) {
	f := setupAdapterFixture(t)
	versionedAdapter(t, f)
	one, two := []byte("first version"), []byte("second version, longer")
	vid1 := putWhole(t, f, "k.txt", one).Header().Get("x-amz-version-id")
	putWhole(t, f, "k.txt", two)
	require.NotEmpty(t, vid1)
	keepVersionBytes(t, f, "k.txt", vid1, "k.txt@v1", one, "")

	w := getWith(t, f, "/test-bucket/k.txt?versionId="+vid1, "k.txt", nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, one, w.Body.Bytes())
	assert.Equal(t, `"`+etagOf(one)+`"`, w.Header().Get("ETag"), "the version's ETag")
	assert.Equal(t, strconv.Itoa(len(one)), w.Header().Get("Content-Length"), "the version's size")
	assert.Equal(t, vid1, w.Header().Get("x-amz-version-id"))

	// Range and conditionals run on the version's bytes.
	w = getWith(t, f, "/test-bucket/k.txt?versionId="+vid1, "k.txt", map[string]string{"Range": "bytes=0-4"})
	require.Equal(t, http.StatusPartialContent, w.Code)
	assert.Equal(t, "first", w.Body.String())
	assert.Equal(t, "bytes 0-4/"+strconv.Itoa(len(one)), w.Header().Get("Content-Range"))
	w = getWith(t, f, "/test-bucket/k.txt?versionId="+vid1, "k.txt", map[string]string{"If-None-Match": `"` + etagOf(one) + `"`})
	assert.Equal(t, http.StatusNotModified, w.Code)
	w = getWith(t, f, "/test-bucket/k.txt?versionId="+vid1, "k.txt", map[string]string{"If-None-Match": `"` + etagOf(two) + `"`})
	assert.Equal(t, http.StatusOK, w.Code, "the current object's ETag does not match the version")

	// The current version is still the current bytes.
	w = getWith(t, f, "/test-bucket/k.txt", "k.txt", nil)
	assert.Equal(t, two, w.Body.Bytes())
}

func TestGetVersion_NonCurrentWithoutBytes_Still501(t *testing.T) {
	f := setupAdapterFixture(t)
	versionedAdapter(t, f)
	vid1 := putWhole(t, f, "n.txt", []byte("one")).Header().Get("x-amz-version-id")
	putWhole(t, f, "n.txt", []byte("two"))

	w := getWith(t, f, "/test-bucket/n.txt?versionId="+vid1, "n.txt", nil)
	assert.Equal(t, http.StatusNotImplemented, w.Code, "no storage_key on the version row: today's answer until 2C")
}

func TestGetVersion_SSES3VersionOpensUnderItsOwnKeyVersion(t *testing.T) {
	f := setupAdapterFixture(t)
	versionedAdapter(t, f)
	keys := testTenantKeys(t, f.db, keyTestMasterA)
	f.adapter.sseService = crypto.NewSSEService(keys)
	one, two := []byte("sealed under key version one"), []byte("sealed under key version two")

	putSSE(t, f, "s.txt", one)
	var vid1 string
	require.NoError(t, f.db.QueryRow(`SELECT version_id FROM object_versions WHERE tenant_id = $1 AND object_key = 's.txt' AND is_latest`,
		f.tenantID).Scan(&vid1))
	// The v1 ciphertext, kept under its own address.
	container := f.tenant.NamespaceContainer("test-bucket")
	rc, err := f.eng.Get(tctxOf(f), container, "s.txt")
	require.NoError(t, err)
	sealed, err := io.ReadAll(rc)
	_ = rc.Close()
	require.NoError(t, err)
	_, err = keys.Rotate(context.Background(), f.tenantID)
	require.NoError(t, err)
	putSSE(t, f, "s.txt", two)
	keepVersionBytes(t, f, "s.txt", vid1, "s.txt@v1", sealed, crypto.SSEAlgorithm)

	w := getWith(t, f, "/test-bucket/s.txt?versionId="+vid1, "s.txt", nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, one, w.Body.Bytes(), "decrypted under the key version its 0x02 header records")
	assert.Equal(t, "AES256", w.Header().Get("x-amz-server-side-encryption"))
}

func retiredBlobs(t *testing.T, f *adapterTestFixture) map[string]string {
	t.Helper()
	rows, err := f.db.Query(`SELECT storage_key, reason FROM retired_blobs WHERE tenant_id = $1`, f.tenantID)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	out := map[string]string{}
	for rows.Next() {
		var k, r string
		require.NoError(t, rows.Scan(&k, &r))
		out[k] = r
	}
	require.NoError(t, rows.Err())
	return out
}

func TestDeleteVersion_WithBytes_RetiresTheBlob(t *testing.T) {
	f := setupAdapterFixture(t)
	versionedAdapter(t, f)
	one := []byte("version one bytes")
	vid1 := putWhole(t, f, "d.txt", one).Header().Get("x-amz-version-id")
	putWhole(t, f, "d.txt", []byte("version two"))
	keepVersionBytes(t, f, "d.txt", vid1, "d.txt@v1", one, "")

	req := httptest.NewRequest("DELETE", "/test-bucket/d.txt?versionId="+vid1, nil)
	req = req.WithContext(s3Ctx(req.Context(), f.tenant))
	w := httptest.NewRecorder()
	f.adapter.HandleDelete(w, req, "test-bucket", "d.txt")

	require.Equal(t, http.StatusNoContent, w.Code, w.Body.String())
	assert.Equal(t, map[string]string{"d.txt@v1": "version_deleted"}, retiredBlobs(t, f),
		"the version's blob waits in retired_blobs for the collector (2C)")
	var backend, container string
	require.NoError(t, f.db.QueryRow(`SELECT backend_id, container FROM retired_blobs WHERE tenant_id = $1`, f.tenantID).Scan(&backend, &container))
	assert.Equal(t, "local", backend)
	assert.Equal(t, f.tenant.NamespaceContainer("test-bucket"), container)
}

// A version whose blob the head row still names (the current version) is
// never retired by its version delete: the current object reads it.
func TestDeleteVersion_BlobTheHeadRowNames_NotRetired(t *testing.T) {
	f := setupAdapterFixture(t)
	versionedAdapter(t, f)
	putWhole(t, f, "c.txt", []byte("one"))
	cur := []byte("current")
	vid2 := putWhole(t, f, "c.txt", cur).Header().Get("x-amz-version-id")
	keepVersionBytes(t, f, "c.txt", vid2, "c.txt@v2", cur, "")
	pointRowAt(t, f, "test-bucket", "c.txt", "c.txt@v2", cur)

	req := httptest.NewRequest("DELETE", "/test-bucket/c.txt?versionId="+vid2, nil)
	req = req.WithContext(s3Ctx(req.Context(), f.tenant))
	w := httptest.NewRecorder()
	f.adapter.HandleDelete(w, req, "test-bucket", "c.txt")

	require.Equal(t, http.StatusNoContent, w.Code)
	assert.Empty(t, retiredBlobs(t, f))
}

// --- the previous slot meets a 2C row (the two-slot window) ------------------

// A PUT from this build writes in place at <key>: it must clear a storage_key
// a 2C slot left on the row (or the row would keep naming the old blob) and
// retire the blob it displaced.
func TestPut_OverARowWithStorageKey_ClearsItAndRetiresTheDisplacedBlob(t *testing.T) {
	f := setupChunkingFixture(t)
	cleanupBlobRows(t, f)
	old, next := []byte("bytes a 2C slot wrote"), []byte("bytes this slot writes in place")
	putWhole(t, f, "p", []byte("legacy"))
	container := f.tenant.NamespaceContainer("test-bucket")
	require.NoError(t, f.fixed.Put(tctxOf(f), container, "p@v1", bytes.NewReader(old)))
	pointRowAt(t, f, "test-bucket", "p", "p@v1", old)

	putWhole(t, f, "p", next)

	w := getWith(t, f, "/test-bucket/p", "p", nil)
	assert.Equal(t, next, w.Body.Bytes())
	var sk *string
	require.NoError(t, f.db.QueryRow(`SELECT storage_key FROM object_head_cache WHERE tenant_id = $1 AND object_key = 'p'`, f.tenantID).Scan(&sk))
	assert.Nil(t, sk, "an in-place write leaves the row at <key>")
	assert.Equal(t, map[string]string{"p@v1": "overwritten"}, retiredBlobs(t, f))
}

func TestDelete_DeletesTheRowsStorageKey(t *testing.T) {
	f := setupChunkingFixture(t)
	cleanupBlobRows(t, f)
	putWhole(t, f, "del", []byte("legacy at key"))
	container := f.tenant.NamespaceContainer("test-bucket")
	fresh := []byte("at the storage key")
	require.NoError(t, f.fixed.Put(tctxOf(f), container, "del@v1", bytes.NewReader(fresh)))
	pointRowAt(t, f, "test-bucket", "del", "del@v1", fresh)

	req := httptest.NewRequest("DELETE", "/test-bucket/del", nil)
	req = req.WithContext(s3Ctx(req.Context(), f.tenant))
	w := httptest.NewRecorder()
	f.adapter.HandleDelete(w, req, "test-bucket", "del")

	require.Equal(t, http.StatusNoContent, w.Code)
	ok, err := f.fixed.Exists(tctxOf(f), container, "del@v1")
	require.NoError(t, err)
	assert.False(t, ok, "the blob the row named is deleted")
}

// A Suspended bucket rewrites its "null" version in place: the row must not
// keep an address a 2C slot recorded on it.
func TestPut_SuspendedNullVersionRewrite_ClearsTheVersionsAddress(t *testing.T) {
	f := setupAdapterFixture(t)
	versionedAdapter(t, f)
	_, err := f.db.Exec(`UPDATE buckets SET versioning_status = 'Suspended' WHERE tenant_id = $1`, f.tenantID)
	require.NoError(t, err)
	putWhole(t, f, "n.txt", []byte("one"))
	_, err = f.db.Exec(`UPDATE object_versions SET storage_key = 'n.txt@v1', encryption_algorithm = 'AES256'
		WHERE tenant_id = $1 AND object_key = 'n.txt' AND version_id = 'null'`, f.tenantID)
	require.NoError(t, err)

	putWhole(t, f, "n.txt", []byte("two"))

	var sk, enc *string
	require.NoError(t, f.db.QueryRow(`SELECT storage_key, encryption_algorithm FROM object_versions
		WHERE tenant_id = $1 AND object_key = 'n.txt' AND version_id = 'null'`, f.tenantID).Scan(&sk, &enc))
	assert.Nil(t, sk)
	assert.Nil(t, enc)
}
