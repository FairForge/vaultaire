package api

// WP-R8-5 (keystone K1, = WP-R3-3): every SSE-S3 path seals and opens as it
// streams (blob format 0x03, internal/crypto/stream.go; sse_stream.go). "Over
// the old cap" (256 MiB, a 413 or a whole-object buffer before) is many
// segments here: the service seals 1 KiB (or smaller) segments, so a body of
// tens of KiB is the shape a 300 MiB one is at 1 MiB — the live proof drives
// the real sizes and the RSS.

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5" // #nosec G501 — S3 ETag semantics
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/FairForge/vaultaire/internal/common"
	"github.com/FairForge/vaultaire/internal/crypto"
	"github.com/FairForge/vaultaire/internal/drivers"
	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/FairForge/vaultaire/internal/usage"
	promtest "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"golang.org/x/crypto/hkdf"
)

// sealedBlob seals pt as every writer does (0x03) and returns the blob and
// its key version.
func sealedBlob(t *testing.T, sse *crypto.SSEService, tenantID string, pt []byte) ([]byte, int) {
	t.Helper()
	r, v, err := sse.Seal(context.Background(), tenantID, bytes.NewReader(pt), int64(len(pt)))
	require.NoError(t, err)
	blob, err := io.ReadAll(r)
	require.NoError(t, err)
	return blob, v
}

// legacySSEV2Blob is a 0x02 blob exactly as the writers before WP-R8-5
// (EncryptBytes) made it, from the tenant's current key under
// keyTestMasterA — a fixture written with its own primitives, so the
// compatibility opener is tested against the old bytes, not against itself.
func legacySSEV2Blob(t *testing.T, keys *crypto.TenantKeys, tenantID string, pt []byte) []byte {
	t.Helper()
	kv, err := keys.Current(context.Background(), tenantID)
	require.NoError(t, err)
	shared, kemCT := kv.Encap.Encapsulate()
	aesKey := make([]byte, 32)
	_, err = io.ReadFull(hkdf.New(sha256.New, shared, nil, []byte("vaultaire-sse-s3")), aesKey)
	require.NoError(t, err)
	block, err := aes.NewCipher(aesKey)
	require.NoError(t, err)
	gcm, err := cipher.NewGCM(block)
	require.NoError(t, err)
	nonce := make([]byte, 12)
	_, _ = rand.Read(nonce)
	header := binary.BigEndian.AppendUint32([]byte{crypto.SSEVersion}, uint32(kv.Version)) // #nosec G115 -- test
	out := append(append(append([]byte(nil), header...), kemCT...), nonce...)
	return gcm.Seal(out, nonce, pt, header)
}

// rangedFixedDriver is the fixed-bucket double with a real ranged read, and
// counts: whole Gets, ranged reads and the bytes those ask for.
type rangedFixedDriver struct {
	*fixedBucketDriver
	gets, ranges atomic.Int32
	rangedBytes  atomic.Int64
}

func (d *rangedFixedDriver) Get(ctx context.Context, container, artifact string) (io.ReadCloser, error) {
	d.gets.Add(1)
	return d.fixedBucketDriver.Get(ctx, container, artifact)
}

func (d *rangedFixedDriver) GetRange(ctx context.Context, container, artifact string, off, n int64) (io.ReadCloser, error) {
	d.ranges.Add(1)
	d.rangedBytes.Add(n)
	rc, err := d.fixedBucketDriver.Get(ctx, container, artifact)
	if err != nil {
		return nil, err
	}
	if _, err := io.CopyN(io.Discard, rc, off); err != nil {
		_ = rc.Close()
		return nil, err
	}
	return &readCloser{Reader: io.LimitReader(rc, n), Closer: rc}, nil
}

func (d *rangedFixedDriver) reset() {
	d.gets.Store(0)
	d.ranges.Store(0)
	d.rangedBytes.Store(0)
}

// sealedFixture: the multipart fixture (fixed-bucket primary, a key) with a
// versioned sse_enabled bucket — a non-chunkable placement, so every write
// is whole and sealed — 1 KiB segments, and the ranged driver as primary.
type sealedFixture struct {
	*mpChunkFixture
	rd *rangedFixedDriver
}

const sealedSeg = 1 << 10

func setupSealedFixture(t *testing.T) *sealedFixture {
	t.Helper()
	f := setupMPChunkFixture(t, true)
	instantCommit(f.server)
	f.server.sseService.SetSegmentSize(sealedSeg)
	f.bucketSet(t, "sse_enabled = TRUE, versioning_status = 'Enabled'")
	rd := &rangedFixedDriver{fixedBucketDriver: f.fixed}
	f.server.engine.AddDriver("fixed", rd)
	return &sealedFixture{mpChunkFixture: f, rd: rd}
}

// blobFile is the one blob file of key (the newest when several).
func (f *sealedFixture) blobFile(t *testing.T, key string) string {
	t.Helper()
	sk := f.row(t, key).storageKey
	require.NotEmpty(t, sk)
	var found string
	require.NoError(t, filepath.WalkDir(f.fixed.dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(filepath.ToSlash(p), "/"+sk) {
			found = p
		}
		return nil
	}))
	require.NotEmpty(t, found, "the blob of %s at %s", key, sk)
	return found
}

func (f *sealedFixture) putWith(t *testing.T, key string, body []byte, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	return httptestPut(f.mpChunkFixture, key, body, hdr)
}

