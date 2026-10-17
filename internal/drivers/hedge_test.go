package drivers

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	promtest "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/FairForge/vaultaire/internal/common"
	"github.com/FairForge/vaultaire/internal/engine"
)

// WP-R6-3, the hedge: a small idempotent call (the probe's HeadBucket,
// HeadObject/Exists, a listing page, DeleteObject) that has not answered
// after DRIVER_HEDGE_AFTER is sent again on a fresh connection; the first
// answer wins and the other attempt is cancelled. Never a PUT, never a GET.
//
// edgeS3 stalls the requests a test chooses (the 2026-10-17 shape at
// Wasabi's edge: the request accepted, then nothing) and answers the rest;
// it records each request's remote address, start, and whether the client
// gave it up.

type edgeReq struct {
	method, path, query, remote string
	start                       time.Time
	cancelled                   atomic.Bool
}

type edgeS3 struct {
	mu    sync.Mutex
	reqs  []*edgeReq
	seen  map[string]int
	stall func(r *edgeReq, n int) bool // n: 1-based count of method+path
}

func (f *edgeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	er := &edgeReq{method: r.Method, path: r.URL.Path, query: r.URL.RawQuery, remote: r.RemoteAddr, start: time.Now()}
	f.mu.Lock()
	if f.seen == nil {
		f.seen = map[string]int{}
	}
	f.seen[r.Method+" "+r.URL.Path]++
	n := f.seen[r.Method+" "+r.URL.Path]
	f.reqs = append(f.reqs, er)
	stall := f.stall != nil && f.stall(er, n)
	f.mu.Unlock()
	if stall {
		// The body taken, then silence (and Go's server sees the client go
		// only once the body is read).
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case <-r.Context().Done():
			er.cancelled.Store(true)
		case <-time.After(10 * time.Second):
		}
		return
	}
	switch {
	case r.Method == http.MethodGet && r.URL.Query().Get("list-type") == "2":
		w.Header().Set("Content-Type", "application/xml")
		_, _ = fmt.Fprintf(w, `<?xml version="1.0"?><ListBucketResult><Name>b</Name><KeyCount>1</KeyCount><IsTruncated>false</IsTruncated><Contents><Key>t-t1/c/listed.bin</Key><Size>1</Size></Contents></ListBucketResult>`)
	case r.Method == http.MethodDelete:
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodHead && strings.Contains(r.URL.Path, "missing"):
		w.WriteHeader(http.StatusNotFound)
	case r.Method == http.MethodPut:
		w.Header().Set("ETag", `"e"`)
		w.WriteHeader(http.StatusOK)
	case r.Method == http.MethodGet:
		w.Header().Set("Content-Length", "2")
		_, _ = w.Write([]byte("ok"))
	default:
		w.WriteHeader(http.StatusOK)
	}
}

func (f *edgeS3) requests(method, pathSuffix string) []*edgeReq {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*edgeReq
	for _, r := range f.reqs {
		if r.method == method && strings.HasSuffix(r.path, pathSuffix) {
			out = append(out, r)
		}
	}
	return out
}

func (f *edgeS3) remotes() map[string]bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	m := map[string]bool{}
	for _, r := range f.reqs {
		m[r.remote] = true
	}
	return m
}

const testHedgeAfter = 150 * time.Millisecond

func newHedgedFixedDriver(t *testing.T, f *edgeS3) *IDriveDriver {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	d, err := NewFixedBucketS3Driver("wasabi", "ak", "sk", srv.URL, "us-west-1", "b", zap.NewNop())
	require.NoError(t, err)
	d.hedge.after = testHedgeAfter
	return d
}

func hedgesOf(driver, op string) (sent, won float64) {
	return promtest.ToFloat64(driverHedges.WithLabelValues(driver, op)), promtest.ToFloat64(driverHedgesWon.WithLabelValues(driver, op))
}

