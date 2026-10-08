package api

import (
	"context"
	"crypto/md5" // #nosec G501 — S3 ETags are MD5
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/FairForge/vaultaire/internal/tenant"
	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// A deploy stops the old slot after HAProxy's drain; the engine then had 30 s
// (cmd/vaultaire shutdownTimeout) and a CompleteMultipartUpload that had
// already committed 200 + whitespace died with it — the client saw a broken
// body and the object was half-done or absent. The server now waits for
// detached long operations up to longOpDrainBound from each operation's
// start, then cancels and logs the rest.

// startComplete fires a CompleteMultipartUpload of key through the handler
// in the background and returns a channel that closes when it has answered.
func startComplete(srv *Server, tnt *tenant.Tenant, key, uploadID, body string) (<-chan *httptest.ResponseRecorder, context.CancelFunc) {
	done := make(chan *httptest.ResponseRecorder, 1)
	ctx, cancel := context.WithCancel(context.Background())
	r := httptest.NewRequest("POST", "/test-bucket/"+key+"?uploadId="+uploadID, strings.NewReader(body))
	r = r.WithContext(s3Ctx(ctx, tnt))
	go func() {
		w := httptest.NewRecorder()
		srv.handleS3Request(w, r)
		done <- w
	}()
	return done, cancel
}

func TestLongOps_ShutdownWaitsForADetachedCompleteToFinish(t *testing.T) {
	// Arrange: the backend write takes 400 ms; the keep-alive commits at 50 ms.
	srv, tnt, _ := newSlowMultipartServer(t, 400*time.Millisecond)
	uploadID, body := uploadTwoParts(t, srv, tnt, "deploy.bin")
	done, cancelClient := startComplete(srv, tnt, "deploy.bin", uploadID, body)
	defer cancelClient()
	require.Eventually(t, func() bool { return srv.longOps().count() == 1 }, time.Second, 5*time.Millisecond)

	// Act: the stop sequence runs while the operation is in flight.
	start := time.Now()
	abandoned := srv.drainLongOps(longOpDrainBound)
	waited := time.Since(start)

	// Assert: it returned only once the operation was done, and the object is whole.
	assert.Equal(t, 0, abandoned)
	assert.GreaterOrEqual(t, waited, 250*time.Millisecond, "the drain must have waited for the write")
	assert.Equal(t, 0, srv.longOps().count())
	w := <-done
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "<ETag>")
	getW := doS3Request(srv, tnt, "GET", "/test-bucket/deploy.bin", nil)
	require.Equal(t, http.StatusOK, getW.Code)
	assert.Equal(t, "hello, slow world", getW.Body.String())
}

func TestLongOps_PastTheBoundTheRestIsCancelledLoggedAndCounted(t *testing.T) {
	// Arrange: a write that would take 5 s, a bound of 150 ms.
	srv, tnt, _ := newSlowMultipartServer(t, 5*time.Second)
	core, logs := observer.New(zap.WarnLevel)
	srv.logger = zap.New(core)
	uploadID, body := uploadTwoParts(t, srv, tnt, "cut.bin")
	before := testutil.ToFloat64(longOpsAbandoned.WithLabelValues("CompleteMultipartUpload"))
	done, cancelClient := startComplete(srv, tnt, "cut.bin", uploadID, body)
	defer cancelClient()
	require.Eventually(t, func() bool { return srv.longOps().count() == 1 }, time.Second, 5*time.Millisecond)

	// Act
	start := time.Now()
	abandoned := srv.drainLongOps(150 * time.Millisecond)
	waited := time.Since(start)

	// Assert: the drain gave up at the bound, the operation was cancelled (not
	// left to die with the engine), and the leftover is named in the log.
	assert.Equal(t, 1, abandoned)
	assert.Less(t, waited, 2*time.Second)
	select {
	case w := <-done:
		assert.Equal(t, http.StatusOK, w.Code, "committed before the cut")
		assert.Contains(t, w.Body.String(), "<Error>", "the client learns it failed")
	case <-time.After(2 * time.Second):
		t.Fatal("the cancelled operation did not answer")
	}
	entries := logs.FilterMessage("long S3 operation abandoned at shutdown").All()
	require.Len(t, entries, 1)
	fields := entries[0].ContextMap()
	assert.Equal(t, "CompleteMultipartUpload", fields["op"])
	assert.Equal(t, tnt.ID, fields["tenant"])
	assert.Equal(t, "test-bucket", fields["bucket"])
	assert.Equal(t, "cut.bin", fields["key"])
	assert.Contains(t, fields, "age")
	assert.Equal(t, before+1, testutil.ToFloat64(longOpsAbandoned.WithLabelValues("CompleteMultipartUpload")))
	// Nothing half-done: no object, and the upload is still there to retry.
	getW := doS3Request(srv, tnt, "GET", "/test-bucket/cut.bin", nil)
	assert.Equal(t, http.StatusNotFound, getW.Code)
	memUploadsMu.RLock()
	assert.Equal(t, "active", memUploads[uploadID].Status)
	memUploadsMu.RUnlock()
}

