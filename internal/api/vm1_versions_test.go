package api

// WP-VM-1 (Stage 1a Prompt 7), Part C and the folded residuals: every
// reader knows a chunked version — GET / HEAD / Range / If-* ?versionId,
// CopyObject of a version, ListObjectVersions — and R61-5 (a Suspended
// DELETE), R61-8 (a version keeps its attributes; the copy-source
// conditions), R85-2 (HEAD names the current version). "Before" = these
// tests on the Part 0 branch.

import (
	"bytes"
	"encoding/xml"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// vbody is n bytes distinct per seed and per fixture (the tenant id leads
// it: the plaintext content index is shared by every test).
func vbody(f *mpChunkFixture, n int, seed byte) []byte {
	b := mpBody(n, seed)
	copy(b, f.tenantID)
	return b
}

// req is one S3 request of the fixture's tenant through the router.
func (f *mpChunkFixture) req(t *testing.T, method, target string, body []byte, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var rd *bytes.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	} else {
		rd = bytes.NewReader(nil)
	}
	r := httptest.NewRequest(method, target, rd)
	if body != nil {
		r.ContentLength = int64(len(body))
	}
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	r = r.WithContext(s3Ctx(r.Context(), f.tenant))
	w := httptest.NewRecorder()
	f.server.handleS3Request(w, r)
	return w
}

// preVersioningChunked writes key chunked while the bucket is unversioned,
// with metadata, then enables versioning and overwrites it whole: the
// chunked object is the key's null version (PM-3).
func (f *mpChunkFixture) preVersioningChunked(t *testing.T, key string) []byte {
	t.Helper()
	a := vbody(f, 4096, 1)
	w := f.req(t, "PUT", "/test-bucket/"+key, a, map[string]string{
		"x-amz-meta-origin": "first", "Content-Disposition": "attachment; filename=\"a.bin\"", "Cache-Control": "max-age=60"})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	chunked, _ := f.head(t, key)
	require.True(t, chunked)
	f.versioning(t, "Enabled")
	w = f.req(t, "PUT", "/test-bucket/"+key, []byte("small whole overwrite"), map[string]string{"x-amz-meta-origin": "second"})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	return a
}

func TestVM1_GetAndHeadOfAChunkedVersion(t *testing.T) {
	f := setupMPChunkFixture(t, false)
	a := f.preVersioningChunked(t, "obj.bin")

	g := f.req(t, "GET", "/test-bucket/obj.bin?versionId=null", nil, nil)
	require.Equal(t, http.StatusOK, g.Code, g.Body.String())
	assert.Equal(t, a, g.Body.Bytes())
	assert.Equal(t, "null", g.Header().Get("x-amz-version-id"))
	etag := g.Header().Get("ETag")
	require.NotEmpty(t, etag)
	assert.Equal(t, "first", g.Header().Get("x-amz-meta-origin"), "the version's own metadata (R61-8)")
	assert.Equal(t, "max-age=60", g.Header().Get("Cache-Control"))
	assert.Contains(t, g.Header().Get("Content-Disposition"), "a.bin")

	rg := f.req(t, "GET", "/test-bucket/obj.bin?versionId=null", nil, map[string]string{"Range": "bytes=100-299"})
	require.Equal(t, http.StatusPartialContent, rg.Code, rg.Body.String())
	assert.Equal(t, a[100:300], rg.Body.Bytes())

	nm := f.req(t, "GET", "/test-bucket/obj.bin?versionId=null", nil, map[string]string{"If-None-Match": etag})
	assert.Equal(t, http.StatusNotModified, nm.Code)
	pf := f.req(t, "GET", "/test-bucket/obj.bin?versionId=null", nil, map[string]string{"If-Match": `"nope"`})
	assert.Equal(t, http.StatusPreconditionFailed, pf.Code)

	h := f.req(t, "HEAD", "/test-bucket/obj.bin?versionId=null", nil, nil)
	require.Equal(t, http.StatusOK, h.Code)
	assert.Equal(t, "4096", h.Header().Get("Content-Length"))
	assert.Equal(t, etag, h.Header().Get("ETag"))
	assert.Equal(t, "first", h.Header().Get("x-amz-meta-origin"))
	assert.Equal(t, "STANDARD", h.Header().Get("x-amz-storage-class"))

	cur := f.req(t, "GET", "/test-bucket/obj.bin", nil, nil)
	assert.Equal(t, "small whole overwrite", cur.Body.String())
	assert.Equal(t, "second", cur.Header().Get("x-amz-meta-origin"))
}