// getRecovering is a GET whose body may be cut after the status line (the
// http.ErrAbortHandler panic a handler with no middleware raises).
func (f *sealedFixture) getRecovering(t *testing.T, key, rng string) (w *httptest.ResponseRecorder, cut bool) {
	t.Helper()
	r := httptest.NewRequest("GET", "/test-bucket/"+key, nil)
	if rng != "" {
		r.Header.Set("Range", rng)
	}
	r = r.WithContext(s3Ctx(r.Context(), f.tenant))
	w = httptest.NewRecorder()
	defer func() {
		if p := recover(); p != nil {
			require.Equal(t, http.ErrAbortHandler, p)
			cut = true
		}
	}()
	f.server.handleS3Request(w, r)
	return w, false
}

func TestPut_SSEBucket_OverTheOldCap_SealedStreaming(t *testing.T) {
	// Before (origin/main 59e2769): a PUT over maxSSEObjectSize into an
	// sse_enabled non-chunkable placement was 413; under it, io.ReadAll of
	// the body and one GCM over it.
	f := setupSealedFixture(t)
	body := mpBody(300<<10+123, 9) // 301 segments
	w := f.putWith(t, "big.bin", body, map[string]string{"x-amz-storage-class": "GLACIER"})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, "AES256", w.Header().Get("x-amz-server-side-encryption"))

	row := f.row(t, "big.bin")
	assert.Equal(t, crypto.SSEAlgorithm, row.enc)
	assert.Equal(t, usage.FloorVault, row.floor)
	assert.Equal(t, int64(len(body)), row.size, "the plaintext size")
	sum := md5.Sum(body) // #nosec G401
	assert.Equal(t, hex.EncodeToString(sum[:]), row.etag, "the plaintext MD5")
	raw, err := os.ReadFile(f.blobFile(t, "big.bin"))
	require.NoError(t, err)
	require.Equal(t, crypto.StreamFormat, raw[0])
	assert.Equal(t, f.server.sseService.SealedLength(int64(len(body))), int64(len(raw)), "the length function")
	assert.Zero(t, plaintextOnBackend(t, f.fixed.dir, f.tempDir))
	assert.Equal(t, int64(len(body)), f.used(t))

	g := f.get(t, "big.bin", "")
	require.Equal(t, http.StatusOK, g.Code)
	assert.Equal(t, body, g.Body.Bytes())
	assert.Equal(t, "AES256", g.Header().Get("x-amz-server-side-encryption"))
	h := doS3Request(f.server, f.tenant, "HEAD", "/test-bucket/big.bin", nil)
	assert.Equal(t, "AES256", h.Header().Get("x-amz-server-side-encryption"))
}