func TestLongOps_TheBoundIsMeasuredFromTheOperationsStart(t *testing.T) {
	// Arrange: an operation already 300 ms old when the stop begins, bound 400 ms.
	srv, tnt, _ := newSlowMultipartServer(t, 5*time.Second)
	uploadID, body := uploadTwoParts(t, srv, tnt, "old.bin")
	done, cancelClient := startComplete(srv, tnt, "old.bin", uploadID, body)
	defer cancelClient()
	require.Eventually(t, func() bool { return srv.longOps().count() == 1 }, time.Second, 5*time.Millisecond)
	time.Sleep(300 * time.Millisecond)

	// Act
	start := time.Now()
	abandoned := srv.drainLongOps(400 * time.Millisecond)
	waited := time.Since(start)

	// Assert: ~100 ms more, not 400 — a deploy's budget is the operation's age, not the stop's.
	assert.Equal(t, 1, abandoned)
	assert.Less(t, waited, 300*time.Millisecond)
	<-done
}

func TestCompleteMultipartUpload_KilledMidWriteIsSafeToRetry(t *testing.T) {
	// Arrange: the first complete is cut by a shutdown mid-write (the client
	// got 200 + whitespace and then an <Error> body, or nothing at all).
	srv, tnt, slow := newSlowMultipartServer(t, 5*time.Second)
	uploadID, body := uploadTwoParts(t, srv, tnt, "retry.bin")
	done, cancelClient := startComplete(srv, tnt, "retry.bin", uploadID, body)
	defer cancelClient()
	require.Eventually(t, func() bool { return srv.longOps().count() == 1 }, time.Second, 5*time.Millisecond)
	require.Equal(t, 1, srv.drainLongOps(100*time.Millisecond))
	<-done

	// Act: the client retries the same CompleteMultipartUpload (the new process).
	slow.delay = 0
	w := doS3Request(srv, tnt, "POST", "/test-bucket/retry.bin?uploadId="+uploadID, strings.NewReader(body))

	// Assert: it finishes the same upload — same ETag, byte-exact object.
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var res CompleteMultipartUploadResult
	require.NoError(t, xml.Unmarshal(w.Body.Bytes(), &res))
	assert.True(t, strings.HasSuffix(res.ETag, `-2"`), res.ETag)
	getW := doS3Request(srv, tnt, "GET", "/test-bucket/retry.bin", nil)
	require.Equal(t, http.StatusOK, getW.Code)
	assert.Equal(t, "hello, slow world", getW.Body.String())
}