func TestVM1_ListObjectVersionsShowsAChunkedVersion(t *testing.T) {
	f := setupMPChunkFixture(t, false)
	a := f.preVersioningChunked(t, "obj.bin")

	w := f.req(t, "GET", "/test-bucket?versions", nil, nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var res ListVersionsResult
	require.NoError(t, xml.Unmarshal(w.Body.Bytes(), &res))
	var null *VersionEntry
	for i := range res.Versions {
		if res.Versions[i].VersionId == "null" {
			null = &res.Versions[i]
		}
	}
	require.NotNil(t, null, "the chunked null version is listed")
	assert.Equal(t, int64(len(a)), null.Size)
	assert.False(t, null.IsLatest)
	assert.Equal(t, "STANDARD", null.StorageClass)
	assert.Len(t, res.Versions, 2)
}

func TestVM1_CopyOfAChunkedVersionRestoresIt(t *testing.T) {
	// The S3 way to restore a version: copy it onto its key. The source is
	// a chunked version (its own manifest); the versioned destination keeps
	// it whole.
	f := setupMPChunkFixture(t, false)
	a := f.preVersioningChunked(t, "obj.bin")

	w := f.req(t, "PUT", "/test-bucket/obj.bin", nil, map[string]string{"x-amz-copy-source": "/test-bucket/obj.bin?versionId=null"})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, "null", w.Header().Get("x-amz-copy-source-version-id"))

	cur := f.req(t, "GET", "/test-bucket/obj.bin", nil, nil)
	require.Equal(t, http.StatusOK, cur.Code)
	assert.Equal(t, a, cur.Body.Bytes())
	assert.Equal(t, "first", cur.Header().Get("x-amz-meta-origin"), "the copy carries the version's attributes (R61-8)")
	assert.Equal(t, a, f.getBytes(t, "obj.bin", "null"), "the source version stays")
	f.ledgerAgrees(t, "after the restore")
}

func TestVM1_CopyOfAChunkedVersionIntoAnUnversionedBucketIsAManifestCopy(t *testing.T) {
	f := setupMPChunkFixture(t, false)
	a := f.preVersioningChunked(t, "obj.bin")
	_, err := f.db.Exec(`INSERT INTO buckets (tenant_id, name, visibility) VALUES ($1, 'dest-bucket', 'private') ON CONFLICT (tenant_id, name) DO NOTHING`, f.tenantID)
	require.NoError(t, err)

	w := f.req(t, "PUT", "/dest-bucket/copy.bin", nil, map[string]string{"x-amz-copy-source": "/test-bucket/obj.bin?versionId=null"})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var chunked bool
	require.NoError(t, f.db.QueryRow(`SELECT is_chunked FROM object_head_cache WHERE tenant_id = $1 AND bucket = 'dest-bucket' AND object_key = 'copy.bin'`,
		f.tenantID).Scan(&chunked))
	assert.True(t, chunked, "a manifest copy where a PUT of the bytes would chunk")
	g := f.req(t, "GET", "/dest-bucket/copy.bin", nil, nil)
	require.Equal(t, http.StatusOK, g.Code)
	assert.Equal(t, a, g.Body.Bytes())
	f.ledgerAgrees(t, "after the manifest copy")
}

