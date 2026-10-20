package api

import (
	"bytes"
	"crypto/rand"
	"encoding/xml"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/FairForge/vaultaire/internal/crypto"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// WP-SSEC-1 B.1–B.4 (Stage 1a Prompt 17 Part B): SSE-C multipart uploads —
// parts sealed at UploadPart under the key in the request, Complete without
// a key writing the 0x05 container — read whole and by range, HEAD's headers,
// UploadPartCopy and CopyObject with keys on either side, and the key
// rotation by a copy onto the same key.

func mpKey(seed byte) []byte { return bytes.Repeat([]byte{seed}, 32) }

func mpRandom(n int) []byte {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return b
}

// mpSSEC runs initiate → parts → complete with the key on every request
// but Complete, as aws-cli does.
func (f *versioningFixture) mpSSEC(t *testing.T, key string, k []byte, parts ...[]byte) (uploadID string, etags []string) {
	t.Helper()
	uploadID = f.mpInitiate(t, key, ssecHeaders(k))
	for i, p := range parts {
		w := f.s3(t, "PUT", fmt.Sprintf("/%s/%s?partNumber=%d&uploadId=%s", f.bucket, key, i+1, uploadID), bytes.NewReader(p), ssecHeaders(k))
		require.Equal(t, http.StatusOK, w.Code, "part %d: %s", i+1, w.Body.String())
		etags = append(etags, w.Header().Get("ETag"))
	}
	return uploadID, etags
}

func (f *versioningFixture) getWith(t *testing.T, key string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	return f.s3(t, "GET", "/"+f.bucket+"/"+key, nil, hdr)
}

func TestSSECMultipart_RoundTripThroughAwsCliShapedRequests(t *testing.T) {
	// Arrange: three parts, the key on Create and every part, none on
	// Complete.
	f := setupVersioningFixture(t)
	k := mpKey(0x11)
	parts := [][]byte{mpRandom(crypto.StreamSegmentSize + 4321), mpRandom(5000), mpRandom(2*crypto.StreamSegmentSize + 1)}
	plain := bytes.Join(parts, nil)
	uploadID, etags := f.mpSSEC(t, "big.bin", k, parts...)

	// Act: Complete, no key.
	w := f.mpComplete(t, "big.bin", uploadID, completeBody(etags...))

	// Assert: the -N ETag, the SSE-C headers on the answer, a 0x05 blob on
	// the backend holding none of the plaintext, the row.
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Contains(t, w.Header().Get("ETag"), "-3")
	assert.Equal(t, "AES256", w.Header().Get("x-amz-server-side-encryption-customer-algorithm"))
	assert.Equal(t, crypto.SSECKeyMD5(k), w.Header().Get("x-amz-server-side-encryption-customer-key-MD5"))
	var enc, keyMD5 string
	var size int64
	var bodyMD5 *string
	require.NoError(t, f.db.QueryRow(`SELECT encryption_algorithm, COALESCE(ssec_key_md5, ''), size_bytes, body_md5 FROM object_head_cache WHERE tenant_id = $1 AND bucket = $2 AND object_key = 'big.bin'`,
		f.tenantID, f.bucket).Scan(&enc, &keyMD5, &size, &bodyMD5))
	assert.Equal(t, crypto.SSECAlgorithmMultipart, enc)
	assert.Equal(t, crypto.SSECKeyMD5(k), keyMD5)
	assert.Equal(t, int64(len(plain)), size, "the plaintext size")
	assert.Nil(t, bodyMD5, "no whole plaintext digest exists for parts sealed one by one")
	blobs, _ := filepath.Glob(filepath.Join(f.tempDir, f.tenant.NamespaceContainer(f.bucket), "big.bin@*"))
	require.Len(t, blobs, 1)
	raw, err := os.ReadFile(blobs[0])
	require.NoError(t, err)
	assert.Equal(t, crypto.MultipartFormat, raw[0])
	assert.Equal(t, crypto.SSECMultipartSealedLength([]int64{int64(len(parts[0])), int64(len(parts[1])), int64(len(parts[2]))}), int64(len(raw)))
	assert.False(t, bytes.Contains(raw, parts[1]), "plaintext on the backend")

	// HEAD: the SSE-C headers; a wrong key 403; none 400.
	h := f.s3(t, "HEAD", "/"+f.bucket+"/big.bin", nil, ssecHeaders(k))
	require.Equal(t, http.StatusOK, h.Code)
	assert.Equal(t, "AES256", h.Header().Get("x-amz-server-side-encryption-customer-algorithm"))
	assert.Equal(t, crypto.SSECKeyMD5(k), h.Header().Get("x-amz-server-side-encryption-customer-key-MD5"))
	assert.Equal(t, fmt.Sprint(len(plain)), h.Header().Get("Content-Length"))
	assert.Equal(t, http.StatusForbidden, f.s3(t, "HEAD", "/"+f.bucket+"/big.bin", nil, ssecHeaders(mpKey(0x22))).Code)
	assert.Equal(t, http.StatusBadRequest, f.s3(t, "HEAD", "/"+f.bucket+"/big.bin", nil, nil).Code)

	// GET whole, byte-equal; wrong key 403; none 400.
	g := f.getWith(t, "big.bin", ssecHeaders(k))
	require.Equal(t, http.StatusOK, g.Code, g.Body.String())
	assert.Equal(t, plain, g.Body.Bytes())
	assert.Equal(t, crypto.SSECKeyMD5(k), g.Header().Get("x-amz-server-side-encryption-customer-key-MD5"))
	assert.Equal(t, http.StatusForbidden, f.getWith(t, "big.bin", ssecHeaders(mpKey(0x22))).Code)
	assert.Equal(t, http.StatusBadRequest, f.getWith(t, "big.bin", nil).Code)

	// Ranges: across the part-1/part-2 boundary, and inside part 3's last
	// segment.
	b := len(parts[0])
	rng := func(from, to int) []byte {
		hdr := ssecHeaders(k)
		hdr["Range"] = fmt.Sprintf("bytes=%d-%d", from, to)
		w := f.getWith(t, "big.bin", hdr)
		require.Equal(t, http.StatusPartialContent, w.Code, w.Body.String())
		assert.Equal(t, fmt.Sprintf("bytes %d-%d/%d", from, to, len(plain)), w.Header().Get("Content-Range"))
		return w.Body.Bytes()
	}
	assert.Equal(t, plain[b-777:b+778], rng(b-777, b+777))
	last := len(plain) - 100
	assert.Equal(t, plain[last:last+50], rng(last, last+49))
	assert.Equal(t, plain[:1], rng(0, 0))
	assert.Equal(t, plain[len(plain)-1:], rng(len(plain)-1, len(plain)-1))
}

func TestSSECMultipart_APartMustCarryTheUploadsKey(t *testing.T) {
	f := setupVersioningFixture(t)
	k := mpKey(0x11)
	uploadID := f.mpInitiate(t, "k.bin", ssecHeaders(k))
	part := func(hdr map[string]string) *httptest.ResponseRecorder {
		return f.s3(t, "PUT", fmt.Sprintf("/%s/k.bin?partNumber=1&uploadId=%s", f.bucket, uploadID), bytes.NewReader(mpRandom(1000)), hdr)
	}
	staged := func() int {
		var n int
		require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM multipart_parts WHERE upload_id = $1`, uploadID).Scan(&n))
		entries, _ := os.ReadDir(multipartDir(uploadID))
		return n + len(entries)
	}

	// A different key: 400 InvalidArgument, nothing staged.
	w := part(ssecHeaders(mpKey(0x22)))
	assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "InvalidArgument")
	assert.Zero(t, staged())
	// No key: 400, nothing staged.
	w = part(nil)
	assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	assert.Zero(t, staged())
	// The right key: 200, the part staged sealed (the file is the sealed
	// length, the row the plaintext size).
	w = part(ssecHeaders(k))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var size int64
	var sealedMD5 string
	require.NoError(t, f.db.QueryRow(`SELECT size_bytes, sealed_md5 FROM multipart_parts WHERE upload_id = $1 AND part_number = 1`, uploadID).Scan(&size, &sealedMD5))
	assert.Equal(t, int64(1000), size)
	assert.Len(t, sealedMD5, 32)
	st, err := os.Stat(partFilePath(uploadID, 1))
	require.NoError(t, err)
	assert.Equal(t, crypto.SSECSealedLength(1000), st.Size())

	// A key on a plain upload: 400.
	plainID := f.mpInitiate(t, "p.bin", nil)
	w = f.s3(t, "PUT", fmt.Sprintf("/%s/p.bin?partNumber=1&uploadId=%s", f.bucket, plainID), bytes.NewReader(mpRandom(10)), ssecHeaders(k))
	assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())

	// Create with SSE-S3 and SSE-C together: 400.
	hdr := ssecHeaders(k)
	hdr["x-amz-server-side-encryption"] = "AES256"
	assert.Equal(t, http.StatusBadRequest, f.s3(t, "POST", "/"+f.bucket+"/both.bin?uploads", nil, hdr).Code)
}

func TestSSECMultipart_ATamperedSealedPartIsCutAtGetNeverServedWrong(t *testing.T) {
	// RP15-1's lab shape: the blob's bytes change at rest after Complete.
	f := setupVersioningFixture(t)
	k := mpKey(0x11)
	parts := [][]byte{mpRandom(crypto.StreamSegmentSize + 10), mpRandom(crypto.StreamSegmentSize + 10)}
	uploadID, etags := f.mpSSEC(t, "t.bin", k, parts...)
	require.Equal(t, http.StatusOK, f.mpComplete(t, "t.bin", uploadID, completeBody(etags...)).Code)
	blobs, _ := filepath.Glob(filepath.Join(f.tempDir, f.tenant.NamespaceContainer(f.bucket), "t.bin@*"))
	require.Len(t, blobs, 1)
	raw, err := os.ReadFile(blobs[0])
	require.NoError(t, err)
	h, err := crypto.ParseMultipartHeader(raw)
	require.NoError(t, err)
	raw[h.StreamOffset(1)+int64(crypto.StreamHeaderSSEC)+5] ^= 0xff // inside part 2's first segment
	require.NoError(t, os.WriteFile(blobs[0], raw, 0o600))

	// Act: the 200 is cut (http.ErrAbortHandler) after part 1, never a
	// wrong byte.
	req := httptest.NewRequest("GET", "/"+f.bucket+"/t.bin", nil)
	for kk, v := range ssecHeaders(k) {
		req.Header.Set(kk, v)
	}
	req = req.WithContext(s3Ctx(req.Context(), f.tenant))
	w := httptest.NewRecorder()
	require.PanicsWithValue(t, http.ErrAbortHandler, func() { f.server.handleS3Request(w, req) })
	assert.Equal(t, http.StatusOK, w.Code, "the status line went out before part 2")
	assert.Equal(t, parts[0], w.Body.Bytes(), "part 1 whole, nothing of part 2")

	// A tampered part caught at Complete too: a staged part changed before
	// Complete is never committed.
	uploadID2, etags2 := f.mpSSEC(t, "u.bin", k, mpRandom(3000))
	p := partFilePath(uploadID2, 1)
	staged, _ := os.ReadFile(p)
	staged[len(staged)-1] ^= 0xff
	require.NoError(t, os.WriteFile(p, staged, 0o600))
	w2 := f.mpComplete(t, "u.bin", uploadID2, completeBody(etags2...))
	assert.NotEqual(t, http.StatusOK, w2.Code, w2.Body.String())
	var n int
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM object_head_cache WHERE tenant_id = $1 AND bucket = $2 AND object_key = 'u.bin'`, f.tenantID, f.bucket).Scan(&n))
	assert.Zero(t, n)
}

