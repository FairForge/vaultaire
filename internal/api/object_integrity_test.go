package api

// WP-R8-3 (R8-10): the whole-object path verified the SIZE of what it served
// and nothing else. A blob whose bytes do not hash to the row's single-part
// ETag is now cut before its last byte, on the plain, parallel, sealed and
// /cdn paths; a Range, a -N ETag and a chunked object are not verified (the
// first two counted, the last verifies its chunks itself).

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	promtest "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

func integrityFailures(path, reason string) float64 {
	return promtest.ToFloat64(objectIntegrityFailures.WithLabelValues(path, reason))
}

func integrityUnverified(path, reason string) float64 {
	return promtest.ToFloat64(objectIntegrityUnverified.WithLabelValues(path, reason))
}

// rewriteBlob replaces, under dir, the bytes of the one stored blob of key
// (the whole-object address `<key>@<26 base32>`) with b — a backend that
// serves other bytes of the right length.
func rewriteBlob(t *testing.T, dir, key string, b []byte) {
	t.Helper()
	var hits []string
	require.NoError(t, filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() && strings.HasPrefix(filepath.Base(p), key+"@") {
			hits = append(hits, p)
		}
		return nil
	}))
	require.Len(t, hits, 1, "one blob of %s", key)
	require.NoError(t, os.WriteFile(hits[0], b, 0o600))
}

// integrityServer serves the fixture's Server through its real dispatch and
// logging middleware (where a cut becomes a reset).
func integrityServer(t *testing.T, f *mpChunkFixture) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(f.server.loggingMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.server.handleS3Request(w, r.WithContext(s3Ctx(r.Context(), f.tenant)))
	})))
	t.Cleanup(ts.Close)
	return ts
}

func clientGet(t *testing.T, ts *httptest.Server, path string, hdr map[string]string) (*http.Response, []byte, error) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, ts.URL+path, nil)
	require.NoError(t, err)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := ts.Client().Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	body, readErr := io.ReadAll(resp.Body)
	return resp, body, readErr
}

func setupWholeFixture(t *testing.T, withKey bool) (*mpChunkFixture, *observer.ObservedLogs) {
	t.Helper()
	f := setupMPChunkFixture(t, withKey)
	f.server.chunkingThreshold = 1 << 30 // stored whole
	core, logs := observer.New(zapcore.DebugLevel)
	f.server.logger = zap.New(core)
	f.server.bandwidthTracker = NewBandwidthTracker(nil) // buffered only: the egress record of each response
	return f, logs
}

func (f *mpChunkFixture) putWhole(t *testing.T, key string, body []byte, hdr map[string]string) {
	t.Helper()
	r := httptest.NewRequest("PUT", "/test-bucket/"+key, bytes.NewReader(body))
	r.ContentLength = int64(len(body))
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	r = r.WithContext(s3Ctx(r.Context(), f.tenant))
	w := httptest.NewRecorder()
	f.server.handleS3Request(w, r)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var chunked bool
	require.NoError(t, f.db.QueryRow(`SELECT is_chunked FROM object_head_cache WHERE tenant_id = $1 AND bucket = 'test-bucket' AND object_key = $2`,
		f.tenantID, key).Scan(&chunked))
	require.False(t, chunked, "the object must be stored whole")
}

func TestObjectIntegrity_APlainGetOfOtherBytesIsCutBeforeItsLastByte(t *testing.T) {
	// Arrange: a whole single-part object; the backend now serves other
	// bytes of the same length under its address.
	f, logs := setupWholeFixture(t, false)
	body := randomBody(t, 256<<10)
	f.putWhole(t, "obj.bin", body, nil)
	other := randomBody(t, len(body))
	rewriteBlob(t, f.fixed.dir, "obj.bin", other)
	ts := integrityServer(t, f)
	before := integrityFailures(integrityPathPlain, "md5")

	// Act
	resp, got, readErr := clientGet(t, ts, "/test-bucket/obj.bin", nil)

	// Assert: the status line was out; the body is cut short — never a
	// clean end of wrong bytes.
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Error(t, readErr, "a body that does not match its ETag must never end cleanly")
	assert.ErrorIs(t, readErr, io.ErrUnexpectedEOF)
	assert.Less(t, len(got), len(other), "the last byte is withheld")
	assert.True(t, bytes.Equal(other[:len(got)], got), "what was sent is a prefix of what the backend served")
	assert.Equal(t, before+1, integrityFailures(integrityPathPlain, "md5"))
	lines := logs.FilterMessage("object integrity: the body served does not match its row — the response is cut").All()
	require.Len(t, lines, 1)
	assert.Equal(t, zapcore.ErrorLevel, lines[0].Level)
	fl := lines[0].ContextMap()
	assert.Equal(t, f.tenantID, fl["tenant_id"])
	assert.Equal(t, "test-bucket", fl["bucket"])
	assert.Equal(t, "obj.bin", fl["key"])
	assert.Equal(t, "fixed", fl["backend"])
	assert.Equal(t, md5Hex(body), fl["recorded_etag"])
	assert.Equal(t, md5Hex(other), fl["measured_md5"])
	assert.Equal(t, "md5", fl["reason"])

	// (g) the egress records see what was written before the cut.
	f.server.bandwidthTracker.mu.Lock()
	var egress int64
	for _, e := range f.server.bandwidthTracker.buffer {
		if e.tenantID == f.tenantID {
			egress += e.egress
		}
	}
	f.server.bandwidthTracker.mu.Unlock()
	assert.Equal(t, int64(len(other)-1), egress, "the bytes sent before the cut are recorded")
}