func TestVM1_R61_8_AWholeVersionKeepsItsAttributes(t *testing.T) {
	f := setupMPChunkFixture(t, false)
	f.versioning(t, "Enabled")
	w := f.req(t, "PUT", "/test-bucket/doc.txt", []byte("v1"), map[string]string{
		"x-amz-meta-rev": "1", "Content-Language": "fr", "Expires": "Wed, 21 Oct 2026 07:28:00 GMT"})
	require.Equal(t, http.StatusOK, w.Code)
	v1 := w.Header().Get("x-amz-version-id")
	require.Equal(t, http.StatusOK, f.req(t, "PUT", "/test-bucket/doc.txt", []byte("v2"), map[string]string{"x-amz-meta-rev": "2"}).Code)

	g := f.req(t, "GET", "/test-bucket/doc.txt?versionId="+v1, nil, nil)
	require.Equal(t, http.StatusOK, g.Code)
	assert.Equal(t, "1", g.Header().Get("x-amz-meta-rev"))
	assert.Equal(t, "fr", g.Header().Get("Content-Language"))
	assert.Equal(t, "Wed, 21 Oct 2026 07:28:00 GMT", g.Header().Get("Expires"))
	h := f.req(t, "HEAD", "/test-bucket/doc.txt?versionId="+v1, nil, nil)
	assert.Equal(t, "1", h.Header().Get("x-amz-meta-rev"))

	// Deleting the current version re-points the head row to v1 with its
	// own attributes.
	require.Equal(t, http.StatusNoContent, f.req(t, "DELETE", "/test-bucket/doc.txt?versionId="+mustLatest(t, f, "doc.txt"), nil, nil).Code)
	cur := f.req(t, "GET", "/test-bucket/doc.txt", nil, nil)
	assert.Equal(t, "v1", cur.Body.String())
	assert.Equal(t, "1", cur.Header().Get("x-amz-meta-rev"))
	assert.Equal(t, "fr", cur.Header().Get("Content-Language"))
}

func mustLatest(t *testing.T, f *mpChunkFixture, key string) string {
	t.Helper()
	var vid string
	require.NoError(t, f.db.QueryRow(`SELECT version_id FROM object_versions WHERE tenant_id = $1 AND object_key = $2 AND is_latest`,
		f.tenantID, key).Scan(&vid))
	return vid
}

func TestVM1_R61_8_CopySourceConditions(t *testing.T) {
	f := setupMPChunkFixture(t, false)
	w := f.req(t, "PUT", "/test-bucket/src.txt", []byte("source"), nil)
	require.Equal(t, http.StatusOK, w.Code)
	etag := w.Header().Get("ETag")
	past := time.Now().Add(-48 * time.Hour).UTC().Format(http.TimeFormat)
	future := time.Now().Add(48 * time.Hour).UTC().Format(http.TimeFormat)
	cases := []struct {
		name string
		hdr  map[string]string
		want int
	}{
		{"if-match holds", map[string]string{"x-amz-copy-source-if-match": etag}, http.StatusOK},
		{"if-match fails", map[string]string{"x-amz-copy-source-if-match": `"other"`}, http.StatusPreconditionFailed},
		{"if-none-match fails", map[string]string{"x-amz-copy-source-if-none-match": etag}, http.StatusPreconditionFailed},
		{"if-none-match holds", map[string]string{"x-amz-copy-source-if-none-match": `"other"`}, http.StatusOK},
		{"if-modified-since fails", map[string]string{"x-amz-copy-source-if-modified-since": future}, http.StatusPreconditionFailed},
		{"if-unmodified-since fails", map[string]string{"x-amz-copy-source-if-unmodified-since": past}, http.StatusPreconditionFailed},
		{"if-match holds over if-unmodified-since", map[string]string{"x-amz-copy-source-if-match": etag, "x-amz-copy-source-if-unmodified-since": past}, http.StatusOK},
		{"if-none-match fails whatever if-modified-since", map[string]string{"x-amz-copy-source-if-none-match": etag, "x-amz-copy-source-if-modified-since": past}, http.StatusPreconditionFailed},
	}
	for i, c := range cases {
		hdr := map[string]string{"x-amz-copy-source": "/test-bucket/src.txt"}
		for k, v := range c.hdr {
			hdr[k] = v
		}
		dst := "/test-bucket/dst-" + strings.ReplaceAll(c.name, " ", "-") + "-" + string(rune('a'+i))
		cw := f.req(t, "PUT", dst, nil, hdr)
		assert.Equal(t, c.want, cw.Code, "%s: %s", c.name, cw.Body.String())
	}
}