// warm leaves a pooled keep-alive connection behind and returns the remote
// addresses seen so far: the hedge must not be sent on one of them.
func warm(t *testing.T, d engine.Driver, f *edgeS3) map[string]bool {
	t.Helper()
	_, err := d.Exists(tctx(), "c", "warm.bin")
	require.NoError(t, err)
	return f.remotes()
}

func TestHedge_AStalledHeadIsAnsweredOnAFreshConnection(t *testing.T) {
	f := &edgeS3{stall: func(r *edgeReq, n int) bool {
		return r.method == http.MethodHead && strings.HasSuffix(r.path, "slow.bin") && n == 1
	}}
	d := newHedgedFixedDriver(t, f)
	pooled := warm(t, d, f)
	sent0, won0 := hedgesOf("wasabi", "head_object")

	start := time.Now()
	ok, err := d.Exists(tctx(), "c", "slow.bin")

	require.NoError(t, err)
	assert.True(t, ok)
	assert.Less(t, time.Since(start), 2*time.Second, "answered by the hedge, not after the 15 s bound")
	reqs := f.requests(http.MethodHead, "slow.bin")
	require.Len(t, reqs, 2, "the stalled HEAD and ONE hedge")
	assert.GreaterOrEqual(t, reqs[1].start.Sub(reqs[0].start), testHedgeAfter-20*time.Millisecond, "the hedge waits DRIVER_HEDGE_AFTER")
	assert.False(t, pooled[reqs[1].remote], "the hedge is on a connection opened for it (%s)", reqs[1].remote)
	assert.Eventually(t, reqs[0].cancelled.Load, 2*time.Second, 10*time.Millisecond, "the loser is cancelled")
	sent, won := hedgesOf("wasabi", "head_object")
	assert.Equal(t, 1.0, sent-sent0)
	assert.Equal(t, 1.0, won-won0)
}

func TestHedge_AQuickAnswerIsNotHedged(t *testing.T) {
	f := &edgeS3{}
	d := newHedgedFixedDriver(t, f)
	sent0, _ := hedgesOf("wasabi", "head_object")

	ok, err := d.Exists(tctx(), "c", "missing.bin")

	require.NoError(t, err)
	assert.False(t, ok, "a 404 is an answer")
	assert.Len(t, f.requests(http.MethodHead, "missing.bin"), 1)
	sent, _ := hedgesOf("wasabi", "head_object")
	assert.Equal(t, sent0, sent)
}

// The probe: HeadBucket hedged — the 2026-10-17 Wasabi probe failures
// (199 × the 15 s deadline in 2.7 days) were single stalls.
func TestHedge_TheProbeIsHedged(t *testing.T) {
	f := &edgeS3{stall: func(r *edgeReq, n int) bool { return r.method == http.MethodHead && r.path == "/b" && n == 1 }}
	d := newHedgedFixedDriver(t, f)
	sent0, won0 := hedgesOf("wasabi", "head_bucket")

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second) // the probe's deadline
	defer cancel()
	start := time.Now()
	require.NoError(t, d.HealthCheck(ctx))

	assert.Less(t, time.Since(start), 2*time.Second)
	sent, won := hedgesOf("wasabi", "head_bucket")
	assert.Equal(t, 1.0, sent-sent0)
	assert.Equal(t, 1.0, won-won0)
}

func TestHedge_DeleteAndAListingPageAreHedged(t *testing.T) {
	f := &edgeS3{stall: func(r *edgeReq, n int) bool {
		return n == 1 && (r.method == http.MethodDelete || (r.method == http.MethodGet && strings.Contains(r.query, "list-type")))
	}}
	d := newHedgedFixedDriver(t, f)

	require.NoError(t, d.Delete(tctx(), "c", "gone.bin"))
	keys, err := d.List(tctx(), "c", "")
	require.NoError(t, err)

	assert.Equal(t, []string{"listed.bin"}, keys)
	assert.Len(t, f.requests(http.MethodDelete, "gone.bin"), 2)
	sentD, wonD := hedgesOf("wasabi", "delete")
	sentL, wonL := hedgesOf("wasabi", "list")
	assert.GreaterOrEqual(t, sentD, 1.0)
	assert.GreaterOrEqual(t, wonD, 1.0)
	assert.GreaterOrEqual(t, sentL, 1.0)
	assert.GreaterOrEqual(t, wonL, 1.0)
}