func TestSSECMultipart_UploadPartCopyFromA0x04AndA0x05Source(t *testing.T) {
	f := setupVersioningFixture(t)
	srcKey, dstKey := mpKey(0x11), mpKey(0x33)
	// A 0x04 source (a single SSE-C PUT) and a 0x05 source (a multipart).
	single := mpRandom(crypto.StreamSegmentSize + 99)
	require.Equal(t, http.StatusOK, f.s3(t, "PUT", "/"+f.bucket+"/single.bin", bytes.NewReader(single), ssecHeaders(srcKey)).Code)
	mparts := [][]byte{mpRandom(crypto.StreamSegmentSize/2 + 1), mpRandom(crypto.StreamSegmentSize + 7)}
	mid, metags := f.mpSSEC(t, "multi.bin", srcKey, mparts...)
	require.Equal(t, http.StatusOK, f.mpComplete(t, "multi.bin", mid, completeBody(metags...)).Code)
	multi := bytes.Join(mparts, nil)

	// Act: an SSE-C upload under ANOTHER key takes part 1 from the single
	// source and part 2 as a range of the multipart source that crosses
	// its part boundary.
	uploadID := f.mpInitiate(t, "copied.bin", ssecHeaders(dstKey))
	copyPart := func(n int, src string, rng string) string {
		hdr := ssecHeaders(dstKey)
		hdr["x-amz-copy-source"] = "/" + f.bucket + "/" + src
		for kk, v := range ssecCopySourceHeaders(srcKey) {
			hdr[kk] = v
		}
		if rng != "" {
			hdr["x-amz-copy-source-range"] = rng
		}
		w := f.s3(t, "PUT", fmt.Sprintf("/%s/copied.bin?partNumber=%d&uploadId=%s", f.bucket, n, uploadID), nil, hdr)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		var res CopyPartResult
		require.NoError(t, xml.Unmarshal(w.Body.Bytes(), &res))
		return res.ETag
	}
	b := len(mparts[0])
	e1 := copyPart(1, "single.bin", "")
	e2 := copyPart(2, "multi.bin", fmt.Sprintf("bytes=%d-%d", b-100, b+100))

	// A copy part without the source key: 400; with a wrong one: 403
	// (while the upload is still active).
	hdr := ssecHeaders(dstKey)
	hdr["x-amz-copy-source"] = "/" + f.bucket + "/single.bin"
	assert.Equal(t, http.StatusBadRequest, f.s3(t, "PUT", fmt.Sprintf("/%s/copied.bin?partNumber=3&uploadId=%s", f.bucket, uploadID), nil, hdr).Code)
	for kk, v := range ssecCopySourceHeaders(mpKey(0x44)) {
		hdr[kk] = v
	}
	assert.Equal(t, http.StatusForbidden, f.s3(t, "PUT", fmt.Sprintf("/%s/copied.bin?partNumber=3&uploadId=%s", f.bucket, uploadID), nil, hdr).Code)

	w := f.mpComplete(t, "copied.bin", uploadID, completeBody(e1, e2))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	// Assert: byte-equal under the destination key.
	want := append(append([]byte(nil), single...), multi[b-100:b+101]...)
	g := f.getWith(t, "copied.bin", ssecHeaders(dstKey))
	require.Equal(t, http.StatusOK, g.Code)
	assert.Equal(t, want, g.Body.Bytes())
}

