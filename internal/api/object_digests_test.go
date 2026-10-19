package api

// WP-R8-3 A.4 + RP14-11 (Stage 1a Prompt 14 Part A, migration 094): what the
// row knows about the bytes it describes.
//
// body_md5 — a multipart upload completed whole has a `-N` ETag (the MD5 of
// the parts' MD5s), so the whole-object read path (WP-R8-3 A.1) could only
// check its size: every SDK sends anything over 8 MiB as multipart, so most
// large whole objects were served unverified. Complete's whole branch now
// tees the staged-parts stream into an MD5 and records it on the head row
// and the version row; every whole 200 verifies COALESCE(body_md5, ETag).
//
// ssec_key_md5 — the MD5 of the customer's SSE-C key, which AWS keeps with
// the object, was kept nowhere: HEAD with a wrong key answered 200 (cache-
// only; the documented deviation) and GET 403 only after the blob failed to
// open. Recorded at write time, a wrong key is 403 from the row on HEAD and
// GET, before any backend call and with no object header.

import (
	"context"
	"crypto/md5" // #nosec G501 — S3 ETags and the SSE-C key MD5 are MD5
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FairForge/vaultaire/internal/crypto"
)

// digests reads a head row's ETag and its two 094 columns.
func (f *mpChunkFixture) digests(t *testing.T, key string) (etag string, bodyMD5, keyMD5 sql.NullString) {
	t.Helper()
	require.NoError(t, f.db.QueryRow(`SELECT etag, body_md5, ssec_key_md5 FROM object_head_cache
		WHERE tenant_id = $1 AND bucket = 'test-bucket' AND object_key = $2`, f.tenantID, key).Scan(&etag, &bodyMD5, &keyMD5))
	return etag, bodyMD5, keyMD5
}

// versionDigests reads a version row's ETag, address and 094 columns.
func (f *mpChunkFixture) versionDigests(t *testing.T, key, vid string) (etag, storageKey string, bodyMD5, keyMD5 sql.NullString) {
	t.Helper()
	require.NoError(t, f.db.QueryRow(`SELECT etag, COALESCE(storage_key, ''), body_md5, ssec_key_md5 FROM object_versions
		WHERE tenant_id = $1 AND bucket = 'test-bucket' AND object_key = $2 AND version_id = $3`, f.tenantID, key, vid).
		Scan(&etag, &storageKey, &bodyMD5, &keyMD5))
	return etag, storageKey, bodyMD5, keyMD5
}

// rewriteBlobAt replaces the bytes of the blob stored at address under dir.
func rewriteBlobAt(t *testing.T, dir, address string, b []byte) {
	t.Helper()
	var hits []string
	require.NoError(t, filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() && filepath.Base(p) == address {
			hits = append(hits, p)
		}
		return nil
	}))
	require.Len(t, hits, 1, "one blob at %s", address)
	require.NoError(t, os.WriteFile(hits[0], b, 0o600))
}

func ssecKey(t *testing.T) []byte {
	t.Helper()
	k := make([]byte, 32)
	_, err := rand.Read(k)
	require.NoError(t, err)
	return k
}

// --- A.4: body_md5 -----------------------------------------------------------

func TestBodyDigest_ACompletedWholeMultipartRecordsTheWholeBodysMD5OnHeadAndVersion(t *testing.T) {
	// Arrange: a versioned bucket (stored whole), a body in three parts.
	f, _ := setupWholeFixture(t, false)
	f.bucketSet(t, "versioning_status = 'Enabled'")
	body := mpBody(300<<10, 7)
	parts := splitParts(body, 3)

	// Act
	u := f.upload(t, "mp.bin", parts, nil)
	w := f.complete(t, "mp.bin", u)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	vid := w.Header().Get("x-amz-version-id")

	// Assert: the ETag stays -N; the head row and the version row carry the
	// MD5 of the whole body.
	etag, bodyMD5, keyMD5 := f.digests(t, "mp.bin")
	assert.Equal(t, awsMultipartETag(parts), etag)
	require.True(t, bodyMD5.Valid, "body_md5 recorded")
	assert.Equal(t, md5Hex(body), bodyMD5.String)
	assert.False(t, keyMD5.Valid, "not SSE-C")
	require.NotEmpty(t, vid)
	vETag, _, vBody, _ := f.versionDigests(t, "mp.bin", vid)
	assert.Equal(t, etag, vETag)
	require.True(t, vBody.Valid, "the version row carries it too")
	assert.Equal(t, md5Hex(body), vBody.String)
}