// Never a GET (a body) and never a PUT (the bytes twice): their second
// request, if any, is the SDK's retry after the header bound or the PUT
// deadline's retry — never one at DRIVER_HEDGE_AFTER.
func TestHedge_NeverAGetOrAPut(t *testing.T) {
	// The GET bound 200 ms; the PUT's header bound above its deadline, as
	// in production (25 s against ≤ 15 s for a part).
	old := s3OpTimeouts
	s3OpTimeouts = OpTimeouts{Meta: 200 * time.Millisecond, Get: 200 * time.Millisecond, Put: 5 * time.Second, Complete: 10 * time.Minute}
	t.Cleanup(func() { s3OpTimeouts = old })
	f := &edgeS3{stall: func(r *edgeReq, n int) bool {
		return n == 1 && (r.method == http.MethodGet || r.method == http.MethodPut)
	}}
	d := newHedgedFixedDriver(t, f)
	d.hedge.after = 50 * time.Millisecond
	d.setPutTimeout(time.Second) // the PUT deadline: the 10 s floor, scaled below
	prev := putDeadlineFloor
	putDeadlineFloor = 500 * time.Millisecond
	t.Cleanup(func() { putDeadlineFloor = prev })

	start := time.Now()
	rc, err := d.Get(tctx(), "c", "body.bin")
	took := time.Since(start)
	require.NoError(t, err, "the SDK's retry after the 200 ms bound answers")
	_ = rc.Close()
	require.Len(t, f.requests(http.MethodGet, "body.bin"), 2)
	assert.GreaterOrEqual(t, took, 190*time.Millisecond, "the GET waited its bound: no hedge at 50 ms answered it")

	retries0 := retriesOf("wasabi")
	body := []byte("bytes")
	start = time.Now()
	require.NoError(t, d.Put(tctx(), "c", "put.bin", bytes.NewReader(body), engine.WithContentLength(int64(len(body)))))
	took = time.Since(start)
	require.Len(t, f.requests(http.MethodPut, "put.bin"), 2)
	assert.GreaterOrEqual(t, took, 490*time.Millisecond, "the PUT waited its deadline: no hedge at 50 ms answered it")
	assert.Equal(t, 1.0, retriesOf("wasabi")-retries0, "the second PUT is the deadline's one retry")
}

func TestHedge_ACallerAlreadyGoneIsNotHedged(t *testing.T) {
	f := &edgeS3{}
	d := newHedgedFixedDriver(t, f)
	sent0, _ := hedgesOf("wasabi", "head_object")
	ctx, cancel := context.WithCancel(tctx())
	cancel()

	_, err := d.Exists(ctx, "c", "any.bin")

	require.Error(t, err)
	assert.Empty(t, f.requests(http.MethodHead, "any.bin"))
	sent, _ := hedgesOf("wasabi", "head_object")
	assert.Equal(t, sent0, sent)
}

// Both attempts without an answer: the error is the first attempt's, and
// the call ends at the bound (the hedge never makes it wait longer).
func TestHedge_NoAnswerFromEitherIsTheFirstAttemptsError(t *testing.T) {
	shortHeaderTimeout(t) // the HEAD bound: 200 ms per attempt
	f := &edgeS3{stall: func(r *edgeReq, _ int) bool { return r.method == http.MethodHead }}
	d := newHedgedFixedDriver(t, f)
	d.hedge.after = 50 * time.Millisecond

	start := time.Now()
	_, err := d.Exists(tctx(), "c", "dead.bin")

	require.Error(t, err)
	assert.Less(t, time.Since(start), 3*time.Second)
}