func TestVM1_R85_2_HeadNamesTheCurrentVersion(t *testing.T) {
	f := setupMPChunkFixture(t, false)
	f.versioning(t, "Enabled")
	w := f.req(t, "PUT", "/test-bucket/k.txt", []byte("v1"), nil)
	require.Equal(t, http.StatusOK, w.Code)
	vid := w.Header().Get("x-amz-version-id")
	require.NotEmpty(t, vid)

	h := f.req(t, "HEAD", "/test-bucket/k.txt", nil, nil)
	require.Equal(t, http.StatusOK, h.Code)
	assert.Equal(t, vid, h.Header().Get("x-amz-version-id"), "as GET does")
	assert.Equal(t, vid, f.req(t, "GET", "/test-bucket/k.txt", nil, nil).Header().Get("x-amz-version-id"))

	// Behind a delete marker: HEAD 404 says so.
	d := f.req(t, "DELETE", "/test-bucket/k.txt", nil, nil)
	require.Equal(t, http.StatusNoContent, d.Code)
	h = f.req(t, "HEAD", "/test-bucket/k.txt", nil, nil)
	assert.Equal(t, http.StatusNotFound, h.Code)
	assert.Equal(t, "true", h.Header().Get("x-amz-delete-marker"))
	assert.Equal(t, d.Header().Get("x-amz-version-id"), h.Header().Get("x-amz-version-id"))

	// Suspended: the current object is the null version.
	f.versioning(t, "Suspended")
	require.Equal(t, http.StatusOK, f.req(t, "PUT", "/test-bucket/s.txt", []byte("s"), nil).Code)
	assert.Equal(t, "null", f.req(t, "HEAD", "/test-bucket/s.txt", nil, nil).Header().Get("x-amz-version-id"))
}

func TestVM1_R61_5_ASuspendedDeleteReplacesTheNullVersion_Whole(t *testing.T) {
	// AWS: a DELETE with no version id on a Suspended bucket replaces the
	// null version with a null delete marker; its bytes go. Before: the
	// head row went and the null version stayed — live in the listing,
	// GET ?versionId=null 200, its bytes billed.
	f := setupMPChunkFixture(t, false)
	f.versioning(t, "Suspended")
	require.Equal(t, http.StatusOK, f.req(t, "PUT", "/test-bucket/k.txt", []byte("null version bytes"), nil).Code)
	require.Equal(t, int64(18), f.used(t))

	d := f.req(t, "DELETE", "/test-bucket/k.txt", nil, nil)
	require.Equal(t, http.StatusNoContent, d.Code)
	assert.Equal(t, "true", d.Header().Get("x-amz-delete-marker"))
	assert.Equal(t, "null", d.Header().Get("x-amz-version-id"))

	assert.Equal(t, http.StatusNotFound, f.req(t, "GET", "/test-bucket/k.txt", nil, nil).Code)
	assert.Equal(t, http.StatusNotFound, f.req(t, "GET", "/test-bucket/k.txt?versionId=null", nil, nil).Code, "the null version is the marker now")
	assert.Zero(t, f.used(t), "its bytes are released")
	f.ledgerAgrees(t, "after the Suspended delete")
	var markers, live, retired int
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FILTER (WHERE is_delete_marker), COUNT(*) FILTER (WHERE NOT is_delete_marker)
		FROM object_versions WHERE tenant_id = $1 AND object_key = 'k.txt'`, f.tenantID).Scan(&markers, &live))
	assert.Equal(t, 1, markers)
	assert.Zero(t, live)
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM retired_blobs WHERE tenant_id = $1`, f.tenantID).Scan(&retired))
	assert.Equal(t, 1, retired, "retired, not deleted in the request")
}