// A body that ends early or runs long under the sealer is the client's 400,
// nothing stored, no breaker charge — and a Content-MD5 mismatch retires the
// written blob (as the plain path does).
func TestPut_Sealed_ShortBodyAndBadDigest(t *testing.T) {
	f := setupSealedFixture(t)
	body := mpBody(5000, 3)
	r := httptest.NewRequest("PUT", "/test-bucket/short.bin", bytes.NewReader(body[:3000]))
	r.ContentLength = int64(len(body))
	r = r.WithContext(f.ctx(r.Context()))
	w := httptest.NewRecorder()
	f.server.handleS3Request(w, r)
	assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), ErrIncompleteBody)
	assert.Equal(t, int64(0), f.used(t))

	w = f.putWith(t, "digest.bin", body, map[string]string{"Content-MD5": "AAAAAAAAAAAAAAAAAAAAAA=="})
	assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), ErrBadDigest)
	var orphans int
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM retired_blobs WHERE tenant_id = $1 AND reason = 'orphan_write'`, f.tenantID).Scan(&orphans))
	assert.Equal(t, 1, orphans, "the written blob is queued for the collector")
}

func TestGet_SealedObject_RangeBySegment(t *testing.T) {
	f := setupSealedFixture(t)
	body := mpBody(12*sealedSeg+300, 5)
	require.Equal(t, http.StatusOK, f.putWith(t, "r.bin", body, nil).Code)
	raw, err := os.ReadFile(f.blobFile(t, "r.bin"))
	require.NoError(t, err)
	h, err := crypto.ParseStreamHeader(raw)
	require.NoError(t, err)
	size := int64(len(body))

	for name, rg := range map[string][2]int64{
		"first byte":          {0, 1},
		"crossing a boundary": {sealedSeg - 3, 7},
		"last byte":           {size - 1, 1},
		"whole last segment":  {12 * sealedSeg, 300},
		"three middle segs":   {2*sealedSeg + 10, 2 * sealedSeg},
	} {
		t.Run(name, func(t *testing.T) {
			f.rd.reset()
			w := f.get(t, "r.bin", fmt.Sprintf("bytes=%d-%d", rg[0], rg[0]+rg[1]-1))
			require.Equal(t, http.StatusPartialContent, w.Code, w.Body.String())
			assert.Equal(t, body[rg[0]:rg[0]+rg[1]], w.Body.Bytes())
			assert.Equal(t, fmt.Sprintf("bytes %d-%d/%d", rg[0], rg[0]+rg[1]-1, size), w.Header().Get("Content-Range"))
			assert.Equal(t, "AES256", w.Header().Get("x-amz-server-side-encryption"))
			plan, err := crypto.PlanStreamRange(h, size, rg[0], rg[1])
			require.NoError(t, err)
			assert.Equal(t, int32(0), f.rd.gets.Load(), "never the whole blob")
			assert.Equal(t, int32(2), f.rd.ranges.Load(), "one header read + one read of the spanned segments")
			assert.Equal(t, int64(crypto.MaxStreamHeader)+plan.CipherLength, f.rd.rangedBytes.Load(),
				"the header and exactly the segments the range spans")
		})
	}
}

func TestGet_SealedObject_HeaderTamperedBeforeStatusLine_Clean500(t *testing.T) {
	f := setupSealedFixture(t)
	body := mpBody(6*sealedSeg, 6)
	require.Equal(t, http.StatusOK, f.putWith(t, "h.bin", body, nil).Code)
	p := f.blobFile(t, "h.bin")
	raw, err := os.ReadFile(p)
	require.NoError(t, err)
	raw[100] ^= 1 // inside the KEM ciphertext: the DEK changes, segment 0 fails
	require.NoError(t, os.WriteFile(p, raw, 0o600))

	for _, rng := range []string{"", "bytes=10-20"} {
		w, cut := f.getRecovering(t, "h.bin", rng)
		assert.False(t, cut, "nothing was sent to cut")
		assert.Equal(t, http.StatusInternalServerError, w.Code, "range %q", rng)
		assert.Contains(t, w.Body.String(), "InternalError")
		assert.NotContains(t, w.Body.String(), mpMarker)
	}
}

func TestGet_SealedObject_SegmentTamperedAfterStatusLine_CutsConnection(t *testing.T) {
	f := setupSealedFixture(t)
	body := mpBody(8*sealedSeg, 7)
	require.Equal(t, http.StatusOK, f.putWith(t, "s.bin", body, nil).Code)
	p := f.blobFile(t, "s.bin")
	raw, err := os.ReadFile(p)
	require.NoError(t, err)
	// Segment 5's ciphertext (the header is 1105 bytes, each segment 1040).
	raw[crypto.StreamHeaderSSES3+5*(sealedSeg+16)+7] ^= 0x40
	require.NoError(t, os.WriteFile(p, raw, 0o600))

	w, cut := f.getRecovering(t, "s.bin", "")
	assert.True(t, cut, "a segment that fails after the status line cuts the connection")
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, body[:5*sealedSeg], w.Body.Bytes(), "exactly the verified segments went out, never a byte of the bad one")
}

// The compatibility opener at the GET: a 0x02 blob (written before WP-R8-5)
// opens whole, a Range of it is sliced; the metric counts the format.
func TestOldBlobs_0x01_0x02_StillOpen(t *testing.T) {
	f := setupSealedFixture(t)
	countSSEOpens(f.server.sseService)
	body := mpBody(3000, 8)
	require.Equal(t, http.StatusOK, f.putWith(t, "old.bin", body, nil).Code)
	keys := testTenantKeys(t, f.db, keyTestMasterA)
	require.NoError(t, os.WriteFile(f.blobFile(t, "old.bin"), legacySSEV2Blob(t, keys, f.tenantID, body), 0o600))
	before := promtest.ToFloat64(sseBlobsOpened.WithLabelValues("0x02"))

	g := f.get(t, "old.bin", "")
	require.Equal(t, http.StatusOK, g.Code, g.Body.String())
	assert.Equal(t, body, g.Body.Bytes())
	g = f.get(t, "old.bin", "bytes=1000-1999")
	require.Equal(t, http.StatusPartialContent, g.Code, g.Body.String())
	assert.Equal(t, body[1000:2000], g.Body.Bytes())
	assert.Equal(t, before+2, promtest.ToFloat64(sseBlobsOpened.WithLabelValues("0x02")))
}

func TestCopy_WholeSource_OverTheOldCap_IntoSSEBucket_Sealed(t *testing.T) {
	// Before: 413 over maxSSEObjectSize (s3_copy.go:343), and every copy of
	// a sealed source 501.
	f := setupSealedFixture(t)
	f.bucketSet(t, "sse_enabled = FALSE")
	body := mpBody(40*sealedSeg+5, 2)
	require.Equal(t, http.StatusOK, f.putWith(t, "src.bin", body, nil).Code)
	require.Empty(t, f.row(t, "src.bin").enc, "a plaintext source")
	f.bucketSet(t, "sse_enabled = TRUE")

	copyTo := func(src, dst string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("PUT", "/test-bucket/"+dst, nil)
		r.Header.Set("x-amz-copy-source", "/test-bucket/"+src)
		r = r.WithContext(f.ctx(r.Context()))
		w := httptest.NewRecorder()
		f.server.handleS3Request(w, r)
		return w
	}
	w := copyTo("src.bin", "dst.bin")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	row := f.row(t, "dst.bin")
	assert.Equal(t, crypto.SSEAlgorithm, row.enc)
	assert.Equal(t, int64(len(body)), row.size)
	raw, err := os.ReadFile(f.blobFile(t, "dst.bin"))
	require.NoError(t, err)
	assert.Equal(t, f.server.sseService.SealedLength(int64(len(body))), int64(len(raw)))
	assert.Equal(t, body, f.get(t, "dst.bin", "").Body.Bytes())

	// A sealed source copies too (WP-R8-5: it was 501): opened as it
	// streams, sealed again for its destination.
	w = copyTo("dst.bin", "dst2.bin")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, crypto.SSEAlgorithm, f.row(t, "dst2.bin").enc)
	assert.Equal(t, row.etag, f.row(t, "dst2.bin").etag, "the plaintext's ETag")
	assert.Equal(t, body, f.get(t, "dst2.bin", "").Body.Bytes())
	// … into a plain bucket: the plaintext (AWS: the destination's setting).
	f.bucketSet(t, "sse_enabled = FALSE")
	w = copyTo("dst.bin", "plain.bin")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Empty(t, f.row(t, "plain.bin").enc)
	assert.Equal(t, body, f.get(t, "plain.bin", "").Body.Bytes())
}

func TestCreateMultipart_SSEHeader_PersistedAndHonouredAtComplete(t *testing.T) {
	// Before: the header on Create was dropped (PM-3): Complete placed the
	// upload as plaintext into a bucket that is not sse_enabled.
	f := setupSealedFixture(t)
	// The response headers of a Complete are delivered when it answers
	// before the keep-alive commits (after it, only the body is: s3_long_op.go).
	f.server.longOpThreshold, f.server.longOpInterval = 0, 0
	f.bucketSet(t, "sse_enabled = FALSE")
	hdr := map[string]string{"x-amz-server-side-encryption": "AES256"}
	c := f.create(t, "mp.bin", hdr)
	require.Equal(t, http.StatusOK, c.Code, c.Body.String())
	assert.Equal(t, "AES256", c.Header().Get("x-amz-server-side-encryption"))
	var init InitiateMultipartUploadResult
	require.NoError(t, xml.Unmarshal(c.Body.Bytes(), &init))
	var kept bool
	require.NoError(t, f.db.QueryRow(`SELECT sse_requested FROM multipart_uploads WHERE upload_id = $1`, init.UploadID).Scan(&kept))
	assert.True(t, kept, "089: the header is on the upload row")

	body := mpBody(9*sealedSeg, 1)
	f.server.sseService.SetSegmentSize(sealedSeg)
	u := f.upload(t, "mp2.bin", splitParts(body, 3), hdr)
	w := f.complete(t, "mp2.bin", u)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, "AES256", w.Header().Get("x-amz-server-side-encryption"))
	assert.Equal(t, crypto.SSEAlgorithm, f.row(t, "mp2.bin").enc)
	assert.Zero(t, plaintextOnBackend(t, f.fixed.dir, f.tempDir))
	assert.Equal(t, body, f.get(t, "mp2.bin", "").Body.Bytes())

	// Without the header, into the same bucket: plaintext, no header.
	u = f.upload(t, "plain.bin", splitParts(body, 3), nil)
	w = f.complete(t, "plain.bin", u)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Empty(t, w.Header().Get("x-amz-server-side-encryption"))
	assert.Empty(t, f.row(t, "plain.bin").enc)
}

// Part 0 W-4: a request asking AES256 on a process with no master key is
// refused, never stored plaintext with a 200. Before: PUT 200 plaintext.
func TestPut_SSEHeader_NoKey_Refused503(t *testing.T) {
	f := setupMPChunkFixture(t, false)
	body := probeBody(t, 900)
	w := httptestPut(f, "k.bin", body, map[string]string{"x-amz-server-side-encryption": "AES256"})
	assert.Equal(t, http.StatusServiceUnavailable, w.Code, w.Body.String())
	assert.NotEmpty(t, w.Header().Get("Retry-After"))
	assert.Empty(t, w.Header().Get("x-amz-server-side-encryption"))
	assert.Zero(t, plaintextOnBackend(t, f.fixed.dir, f.tempDir), "nothing stored")
	var n int
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM object_head_cache WHERE tenant_id = $1`, f.tenantID).Scan(&n))
	assert.Zero(t, n)

	c := f.create(t, "mp.bin", map[string]string{"x-amz-server-side-encryption": "AES256"})
	assert.Equal(t, http.StatusServiceUnavailable, c.Code, c.Body.String())
}