func TestObjectIntegrity_AMatchingGetIsServedWholeAndCountsNothing(t *testing.T) {
	// Arrange
	f, logs := setupWholeFixture(t, false)
	body := randomBody(t, 256<<10)
	f.putWhole(t, "ok.bin", body, nil)
	ts := integrityServer(t, f)
	before := integrityFailures(integrityPathPlain, "md5")

	// Act
	resp, got, readErr := clientGet(t, ts, "/test-bucket/ok.bin", nil)

	// Assert
	require.NoError(t, readErr)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, body, got)
	assert.Equal(t, before, integrityFailures(integrityPathPlain, "md5"))
	assert.Empty(t, logs.FilterLevelExact(zapcore.ErrorLevel).All())
}

func TestObjectIntegrity_ARangeAndAMultipartETagAreServedUnverifiedAndCounted(t *testing.T) {
	// Arrange
	f, _ := setupWholeFixture(t, false)
	body := randomBody(t, 64<<10)
	f.putWhole(t, "r.bin", body, nil)
	f.putWhole(t, "mp.bin", body, nil)
	// A -N row with no body_md5: written before migration 094 (A.4), when
	// Complete recorded no whole digest — served exactly as before.
	_, err := f.db.Exec(`UPDATE object_head_cache SET etag = $3, body_md5 = NULL WHERE tenant_id = $1 AND bucket = 'test-bucket' AND object_key = $2`,
		f.tenantID, "mp.bin", md5Hex([]byte("parts"))+"-2")
	require.NoError(t, err)
	// Both serve other bytes now: unverified means served as they are.
	other := randomBody(t, len(body))
	rewriteBlob(t, f.fixed.dir, "r.bin", other)
	rewriteBlob(t, f.fixed.dir, "mp.bin", other)
	ts := integrityServer(t, f)
	rangeBefore, mpBefore := integrityUnverified(integrityPathPlain, "range"), integrityUnverified(integrityPathPlain, "multipart")
	failBefore := integrityFailures(integrityPathPlain, "md5")

	// Act
	resp, got, readErr := clientGet(t, ts, "/test-bucket/r.bin", map[string]string{"Range": "bytes=10-1033"})
	mresp, mgot, mreadErr := clientGet(t, ts, "/test-bucket/mp.bin", nil)

	// Assert
	require.NoError(t, readErr)
	assert.Equal(t, http.StatusPartialContent, resp.StatusCode)
	assert.Equal(t, other[10:1034], got)
	require.NoError(t, mreadErr)
	assert.Equal(t, http.StatusOK, mresp.StatusCode)
	assert.Equal(t, other, mgot)
	assert.Equal(t, rangeBefore+1, integrityUnverified(integrityPathPlain, "range"), "counted once per response")
	assert.Equal(t, mpBefore+1, integrityUnverified(integrityPathPlain, "multipart"))
	assert.Equal(t, failBefore, integrityFailures(integrityPathPlain, "md5"))
}