func TestVM1_R61_5_ASuspendedDeleteReplacesTheNullVersion_Chunked(t *testing.T) {
	f := setupMPChunkFixture(t, false)
	require.Equal(t, http.StatusOK, mustCode(f.putBody(t, "obj.bin", vbody(f, 4096, 1))))
	f.versioning(t, "Suspended")

	d := f.req(t, "DELETE", "/test-bucket/obj.bin", nil, nil)
	require.Equal(t, http.StatusNoContent, d.Code)
	assert.Equal(t, "null", d.Header().Get("x-amz-version-id"))
	assert.Equal(t, http.StatusNotFound, f.req(t, "GET", "/test-bucket/obj.bin", nil, nil).Code)
	assert.Zero(t, f.used(t))
	f.ledgerAgrees(t, "after the Suspended delete")
	assert.Equal(t, 1, f.retiredManifests(t), "the manifest is retired")
	f.collectAfterGrace(t)
	assert.Zero(t, f.retiredManifests(t))
}

func TestVM1_R61_5_ASuspendedDeleteKeepsAnEnabledEraVersion(t *testing.T) {
	// The current object is an Enabled-era version (a real id): AWS keeps
	// it as a non-current version behind the null marker.
	f := setupMPChunkFixture(t, false)
	f.versioning(t, "Enabled")
	w := f.req(t, "PUT", "/test-bucket/k.txt", []byte("enabled-era"), nil)
	require.Equal(t, http.StatusOK, w.Code)
	vid := w.Header().Get("x-amz-version-id")
	f.versioning(t, "Suspended")

	require.Equal(t, http.StatusNoContent, f.req(t, "DELETE", "/test-bucket/k.txt", nil, nil).Code)

	g := f.req(t, "GET", "/test-bucket/k.txt?versionId="+vid, nil, nil)
	require.Equal(t, http.StatusOK, g.Code, g.Body.String())
	assert.Equal(t, "enabled-era", g.Body.String())
	assert.Equal(t, int64(11), f.used(t), "the version keeps its bill")
	f.ledgerAgrees(t, "behind the null marker")
}

func TestVM1_ObjectLock_ALockedChunkedVersionIsNeitherDeletedNorRetired(t *testing.T) {
	f := setupMPChunkFixture(t, false)
	a := f.preVersioningChunked(t, "obj.bin")
	_, nullManifest := f.versionAddress(t, "obj.bin", "null")
	_, err := f.db.Exec(`INSERT INTO object_locks (tenant_id, bucket, object_key, retention_mode, retain_until_date)
		VALUES ($1, 'test-bucket', 'obj.bin', 'COMPLIANCE', NOW() + INTERVAL '30 days')`, f.tenantID)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = f.db.Exec(`DELETE FROM object_locks WHERE tenant_id = $1`, f.tenantID) })

	assert.Equal(t, http.StatusForbidden, f.req(t, "DELETE", "/test-bucket/obj.bin?versionId=null", nil, nil).Code)
	assert.Equal(t, http.StatusForbidden, f.req(t, "DELETE", "/test-bucket/obj.bin", nil, nil).Code)

	// Even a retired row a buggy path might queue is kept by the collector
	// while the version names the manifest.
	_, err = f.db.Exec(`INSERT INTO retired_blobs (backend_id, container, storage_key, tenant_id, reason, retire_after)
		VALUES ('chunked', $1, $2, $3, 'overwritten', NOW() - INTERVAL '1 day')`, f.tenantID+"_test-bucket", nullManifest, f.tenantID)
	require.NoError(t, err)
	f.collectAfterGrace(t)
	refs, _ := f.manifestRows(t, nullManifest)
	assert.Positive(t, refs, "never released while the version exists")
	assert.Equal(t, a, f.getBytes(t, "obj.bin", "null"))
}

// versionAddress is a version row's backend and address.
func (f *mpChunkFixture) versionAddress(t *testing.T, key, vid string) (backend, storageKey string) {
	t.Helper()
	require.NoError(t, f.db.QueryRow(`SELECT COALESCE(backend_name, ''), COALESCE(storage_key, '') FROM object_versions
		WHERE tenant_id = $1 AND object_key = $2 AND version_id = $3`, f.tenantID, key, vid).Scan(&backend, &storageKey))
	return backend, storageKey
}