func TestKeyReencrypt_ReSealsAs0x03_Streaming(t *testing.T) {
	f := setupWholeKeyFixture(t, "Enabled") // versioned: whole and sealed, never chunked
	countSSEOpens(f.adapter.sseService)
	f.adapter.sseService.SetSegmentSize(256)
	old := bytes.Repeat([]byte("a 0x02 blob written before WP-R8-5 "), 40)
	cur := bytes.Repeat([]byte("a 0x03 blob of many segments "), 300)
	f.putSSEV(t, "old", old)
	f.putSSEV(t, "cur", cur)
	// The old one as the old writer left it: a 0x02 blob at its address.
	require.NoError(t, os.WriteFile(f.fixedBlob(t, "old"), legacySSEV2Blob(t, f.keys, f.tenantID, old), 0o600))
	f.rotate(t)
	before02 := promtest.ToFloat64(sseBlobsOpened.WithLabelValues("0x02"))

	res, err := f.k.RunOnce(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 2, res.ObjectsReencrypted, "%+v", res)

	for key, body := range map[string][]byte{"old": old, "cur": cur} {
		raw, err := os.ReadFile(f.fixedBlob(t, key))
		require.NoError(t, err)
		h, err := crypto.ParseStreamHeader(raw)
		require.NoError(t, err, key)
		assert.Equal(t, 2, h.KeyVersion, "%s: sealed under the newest version", key)
		assert.Equal(t, f.adapter.sseService.SealedLength(int64(len(body))), int64(len(raw)), key)
		g := f.getVersion(t, key, "")
		require.Equal(t, http.StatusOK, g.Code, g.Body.String())
		assert.Equal(t, body, g.Body.Bytes(), key)
	}
	assert.Equal(t, before02+1, promtest.ToFloat64(sseBlobsOpened.WithLabelValues("0x02")), "the 0x02 blob was opened once, by the job")
}