func TestSSECMultipart_CopyObject0x05To0x04AndTheKeyRotation(t *testing.T) {
	f := setupVersioningFixture(t)
	k1, k2 := mpKey(0x11), mpKey(0x22)
	parts := [][]byte{mpRandom(crypto.StreamSegmentSize + 3), mpRandom(777)}
	plain := bytes.Join(parts, nil)
	mid, etags := f.mpSSEC(t, "m.bin", k1, parts...)
	require.Equal(t, http.StatusOK, f.mpComplete(t, "m.bin", mid, completeBody(etags...)).Code)

	// Act: 0x05 → 0x04 under a new key, to another key.
	hdr := ssecHeaders(k2)
	for kk, v := range ssecCopySourceHeaders(k1) {
		hdr[kk] = v
	}
	w := f.copyObject(t, "m.bin", "c.bin", hdr)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var enc, keyMD5 string
	require.NoError(t, f.db.QueryRow(`SELECT encryption_algorithm, COALESCE(ssec_key_md5, '') FROM object_head_cache WHERE tenant_id = $1 AND bucket = $2 AND object_key = 'c.bin'`, f.tenantID, f.bucket).Scan(&enc, &keyMD5))
	assert.Equal(t, crypto.SSECAlgorithmV3, enc, "a copy is one 0x04 stream whatever the source")
	assert.Equal(t, crypto.SSECKeyMD5(k2), keyMD5)
	g := f.getWith(t, "c.bin", ssecHeaders(k2))
	require.Equal(t, http.StatusOK, g.Code)
	assert.Equal(t, plain, g.Body.Bytes())

	// Without the source key: 400; a wrong source key: 403; a source key on
	// a plaintext source is ignored.
	assert.Equal(t, http.StatusBadRequest, f.copyObject(t, "m.bin", "d.bin", ssecHeaders(k2)).Code)
	bad := ssecHeaders(k2)
	for kk, v := range ssecCopySourceHeaders(mpKey(0x44)) {
		bad[kk] = v
	}
	assert.Equal(t, http.StatusForbidden, f.copyObject(t, "m.bin", "d.bin", bad).Code)

	// The documented rotation: the object copied onto itself under a NEW
	// key. Then the new key reads it and the old one is 403.
	rot := ssecHeaders(k2)
	for kk, v := range ssecCopySourceHeaders(k1) {
		rot[kk] = v
	}
	rot["x-amz-metadata-directive"] = "REPLACE"
	w = f.copyObject(t, "m.bin", "m.bin", rot)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	g = f.getWith(t, "m.bin", ssecHeaders(k2))
	require.Equal(t, http.StatusOK, g.Code, g.Body.String())
	assert.Equal(t, plain, g.Body.Bytes())
	assert.Equal(t, http.StatusForbidden, f.getWith(t, "m.bin", ssecHeaders(k1)).Code)

	// A plaintext source copied into an SSE-C destination: sealed.
	require.Equal(t, http.StatusOK, f.s3(t, "PUT", "/"+f.bucket+"/plain.txt", strings.NewReader("hello plaintext"), nil).Code)
	require.Equal(t, http.StatusOK, f.copyObject(t, "plain.txt", "sealed.txt", ssecHeaders(k2)).Code)
	g = f.getWith(t, "sealed.txt", ssecHeaders(k2))
	require.Equal(t, http.StatusOK, g.Code)
	assert.Equal(t, "hello plaintext", g.Body.String())
	assert.Equal(t, http.StatusBadRequest, f.getWith(t, "sealed.txt", nil).Code)
}

func ssecCopySourceHeaders(key []byte) map[string]string {
	out := map[string]string{}
	for k, v := range ssecHeaders(key) {
		out[strings.Replace(k, "x-amz-server-side-encryption", "x-amz-copy-source-server-side-encryption", 1)] = v
	}
	return out
}