func TestBodyDigest_AWholeGetOfAMultipartObjectIsVerified(t *testing.T) {
	// Arrange: a -N object stored whole.
	f, logs := setupWholeFixture(t, false)
	body := randomBody(t, 256<<10)
	f.multipart(t, "mp.bin", body, 4, nil)
	ts := integrityServer(t, f)
	unverified0 := integrityUnverified(integrityPathPlain, "multipart")
	fail0 := integrityFailures(integrityPathPlain, "md5")

	// Act + Assert: unaltered → served whole and verified (nothing counted).
	resp, got, readErr := clientGet(t, ts, "/test-bucket/mp.bin", nil)
	require.NoError(t, readErr)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, body, got)
	assert.Equal(t, unverified0, integrityUnverified(integrityPathPlain, "multipart"), "a -N object with its body_md5 is verified, not counted unverified")
	assert.Equal(t, fail0, integrityFailures(integrityPathPlain, "md5"))

	// The backend now serves other bytes of the right length: cut before
	// the last byte, counted, one Error line naming the key.
	other := randomBody(t, len(body))
	rewriteBlob(t, f.fixed.dir, "mp.bin", other)
	resp, got, readErr = clientGet(t, ts, "/test-bucket/mp.bin", nil)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Error(t, readErr, "the body is cut")
	assert.Len(t, got, len(body)-1)
	assert.Equal(t, fail0+1, integrityFailures(integrityPathPlain, "md5"))
	lines := logs.FilterMessage("object integrity: the body served does not match its row — the response is cut").All()
	require.Len(t, lines, 1)
	// RP16-6: the line names the -N ETag and, apart, the whole-body digest
	// the bytes were checked against (it used to call that digest
	// recorded_etag).
	fl := lines[0].ContextMap()
	assert.Equal(t, md5Hex(body), fl["expected_md5"])
	assert.Contains(t, fl["etag"], "-4", "the row's multipart ETag")
	assert.NotContains(t, fl, "recorded_etag")
}

func TestBodyDigest_AVersionIdGetOfARetainedMultipartVersionVerifiesAgainstItsRow(t *testing.T) {
	// Arrange: v1 multipart, then a PUT displaces it; v1 keeps its bytes.
	f, _ := setupWholeFixture(t, false)
	f.bucketSet(t, "versioning_status = 'Enabled'")
	v1 := randomBody(t, 200<<10)
	u := f.upload(t, "k.bin", splitParts(v1, 2), nil)
	w := f.complete(t, "k.bin", u)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	vid := w.Header().Get("x-amz-version-id")
	require.NotEmpty(t, vid)
	f.putWhole(t, "k.bin", randomBody(t, 64<<10), nil)
	ts := integrityServer(t, f)
	fail0 := integrityFailures(integrityPathPlain, "md5")

	// Act + Assert: the version reads back whole …
	resp, got, readErr := clientGet(t, ts, "/test-bucket/k.bin?versionId="+vid, nil)
	require.NoError(t, readErr)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, v1, got)
	// … and its blob altered at rest is cut, against the VERSION row's digest.
	_, addr, vBody, _ := f.versionDigests(t, "k.bin", vid)
	require.True(t, vBody.Valid)
	rewriteBlobAt(t, f.fixed.dir, addr, randomBody(t, len(v1)))
	resp, got, readErr = clientGet(t, ts, "/test-bucket/k.bin?versionId="+vid, nil)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Error(t, readErr)
	assert.Len(t, got, len(v1)-1)
	assert.Equal(t, fail0+1, integrityFailures(integrityPathPlain, "md5"))
}

