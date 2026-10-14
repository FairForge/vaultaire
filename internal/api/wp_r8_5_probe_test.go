package api

// WP-R6-1 Part 0: the post-merge probe of WP-R8-5 (#708/#709) — what its
// note did not drive, at the handlers — and its two small fixes: R85-1 (a
// write into a bucket with no row was accepted and stored) and R85-9 (the
// keyless refusal of a request asking AES256 logged at Error).

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/FairForge/vaultaire/internal/common"
	"github.com/FairForge/vaultaire/internal/crypto"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// --- R85-1: NoSuchBucket on PUT, Create and Complete ------------------------

// touchedBody records whether anything read it.
type touchedBody struct {
	r    io.Reader
	read atomic.Bool
}

func (b *touchedBody) Read(p []byte) (int, error) {
	b.read.Store(true)
	return b.r.Read(p)
}

// requireTestBucket gives a fixture's tenant the bucket its tests write
// into (a write into a bucket with no row is NoSuchBucket, R85-1).
func requireTestBucket(t *testing.T, db *sql.DB, tenantID, bucket string) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO buckets (tenant_id, name, visibility) VALUES ($1, $2, 'private') ON CONFLICT (tenant_id, name) DO NOTHING`,
		tenantID, bucket)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM buckets WHERE tenant_id = $1 AND name = $2`, tenantID, bucket) })
}

// objectRows counts this tenant's head rows in bucket.
func (f *mpChunkFixture) objectRows(t *testing.T, bucket string) int {
	t.Helper()
	var n int
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM object_head_cache WHERE tenant_id = $1 AND bucket = $2`,
		f.tenantID, bucket).Scan(&n))
	return n
}

func (f *mpChunkFixture) storedBytes(t *testing.T) int64 {
	t.Helper()
	var n int64
	require.NoError(t, f.db.QueryRow(`SELECT COALESCE(storage_used_bytes, 0) FROM tenant_quotas WHERE tenant_id = $1`, f.tenantID).Scan(&n))
	return n
}

func TestPut_NoSuchBucket_404BeforeTheBody(t *testing.T) {
	// Before (origin/main 9f3d0c1; the 2026-10-13 prod proof, WP-R8-5 §
	// Prod proof): 200, the object stored and billed under a bucket no row
	// names — invisible to ListBuckets, without the bucket's settings.
	f := setupMPChunkFixture(t, false)
	usedBefore := f.storedBytes(t)
	body := &touchedBody{r: bytes.NewReader(mpBody(4096, 1))}
	r := httptest.NewRequest("PUT", "/test-buckett/k.bin", body)
	r.ContentLength = 4096
	r = r.WithContext(f.ctx(r.Context()))
	w := httptest.NewRecorder()
	f.server.handleS3Request(w, r)

	assert.Equal(t, http.StatusNotFound, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "<Code>NoSuchBucket</Code>")
	assert.Contains(t, w.Body.String(), "test-bucket", "the near name is suggested (writeNoSuchBucket)")
	assert.False(t, body.read.Load(), "refused before any body byte")
	assert.Zero(t, f.objectRows(t, "test-buckett"))
	assert.Zero(t, plaintextOnBackend(t, f.fixed.dir, f.tempDir), "nothing stored")
	assert.Equal(t, usedBefore, f.storedBytes(t), "nothing reserved or billed")

	// The bucket that exists still takes the same PUT.
	assert.Equal(t, http.StatusOK, httptestPut(f, "k.bin", mpBody(4096, 1), nil).Code)
}

func TestCreateMultipart_NoSuchBucket_404(t *testing.T) {
	// Before: 200 + an upload row; the parts were staged and Complete stored
	// the object under the missing bucket.
	f := setupMPChunkFixture(t, false)
	r := httptest.NewRequest("POST", "/no-such-bucket/mp.bin?uploads", nil)
	r = r.WithContext(s3Ctx(r.Context(), f.tenant))
	w := httptest.NewRecorder()
	f.server.handleS3Request(w, r)

	assert.Equal(t, http.StatusNotFound, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "<Code>NoSuchBucket</Code>")
	var n int
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM multipart_uploads WHERE tenant_id = $1 AND bucket = 'no-such-bucket'`,
		f.tenantID).Scan(&n))
	assert.Zero(t, n, "no upload row")
}

