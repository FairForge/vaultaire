package api

// Prompt 2a.2 G1 (post-merge review of #634/#635): a shutdown's cut may stop
// the backend write or delete itself, never the bookkeeping after it. Each
// test reproduces one shape the review found on the code as merged.

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/FairForge/vaultaire/internal/drivers"
	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// storeThenAnswerDriver stores the whole body first, then answers after
// answerDelay — a backend that has the bytes when the cut arrives. Its
// answer dies with its context, as a real SDK call's does.
type storeThenAnswerDriver struct {
	engine.Driver
	answerDelay time.Duration
	stored      chan struct{}
}

func (d *storeThenAnswerDriver) Put(ctx context.Context, c, a string, r io.Reader, opts ...engine.PutOption) error {
	if err := d.Driver.Put(context.WithoutCancel(ctx), c, a, r, opts...); err != nil {
		return err
	}
	select {
	case d.stored <- struct{}{}:
	default:
	}
	select {
	case <-time.After(d.answerDelay):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// slowDeleteDriver takes `delay` to delete and says when one starts.
type slowDeleteDriver struct {
	engine.Driver
	delay    time.Duration
	deleting chan struct{}
}

func (d *slowDeleteDriver) Delete(ctx context.Context, c, a string) error {
	select {
	case d.deleting <- struct{}{}:
	default:
	}
	time.Sleep(d.delay)
	return d.Driver.Delete(ctx, c, a)
}

// longOpFixture is the DB-backed quota fixture with the keep-alive shortened
// and test-bucket registered (lockDays > 0 = Object Lock with a COMPLIANCE
// default retention of that many days).
func longOpFixture(t *testing.T, lockDays int) *quotaAccountingFixture {
	t.Helper()
	f := setupQuotaAccountingFixture(t, 1<<30)
	f.server.longOpThreshold = 50 * time.Millisecond
	f.server.longOpInterval = 20 * time.Millisecond
	f.server.longOpGrace = 2 * time.Second
	mode := ""
	if lockDays > 0 {
		mode = "COMPLIANCE"
	}
	_, err := f.db.Exec(`
		INSERT INTO buckets (tenant_id, name, visibility, object_lock_enabled, default_retention_mode, default_retention_days)
		VALUES ($1, 'test-bucket', 'private', $2, $3, $4)`, f.tenantID, lockDays > 0, mode, lockDays)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = f.db.Exec(`DELETE FROM object_locks WHERE tenant_id = $1`, f.tenantID)
		_, _ = f.db.Exec(`DELETE FROM object_versions WHERE tenant_id = $1`, f.tenantID)
		_, _ = f.db.Exec(`DELETE FROM s3_long_op_incidents WHERE tenant_id = $1`, f.tenantID)
		_, _ = f.db.Exec(`DELETE FROM buckets WHERE tenant_id = $1`, f.tenantID)
	})
	return f
}

func (f *quotaAccountingFixture) headRow(t *testing.T, key string) (size int64, etag string, ok bool) {
	t.Helper()
	err := f.db.QueryRow(`SELECT size_bytes, etag FROM object_head_cache WHERE tenant_id = $1 AND bucket = 'test-bucket' AND object_key = $2`,
		f.tenantID, key).Scan(&size, &etag)
	return size, etag, err == nil
}