func TestBodyDigest_ACopyOfAMultipartSourceIsASinglePartObjectVerifiedByItsETag(t *testing.T) {
	// Arrange: a -N source.
	f, _ := setupWholeFixture(t, false)
	body := randomBody(t, 128<<10)
	f.multipart(t, "src.bin", body, 3, nil)

	// Act: CopyObject whole.
	w := f.req(t, "PUT", "/test-bucket/dst.bin", nil, map[string]string{"x-amz-copy-source": "/test-bucket/src.bin"})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	// Assert: the destination's ETag is the MD5 of the bytes written (as on
	// AWS: a copy is a single-part object), so body_md5 stays NULL and the
	// read verifies against the ETag.
	etag, bodyMD5, _ := f.digests(t, "dst.bin")
	assert.Equal(t, md5Hex(body), etag)
	assert.False(t, bodyMD5.Valid)
	ts := integrityServer(t, f)
	fail0 := integrityFailures(integrityPathPlain, "md5")
	_, got, readErr := clientGet(t, ts, "/test-bucket/dst.bin", nil)
	require.NoError(t, readErr)
	assert.Equal(t, body, got)
	rewriteBlob(t, f.fixed.dir, "dst.bin", randomBody(t, len(body)))
	_, got, readErr = clientGet(t, ts, "/test-bucket/dst.bin", nil)
	assert.Error(t, readErr)
	assert.Len(t, got, len(body)-1)
	assert.Equal(t, fail0+1, integrityFailures(integrityPathPlain, "md5"))
}

func TestBodyDigest_TheWholeReencryptKeepsTheDigestAndTheReadStaysVerified(t *testing.T) {
	// Arrange: a sealed -N object (an sse_enabled bucket, a master key), its
	// tenant key then rotated.
	f, _ := setupWholeFixture(t, true)
	f.bucketSet(t, "sse_enabled = TRUE")
	body := randomBody(t, 200<<10)
	f.multipart(t, "sealed.bin", body, 2, nil)
	etag0, bodyMD5, _ := f.digests(t, "sealed.bin")
	require.True(t, bodyMD5.Valid)
	assert.Equal(t, md5Hex(body), bodyMD5.String)
	addr0 := f.row(t, "sealed.bin").storageKey
	require.Equal(t, crypto.SSEAlgorithm, f.row(t, "sealed.bin").enc)
	_, err := f.keys.Rotate(context.Background(), f.tenantID)
	require.NoError(t, err)
	k := NewKeyReencryptRunner(f.db, f.server.engine, f.server.gci, f.server.chunkEncSvc, f.server.logger)
	require.NotNil(t, k)
	k.sse = f.server.sseService
	k.JobName = "test_key_reencrypt_" + f.tenantID[:8]
	k.onlyScope = f.tenantID
	t.Cleanup(func() { _, _ = f.db.Exec(`DELETE FROM job_runs WHERE job = $1`, k.JobName) })

	// Act: the whole pass re-seals it at a new address.
	_, err = k.RunOnce(context.Background())
	require.NoError(t, err)

	// Assert: the digest and the ETag are the plaintext's and unchanged;
	// the read verifies against them.
	etag1, bodyMD51, _ := f.digests(t, "sealed.bin")
	assert.Equal(t, etag0, etag1)
	require.True(t, bodyMD51.Valid, "the re-encrypt keeps body_md5")
	assert.Equal(t, bodyMD5.String, bodyMD51.String)
	assert.NotEqual(t, addr0, f.row(t, "sealed.bin").storageKey, "re-sealed at a new address")
	ts := integrityServer(t, f)
	fail0 := integrityFailures(integrityPathSealed, "md5")
	_, got, readErr := clientGet(t, ts, "/test-bucket/sealed.bin", nil)
	require.NoError(t, readErr)
	assert.Equal(t, body, got)
	assert.Equal(t, fail0, integrityFailures(integrityPathSealed, "md5"))
}