func TestComplete_BucketDeletedSinceCreate_404BeforeCommit(t *testing.T) {
	// Before: the Complete of an upload whose bucket was deleted after
	// Create stored the object (200) under a bucket no row names.
	f := setupMPChunkFixture(t, false)
	instantCommit(f.server) // anything after longOpBegin would be a committed 200
	body := mpBody(3000, 2)
	u := f.upload(t, "mp.bin", splitParts(body, 3), nil)
	_, err := f.db.Exec(`DELETE FROM buckets WHERE tenant_id = $1 AND name = 'test-bucket'`, f.tenantID)
	require.NoError(t, err)

	w := f.complete(t, "mp.bin", u)
	assert.Equal(t, http.StatusNotFound, w.Code, "refused before the keep-alive can commit: %s", w.Body.String())
	assert.Contains(t, w.Body.String(), "<Code>NoSuchBucket</Code>")
	assert.Zero(t, f.objectRows(t, "test-bucket"))
	assert.Zero(t, plaintextOnBackend(t, f.fixed.dir), "nothing stored on the backend")
	var status string
	require.NoError(t, f.db.QueryRow(`SELECT status FROM multipart_uploads WHERE upload_id = $1`, u.id).Scan(&status))
	assert.Equal(t, "active", status, "the upload is left for Abort or the reaper")
}

// A bucket older than the registry (objects recorded under its name, no
// row) keeps taking writes: the rule is ListObjects' and CopyObject's
// (longOpBucketExists).
func TestPut_LegacyBucketWithObjectsButNoRow_StillWritable(t *testing.T) {
	f := setupMPChunkFixture(t, false)
	require.Equal(t, http.StatusOK, httptestPut(f, "a.bin", mpBody(100, 1), nil).Code)
	_, err := f.db.Exec(`DELETE FROM buckets WHERE tenant_id = $1 AND name = 'test-bucket'`, f.tenantID)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, httptestPut(f, "b.bin", mpBody(100, 2), nil).Code)
}

// --- R85-9: the keyless header refusal is the client's, at Warn -------------