// fixedBlob is the file of key's current blob on the fixture's fixed-bucket
// primary.
func (f *wholeKeyFixture) fixedBlob(t *testing.T, key string) string {
	t.Helper()
	sk := f.head(t, key).storageKey
	require.NotEmpty(t, sk)
	return f.fixed.path(f.tenantID, f.tenant.NamespaceContainer("test-bucket"), sk)
}

// The vault parity of a sealed object: shards over the STORED bytes (the
// ciphertext), rebuilt as stored, then opened. Before: the job sized the
// layout by the plaintext and failed every sealed object ("longer than its
// recorded bytes"), so no sealed vault object ever had a parity copy.
func TestVaultParity_SealedObject_RebuiltThenOpened(t *testing.T) {
	f := setupParityFixture(t)
	sse, err := testSSEService(f.db, keyTestMasterA)
	require.NoError(t, err)
	sse.SetSegmentSize(4 << 10)
	t.Cleanup(func() { _, _ = f.db.Exec(`DELETE FROM tenant_key_versions WHERE tenant_id = $1`, f.tenantID) })
	f.svc.SSE = sse

	body := make([]byte, 300<<10+19)
	_, _ = rand.Read(body)
	sum := md5.Sum(body) // #nosec G401
	etag := hex.EncodeToString(sum[:])
	blob, v := sealedBlob(t, sse, f.tenantID, body)
	container := f.tn.NamespaceContainer(f.bucket)
	require.NoError(t, os.MkdirAll(filepath.Join(f.geyserDir, container), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(f.geyserDir, container, "sealed.bin"), blob, 0o600))
	_, err = f.db.Exec(`
		INSERT INTO object_head_cache (tenant_id, bucket, object_key, size_bytes, etag, backend_name, floor, is_chunked, content_type,
			encryption_algorithm, encryption_key_version)
		VALUES ($1,$2,'sealed.bin',$3,$4,'geyser','vault',false,'application/octet-stream',$5,$6)`,
		f.tenantID, f.bucket, len(body), etag, crypto.SSEAlgorithm, v)
	require.NoError(t, err)

	f.run()
	state, _, lastErr, _ := f.row("sealed.bin")
	require.Equal(t, "complete", state, lastErr)
	var paritySize int64
	require.NoError(t, f.db.QueryRow(`SELECT size_bytes FROM vault_parity WHERE tenant_id = $1 AND object_key = 'sealed.bin'`, f.tenantID).Scan(&paritySize))
	assert.Equal(t, int64(len(blob)), paritySize, "the parity covers the stored bytes")

	a := f.adapter()
	a.sseService = sse
	f.geyser.failGet.Store(true)
	f.geyser.failRange.Store(true)
	w := f.get(a, "sealed.bin", "")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, "parity", w.Header().Get("x-vaultaire-served-from"))
	assert.True(t, bytes.Equal(body, w.Body.Bytes()), "rebuilt ciphertext, opened")
	w = f.get(a, "sealed.bin", "bytes=100000-200000")
	require.Equal(t, http.StatusPartialContent, w.Code, w.Body.String())
	assert.Equal(t, "parity", w.Header().Get("x-vaultaire-served-from"))
	assert.True(t, bytes.Equal(body[100000:200001], w.Body.Bytes()))
}

func TestCDN_SealedPublicObject_StreamsAndRanges(t *testing.T) {
	f := setupCDNFixture(t)
	sse := sseFor(t, f.db, f.tenantID)
	sse.SetSegmentSize(64)
	content := mpBody(64*20+13, 4)
	ctx := common.WithTenantID(context.Background(), f.tenantID)
	ct, v := sealedBlob(t, sse, f.tenantID, content)
	_, err := f.server.engine.Put(ctx, f.tenantID+"_"+f.bucket, f.key, bytes.NewReader(ct))
	require.NoError(t, err)
	sum := md5.Sum(content) // #nosec G401
	_, err = f.db.Exec(`UPDATE object_head_cache SET encryption_algorithm = $4, encryption_key_version = $5, size_bytes = $6, etag = $7
		WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3`, f.tenantID, f.bucket, f.key, crypto.SSEAlgorithm, v, len(content), hex.EncodeToString(sum[:]))
	require.NoError(t, err)
	f.server.sseService = sse

	get := func(rng string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", "/cdn/"+f.slug+"/"+f.bucket+"/"+f.key, nil)
		if rng != "" {
			req.Header.Set("Range", rng)
		}
		w := httptest.NewRecorder()
		f.router.ServeHTTP(w, req)
		return w
	}
	w := get("")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, content, w.Body.Bytes())
	size := len(content)
	for _, rg := range [][2]int{{0, 0}, {60, 70}, {size - 1, size - 1}, {64 * 20, size - 1}, {100, 900}} {
		w = get(fmt.Sprintf("bytes=%d-%d", rg[0], rg[1]))
		require.Equal(t, http.StatusPartialContent, w.Code, "range %v: %s", rg, w.Body.String())
		assert.Equal(t, content[rg[0]:rg[1]+1], w.Body.Bytes(), "range %v", rg)
		assert.Equal(t, fmt.Sprintf("bytes %d-%d/%d", rg[0], rg[1], size), w.Header().Get("Content-Range"))
	}
}