func TestBodyDigest_SinglePartWritersLeaveItNull(t *testing.T) {
	// Arrange + Act: a PUT (its ETag is the digest).
	f, _ := setupWholeFixture(t, false)
	body := randomBody(t, 32<<10)
	f.putWhole(t, "one.bin", body, nil)

	// Assert
	etag, bodyMD5, keyMD5 := f.digests(t, "one.bin")
	assert.Equal(t, md5Hex(body), etag)
	assert.False(t, bodyMD5.Valid)
	assert.False(t, keyMD5.Valid)
}

// --- RP14-11: ssec_key_md5 ---------------------------------------------------

func TestSSECKeyMD5_APutRecordsTheKeysMD5OnHeadAndVersion(t *testing.T) {
	// Arrange
	f, _ := setupWholeFixture(t, false)
	f.bucketSet(t, "versioning_status = 'Enabled'")
	key := ssecKey(t)
	sum := md5.Sum(key) // #nosec G401
	want := base64.StdEncoding.EncodeToString(sum[:])

	// Act
	w := f.req(t, "PUT", "/test-bucket/c.bin", randomBody(t, 8<<10), ssecHeaders(key))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	vid := w.Header().Get("x-amz-version-id")

	// Assert: the key's MD5 (what the request's header carried and the
	// write verified), never the key, on both rows.
	_, bodyMD5, keyMD5 := f.digests(t, "c.bin")
	assert.False(t, bodyMD5.Valid)
	require.True(t, keyMD5.Valid)
	assert.Equal(t, want, keyMD5.String)
	require.NotEmpty(t, vid)
	_, _, _, vKey := f.versionDigests(t, "c.bin", vid)
	require.True(t, vKey.Valid)
	assert.Equal(t, want, vKey.String)
	var leaked int
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM object_head_cache WHERE tenant_id = $1 AND ssec_key_md5 = $2`,
		f.tenantID, base64.StdEncoding.EncodeToString(key)).Scan(&leaked))
	assert.Zero(t, leaked, "the key itself is stored nowhere")
}

func TestSSECKeyMD5_HeadAndGetWithAnotherKeyAre403BeforeAnyBackendCall(t *testing.T) {
	// Arrange: an SSE-C object; the backend counts its reads.
	f, _ := setupWholeFixture(t, false)
	key, other := ssecKey(t), ssecKey(t)
	body := randomBody(t, 8<<10)
	require.Equal(t, http.StatusOK, f.req(t, "PUT", "/test-bucket/c.bin", body, ssecHeaders(key)).Code)
	var reads int
	f.fixed.onGet = func(string, string) { reads++ }

	// Act + Assert: a wrong key is 403 on HEAD and GET, from the row alone,
	// with no object header and no backend call.
	for _, method := range []string{"HEAD", "GET"} {
		w := f.req(t, method, "/test-bucket/c.bin", nil, ssecHeaders(other))
		assert.Equal(t, http.StatusForbidden, w.Code, "%s with a wrong key: %s", method, w.Body.String())
		assert.Empty(t, w.Header().Get("ETag"), "%s: no object header", method)
		assert.Empty(t, w.Header().Get("x-amz-server-side-encryption-customer-algorithm"), "%s: no object header", method)
		assert.Empty(t, w.Header().Get("Last-Modified"), "%s: no object header", method)
	}
	assert.Zero(t, reads, "no backend call for a key the row refuses")
	// The right key: 200 as today. No key: the 400 of Prompt 13.
	w := f.req(t, "HEAD", "/test-bucket/c.bin", nil, ssecHeaders(key))
	assert.Equal(t, http.StatusOK, w.Code)
	w = f.req(t, "GET", "/test-bucket/c.bin", nil, ssecHeaders(key))
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, body, w.Body.Bytes())
	assert.Equal(t, http.StatusBadRequest, f.req(t, "HEAD", "/test-bucket/c.bin", nil, nil).Code)
	assert.Equal(t, http.StatusBadRequest, f.req(t, "GET", "/test-bucket/c.bin", nil, nil).Code)
}

func TestSSECKeyMD5_ARowWrittenBefore094KeepsTodaysAnswers(t *testing.T) {
	// Arrange: an SSE-C object whose row has no key MD5 (written before).
	f, _ := setupWholeFixture(t, false)
	key, other := ssecKey(t), ssecKey(t)
	require.Equal(t, http.StatusOK, f.req(t, "PUT", "/test-bucket/old.bin", randomBody(t, 8<<10), ssecHeaders(key)).Code)
	_, err := f.db.Exec(`UPDATE object_head_cache SET ssec_key_md5 = NULL WHERE tenant_id = $1 AND bucket = 'test-bucket' AND object_key = 'old.bin'`, f.tenantID)
	require.NoError(t, err)
	var reads int
	f.fixed.onGet = func(string, string) { reads++ }

	// Act + Assert: HEAD cannot tell (200, the deviation); GET refuses once
	// the blob fails to open (403 after the read).
	assert.Equal(t, http.StatusOK, f.req(t, "HEAD", "/test-bucket/old.bin", nil, ssecHeaders(other)).Code)
	assert.Equal(t, http.StatusForbidden, f.req(t, "GET", "/test-bucket/old.bin", nil, ssecHeaders(other)).Code)
	assert.Equal(t, 1, reads, "the blob was read to find the key wrong")
}

func TestSSECKeyMD5_AVersionIdUsesTheVersionRows(t *testing.T) {
	// Arrange: v1 under key one, v2 under key two.
	f, _ := setupWholeFixture(t, false)
	f.bucketSet(t, "versioning_status = 'Enabled'")
	one, two := ssecKey(t), ssecKey(t)
	b1 := randomBody(t, 8<<10)
	w := f.req(t, "PUT", "/test-bucket/v.bin", b1, ssecHeaders(one))
	require.Equal(t, http.StatusOK, w.Code)
	vid1 := w.Header().Get("x-amz-version-id")
	require.Equal(t, http.StatusOK, f.req(t, "PUT", "/test-bucket/v.bin", randomBody(t, 8<<10), ssecHeaders(two)).Code)

	// Act + Assert: v1 answers to key one only; the current object to key two.
	assert.Equal(t, http.StatusForbidden, f.req(t, "HEAD", "/test-bucket/v.bin?versionId="+vid1, nil, ssecHeaders(two)).Code)
	assert.Equal(t, http.StatusOK, f.req(t, "HEAD", "/test-bucket/v.bin?versionId="+vid1, nil, ssecHeaders(one)).Code)
	assert.Equal(t, http.StatusForbidden, f.req(t, "GET", "/test-bucket/v.bin?versionId="+vid1, nil, ssecHeaders(two)).Code)
	g := f.req(t, "GET", "/test-bucket/v.bin?versionId="+vid1, nil, ssecHeaders(one))
	assert.Equal(t, http.StatusOK, g.Code)
	assert.Equal(t, b1, g.Body.Bytes())
	assert.Equal(t, http.StatusForbidden, f.req(t, "HEAD", "/test-bucket/v.bin", nil, ssecHeaders(one)).Code)
	assert.Equal(t, http.StatusOK, f.req(t, "HEAD", "/test-bucket/v.bin", nil, ssecHeaders(two)).Code)
}

// --- RP16-1 (WP-P14 post-merge review): the repointed head row -------------

func TestBodyDigest_ARepointedHeadRowCarriesTheNewLatestVersionsDigest(t *testing.T) {
	// DELETE ?versionId of the CURRENT version re-points the head row to
	// the newly-latest version (repointHeadAfterVersionDelete). It copied
	// every attribute of that version but the 094 columns: the head row
	// kept the DELETED version's body_md5 and every whole GET of the
	// re-pointed object was cut against the wrong digest.
	f, _ := setupWholeFixture(t, false)
	f.bucketSet(t, "versioning_status = 'Enabled'")
	v1 := randomBody(t, 200<<10)
	u := f.upload(t, "k.bin", splitParts(v1, 2), nil)
	require.Equal(t, http.StatusOK, f.complete(t, "k.bin", u).Code)
	v2 := randomBody(t, 160<<10)
	u = f.upload(t, "k.bin", splitParts(v2, 2), nil)
	w := f.complete(t, "k.bin", u)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	vid2 := w.Header().Get("x-amz-version-id")
	require.NotEmpty(t, vid2)

	// Act: delete the current version; v1 is the object again.
	require.Equal(t, http.StatusNoContent, f.req(t, "DELETE", "/test-bucket/k.bin?versionId="+vid2, nil, nil).Code)

	// Assert: the head row carries v1's digest, and a whole GET serves v1
	// to the last byte instead of being cut.
	_, bodyMD5, _ := f.digests(t, "k.bin")
	require.True(t, bodyMD5.Valid, "the re-pointed head row has a digest")
	assert.Equal(t, md5Hex(v1), bodyMD5.String, "the digest is the newly-latest version's")
	ts := integrityServer(t, f)
	fail0 := integrityFailures(integrityPathPlain, "md5")
	resp, got, readErr := clientGet(t, ts, "/test-bucket/k.bin", nil)
	require.NoError(t, readErr, "the whole GET is served, not cut")
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, v1, got)
	assert.Equal(t, fail0, integrityFailures(integrityPathPlain, "md5"))
}

func TestSSECKeyMD5_ARepointedHeadRowAnswersToTheNewLatestVersionsKey(t *testing.T) {
	// The SSE-C half of the same repoint: the head row kept the deleted
	// version's ssec_key_md5, so the right key was 403 from the row.
	f, _ := setupWholeFixture(t, false)
	f.bucketSet(t, "versioning_status = 'Enabled'")
	one, two := ssecKey(t), ssecKey(t)
	b1 := randomBody(t, 8<<10)
	require.Equal(t, http.StatusOK, f.req(t, "PUT", "/test-bucket/v.bin", b1, ssecHeaders(one)).Code)
	w := f.req(t, "PUT", "/test-bucket/v.bin", randomBody(t, 8<<10), ssecHeaders(two))
	require.Equal(t, http.StatusOK, w.Code)
	vid2 := w.Header().Get("x-amz-version-id")
	require.NotEmpty(t, vid2)

	// Act
	require.Equal(t, http.StatusNoContent, f.req(t, "DELETE", "/test-bucket/v.bin?versionId="+vid2, nil, nil).Code)

	// Assert: the current object is v1 again — its own key answers, the
	// deleted version's key is refused.
	assert.Equal(t, http.StatusOK, f.req(t, "HEAD", "/test-bucket/v.bin", nil, ssecHeaders(one)).Code)
	g := f.req(t, "GET", "/test-bucket/v.bin", nil, ssecHeaders(one))
	assert.Equal(t, http.StatusOK, g.Code, g.Body.String())
	assert.Equal(t, b1, g.Body.Bytes())
	assert.Equal(t, http.StatusForbidden, f.req(t, "HEAD", "/test-bucket/v.bin", nil, ssecHeaders(two)).Code)
	assert.Equal(t, http.StatusForbidden, f.req(t, "GET", "/test-bucket/v.bin", nil, ssecHeaders(two)).Code)
}