func TestSSEHeaderRefusal_NoKey_IsWarnAndClientRefusal(t *testing.T) {
	// Before: Error lines ("write asking for SSE-S3 refused", "multipart
	// upload asking for SSE-S3 refused") and a 5xx on vaultaire_errors_total
	// for every keyless request any client chose to send.
	f := setupMPChunkFixture(t, false)
	core, logs := observer.New(zapcore.DebugLevel)
	f.server.logger = zap.New(core)
	h := f.server.loggingMiddleware(http.HandlerFunc(f.server.handleS3Request))
	hdr := map[string]string{"x-amz-server-side-encryption": "AES256"}
	send := func(method, target string, body []byte) int {
		r := httptest.NewRequest(method, target, bytes.NewReader(body))
		r.ContentLength = int64(len(body))
		for k, v := range hdr {
			r.Header.Set(k, v)
		}
		r = r.WithContext(f.ctx(r.Context()))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	errorsBefore := atomic.LoadInt64(&f.server.errorCount)

	assert.Equal(t, http.StatusServiceUnavailable, send("PUT", "/test-bucket/k.bin", mpBody(900, 1)))
	assert.Equal(t, http.StatusServiceUnavailable, send("POST", "/test-bucket/mp.bin?uploads", nil))

	assert.Equal(t, errorsBefore, atomic.LoadInt64(&f.server.errorCount), "a client's choice is not a server failure")
	assert.Zero(t, logs.FilterLevelExact(zapcore.ErrorLevel).Len(), "no Error line: %v", logs.FilterLevelExact(zapcore.ErrorLevel).All())
	assert.Equal(t, 2, logs.FilterMessageSnippet("asking for SSE-S3 refused").FilterLevelExact(zapcore.WarnLevel).Len())

	// The bucket case stays an operator condition: Error, and a 5xx.
	f.bucketSet(t, "sse_enabled = TRUE")
	hdr = nil
	assert.Equal(t, http.StatusServiceUnavailable, send("PUT", "/test-bucket/k2.bin", mpBody(900, 1)))
	assert.Equal(t, http.StatusServiceUnavailable, send("POST", "/test-bucket/mp2.bin?uploads", nil))
	assert.Equal(t, errorsBefore+2, atomic.LoadInt64(&f.server.errorCount))
	assert.Equal(t, 2, logs.FilterMessageSnippet("encrypted bucket refused").FilterLevelExact(zapcore.ErrorLevel).Len())
}

// --- Probe 1: a sealed blob of another length than the length function ------

// sealedOneOf PUTs a sealed object of n bytes and returns it and its blob file.
func (f *sealedFixture) sealedOneOf(t *testing.T, key string, n int, seed byte) ([]byte, string, []byte) {
	t.Helper()
	body := mpBody(n, seed)
	require.Equal(t, http.StatusOK, f.putWith(t, key, body, nil).Code)
	p := f.blobFile(t, key)
	raw, err := os.ReadFile(p)
	require.NoError(t, err)
	require.Equal(t, f.server.sseService.SealedLength(int64(n)), int64(len(raw)))
	return body, p, raw
}

func (f *sealedFixture) copyTo(t *testing.T, src, dst string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest("PUT", "/test-bucket/"+dst, nil)
	r.Header.Set("x-amz-copy-source", "/test-bucket/"+src)
	r = r.WithContext(f.ctx(r.Context()))
	w := httptest.NewRecorder()
	f.server.handleS3Request(w, r)
	return w
}

// assertNeverPastTheTruth: a 500 before the status line, or a cut after it
// with only a prefix of the plaintext; never a clean short (or long) 200.
func assertNeverPastTheTruth(t *testing.T, label string, w *httptest.ResponseRecorder, cut bool, want []byte, okStatus int) {
	t.Helper()
	t.Logf("%s: status %d, cut %v, %d of %d bytes", label, w.Code, cut, w.Body.Len(), len(want))
	switch w.Code {
	case http.StatusInternalServerError:
		assert.False(t, cut, "%s: a 500 has nothing to cut", label)
		assert.NotContains(t, w.Body.String(), mpMarker, label)
	case okStatus:
		assert.True(t, cut, "%s: a %d must be cut, never end clean (%d bytes of %d)", label, okStatus, w.Body.Len(), len(want))
		assert.True(t, bytes.HasPrefix(want, w.Body.Bytes()), "%s: only verified plaintext went out", label)
		assert.Less(t, w.Body.Len(), len(want)+1, label)
	default:
		t.Errorf("%s: status %d (%s)", label, w.Code, w.Body.String())
	}
}

func TestProbe_SealedBlobShorterThanItsLength(t *testing.T) {
	f := setupSealedFixture(t)
	const segs = 6
	seg := int64(sealedSeg + 16)
	hdrLen := int64(crypto.StreamHeaderSSES3)
	for name, cutAt := range map[string]func(n int64) int64{
		"inside the last segment":       func(n int64) int64 { return n - 40 },
		"the last segment dropped":      func(n int64) int64 { return hdrLen + (segs-1)*seg },
		"at the end of the first":       func(int64) int64 { return hdrLen + seg },
		"inside the first segment":      func(int64) int64 { return hdrLen + 100 },
		"the header only (interrupted)": func(int64) int64 { return hdrLen },
	} {
		t.Run(name, func(t *testing.T) {
			key := strings.ReplaceAll(name, " ", "-") + ".bin"
			body, p, raw := f.sealedOneOf(t, key, segs*sealedSeg-100, 3)
			require.NoError(t, os.WriteFile(p, raw[:cutAt(int64(len(raw)))], 0o600))

			w, cut := f.getRecovering(t, key, "")
			assertNeverPastTheTruth(t, "whole GET", w, cut, body, http.StatusOK)
			size := len(body)
			w, cut = f.getRecovering(t, key, fmt.Sprintf("bytes=%d-%d", size-50, size-1))
			assertNeverPastTheTruth(t, "Range in the last segment", w, cut, body[size-50:], http.StatusPartialContent)

			c := f.copyTo(t, key, key+".copy")
			t.Logf("CopyObject: status %d, error code %q", c.Code, probeErrCode(c.Body.String()))
			// R85-10: an integrity failure, never a 503 retried for ever.
			assert.Equal(t, "InternalError", probeErrCode(c.Body.String()), "CopyObject of a sealed source that does not open")
			assert.NotContains(t, c.Body.String(), "<CopyObjectResult>", "CopyObject: %d %s", c.Code, c.Body.String())
			var n int
			require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM object_head_cache WHERE tenant_id = $1 AND object_key = $2`,
				f.tenantID, key+".copy").Scan(&n))
			assert.Zero(t, n, "CopyObject: no destination row")
		})
	}
}

func TestProbe_SealedBlobLongerThanItsLength(t *testing.T) {
	f := setupSealedFixture(t)
	// A stale overwrite (Sync: the old, longer file's tail left behind), a
	// byte appended, a whole stale segment appended.
	_, _, oldRaw := f.sealedOneOf(t, "old.bin", 9*sealedSeg, 4)
	for name, tail := range map[string]func(cur []byte) []byte{
		"one byte":            func([]byte) []byte { return []byte{0} },
		"a stale segment":     func([]byte) []byte { return oldRaw[crypto.StreamHeaderSSES3 : crypto.StreamHeaderSSES3+sealedSeg+16] },
		"the old file's tail": func(cur []byte) []byte { return oldRaw[len(cur):] },
	} {
		t.Run(name, func(t *testing.T) {
			key := strings.ReplaceAll(name, " ", "-") + ".bin"
			body, p, raw := f.sealedOneOf(t, key, 5*sealedSeg+77, 5)
			require.NoError(t, os.WriteFile(p, append(append([]byte(nil), raw...), tail(raw)...), 0o600))

			w, cut := f.getRecovering(t, key, "")
			assertNeverPastTheTruth(t, "whole GET", w, cut, body, http.StatusOK)

			// A Range reads exactly the segments the length function names:
			// the bytes after them are never read, and what it serves is the
			// truth (verified segments of this blob).
			size := len(body)
			w, cut = f.getRecovering(t, key, fmt.Sprintf("bytes=%d-%d", size-50, size-1))
			require.False(t, cut)
			require.Equal(t, http.StatusPartialContent, w.Code, w.Body.String())
			assert.Equal(t, body[size-50:], w.Body.Bytes())

			c := f.copyTo(t, key, key+".copy")
			t.Logf("CopyObject: status %d, error code %q", c.Code, probeErrCode(c.Body.String()))
			// R85-10: an integrity failure, never a 503 retried for ever.
			assert.Equal(t, "InternalError", probeErrCode(c.Body.String()), "CopyObject of a sealed source that does not open")
			assert.NotContains(t, c.Body.String(), "<CopyObjectResult>", "CopyObject: %d %s", c.Code, c.Body.String())
		})
	}
}

func TestProbe_CDN_SealedBlobOfTheWrongLength(t *testing.T) {
	f := setupCDNFixture(t)
	sse := sseFor(t, f.db, f.tenantID)
	sse.SetSegmentSize(64)
	f.server.sseService = sse
	content := mpBody(64*10+13, 4)
	ct, v := sealedBlob(t, sse, f.tenantID, content)
	ctx := common.WithTenantID(context.Background(), f.tenantID)
	sum := md5hex(content)
	_, err := f.db.Exec(`UPDATE object_head_cache SET encryption_algorithm = $4, encryption_key_version = $5, size_bytes = $6, etag = $7
		WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3`, f.tenantID, f.bucket, f.key, crypto.SSEAlgorithm, v, len(content), sum)
	require.NoError(t, err)
	get := func(rng string) (w *httptest.ResponseRecorder, cut bool) {
		req := httptest.NewRequest("GET", "/cdn/"+f.slug+"/"+f.bucket+"/"+f.key, nil)
		if rng != "" {
			req.Header.Set("Range", rng)
		}
		w = httptest.NewRecorder()
		defer func() {
			if p := recover(); p != nil {
				require.Equal(t, http.ErrAbortHandler, p)
				cut = true
			}
		}()
		f.router.ServeHTTP(w, req)
		return w, false
	}
	size := len(content)
	for name, blob := range map[string][]byte{
		"short (last segment cut)": ct[:len(ct)-30],
		"long (a byte appended)":   append(append([]byte(nil), ct...), 0),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := f.server.engine.Put(ctx, f.tenantID+"_"+f.bucket, f.key, bytes.NewReader(blob))
			require.NoError(t, err)
			w, cut := get("")
			assertNeverPastTheTruth(t, "cdn whole", w, cut, content, http.StatusOK)
			w, cut = get(fmt.Sprintf("bytes=%d-%d", size-20, size-1))
			if strings.HasPrefix(name, "long") {
				require.Equal(t, http.StatusPartialContent, w.Code)
				assert.Equal(t, content[size-20:], w.Body.Bytes(), "the segments the range spans, verified")
				return
			}
			assertNeverPastTheTruth(t, "cdn range", w, cut, content[size-20:], http.StatusPartialContent)
		})
	}
}

// --- Probe 2: key_reencrypt racing a GET of the same sealed object -----------

// pausingDriver's Get hands out a reader that stops after `after` bytes
// until release is closed.
type pausingDriver struct {
	*rangedFixedDriver
	pauseKey string
	after    int
	paused   chan struct{}
	release  chan struct{}
}

type pausingReader struct {
	io.ReadCloser
	d    *pausingDriver
	n    int
	once bool
}

func (p *pausingReader) Read(b []byte) (int, error) {
	if !p.once && p.n >= p.d.after {
		p.once = true
		close(p.d.paused)
		<-p.d.release
	}
	if !p.once && p.n+len(b) > p.d.after {
		b = b[:p.d.after-p.n]
	}
	k, err := p.ReadCloser.Read(b)
	p.n += k
	return k, err
}

func (d *pausingDriver) Get(ctx context.Context, container, artifact string) (io.ReadCloser, error) {
	rc, err := d.rangedFixedDriver.Get(ctx, container, artifact)
	if err != nil || !strings.HasPrefix(artifact, d.pauseKey+"@") {
		return rc, err
	}
	d.pauseKey = "\x00" // once: the job's own read is not paused
	return &pausingReader{ReadCloser: rc, d: d}, nil
}

func TestProbe_KeyReencryptRacingAGet_GetFinishesByteEqual(t *testing.T) {
	f := setupWholeKeyFixture(t, "Enabled")
	f.adapter.sseService.SetSegmentSize(256)
	body := bytes.Repeat([]byte("a sealed object read while its key rotates "), 200)
	f.putSSEV(t, "k", body)
	oldAddr := f.head(t, "k").storageKey
	pd := &pausingDriver{rangedFixedDriver: &rangedFixedDriver{fixedBucketDriver: f.fixed}, pauseKey: "k",
		after: 1500, paused: make(chan struct{}), release: make(chan struct{})}
	f.eng.AddDriver("fixed", pd)

	// The GET resolves the old row and opens the old address; it stops
	// after 1500 stored bytes (the status line and a few segments out).
	got := make(chan *httptest.ResponseRecorder, 1)
	go func() { got <- f.getVersion(t, "k", "") }()
	select {
	case <-pd.paused:
	case <-time.After(10 * time.Second):
		t.Fatal("the GET never reached its pause")
	}

	// Meanwhile the key rotates and the job moves the object.
	f.rotate(t)
	res, err := f.k.RunOnce(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, res.ObjectsReencrypted, "%+v", res)
	require.NotEqual(t, oldAddr, f.head(t, "k").storageKey, "the row names the new address")
	assert.Equal(t, retiredReencrypted, f.retiredBlobs(t)[oldAddr])
	var graceLeft time.Duration
	var secs float64
	require.NoError(t, f.db.QueryRow(`SELECT EXTRACT(EPOCH FROM retire_after - NOW()) FROM retired_blobs WHERE tenant_id = $1 AND storage_key = $2`,
		f.tenantID, oldAddr).Scan(&secs))
	graceLeft = time.Duration(secs * float64(time.Second))
	assert.Greater(t, graceLeft, 23*time.Hour, "the old address outlives the GET: the collector's grace")

	// The collector runs now: the old blob is not due, so it stays.
	c := NewRetiredBlobCollector(f.db, f.eng, nil, zap.NewNop())
	c.onlyTenant = f.tenantID
	_, err = c.RunOnce(context.Background())
	require.NoError(t, err)
	_, statErr := os.Stat(f.fixed.path(f.tenantID, f.tenant.NamespaceContainer("test-bucket"), oldAddr))
	assert.NoError(t, statErr, "the old blob is still there")

	close(pd.release)
	w := <-got
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, body, w.Body.Bytes(), "the GET finished byte-equal from the old address")
	// And the next GET reads the new one.
	assert.Equal(t, body, f.getVersion(t, "k", "").Body.Bytes())
}

// --- Probe 3: SSE-C under ?versionId, an old-format and a v3 version --------

func TestProbe_SSEC_VersionId_EachVersionByItsOwnRow(t *testing.T) {
	f := setupSealedFixture(t) // versioned
	oldKey, newKey := bytes.Repeat([]byte{0x31}, 32), bytes.Repeat([]byte{0x32}, 32)
	oldBody, newBody := mpBody(3000, 1), mpBody(5000, 2)
	w := f.putWith(t, "v.bin", oldBody, ssecHeaders(oldKey))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	vOld := w.Header().Get("x-amz-version-id")
	// The old version as the writer before #709 left it: the old format, the
	// old row value on its head row and its version row (the next PUT's flip
	// carries the head row's value onto the version it displaces).
	require.NoError(t, os.WriteFile(f.blobFile(t, "v.bin"), legacySSECBlob(t, oldKey, oldBody), 0o600))
	_, err := f.db.Exec(`UPDATE object_versions SET encryption_algorithm = $3 WHERE tenant_id = $1 AND version_id = $2`,
		f.tenantID, vOld, crypto.SSECAlgorithm)
	require.NoError(t, err)
	_, err = f.db.Exec(`UPDATE object_head_cache SET encryption_algorithm = $3 WHERE tenant_id = $1 AND object_key = $2`,
		f.tenantID, "v.bin", crypto.SSECAlgorithm)
	require.NoError(t, err)
	w = f.putWith(t, "v.bin", newBody, ssecHeaders(newKey))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	vNew := w.Header().Get("x-amz-version-id")
	require.NotEqual(t, vOld, vNew)
	require.Equal(t, crypto.SSECAlgorithmV3, f.row(t, "v.bin").enc)

	getV := func(vid, rng string, key []byte) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", "/test-bucket/v.bin?versionId="+vid, nil)
		if rng != "" {
			r.Header.Set("Range", rng)
		}
		for k, v := range ssecHeaders(key) {
			r.Header.Set(k, v)
		}
		r = r.WithContext(s3Ctx(r.Context(), f.tenant))
		rw := httptest.NewRecorder()
		f.server.handleS3Request(rw, r)
		return rw
	}
	for _, c := range []struct {
		name string
		vid  string
		key  []byte
		body []byte
	}{{"old format (AES256-SSE-C)", vOld, oldKey, oldBody}, {"v3", vNew, newKey, newBody}} {
		t.Run(c.name, func(t *testing.T) {
			g := getV(c.vid, "", c.key)
			require.Equal(t, http.StatusOK, g.Code, g.Body.String())
			assert.Equal(t, c.body, g.Body.Bytes())
			g = getV(c.vid, "bytes=1000-1999", c.key)
			require.Equal(t, http.StatusPartialContent, g.Code, g.Body.String())
			assert.Equal(t, c.body[1000:2000], g.Body.Bytes())
			for _, rng := range []string{"", "bytes=10-20"} {
				g = getV(c.vid, rng, bytes.Repeat([]byte{0x99}, 32))
				assert.Equal(t, http.StatusForbidden, g.Code, "a wrong key, range %q", rng)
				assert.NotContains(t, g.Body.String(), mpMarker)
			}
		})
	}
	// The other version's key on each: 403 too (never the other's bytes).
	assert.Equal(t, http.StatusForbidden, getV(vOld, "", newKey).Code)
	assert.Equal(t, http.StatusForbidden, getV(vNew, "", oldKey).Code)
}

// --- Probe 4: a sealed Complete answered after the keep-alive committed -----

func TestProbe_SealedComplete_AfterKeepAlive_BodyRightHeaderAbsent(t *testing.T) {
	f := setupSealedFixture(t)
	instantCommit(f.server)
	f.bucketSet(t, "sse_enabled = FALSE")
	hdr := map[string]string{"x-amz-server-side-encryption": "AES256"}
	body := mpBody(9*sealedSeg, 1)
	u := f.upload(t, "mp.bin", splitParts(body, 3), hdr)
	w := f.complete(t, "mp.bin", u)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var res CompleteMultipartUploadResult
	require.NoError(t, xml.Unmarshal(bytes.TrimSpace(w.Body.Bytes()), &res), w.Body.String())
	assert.Equal(t, awsMultipartETag(u.parts), strings.Trim(res.ETag, `"`), "the body is right")
	assert.Empty(t, w.Header().Get("x-amz-server-side-encryption"),
		"documented: the headers went out with the keep-alive's 200, before the operation knew them")
	assert.Equal(t, crypto.SSEAlgorithm, f.row(t, "mp.bin").enc, "the object is sealed all the same")
	assert.Equal(t, body, f.get(t, "mp.bin", "").Body.Bytes())
}

// probeErrCode is the <Code> of an S3 error document ("" when none).
func probeErrCode(body string) string {
	i := strings.Index(body, "<Code>")
	j := strings.Index(body, "</Code>")
	if i < 0 || j < i {
		return ""
	}
	return body[i+len("<Code>") : j]
}