func (f *quotaAccountingFixture) lockRows(t *testing.T, key string) int {
	t.Helper()
	var n int
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM object_locks WHERE tenant_id = $1 AND bucket = 'test-bucket' AND object_key = $2`,
		f.tenantID, key).Scan(&n))
	return n
}

// startS3 runs one request through the S3 router in the background.
func startS3(srv *Server, f *quotaAccountingFixture, method, path, body string) <-chan *httptest.ResponseRecorder {
	done := make(chan *httptest.ResponseRecorder, 1)
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r = r.WithContext(f.ctx(context.Background()))
	go func() {
		w := httptest.NewRecorder()
		srv.handleS3Request(w, r)
		done <- w
	}()
	return done
}

func waitRecorder(t *testing.T, ch <-chan *httptest.ResponseRecorder) *httptest.ResponseRecorder {
	t.Helper()
	select {
	case w := <-ch:
		return w
	case <-time.After(10 * time.Second):
		t.Fatal("the operation did not answer")
		return nil
	}
}

func TestCompleteMultipartUpload_CutInTheBookkeepingStillAppliesTheDefaultRetention(t *testing.T) {
	// Arrange: a COMPLIANCE-30d bucket; the key's current row routes to a
	// second backend whose delete (the displaced blob) takes 600 ms — the
	// cut lands there, after the commit.
	f := longOpFixture(t, 30)
	otherDir := t.TempDir()
	other := &slowDeleteDriver{Driver: drivers.NewLocalDriver(otherDir, zap.NewNop()), delay: 600 * time.Millisecond, deleting: make(chan struct{}, 1)}
	f.server.engine.AddDriver("other", other)
	_, err := f.db.Exec(`
		INSERT INTO object_head_cache (tenant_id, bucket, object_key, size_bytes, etag, content_type, backend_name)
		VALUES ($1, 'test-bucket', 'worm.bin', 5, 'old', 'application/octet-stream', 'other')`, f.tenantID)
	require.NoError(t, err)
	uploadID, body := uploadTwoParts(t, f.server, f.tenant, "worm.bin")

	// Act: the complete runs; the stop sequence cuts it during the displaced delete.
	done := startS3(f.server, f, "POST", "/test-bucket/worm.bin?uploadId="+uploadID, body)
	select {
	case <-other.deleting:
	case <-time.After(5 * time.Second):
		t.Fatal("the complete never reached the displaced blob's delete")
	}
	f.server.drainLongOps(0)
	w := waitRecorder(t, done)

	// Assert: the client was told it succeeded — so the object must carry
	// the bucket's retention: a DELETE is refused.
	require.Contains(t, w.Body.String(), "<ETag>", w.Body.String())
	assert.Equal(t, 1, f.lockRows(t, "worm.bin"), "the default retention must be written after a cut")
	del := doS3Request(f.server, f.tenant, "DELETE", "/test-bucket/worm.bin", nil)
	assert.Equal(t, http.StatusForbidden, del.Code, "a COMPLIANCE object must not be deletable")
}

func TestCompleteMultipartUpload_RetryRepairsAnEarlierCutsMissingRetention(t *testing.T) {
	// Arrange: a complete that landed (upload completed, head row) but whose
	// lock row was never written — what the cut above left before the fix.
	f := longOpFixture(t, 30)
	uploadID, body := uploadTwoParts(t, f.server, f.tenant, "landed.bin")
	first := doS3Request(f.server, f.tenant, "POST", "/test-bucket/landed.bin?uploadId="+uploadID, strings.NewReader(body))
	require.Equal(t, http.StatusOK, first.Code, first.Body.String())
	_, err := f.db.Exec(`DELETE FROM object_locks WHERE tenant_id = $1 AND object_key = 'landed.bin'`, f.tenantID)
	require.NoError(t, err)

	// Act: the client retries the complete.
	again := doS3Request(f.server, f.tenant, "POST", "/test-bucket/landed.bin?uploadId="+uploadID, strings.NewReader(body))

	// Assert: the same result, and the retention is back.
	require.Equal(t, http.StatusOK, again.Code, again.Body.String())
	assert.Equal(t, 1, f.lockRows(t, "landed.bin"))
	del := doS3Request(f.server, f.tenant, "DELETE", "/test-bucket/landed.bin", nil)
	assert.Equal(t, http.StatusForbidden, del.Code)
}

func TestCompleteMultipartUpload_RetryNeverShortensALaterRetention(t *testing.T) {
	// Arrange: a landed complete whose retention was extended since.
	f := longOpFixture(t, 1)
	uploadID, body := uploadTwoParts(t, f.server, f.tenant, "kept.bin")
	require.Equal(t, http.StatusOK, doS3Request(f.server, f.tenant, "POST", "/test-bucket/kept.bin?uploadId="+uploadID, strings.NewReader(body)).Code)
	later := time.Now().Add(400 * 24 * time.Hour).UTC().Truncate(time.Second)
	_, err := f.db.Exec(`UPDATE object_locks SET retain_until_date = $2 WHERE tenant_id = $1 AND object_key = 'kept.bin'`, f.tenantID, later)
	require.NoError(t, err)

	// Act
	require.Equal(t, http.StatusOK, doS3Request(f.server, f.tenant, "POST", "/test-bucket/kept.bin?uploadId="+uploadID, strings.NewReader(body)).Code)

	// Assert
	var until time.Time
	require.NoError(t, f.db.QueryRow(`SELECT retain_until_date FROM object_locks WHERE tenant_id = $1 AND object_key = 'kept.bin'`, f.tenantID).Scan(&until))
	assert.True(t, until.Equal(later), "the retry re-dated an unexpired retention: %v", until)
}

func TestCompleteMultipartUpload_OverwriteCutAfterTheBackendStoredGetsItsHeadRow(t *testing.T) {
	// Arrange: a 21-byte object; the backend stores the new bytes first and
	// answers 300 ms later — the cut arrives in between.
	f := longOpFixture(t, 0)
	require.Equal(t, http.StatusOK, f.put(t, "over.bin", []byte("twenty-one bytes old!")))
	local, ok := f.server.engine.GetDriver("local")
	require.True(t, ok)
	st := &storeThenAnswerDriver{Driver: local, answerDelay: 300 * time.Millisecond, stored: make(chan struct{}, 1)}
	f.server.engine.AddDriver("local", st)
	f.server.engine.SetPrimary("local")
	uploadID, body := uploadTwoParts(t, f.server, f.tenant, "over.bin")

	// Act
	done := startS3(f.server, f, "POST", "/test-bucket/over.bin?uploadId="+uploadID, body)
	select {
	case <-st.stored:
	case <-time.After(5 * time.Second):
		t.Fatal("the backend never stored the assembled object")
	}
	f.server.drainLongOps(0)
	w := waitRecorder(t, done)

	// Assert: the head row describes the bytes the backend holds.
	get := doS3Request(f.server, f.tenant, "GET", "/test-bucket/over.bin", nil)
	require.Equal(t, http.StatusOK, get.Code)
	size, etag, found := f.headRow(t, "over.bin")
	require.True(t, found)
	assert.Equal(t, "hello, slow world", get.Body.String())
	assert.Equal(t, int64(get.Body.Len()), size, "head row size vs the bytes served")
	assert.True(t, strings.HasSuffix(etag, "-2"), "head row ETag must be the new multipart ETag, got %q", etag)
	assert.Contains(t, w.Body.String(), "<ETag>")
}

// batchBody is a DeleteObjects body for keys.
func batchBody(keys []string) string {
	var b strings.Builder
	b.WriteString("<Delete>")
	for _, k := range keys {
		fmt.Fprintf(&b, "<Object><Key>%s</Key></Object>", k)
	}
	b.WriteString("</Delete>")
	return b.String()
}

func TestDeleteObjects_ACutBatchNeverLeavesAPhantomOrBlamesObjectLock(t *testing.T) {
	// Arrange: 32 keys; deletes take 400 ms and run 16 at a time — the cut
	// lands while the first 16 are in flight.
	f := longOpFixture(t, 0)
	keys := make([]string, 32)
	for i := range keys {
		keys[i] = fmt.Sprintf("k%02d", i)
		require.Equal(t, http.StatusOK, f.put(t, keys[i], []byte("x"+keys[i])))
	}
	local, _ := f.server.engine.GetDriver("local")
	slow := &slowDriver{Driver: local, delay: 400 * time.Millisecond}
	f.server.engine.AddDriver("local", slow)
	f.server.engine.SetPrimary("local")

	// Act
	done := startS3(f.server, f, "POST", "/test-bucket?delete", batchBody(keys))
	require.Eventually(t, func() bool { return slow.inFlight.Load() == batchDeleteConcurrency }, 5*time.Second, time.Millisecond)
	f.server.drainLongOps(0)
	w := waitRecorder(t, done)

	// Assert
	var res DeleteResult
	require.NoError(t, xml.Unmarshal([]byte(strings.TrimSpace(w.Body.String())), &res), w.Body.String())
	phantoms, accessDenied := 0, 0
	for _, d := range res.Deleted {
		if _, _, found := f.headRow(t, d.Key); found {
			phantoms++
		}
	}
	for _, e := range res.Errors {
		if e.Code == ErrAccessDenied {
			accessDenied++
		}
		_, _, found := f.headRow(t, e.Key)
		assert.True(t, found, "a key reported as an error must still be there: %s", e.Key)
	}
	assert.Equal(t, 0, phantoms, "keys reported Deleted that kept their head row")
	assert.Equal(t, 0, accessDenied, "no key is Object-Locked here")
	assert.Equal(t, len(keys), len(res.Deleted)+len(res.Errors))
	assert.NotEmpty(t, res.Deleted, "the keys in flight at the cut finish")
}

func TestDeleteObjects_EveryKeyFailedIsNotCountedOK(t *testing.T) {
	// Arrange: two keys under a COMPLIANCE retention.
	f := longOpFixture(t, 30)
	for _, k := range []string{"a", "b"} {
		require.Equal(t, http.StatusOK, f.put(t, k, []byte(k)))
	}
	before := outcomeCount(longOpBatch, longOpErrorAfterCommit)
	beforeOK := outcomeCount(longOpBatch, longOpOK)

	// Act
	w := doS3Request(f.server, f.tenant, "POST", "/test-bucket?delete", strings.NewReader(batchBody([]string{"a", "b"})))

	// Assert
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, 2, strings.Count(w.Body.String(), "<Code>AccessDenied</Code>"))
	assert.Equal(t, before+1, outcomeCount(longOpBatch, longOpErrorAfterCommit))
	assert.Equal(t, beforeOK, outcomeCount(longOpBatch, longOpOK))
}

func TestLongOps_AMissingBucketIsNoSuchBucketNot200(t *testing.T) {
	// Arrange
	f := longOpFixture(t, 0)
	require.Equal(t, http.StatusOK, f.put(t, "src.bin", []byte("source")))

	// Act
	batch := doS3Request(f.server, f.tenant, "POST", "/no-such-bucket?delete", strings.NewReader(batchBody([]string{"x"})))
	r := httptest.NewRequest("PUT", "/no-such-bucket/copy.bin", nil)
	r.Header.Set("x-amz-copy-source", "/test-bucket/src.bin")
	r = r.WithContext(f.ctx(r.Context()))
	cp := httptest.NewRecorder()
	f.server.handleS3Request(cp, r)

	// Assert
	assert.Equal(t, http.StatusNotFound, batch.Code, batch.Body.String())
	assert.Contains(t, batch.Body.String(), "NoSuchBucket")
	assert.Equal(t, http.StatusNotFound, cp.Code, cp.Body.String())
	assert.Contains(t, cp.Body.String(), "NoSuchBucket")
}

func TestChunkedCopy_BeginsTheKeepAliveAfterItsRefusals(t *testing.T) {
	// Arrange
	f := setupChunkedCopyFixture(t)
	f.seedChunked(t, "src.bin", generateTestData(8*1024))
	gate := &longOpGate{begun: make(chan struct{})}
	r := httptest.NewRequest("PUT", "/dest-bucket/copy.bin", nil)
	r.Header.Set("x-amz-copy-source", "/test-bucket/src.bin")
	r = r.WithContext(context.WithValue(f.ctx(r.Context()), longOpGateKey{}, gate))

	// Act
	w := httptest.NewRecorder()
	f.server.copyObject(w, r, f.s3Req("dest-bucket", "copy.bin"))

	// Assert
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	select {
	case <-gate.begun:
	default:
		t.Fatal("the chunked copy never called longOpBegin: it waits out the 60 s prelude cap")
	}
}

// slowWriter is a client whose writes take a while to go out.
type slowWriter struct {
	*httptest.ResponseRecorder
	delay   time.Duration
	written atomic.Bool
}

func (w *slowWriter) Write(p []byte) (int, error) {
	time.Sleep(w.delay)
	n, err := w.ResponseRecorder.Write(p)
	w.written.Store(true)
	return n, err
}

func TestLongOps_TheDrainWaitsUntilTheAnswerIsWritten(t *testing.T) {
	// Arrange: an operation that finishes on a signal; its client's write takes 300 ms.
	srv := &Server{logger: zap.NewNop()}
	release := make(chan struct{})
	w := &slowWriter{ResponseRecorder: httptest.NewRecorder(), delay: 300 * time.Millisecond}
	r := httptest.NewRequest("POST", "/b?delete", nil)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		srv.runLongS3Op(w, r, longOpInfo{Op: longOpBatch, Bucket: "b"}, func(w http.ResponseWriter, r *http.Request) {
			<-release
			_, _ = w.Write([]byte("<DeleteResult/>"))
		})
	}()
	require.Eventually(t, func() bool { return srv.longOps().count() == 1 }, time.Second, time.Millisecond)

	// Act
	close(release)
	srv.drainLongOps(time.Hour)

	// Assert: by the time the stop sequence moves on, the answer is out.
	assert.True(t, w.written.Load(), "the drain returned before the response was written")
	<-finished
}

func TestLongOps_EachOperationIsCutAtItsOwnBound(t *testing.T) {
	// Arrange: A starts, B starts 300 ms later; the bound is 400 ms.
	srv, tnt, _ := newSlowMultipartServer(t, 5*time.Second)
	core, logs := observer.New(zap.WarnLevel)
	srv.logger = zap.New(core)
	upA, bodyA := uploadTwoParts(t, srv, tnt, "a.bin")
	upB, bodyB := uploadTwoParts(t, srv, tnt, "b.bin")
	doneA, cancelA := startComplete(srv, tnt, "a.bin", upA, bodyA)
	defer cancelA()
	require.Eventually(t, func() bool { return srv.longOps().count() == 1 }, time.Second, time.Millisecond)
	time.Sleep(300 * time.Millisecond)
	doneB, cancelB := startComplete(srv, tnt, "b.bin", upB, bodyB)
	defer cancelB()
	require.Eventually(t, func() bool { return srv.longOps().count() == 2 }, time.Second, time.Millisecond)

	// Act
	assert.Equal(t, 2, srv.drainLongOps(400*time.Millisecond))
	<-doneA
	<-doneB

	// Assert: A got its 400 ms, not B's start + 400 ms.
	ages := map[string]time.Duration{}
	for _, e := range logs.FilterMessage("long S3 operation abandoned at shutdown").All() {
		m := e.ContextMap()
		ages[m["key"].(string)] = m["age"].(time.Duration)
	}
	require.Len(t, ages, 2)
	assert.Less(t, ages["a.bin"], 550*time.Millisecond, "A was cut at %v", ages["a.bin"])
	assert.GreaterOrEqual(t, ages["a.bin"], 400*time.Millisecond)
	assert.Less(t, ages["b.bin"], 550*time.Millisecond)
}

func TestLongOps_AStoppingSlotPersistsWhatItCutAndTheActiveSlotExportsIt(t *testing.T) {
	// Arrange: a complete whose write would take 5 s.
	f := longOpFixture(t, 0)
	local, _ := f.server.engine.GetDriver("local")
	f.server.engine.AddDriver("local", &slowDriver{Driver: local, delay: 5 * time.Second})
	f.server.engine.SetPrimary("local")
	f.server.longOpGrace = 50 * time.Millisecond
	uploadID, body := uploadTwoParts(t, f.server, f.tenant, "cut.bin")
	done := startS3(f.server, f, "POST", "/test-bucket/cut.bin?uploadId="+uploadID, body)
	require.Eventually(t, func() bool { return f.server.longOps().count() == 1 }, 5*time.Second, time.Millisecond)

	// Act: the stopping slot cuts it; another server (the active slot) is scraped.
	require.Equal(t, 1, f.server.drainLongOps(0))
	waitRecorder(t, done)
	active := &Server{logger: zap.NewNop(), db: f.db}
	m := httptest.NewRecorder()
	active.handleMetrics(m, httptest.NewRequest("GET", "/metrics", nil))

	// Assert
	var op, outcome, bucket, key, version string
	require.NoError(t, f.db.QueryRow(`
		SELECT op, outcome, bucket, object_key, version FROM s3_long_op_incidents WHERE tenant_id = $1`, f.tenantID).
		Scan(&op, &outcome, &bucket, &key, &version))
	assert.Equal(t, []string{longOpComplete, longOpAbandoned, "test-bucket", "cut.bin", BuildSHA}, []string{op, outcome, bucket, key, version})
	assert.Regexp(t, `vaultaire_s3_long_ops_abandoned_total\{op="CompleteMultipartUpload"\} [1-9]`, m.Body.String())
	assert.Contains(t, m.Body.String(), `vaultaire_s3_long_ops_drain_errors_total{op="DeleteObjects"}`)
	// The upload is still there to retry.
	var status string
	require.NoError(t, f.db.QueryRow(`SELECT status FROM multipart_uploads WHERE upload_id = $1`, uploadID).Scan(&status))
	assert.Equal(t, "active", status)
}

func TestLongOps_AFailureAfterTheCommitDuringADrainIsPersisted(t *testing.T) {
	// Arrange: a complete that commits its 200 and then fails, while the slot stops.
	f := longOpFixture(t, 0)
	local, _ := f.server.engine.GetDriver("local")
	f.server.engine.AddDriver("local", &slowDriver{Driver: local, delay: 300 * time.Millisecond, failPut: true})
	f.server.engine.SetPrimary("local")
	uploadID, body := uploadTwoParts(t, f.server, f.tenant, "fail.bin")
	done := startS3(f.server, f, "POST", "/test-bucket/fail.bin?uploadId="+uploadID, body)
	require.Eventually(t, func() bool { return f.server.longOps().count() == 1 }, 5*time.Second, time.Millisecond)

	// Act
	assert.Equal(t, 0, f.server.drainLongOps(time.Hour))
	w := waitRecorder(t, done)

	// Assert
	assert.Contains(t, w.Body.String(), "<Error>")
	var n int
	require.NoError(t, f.db.QueryRow(`
		SELECT COUNT(*) FROM s3_long_op_incidents WHERE tenant_id = $1 AND outcome = 'error_after_commit' AND object_key = 'fail.bin'`,
		f.tenantID).Scan(&n))
	assert.Equal(t, 1, n)
}