func TestCompleteMultipartUpload_RetryAfterTheCommitAnswersTheSameResult(t *testing.T) {
	// Arrange: the first complete landed (rows written) but its answer never
	// reached the client — it retries with the same part list.
	srv, tnt, _ := newTestMultipartServer(t)
	uploadID, body := uploadTwoParts(t, srv, tnt, "twice.bin")
	first := doS3Request(srv, tnt, "POST", "/test-bucket/twice.bin?uploadId="+uploadID, strings.NewReader(body))
	require.Equal(t, http.StatusOK, first.Code)
	var firstRes CompleteMultipartUploadResult
	require.NoError(t, xml.Unmarshal(first.Body.Bytes(), &firstRes))

	// Act
	again := doS3Request(srv, tnt, "POST", "/test-bucket/twice.bin?uploadId="+uploadID, strings.NewReader(body))

	// Assert: the same result, not NoSuchUpload (what AWS answers a retry of a completed upload with the same parts).
	require.Equal(t, http.StatusOK, again.Code, again.Body.String())
	var againRes CompleteMultipartUploadResult
	require.NoError(t, xml.Unmarshal(again.Body.Bytes(), &againRes))
	assert.Equal(t, firstRes.ETag, againRes.ETag)
	getW := doS3Request(srv, tnt, "GET", "/test-bucket/twice.bin", nil)
	assert.Equal(t, "hello, slow world", getW.Body.String())

	// A retry naming other parts is a different upload: NoSuchUpload, the object untouched.
	other := strings.Replace(body, "<PartNumber>2</PartNumber>", "<PartNumber>3</PartNumber>", 1)
	otherW := doS3Request(srv, tnt, "POST", "/test-bucket/twice.bin?uploadId="+uploadID, strings.NewReader(other))
	assert.Equal(t, http.StatusNotFound, otherW.Code)
	assert.Contains(t, otherW.Body.String(), "NoSuchUpload")
	// An upload that was never completed and whose key now holds other bytes is NoSuchUpload too.
	getW = doS3Request(srv, tnt, "GET", "/test-bucket/twice.bin", nil)
	assert.Equal(t, "hello, slow world", getW.Body.String())
}

func TestHealth_ReportsLongOpsInFlightToLoopbackOnly(t *testing.T) {
	// Arrange: one detached operation in flight.
	srv, tnt, _ := newSlowMultipartServer(t, 2*time.Second)
	srv.healthChecker = NewBackendHealthChecker()
	uploadID, body := uploadTwoParts(t, srv, tnt, "health.bin")
	done, cancelClient := startComplete(srv, tnt, "health.bin", uploadID, body)
	defer cancelClient()
	require.Eventually(t, func() bool { return srv.longOps().count() == 1 }, time.Second, 5*time.Millisecond)
	defer func() { srv.drainLongOps(0); <-done }()

	health := func(xff string) map[string]any {
		r := httptest.NewRequest("GET", "/health", nil)
		r.RemoteAddr = "127.0.0.1:40000"
		if xff != "" {
			r.Header.Set("X-Forwarded-For", xff)
		}
		w := httptest.NewRecorder()
		srv.handleHealthEnhanced(w, r)
		var m map[string]any
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &m))
		return m
	}

	// Act + Assert: the slot port sees the count; a request HAProxy forwarded does not.
	assert.EqualValues(t, 1, health("")["long_ops_in_flight"])
	_, exposed := health("203.0.113.9")["long_ops_in_flight"]
	assert.False(t, exposed)
}

func TestMetrics_LongOpsInFlightGauge(t *testing.T) {
	// Arrange
	srv, tnt, _ := newSlowMultipartServer(t, 2*time.Second)
	uploadID, body := uploadTwoParts(t, srv, tnt, "gauge.bin")
	done, cancelClient := startComplete(srv, tnt, "gauge.bin", uploadID, body)
	defer cancelClient()
	require.Eventually(t, func() bool { return srv.longOps().count() == 1 }, time.Second, 5*time.Millisecond)

	// Act
	w := httptest.NewRecorder()
	srv.handleMetrics(w, httptest.NewRequest("GET", "/metrics", nil))
	srv.drainLongOps(0)
	<-done
	after := httptest.NewRecorder()
	srv.handleMetrics(after, httptest.NewRequest("GET", "/metrics", nil))

	// Assert
	assert.Contains(t, w.Body.String(), `vaultaire_s3_long_ops_in_flight{op="CompleteMultipartUpload"} 1`)
	assert.Contains(t, after.Body.String(), `vaultaire_s3_long_ops_in_flight{op="CompleteMultipartUpload"} 0`)
	assert.Contains(t, after.Body.String(), `vaultaire_s3_long_ops_abandoned_total{op="CompleteMultipartUpload"}`)
	for _, op := range []string{"CopyObject", "DeleteObjects"} {
		assert.Contains(t, after.Body.String(), fmt.Sprintf(`vaultaire_s3_long_ops_in_flight{op="%s"} 0`, op), "every op has a series from boot")
	}
}

