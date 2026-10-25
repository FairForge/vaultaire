package api

import (
	"bytes"
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/FairForge/vaultaire/internal/crypto"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// WP-R2-1 Part C, PR C2: key_reencrypt moves whole SSE-S3 objects — head
// rows and retained versions — onto the tenant's newest key version: the
// re-sealed bytes go to a NEW address on the same backend, one transaction
// re-points every row that still names the old address and the old key
// version, and the old address is retired ('reencrypted'), never deleted in
// the run. Red on 70d38b9 (C1): whole objects stayed on their version.

// wholeKeyFixture is the keyed fixture with a versioned (or not) bucket and
// the runner wired with the SSE service.
type wholeKeyFixture struct {
	*adapterTestFixture
	keys *crypto.TenantKeys
	k    *KeyReencryptRunner
}

func setupWholeKeyFixture(t *testing.T, versioning string) *wholeKeyFixture {
	t.Helper()
	f, keys := setupKeyedChunkingFixture(t)
	if versioning != "" {
		_, err := f.db.Exec(`INSERT INTO buckets (tenant_id, name, visibility, versioning_status) VALUES ($1, 'test-bucket', 'private', $2)
			ON CONFLICT (tenant_id, name) DO UPDATE SET versioning_status = EXCLUDED.versioning_status`, f.tenantID, versioning)
		require.NoError(t, err)
	}
	t.Cleanup(func() {
		_, _ = f.db.Exec(`DELETE FROM object_versions WHERE tenant_id = $1`, f.tenantID)
		_, _ = f.db.Exec(`DELETE FROM retired_blobs WHERE tenant_id = $1`, f.tenantID)
		_, _ = f.db.Exec(`DELETE FROM buckets WHERE tenant_id = $1`, f.tenantID)
	})
	k := newKeyReencryptForTest(t, f)
	k.sse = f.adapter.sseService
	return &wholeKeyFixture{adapterTestFixture: f, keys: keys, k: k}
}

// putSSEV PUTs an SSE-S3 object and returns its version id ("" unversioned).
func (f *wholeKeyFixture) putSSEV(t *testing.T, key string, body []byte) string {
	t.Helper()
	req := httptest.NewRequest("PUT", "/test-bucket/"+key, bytes.NewReader(body))
	req.ContentLength = int64(len(body))
	req.Header.Set("x-amz-server-side-encryption", "AES256")
	req = req.WithContext(s3Ctx(req.Context(), f.tenant))
	w := httptest.NewRecorder()
	f.adapter.HandlePut(w, req, "test-bucket", key)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	return w.Header().Get("x-amz-version-id")
}

func (f *wholeKeyFixture) getVersion(t *testing.T, key, vid string) *httptest.ResponseRecorder {
	t.Helper()
	target := "/test-bucket/" + key
	if vid != "" {
		target += "?versionId=" + vid
	}
	req := httptest.NewRequest("GET", target, nil)
	req = req.WithContext(s3Ctx(req.Context(), f.tenant))
	w := httptest.NewRecorder()
	f.adapter.HandleGet(w, req, "test-bucket", key)
	return w
}

type blobRow struct {
	storageKey string
	keyVersion sql.NullInt32
	etag       string
}

func (f *wholeKeyFixture) head(t *testing.T, key string) blobRow {
	t.Helper()
	var r blobRow
	require.NoError(t, f.db.QueryRow(`SELECT COALESCE(storage_key, ''), encryption_key_version, etag FROM object_head_cache
		WHERE tenant_id = $1 AND bucket = 'test-bucket' AND object_key = $2`, f.tenantID, key).Scan(&r.storageKey, &r.keyVersion, &r.etag))
	return r
}

func (f *wholeKeyFixture) version(t *testing.T, key, vid string) blobRow {
	t.Helper()
	var r blobRow
	require.NoError(t, f.db.QueryRow(`SELECT COALESCE(storage_key, ''), encryption_key_version, etag FROM object_versions
		WHERE tenant_id = $1 AND bucket = 'test-bucket' AND object_key = $2 AND version_id = $3`, f.tenantID, key, vid).
		Scan(&r.storageKey, &r.keyVersion, &r.etag))
	return r
}

func (f *wholeKeyFixture) retiredBlobs(t *testing.T) map[string]string {
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

// pendingWhole is this tenant's share of the gauge's "whole" backlog (the
// gauge's statement, scoped: the table is shared with other packages).
func (f *wholeKeyFixture) pendingWhole(t *testing.T) int64 {
	t.Helper()
	var chunk, whole int64
	require.NoError(t, f.db.QueryRow(keyRotationPendingSQL, crypto.SSEAlgorithm, f.tenantID).Scan(&chunk, &whole, new(int64)))
	return whole
}

func (f *wholeKeyFixture) rotate(t *testing.T) {
	t.Helper()
	_, err := f.keys.Rotate(context.Background(), f.tenantID)
	require.NoError(t, err)
}

var keyV2 = sql.NullInt32{Int32: 2, Valid: true}

func TestKeyReencrypt_WholeObject_FlipsAndRetires(t *testing.T) {
	f := setupWholeKeyFixture(t, "Enabled")
	one, two := []byte("whole object, the first version"), []byte("whole object, the second version, longer")
	vOne := f.putSSEV(t, "k", one)
	f.putSSEV(t, "k", two)
	oldHead, oldV1 := f.head(t, "k"), f.version(t, "k", vOne)
	require.NotEmpty(t, oldV1.storageKey)
	f.rotate(t)
	assert.Equal(t, int64(2), f.pendingWhole(t), "the head row's blob and the retained version's blob (the latest version IS the head's blob: counted once)")

	res, err := f.k.RunOnce(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 2, res.ObjectsReencrypted, "%+v", res)

	newHead, newV1 := f.head(t, "k"), f.version(t, "k", vOne)
	assert.NotEqual(t, oldHead.storageKey, newHead.storageKey, "the head row names a new address")
	assert.NotEqual(t, oldV1.storageKey, newV1.storageKey, "the retained version names a new address")
	assert.True(t, isBlobAddressOf("k", newHead.storageKey) && isBlobAddressOf("k", newV1.storageKey))
	assert.Equal(t, keyV2, newHead.keyVersion)
	assert.Equal(t, keyV2, newV1.keyVersion)
	assert.Equal(t, oldHead.etag, newHead.etag, "the ETag is the plaintext's")
	assert.Equal(t, oldV1.etag, newV1.etag)
	var latest blobRow
	require.NoError(t, f.db.QueryRow(`SELECT COALESCE(storage_key, ''), encryption_key_version, etag FROM object_versions
		WHERE tenant_id = $1 AND object_key = 'k' AND is_latest`, f.tenantID).Scan(&latest.storageKey, &latest.keyVersion, &latest.etag))
	assert.Equal(t, newHead.storageKey, latest.storageKey, "the latest version row follows the head row's blob")
	assert.Equal(t, keyV2, latest.keyVersion)

	g := f.getVersion(t, "k", "")
	require.Equal(t, http.StatusOK, g.Code, g.Body.String())
	assert.Equal(t, two, g.Body.Bytes())
	g = f.getVersion(t, "k", vOne)
	require.Equal(t, http.StatusOK, g.Code, g.Body.String())
	assert.Equal(t, one, g.Body.Bytes())

	retired := f.retiredBlobs(t)
	assert.Equal(t, retiredReencrypted, retired[oldHead.storageKey])
	assert.Equal(t, retiredReencrypted, retired[oldV1.storageKey])
	assert.Equal(t, int64(0), f.pendingWhole(t))

	// A second run has nothing left to move.
	res, err = f.k.RunOnce(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 0, res.ObjectsReencrypted)
}

// The re-encrypt loses to an overwrite that lands between its write and its
// swap: the overwrite's row stands, the re-sealed blob is an orphan, the
// old blob is the overwrite's to retire, nothing is lost.
func TestKeyReencrypt_WholeObject_LosesToAnOverwrite(t *testing.T) {
	f := setupWholeKeyFixture(t, "")
	f.putSSEV(t, "k", []byte("the old object"))
	old := f.head(t, "k")
	f.rotate(t)

	var written string
	keyReencryptBeforeSwapHook = func(addr string) {
		written = addr
		f.putSSEV(t, "k", []byte("the customer's overwrite"))
	}
	t.Cleanup(func() { keyReencryptBeforeSwapHook = func(string) {} })

	res, err := f.k.RunOnce(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 0, res.ObjectsReencrypted)
	assert.Equal(t, 1, res.ObjectsChanged, "%+v", res)

	require.NotEmpty(t, written)
	cur := f.head(t, "k")
	assert.NotEqual(t, written, cur.storageKey, "the overwrite's row stands")
	assert.NotEqual(t, old.storageKey, cur.storageKey)
	retired := f.retiredBlobs(t)
	assert.Equal(t, retiredOrphanWrite, retired[written], "the re-sealed copy no row names is an orphan")
	assert.Equal(t, retiredOverwritten, retired[old.storageKey], "the old blob is the overwrite's to retire")
	g := f.getVersion(t, "k", "")
	require.Equal(t, http.StatusOK, g.Code)
	assert.Equal(t, "the customer's overwrite", g.Body.String())
}

func TestKeyReencrypt_WholeObject_BudgetResumesNextRun(t *testing.T) {
	f := setupWholeKeyFixture(t, "")
	f.putSSEV(t, "a", []byte("object a"))
	f.putSSEV(t, "b", []byte("object b"))
	f.rotate(t)
	f.k.MaxBytesPerRun = 1 // one object, then the budget is spent

	res, err := f.k.RunOnce(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, res.ObjectsReencrypted)
	assert.True(t, res.BudgetReached)
	assert.Equal(t, int64(1), f.pendingWhole(t))

	res, err = f.k.RunOnce(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, res.ObjectsReencrypted, "the next run moves the rest")
	assert.Equal(t, int64(0), f.pendingWhole(t))
	for key, want := range map[string]string{"a": "object a", "b": "object b"} {
		g := f.getVersion(t, key, "")
		require.Equal(t, http.StatusOK, g.Code)
		assert.Equal(t, want, g.Body.String())
	}
}

// An object on the archive backend is never read (a tape restore): skipped,
// counted, still pending.
func TestKeyReencrypt_WholeObject_ArchiveSkipped(t *testing.T) {
	f := setupWholeKeyFixture(t, "")
	f.putSSEV(t, "cold", []byte("on tape"))
	_, err := f.db.Exec(`UPDATE object_head_cache SET backend_name = 'geyser' WHERE tenant_id = $1 AND object_key = 'cold'`, f.tenantID)
	require.NoError(t, err)
	before := f.head(t, "cold")
	f.rotate(t)

	res, err := f.k.RunOnce(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 0, res.ObjectsReencrypted)
	assert.Equal(t, 1, res.ObjectsSkipped["archive"], "%+v", res)
	assert.Equal(t, before, f.head(t, "cold"))
	assert.Equal(t, int64(1), f.pendingWhole(t), "skipped = still pending")
}

func TestKeyRotationPending_CountsRetainedVersions(t *testing.T) {
	f := setupWholeKeyFixture(t, "Enabled")
	f.putSSEV(t, "k", []byte("one"))
	f.putSSEV(t, "k", []byte("two"))
	f.putSSEV(t, "k", []byte("three"))
	f.rotate(t)
	assert.Equal(t, int64(3), f.pendingWhole(t), "the head's blob + two retained versions")
}

// GET and HEAD ?versionId: a retained version answers from its own row; the
// 501 is kept only for a version written before C1 (no address).
func TestVersionId_HeadAndGet_501OnlyWithoutAddress(t *testing.T) {
	f := setupLedgerFixture(t)
	cleanupRetired(t, f)
	f.versioned("b")
	v1 := f.putV("b", "k", "one")
	v2 := f.putV("b", "k", "version two, longer")

	head := func(vid string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		f.server.handleHeadObject(w, f.req("HEAD", "/b/k?versionId="+vid, nil), &S3Request{Bucket: "b", Object: "k", TenantID: f.tenantID})
		return w
	}
	h := head(v1)
	require.Equal(t, http.StatusOK, h.Code, h.Body.String())
	assert.Equal(t, strconv.Itoa(len("one")), h.Header().Get("Content-Length"), "HEAD ?versionId describes that version")
	assert.Equal(t, `"`+etagOf([]byte("one"))+`"`, h.Header().Get("ETag"))
	assert.Equal(t, v1, h.Header().Get("x-amz-version-id"))
	h = head(v2)
	require.Equal(t, http.StatusOK, h.Code)
	assert.Equal(t, strconv.Itoa(len("version two, longer")), h.Header().Get("Content-Length"))
	assert.Equal(t, v2, h.Header().Get("x-amz-version-id"))
	assert.Equal(t, http.StatusNotFound, head("no-such-version").Code)

	// A version written before C1: no address, its bytes were overwritten.
	_, err := f.db.Exec(`UPDATE object_versions SET storage_key = NULL WHERE tenant_id = $1 AND version_id = $2`, f.tenantID, v1)
	require.NoError(t, err)
	assert.Equal(t, http.StatusNotImplemented, f.getV("b", "k", v1).Code)
	assert.Equal(t, http.StatusNotImplemented, head(v1).Code)
}