// The Lyve driver (prod's primary) hedges too, under its own label.
func TestHedge_LyveIsHedged(t *testing.T) {
	f := &edgeS3{stall: func(r *edgeReq, n int) bool { return r.method == http.MethodHead && n == 1 }}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	client := s3.New(s3.Options{
		BaseEndpoint: aws.String(srv.URL), Region: "us-east-1",
		Credentials:  credentials.NewStaticCredentialsProvider("ak", "sk", ""),
		UsePathStyle: true, RetryMaxAttempts: requestRetryMaxAttempts,
	})
	d := &LyveDriver{client: client, region: "us-east-1", logger: zap.NewNop()}
	d.setPutTimeout(time.Second)
	d.hedge = newS3Hedger("lyve", testHedgeAfter, d.putClient.(*deadlinePutClient).fresh)
	sent0, won0 := hedgesOf("lyve", "head_object")

	ok, err := d.Exists(tctx(), "c", "k.bin")

	require.NoError(t, err)
	assert.True(t, ok)
	sent, won := hedgesOf("lyve", "head_object")
	assert.Equal(t, 1.0, sent-sent0)
	assert.Equal(t, 1.0, won-won0)
}

// SetBackendName moves the series to the engine's name for a regional driver.
func TestHedge_ARegionalDriverCountsUnderItsBackendName(t *testing.T) {
	f := &edgeS3{stall: func(r *edgeReq, n int) bool { return r.method == http.MethodHead && n == 1 }}
	d := newHedgedFixedDriver(t, f)
	d.SetBackendName("idrive-eu-central-1")
	sent0, _ := hedgesOf("idrive-eu-central-1", "head_object")

	_, err := d.Exists(tctx(), "c", "r.bin")

	require.NoError(t, err)
	sent, _ := hedgesOf("idrive-eu-central-1", "head_object")
	assert.Equal(t, 1.0, sent-sent0)
}

func TestDriverHedgeAfter_EnvRule(t *testing.T) {
	core, logs := observer.New(zap.WarnLevel)
	logger := zap.New(core)
	for _, c := range []struct {
		v    string
		want time.Duration
		warn bool
	}{
		{"", 2 * time.Second, false},
		{"500ms", 500 * time.Millisecond, false},
		{"30s", 30 * time.Second, false},
		{"off", 0, false},
		{"0", 0, false},
		{"100ms", 2 * time.Second, true},
		{"31s", 2 * time.Second, true},
		{"soon", 2 * time.Second, true},
	} {
		t.Setenv("DRIVER_HEDGE_AFTER", c.v)
		before := logs.Len()
		assert.Equal(t, c.want, driverHedgeAfterFromEnv(logger), "value %q", c.v)
		assert.Equal(t, c.warn, logs.Len() > before, "value %q: a rejected value is a Warn", c.v)
	}
}

// The SDK's attempts per call: two on a request path, three on a job
// (R7-13: three attempts × a header bound occupied a request for minutes).
func TestSDKAttempts_TwoOnARequestThreeOnAJob(t *testing.T) {
	shortHeaderTimeout(t)
	h := newHangingS3(t)
	d, err := NewFixedBucketS3Driver("wasabi", "ak", "sk", h.srv.URL, "us-west-1", "b", zap.NewNop())
	require.NoError(t, err)
	d.hedge.after = 0 // count the SDK's attempts alone

	_, err = d.Get(tctx(), "c", "k")
	require.Error(t, err)
	request := int(h.arrivals.Load())

	_, err = d.Get(common.WithJob(tctx()), "c", "k")
	require.Error(t, err)
	job := int(h.arrivals.Load()) - request

	assert.Equal(t, requestRetryMaxAttempts, request, "a request path")
	assert.Equal(t, jobRetryMaxAttempts, job, "a background job")
}