// TestSSEPaths_NeverBufferTheObject walks the source: a function on an
// SSE-S3 path — one that seals or opens (Seal / Open / OpenRange on the
// service, openSealed*, newSSEWriteSealer) — never reads a stream whole
// (io.ReadAll) nor sizes a buffer by a variable (make([]byte, n)). The
// exceptions are named: the compatibility openers of the formats written
// before WP-R8-5 (bounded: no such blob is over 256 MiB) — SSE-S3 0x01/0x02
// and old-format SSE-C.
func TestSSEPaths_NeverBufferTheObject(t *testing.T) {
	allowed := map[string]string{
		"internal/crypto/sse_s3.go:Open":         "the 0x01/0x02 compatibility opener",
		"internal/crypto/ssec.go:SSECOpenLegacy": "the old-format SSE-C compatibility opener",
	}
	sealing := regexp.MustCompile(`(^|\.)(sse|sseService|SSE|svc)\.(Seal|Open|OpenRange)$|(^|\.)(openSealed|openSealedRange|openSealedGet|newSSEWriteSealer)$|(^|\.)SSEC(Seal|Open|OpenRange|OpenLegacy|Decrypt)$`)
	var violations, paths []string
	for _, dir := range []string{"../crypto", "."} {
		entries, err := os.ReadDir(dir)
		require.NoError(t, err)
		for _, e := range entries {
			name := e.Name()
			if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
			require.NoError(t, err)
			rel := "internal/api/" + name
			if dir == "../crypto" {
				rel = "internal/crypto/" + name
			}
			for _, d := range file.Decls {
				fn, ok := d.(*ast.FuncDecl)
				if !ok || fn.Body == nil {
					continue
				}
				where := rel + ":" + fn.Name.Name
				onPath := false
				var bad []string
				ast.Inspect(fn.Body, func(n ast.Node) bool {
					call, ok := n.(*ast.CallExpr)
					if !ok {
						return true
					}
					var b strings.Builder
					_ = printer.Fprint(&b, fset, call.Fun)
					callee := b.String()
					if sealing.MatchString(callee) {
						onPath = true
					}
					switch {
					case callee == "io.ReadAll":
						bad = append(bad, "io.ReadAll")
					case callee == "make" && len(call.Args) >= 2:
						if _, lit := call.Args[1].(*ast.BasicLit); !lit {
							var a strings.Builder
							_ = printer.Fprint(&a, fset, call.Args[0])
							if a.String() == "[]byte" {
								bad = append(bad, "make([]byte, n)")
							}
						}
					}
					return true
				})
				if !onPath {
					continue
				}
				paths = append(paths, where)
				if len(bad) > 0 && allowed[where] == "" {
					violations = append(violations, where+": "+strings.Join(bad, ", "))
				}
			}
		}
	}
	t.Logf("SSE-S3 paths: %v", paths)
	assert.Empty(t, violations, "a whole-object buffer on an SSE-S3 path")
	for _, must := range []string{"internal/api/sse_stream.go:openSealed", "internal/api/s3_copy.go:copyObject",
		"internal/api/s3_engine_adapter.go:HandlePut", "internal/api/s3_engine_adapter.go:HandleGet", "internal/crypto/ssec.go:SSECOpenLegacy",
		"internal/api/background_put.go:writeOpts", "internal/api/key_reencrypt_whole.go:reencryptObject",
		"internal/api/sse_stream.go:openSealedGet", "internal/api/cdn.go:handleCDNRequest"} {
		assert.Contains(t, paths, must, "the walk sees every SSE-S3 path")
	}

	// The names of the old whole-object API are gone from the product code.
	for _, gone := range []string{"MaxEncryptableSize", "maxSSEObjectSize", "sseSealingReader", "openSSEBlob", "EncryptBytes(", "errCopyTooLargeForSSE", "refuseTooLargeToSeal",
		"SSECEncrypt(", "ssecMaxObjectSize"} {
		for _, dir := range []string{"../crypto", "."} {
			entries, err := os.ReadDir(dir)
			require.NoError(t, err)
			for _, e := range entries {
				if !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
					continue
				}
				src, err := os.ReadFile(filepath.Join(dir, e.Name()))
				require.NoError(t, err)
				assert.NotContains(t, string(src), gone, "%s/%s", dir, e.Name())
			}
		}
	}
}

// declaredLengthDriver stores what an S3 backend stores: the first
// ContentLength bytes of the body (a PUT whose declared length is short is
// cut short there), and records the declared length.
type declaredLengthDriver struct {
	*drivers.LocalDriver
	declared atomic.Int64
}

func (d *declaredLengthDriver) Put(ctx context.Context, container, artifact string, data io.Reader, opts ...engine.PutOption) error {
	o := engine.ApplyPutOptions(opts...)
	d.declared.Store(o.ContentLength)
	if o.ContentLength > 0 {
		data = io.LimitReader(data, o.ContentLength)
	}
	return d.LocalDriver.Put(ctx, container, artifact, data, opts...)
}