func TestObjectIntegrity_AChunkedObjectIsNeitherCheckedNorCounted(t *testing.T) {
	// Arrange: chunked (the fixture's 1 KiB threshold).
	f := setupMPChunkFixture(t, false)
	body := randomBody(t, 3<<20)
	r := httptest.NewRequest("PUT", "/test-bucket/c.bin", bytes.NewReader(body))
	r.ContentLength = int64(len(body))
	r = r.WithContext(s3Ctx(r.Context(), f.tenant))
	w := httptest.NewRecorder()
	f.server.handleS3Request(w, r)
	require.Equal(t, http.StatusOK, w.Code)
	ts := integrityServer(t, f)
	snapshot := func() []float64 {
		var out []float64
		for _, p := range []string{integrityPathPlain, integrityPathParallel, integrityPathSealed} {
			for _, reason := range []string{"range", "multipart", "etag"} {
				out = append(out, integrityUnverified(p, reason))
			}
			out = append(out, integrityFailures(p, "md5"))
		}
		return out
	}
	before := snapshot()

	// Act
	_, got, readErr := clientGet(t, ts, "/test-bucket/c.bin", nil)

	// Assert
	require.NoError(t, readErr)
	assert.Equal(t, body, got)
	assert.Equal(t, before, snapshot())
}

func TestObjectIntegrity_AClientThatLeftIsNeitherCutNorCounted(t *testing.T) {
	// Arrange
	f, _ := setupWholeFixture(t, false)
	body := randomBody(t, 256<<10)
	f.putWhole(t, "gone.bin", body, nil)
	rewriteBlob(t, f.fixed.dir, "gone.bin", randomBody(t, len(body)))
	before := integrityFailures(integrityPathPlain, "md5")
	ctx, cancel := context.WithCancel(s3Ctx(context.Background(), f.tenant))
	req := httptest.NewRequest("GET", "/test-bucket/gone.bin", nil).WithContext(ctx)
	rec := httptest.NewRecorder()

	// Act: no recover — a cut would panic out of the test.
	f.server.handleS3Request(&cancelOnWrite{ResponseWriter: rec, cancel: cancel}, req)

	// Assert
	assert.Equal(t, before, integrityFailures(integrityPathPlain, "md5"))
}

func TestObjectIntegrity_AParallelGetOfOtherBytesIsCut(t *testing.T) {
	// Arrange: the parallel ranged reader fetched the bytes; the check sits
	// on the client-facing stream whatever fetched them.
	f := setupParallelFixture(t, true)
	content := randomBytes(3 << 20)
	f.put(t, "big.bin", content)
	f.drv.mu.Lock()
	for k, v := range f.drv.objects {
		if strings.Contains(k, "big.bin@") {
			altered := append([]byte(nil), v...)
			altered[len(altered)/2] ^= 0xff // one flipped byte
			f.drv.objects[k] = altered
		}
	}
	f.drv.mu.Unlock()
	before := integrityFailures(integrityPathParallel, "md5")

	// Act
	w := f.get(t, "big.bin", nil)

	// Assert
	assert.True(t, f.cut, "the body is cut")
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, len(content)-1, w.Body.Len(), "every byte but the last was sent")
	assert.Positive(t, f.drv.ranges.Load(), "the parallel reader fetched it")
	assert.Equal(t, before+1, integrityFailures(integrityPathParallel, "md5"))
}

func TestObjectIntegrity_ASealedBlobCopiedToAnotherKeyIsNeverServedAsIt(t *testing.T) {
	// Arrange: two SSE-S3 objects of one tenant and one size; B's address
	// now holds A's sealed blob (a key-swapped blob: it decrypts under the
	// tenant's key with a header-only AAD).
	f, _ := setupWholeFixture(t, true)
	a, b := randomBody(t, 200<<10), randomBody(t, 200<<10)
	sse := map[string]string{"x-amz-server-side-encryption": "AES256"}
	f.putWhole(t, "a.bin", a, sse)
	f.putWhole(t, "b.bin", b, sse)
	var aBlob []byte
	require.NoError(t, filepath.Walk(f.fixed.dir, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() && strings.HasPrefix(filepath.Base(p), "a.bin@") {
			aBlob, err = os.ReadFile(p) // #nosec G304 -- the test's own temp dir
			return err
		}
		return nil
	}))
	require.NotEmpty(t, aBlob)
	rewriteBlob(t, f.fixed.dir, "b.bin", aBlob)
	ts := integrityServer(t, f)

	// Act
	resp, got, readErr := clientGet(t, ts, "/test-bucket/b.bin", nil)

	// Assert: B is never served as A's bytes — cut (the MD5 check) or
	// refused before the status line (the identity-bound AAD, WP-R8-3 A.2).
	if resp.StatusCode == http.StatusOK {
		require.Error(t, readErr, "a cut")
		assert.Less(t, len(got), len(a))
	} else {
		assert.GreaterOrEqual(t, resp.StatusCode, 500)
		assert.False(t, bytes.Contains(got, a[:64]), "none of A's bytes")
	}
	assert.NotEqual(t, a, got)
}