// Each class waits its own bound: a HEAD the meta bound, a GET's first byte
// the get bound (scaled down: 100 ms and 400 ms).
func TestOpTimeouts_EachClassWaitsItsOwnBound(t *testing.T) {
	old := s3OpTimeouts
	s3OpTimeouts = OpTimeouts{Meta: 100 * time.Millisecond, Get: 400 * time.Millisecond, Put: 400 * time.Millisecond, Complete: 10 * time.Minute}
	t.Cleanup(func() { s3OpTimeouts = old })
	h := newHangingS3(t)
	d, err := NewFixedBucketS3Driver("wasabi", "ak", "sk", h.srv.URL, "us-west-1", "b", zap.NewNop())
	require.NoError(t, err)
	d.hedge.after = 0
	taken := 0
	lives := func() []time.Duration { // every request so far, once each has ended
		require.Eventually(t, func() bool {
			h.mu.Lock()
			defer h.mu.Unlock()
			return len(h.lives) == int(h.arrivals.Load())
		}, 5*time.Second, 10*time.Millisecond)
		h.mu.Lock()
		defer h.mu.Unlock()
		out := append([]time.Duration(nil), h.lives[taken:]...)
		taken = len(h.lives)
		return out
	}

	_, err = d.Exists(tctx(), "c", "k")
	require.Error(t, err)
	for _, l := range lives() {
		assert.Less(t, l, 300*time.Millisecond, "a HEAD waits the meta bound")
	}
	_, err = d.Get(tctx(), "c", "k")
	require.Error(t, err)
	for _, l := range lives() {
		assert.GreaterOrEqual(t, l, 350*time.Millisecond, "a GET's first byte waits the get bound")
		assert.Less(t, l, 2*time.Second)
	}
}

func TestRequestClass_TheSDKsShapes(t *testing.T) {
	req := func(method, target string, hdr ...string) *http.Request {
		r := httptest.NewRequest(method, "https://s3.example"+target, nil)
		if len(hdr) == 2 {
			r.Header.Set(hdr[0], hdr[1])
		}
		return r
	}
	for _, c := range []struct {
		name string
		r    *http.Request
		want opClass
	}{
		{"HeadObject", req(http.MethodHead, "/b/t-1/c/k"), classMeta},
		{"HeadBucket", req(http.MethodHead, "/b"), classMeta},
		{"GetObject", req(http.MethodGet, "/b/t-1/c/k?x-id=GetObject"), classGet},
		{"ListObjectsV2", req(http.MethodGet, "/b?list-type=2&prefix=t-1%2F"), classMeta},
		{"GetObjectLockConfiguration", req(http.MethodGet, "/b?object-lock="), classMeta},
		{"PutObject", req(http.MethodPut, "/b/t-1/c/k?x-id=PutObject"), classPut},
		{"UploadPart", req(http.MethodPut, "/b/t-1/c/k?partNumber=1&uploadId=u&x-id=UploadPart"), classPut},
		{"CopyObject", req(http.MethodPut, "/b/t-1/c/k?x-id=CopyObject", "X-Amz-Copy-Source", "b/src"), classComplete},
		{"CreateBucket", req(http.MethodPut, "/b"), classMeta},
		{"CreateMultipartUpload", req(http.MethodPost, "/b/t-1/c/k?uploads=&x-id=CreateMultipartUpload"), classMeta},
		{"CompleteMultipartUpload", req(http.MethodPost, "/b/t-1/c/k?uploadId=u"), classComplete},
		{"DeleteObjects", req(http.MethodPost, "/b?delete="), classMeta},
		{"DeleteObject", req(http.MethodDelete, "/b/t-1/c/k?x-id=DeleteObject"), classMeta},
		{"AbortMultipartUpload", req(http.MethodDelete, "/b/t-1/c/k?uploadId=u&x-id=AbortMultipartUpload"), classMeta},
	} {
		assert.Equal(t, c.want, requestClass(c.r), c.name)
	}
}