// Smart demotion and promotion move the STORED bytes: a sealed object's copy
// carries the blob's length, never the object's (sized by the plaintext, an
// S3-class backend kept the first size bytes of the ciphertext — an object
// that no longer opens). Before WP-R8-5 the declared length was size_bytes.
func TestSmartDemotion_SealedObject_CopiedAtItsStoredLength(t *testing.T) {
	f := setupDemotionFixture(t, 1<<40, "standard")
	cold := &declaredLengthDriver{LocalDriver: drivers.NewLocalDriver(f.coldDir, zap.NewNop())}
	f.eng.AddDriver("geyser", cold)
	sse, err := testSSEService(f.db, keyTestMasterA)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = f.db.Exec(`DELETE FROM tenant_key_versions WHERE tenant_id = $1`, f.tenantID) })
	body := mpBody(3000, 2)
	blob, v := sealedBlob(t, sse, f.tenantID, body)
	h := f.object("b", "sealed.bin", int64(len(body)), 60, 60)
	_, err = f.db.Exec(`UPDATE object_head_cache SET encryption_algorithm = $3, encryption_key_version = $4 WHERE tenant_id = $1 AND object_key = $2`,
		f.tenantID, h.key, crypto.SSEAlgorithm, v)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(f.hotDir, f.tenantID+"_b", "sealed.bin"), blob, 0o600))

	_, err = f.runner.RunOnce(context.Background(), false)
	require.NoError(t, err)
	require.Equal(t, "geyser", f.backendOf("b", "sealed.bin"), "demoted")
	assert.Equal(t, int64(len(blob)), cold.declared.Load(), "the blob's length, not the object's")
	got, err := os.ReadFile(filepath.Join(f.coldDir, f.tenantID+"_b", "sealed.bin"))
	require.NoError(t, err)
	assert.Equal(t, blob, got, "the whole sealed blob on the cold backend")
}

// The counter is on the server's own registry (/metrics), every format at 0
// from the first scrape: the live proof found it registered on the default
// registry, which /metrics does not serve.
func TestSSEBlobsOpened_OnTheServersRegistry(t *testing.T) {
	s := &Server{}
	s.initMetrics()
	body := scrapeMetrics(t, s)
	for _, f := range []string{"0x01", "0x02", "0x03"} {
		assert.Contains(t, body, `vaultaire_sse_blobs_opened_total{format="`+f+`"}`)
	}
}

// --- WP-R8-5 Part B: SSE-C on the streaming codec ---------------------------

func ssecHeaders(key []byte) map[string]string {
	sum := md5.Sum(key) // #nosec G401 -- the SSE-C key MD5 header
	return map[string]string{
		"x-amz-server-side-encryption-customer-algorithm": "AES256",
		"x-amz-server-side-encryption-customer-key":       base64.StdEncoding.EncodeToString(key),
		"x-amz-server-side-encryption-customer-key-MD5":   base64.StdEncoding.EncodeToString(sum[:]),
	}
}

func (f *sealedFixture) getWith(t *testing.T, key, rng string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest("GET", "/test-bucket/"+key, nil)
	if rng != "" {
		r.Header.Set("Range", rng)
	}
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	r = r.WithContext(s3Ctx(r.Context(), f.tenant))
	w := httptest.NewRecorder()
	f.server.handleS3Request(w, r)
	return w
}

// legacySSECBlob is an SSE-C blob as the writer before Part B made it:
// [nonce 12][AES-GCM ct+tag]. firstBytes, when set, are forced into the
// nonce (the magic-collision case).
func legacySSECBlob(t *testing.T, key, pt []byte, firstBytes ...byte) []byte {
	t.Helper()
	block, err := aes.NewCipher(key)
	require.NoError(t, err)
	gcm, err := cipher.NewGCM(block)
	require.NoError(t, err)
	nonce := make([]byte, 12)
	_, _ = rand.Read(nonce)
	copy(nonce, firstBytes)
	return gcm.Seal(append([]byte(nil), nonce...), nonce, pt, nil)
}