func TestCompleteMultipartUpload_RetryAfterTheCommit_DB(t *testing.T) {
	// Arrange: the rows a landed complete leaves behind — upload completed,
	// its parts, the head row with the assembled ETag.
	f := setupQuotaAccountingFixture(t, 1<<30)
	uploadID := "upload-" + strings.ReplaceAll(uuid.New().String(), "-", "")
	_, err := f.db.Exec(`
		INSERT INTO multipart_uploads (upload_id, tenant_id, bucket, object_key, status, created_at)
		VALUES ($1, $2, 'test-bucket', 'landed.bin', 'completed', NOW())`, uploadID, f.tenantID)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = f.db.Exec(`DELETE FROM multipart_uploads WHERE upload_id = $1`, uploadID) })
	p1, p2 := md5hex([]byte("hello, ")), md5hex([]byte("slow world"))
	_, err = f.db.Exec(`
		INSERT INTO multipart_parts (upload_id, part_number, etag, size_bytes, created_at)
		VALUES ($1, 1, $2, 7, NOW()), ($1, 2, $3, 10, NOW())`, uploadID, p1, p2)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = f.db.Exec(`DELETE FROM multipart_parts WHERE upload_id = $1`, uploadID) })
	h := md5.New() // #nosec G401
	for _, p := range []string{p1, p2} {
		b, _ := hex.DecodeString(p)
		h.Write(b)
	}
	etag := fmt.Sprintf("%x-2", h.Sum(nil))
	_, err = f.db.Exec(`
		INSERT INTO object_head_cache (tenant_id, bucket, object_key, size_bytes, etag, content_type, backend_name)
		VALUES ($1, 'test-bucket', 'landed.bin', 17, $2, 'application/octet-stream', 'local')`, f.tenantID, etag)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = f.db.Exec(`DELETE FROM object_head_cache WHERE tenant_id = $1 AND bucket = 'test-bucket' AND object_key = 'landed.bin'`, f.tenantID)
	})
	body := fmt.Sprintf(`<CompleteMultipartUpload><Part><PartNumber>1</PartNumber><ETag>"%s"</ETag></Part><Part><PartNumber>2</PartNumber><ETag>"%s"</ETag></Part></CompleteMultipartUpload>`, p1, p2)

	// Act
	w := doS3Request(f.server, f.tenant, "POST", "/test-bucket/landed.bin?uploadId="+uploadID, strings.NewReader(body))

	// Assert: the same result; another part list is NoSuchUpload; an overwritten key is NoSuchUpload.
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var res CompleteMultipartUploadResult
	require.NoError(t, xml.Unmarshal(w.Body.Bytes(), &res))
	assert.Equal(t, `"`+etag+`"`, res.ETag)
	only1 := fmt.Sprintf(`<CompleteMultipartUpload><Part><PartNumber>1</PartNumber><ETag>"%s"</ETag></Part></CompleteMultipartUpload>`, p1)
	w = doS3Request(f.server, f.tenant, "POST", "/test-bucket/landed.bin?uploadId="+uploadID, strings.NewReader(only1))
	assert.Equal(t, http.StatusNotFound, w.Code)
	_, err = f.db.Exec(`UPDATE object_head_cache SET etag = 'other' WHERE tenant_id = $1 AND bucket = 'test-bucket' AND object_key = 'landed.bin'`, f.tenantID)
	require.NoError(t, err)
	w = doS3Request(f.server, f.tenant, "POST", "/test-bucket/landed.bin?uploadId="+uploadID, strings.NewReader(body))
	assert.Equal(t, http.StatusNotFound, w.Code)
	assert.Contains(t, w.Body.String(), "NoSuchUpload")
}