func TestObjectIntegrity_ACDNGetOfOtherBytesIsCut(t *testing.T) {
	// Arrange: the public object's row records the MD5 of its bytes; the
	// backend now serves other bytes of the same length.
	f := setupCDNFixture(t)
	_, err := f.db.Exec(`UPDATE object_head_cache SET etag = $3 WHERE tenant_id = $1 AND object_key = $2`,
		f.tenantID, f.key, md5Hex(f.content))
	require.NoError(t, err)
	ok := httptest.NewRecorder()
	f.router.ServeHTTP(ok, httptest.NewRequest("GET", fmt.Sprintf("/cdn/%s/%s/%s", f.slug, f.bucket, f.key), nil))
	require.Equal(t, http.StatusOK, ok.Code)
	require.Equal(t, f.content, ok.Body.Bytes(), "a matching object is served whole")
	_, err = f.db.Exec(`UPDATE object_head_cache SET etag = $3 WHERE tenant_id = $1 AND object_key = $2`,
		f.tenantID, f.key, md5Hex([]byte("hello from cdN")))
	require.NoError(t, err)
	before := integrityFailures(integrityPathCDN, "md5")

	// Act
	rec := httptest.NewRecorder()
	cut := false
	func() {
		defer func() {
			if p := recover(); p != nil {
				require.Equal(t, http.ErrAbortHandler, p)
				cut = true
			}
		}()
		f.router.ServeHTTP(rec, httptest.NewRequest("GET", fmt.Sprintf("/cdn/%s/%s/%s", f.slug, f.bucket, f.key), nil))
	}()

	// Assert
	assert.True(t, cut, "the body is cut")
	assert.Equal(t, len(f.content)-1, rec.Body.Len())
	assert.Equal(t, before+1, integrityFailures(integrityPathCDN, "md5"))
}

func TestIntegrityReader_SizeMismatchesFail(t *testing.T) {
	body := randomBody(t, 1000)
	etag := md5Hex(body)
	for name, src := range map[string][]byte{"short": body[:999], "long": append(append([]byte(nil), body...), 'x')} {
		t.Run(name, func(t *testing.T) {
			r, armed := newIntegrityReader(bytes.NewReader(src), etag, int64(len(body)))
			require.True(t, armed)
			got, err := io.ReadAll(r)
			require.ErrorIs(t, err, errObjectIntegrity)
			assert.Less(t, len(got), len(body), "never the whole length")
		})
	}
	r, armed := newIntegrityReader(bytes.NewReader(body), etag, int64(len(body)))
	require.True(t, armed)
	got, err := io.ReadAll(r)
	require.NoError(t, err)
	assert.Equal(t, body, got)
}

func BenchmarkIntegrityReader_1GiB(b *testing.B) {
	buf := make([]byte, 1<<20)
	b.SetBytes(1 << 30)
	for i := 0; i < b.N; i++ {
		src := io.LimitReader(repeatReader{buf}, 1<<30)
		r, _ := newIntegrityReader(src, strings.Repeat("0", 32), 1<<30)
		start := time.Now()
		_, _ = io.Copy(io.Discard, r)
		b.ReportMetric(float64(time.Since(start).Milliseconds()), "ms/GiB")
	}
}

type repeatReader struct{ b []byte }

func (r repeatReader) Read(p []byte) (int, error) { return copy(p, r.b), nil }

// Prometheus's increase() and rate() compare samples of a series: a counter
// that is CREATED by its first event is born at 1 and shows no rise, so the
// ObjectIntegrityFailure rule (increase[1h] > 0) never saw a path's first
// failure — proven with `promtool test rules` on the box (WP-P13 post-merge
// review, RP14-9). Every path × reason series exists at 0 from boot.
func TestObjectIntegrity_EverySeriesExistsBeforeTheFirstFailure(t *testing.T) {
	assert.Equal(t, 5*2, promtest.CollectAndCount(objectIntegrityFailures),
		"failures: five paths × md5|size, all at 0 before any failure")
	assert.Equal(t, 5*3, promtest.CollectAndCount(objectIntegrityUnverified),
		"unverified: five paths × range|multipart|etag")
}