func TestSSEC_Put_Get_Range_OverTheOldCap(t *testing.T) {
	// Before (Part A): the SSE-C PUT io.ReadAll'd the body and sealed it with
	// one GCM, 413 over 256 MiB; the GET read the whole blob first.
	f := setupSealedFixture(t)
	key := bytes.Repeat([]byte{0x5c}, 32)
	hdr := ssecHeaders(key)
	body := mpBody(3*crypto.StreamSegmentSize+4321, 6) // 4 segments of the default 1 MiB
	w := f.putWith(t, "c.bin", body, hdr)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, "AES256", w.Header().Get("x-amz-server-side-encryption-customer-algorithm"))
	row := f.row(t, "c.bin")
	assert.Equal(t, crypto.SSECAlgorithmV3, row.enc)
	raw, err := os.ReadFile(f.blobFile(t, "c.bin"))
	require.NoError(t, err)
	assert.Equal(t, []byte{crypto.StreamFormat, crypto.StreamKindSSEC}, raw[:2])
	assert.Equal(t, crypto.SSECSealedLength(int64(len(body))), int64(len(raw)))
	assert.Zero(t, plaintextOnBackend(t, f.fixed.dir, f.tempDir))

	g := f.getWith(t, "c.bin", "", hdr)
	require.Equal(t, http.StatusOK, g.Code, g.Body.String())
	assert.Equal(t, body, g.Body.Bytes())
	assert.Equal(t, "AES256", g.Header().Get("x-amz-server-side-encryption-customer-algorithm"))

	size := int64(len(body))
	for _, rg := range [][2]int64{{0, 0}, {crypto.StreamSegmentSize - 3, crypto.StreamSegmentSize + 3}, {size - 1, size - 1}} {
		f.rd.reset()
		g = f.getWith(t, "c.bin", fmt.Sprintf("bytes=%d-%d", rg[0], rg[1]), hdr)
		require.Equal(t, http.StatusPartialContent, g.Code, g.Body.String())
		assert.Equal(t, body[rg[0]:rg[1]+1], g.Body.Bytes())
		assert.Equal(t, int32(0), f.rd.gets.Load(), "by segment, never the whole blob")
		assert.Equal(t, int32(2), f.rd.ranges.Load())
	}

	// A wrong key: 403 before the status line, whole or ranged; no key: 403.
	wrong := ssecHeaders(bytes.Repeat([]byte{0x11}, 32))
	for _, rng := range []string{"", "bytes=10-20"} {
		g = f.getWith(t, "c.bin", rng, wrong)
		assert.Equal(t, http.StatusForbidden, g.Code, "range %q", rng)
		assert.NotContains(t, g.Body.String(), mpMarker)
	}
	g = f.getWith(t, "c.bin", "", nil)
	assert.Equal(t, http.StatusBadRequest, g.Code, "no key: AWS's 400 (R8-14)")

	// The size gate is gone: a declared length over the old cap is read
	// (this short body is the client's 400, never a 413).
	r := httptest.NewRequest("PUT", "/test-bucket/big.bin", bytes.NewReader([]byte("tiny")))
	r.ContentLength = 256<<20 + 1
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	r = r.WithContext(f.ctx(r.Context()))
	pw := httptest.NewRecorder()
	f.server.handleS3Request(pw, r)
	assert.Equal(t, http.StatusBadRequest, pw.Code, pw.Body.String())
	assert.Contains(t, pw.Body.String(), ErrIncompleteBody)
}

func TestSSEC_OldBlob_StillOpens(t *testing.T) {
	f := setupSealedFixture(t)
	key := bytes.Repeat([]byte{0x7e}, 32)
	hdr := ssecHeaders(key)
	body := mpBody(5000, 3)
	require.Equal(t, http.StatusOK, f.putWith(t, "old.bin", body, hdr).Code)
	// As the old writer left it: the old format, the old row value.
	require.NoError(t, os.WriteFile(f.blobFile(t, "old.bin"), legacySSECBlob(t, key, body), 0o600))
	_, err := f.db.Exec(`UPDATE object_head_cache SET encryption_algorithm = $3 WHERE tenant_id = $1 AND object_key = $2`,
		f.tenantID, "old.bin", crypto.SSECAlgorithm)
	require.NoError(t, err)

	g := f.getWith(t, "old.bin", "", hdr)
	require.Equal(t, http.StatusOK, g.Code, g.Body.String())
	assert.Equal(t, body, g.Body.Bytes())
	g = f.getWith(t, "old.bin", "bytes=1000-1999", hdr)
	require.Equal(t, http.StatusPartialContent, g.Code, g.Body.String())
	assert.Equal(t, body[1000:2000], g.Body.Bytes())
	g = f.getWith(t, "old.bin", "", ssecHeaders(bytes.Repeat([]byte{1}, 32)))
	assert.Equal(t, http.StatusForbidden, g.Code)
}

// An old SSE-C blob starts with its random nonce: one in 256 starts with the
// 0x03 magic, one in 65,536 with 0x03 0x02 (what a streaming SSE-C header
// starts with). The row says which format a blob is in, so such a blob is
// opened as the old format it is — whole and ranged.
func TestSSEC_MagicCollision_OldBlobStartingWith0x03(t *testing.T) {
	f := setupSealedFixture(t)
	key := bytes.Repeat([]byte{0x2a}, 32)
	hdr := ssecHeaders(key)
	body := mpBody(4000, 9)
	require.Equal(t, http.StatusOK, f.putWith(t, "coll.bin", body, hdr).Code)
	blob := legacySSECBlob(t, key, body, crypto.StreamFormat, crypto.StreamKindSSEC)
	require.Equal(t, []byte{crypto.StreamFormat, crypto.StreamKindSSEC}, blob[:2])
	require.NoError(t, os.WriteFile(f.blobFile(t, "coll.bin"), blob, 0o600))
	_, err := f.db.Exec(`UPDATE object_head_cache SET encryption_algorithm = $3 WHERE tenant_id = $1 AND object_key = $2`,
		f.tenantID, "coll.bin", crypto.SSECAlgorithm)
	require.NoError(t, err)

	g := f.getWith(t, "coll.bin", "", hdr)
	require.Equal(t, http.StatusOK, g.Code, g.Body.String())
	assert.Equal(t, body, g.Body.Bytes())
	g = f.getWith(t, "coll.bin", "bytes=100-199", hdr)
	require.Equal(t, http.StatusPartialContent, g.Code, g.Body.String())
	assert.Equal(t, body[100:200], g.Body.Bytes())
}
