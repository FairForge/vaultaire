package drivers

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/FairForge/vaultaire/internal/common"
	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// WP-R7-4 R7-08: graphDo made up to 5 attempts, slept up to 30 s between
// them with time.Sleep and honoured Retry-After unbounded (a 429 with
// Retry-After: 120 slept 120 s inside a request), none of it watching ctx;
// putChunk slept 1/2/4 s the same way. Now every wait selects on ctx, a
// Retry-After past the caller's deadline is not waited for, and the attempts
// are the S3-class drivers' bound: 2 on a request path, 3 under
// common.WithJob. A caller already gone is never retried.

// odScriptRT answers each request with fn(n, req), n counting from 0.
type odScriptRT struct {
	mu sync.Mutex
	n  int
	fn func(n int, req *http.Request) (*http.Response, error)
}

func (s *odScriptRT) RoundTrip(req *http.Request) (*http.Response, error) {
	s.mu.Lock()
	n := s.n
	s.n++
	s.mu.Unlock()
	if req.Body != nil {
		_, _ = io.Copy(io.Discard, req.Body)
		_ = req.Body.Close()
	}
	return s.fn(n, req)
}

func (s *odScriptRT) calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.n
}

func odResp(req *http.Request, status int, body string, hdr map[string]string) *http.Response {
	h := make(http.Header)
	for k, v := range hdr {
		h.Set(k, v)
	}
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: h, Request: req}
}

// waitLog records the waits a tenant asked for and returns at once.
type waitLog struct {
	mu    sync.Mutex
	waits []time.Duration
}

func (w *waitLog) wait(ctx context.Context, d time.Duration) error {
	w.mu.Lock()
	w.waits = append(w.waits, d)
	w.mu.Unlock()
	return ctx.Err()
}

func (w *waitLog) all() []time.Duration {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]time.Duration(nil), w.waits...)
}

func scriptedTenant(rt http.RoundTripper, wl *waitLog) *odTenant {
	t := stubTenant("a", rt)
	t.uploadHTTP = &http.Client{Transport: rt}
	if wl != nil {
		t.wait = wl.wait
	}
	return t
}

func graphGet(ctx context.Context, t *testing.T) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, graphBase+"/drives/DRIVE-a", nil)
	require.NoError(t, err)
	return req
}

func TestGraphDo_AFailingCallIsTriedTwiceOnARequestPath(t *testing.T) {
	rt := &odScriptRT{fn: func(_ int, req *http.Request) (*http.Response, error) { return odResp(req, 503, "", nil), nil }}
	wl := &waitLog{}
	tn := scriptedTenant(rt, wl)
	ctx := context.Background()

	_, err := tn.graphDo(ctx, graphGet(ctx, t))

	require.Error(t, err)
	assert.Equal(t, 2, rt.calls(), "request path: two attempts (requestRetryMaxAttempts)")
	assert.Len(t, wl.all(), 1, "one wait between the two")
}

func TestGraphDo_AJobGetsThreeAttempts(t *testing.T) {
	rt := &odScriptRT{fn: func(_ int, req *http.Request) (*http.Response, error) { return odResp(req, 500, "", nil), nil }}
	tn := scriptedTenant(rt, &waitLog{})
	ctx := common.WithJob(context.Background())

	_, err := tn.graphDo(ctx, graphGet(ctx, t))

	require.Error(t, err)
	assert.Equal(t, 3, rt.calls(), "under common.WithJob: three attempts (jobRetryMaxAttempts)")
}

func TestGraphDo_ARetryAfterPastTheDeadlineIsNotWaitedFor(t *testing.T) {
	rt := &odScriptRT{fn: func(_ int, req *http.Request) (*http.Response, error) {
		return odResp(req, 429, "", map[string]string{"Retry-After": "120"}), nil
	}}
	tn := scriptedTenant(rt, nil) // the real wait: a 120 s sleep would hang the test
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	start := time.Now()
	_, err := tn.graphDo(ctx, graphGet(ctx, t))

	require.Error(t, err)
	assert.Less(t, time.Since(start), time.Second, "a Retry-After beyond the caller's deadline is given up on at once")
	assert.Equal(t, 1, rt.calls())
	assert.Contains(t, err.Error(), "throttled")
}

func TestGraphDo_ARetryAfterIsCappedWithNoDeadline(t *testing.T) {
	rt := &odScriptRT{fn: func(n int, req *http.Request) (*http.Response, error) {
		if n == 0 {
			return odResp(req, 429, "", map[string]string{"Retry-After": "120"}), nil
		}
		return odResp(req, 200, `{}`, nil), nil
	}}
	wl := &waitLog{}
	tn := scriptedTenant(rt, wl)
	ctx := context.Background()

	resp, err := tn.graphDo(ctx, graphGet(ctx, t))

	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Len(t, wl.all(), 1)
	assert.LessOrEqual(t, wl.all()[0], odMaxRetryWait, "Retry-After: 120 waits at most odMaxRetryWait")
	assert.GreaterOrEqual(t, wl.all()[0], odMaxRetryWait/2)
}

func TestGraphDo_AWaitEndsWhenTheCallerGoes(t *testing.T) {
	rt := &odScriptRT{fn: func(_ int, req *http.Request) (*http.Response, error) {
		return odResp(req, 503, "", map[string]string{"Retry-After": "10"}), nil
	}}
	tn := scriptedTenant(rt, nil)
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)

	start := time.Now()
	_, err := tn.graphDo(ctx, graphGet(ctx, t))

	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)
	assert.Less(t, time.Since(start), 2*time.Second, "the wait selects on ctx")
	assert.Equal(t, 1, rt.calls(), "no attempt after the caller went")
}

func TestGraphDo_ACallerAlreadyGoneIsNeverRetried(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	rt := &odScriptRT{fn: func(_ int, req *http.Request) (*http.Response, error) {
		cancel()
		return nil, context.Canceled
	}}
	wl := &waitLog{}
	tn := scriptedTenant(rt, wl)

	_, err := tn.graphDo(ctx, graphGet(ctx, t))

	require.Error(t, err)
	assert.Equal(t, 1, rt.calls())
	assert.Empty(t, wl.all())
}

func TestPutChunk_RetriesAreBoundedAndWaitOnCtx(t *testing.T) {
	rt := &odScriptRT{fn: func(_ int, req *http.Request) (*http.Response, error) { return odResp(req, 500, "", nil), nil }}
	wl := &waitLog{}
	tn := scriptedTenant(rt, wl)

	err := tn.putChunk(context.Background(), "https://upload.example.test/s", []byte("abc"), 0, 2, 3)

	require.Error(t, err)
	assert.Equal(t, 2, rt.calls(), "a fragment on a request path: two attempts")
	assert.Len(t, wl.all(), 1)

	rt2 := &odScriptRT{fn: func(_ int, req *http.Request) (*http.Response, error) { return odResp(req, 503, "", nil), nil }}
	tn2 := scriptedTenant(rt2, &waitLog{})
	require.Error(t, tn2.putChunk(common.WithJob(context.Background()), "https://upload.example.test/s", []byte("abc"), 0, 2, 3))
	assert.Equal(t, 3, rt2.calls(), "under a job: three")
}

func TestPutChunk_ACallerGoneMidWaitStopsAtOnce(t *testing.T) {
	rt := &odScriptRT{fn: func(_ int, req *http.Request) (*http.Response, error) { return odResp(req, 503, "", nil), nil }}
	tn := scriptedTenant(rt, nil)
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(20*time.Millisecond, cancel)

	start := time.Now()
	err := tn.putChunk(ctx, "https://upload.example.test/s", []byte("abc"), 0, 2, 3)

	require.Error(t, err)
	assert.Less(t, time.Since(start), 500*time.Millisecond, "the 1 s backoff selects on ctx")
}

// R7-18 / Prompt 2b.5 F1.1: a Graph error is typed — status and Graph code —
// and the miss is engine.NotFoundError, classified by type.
func TestODError_CarriesStatusAndGraphCode(t *testing.T) {
	rt := &odScriptRT{fn: func(_ int, req *http.Request) (*http.Response, error) {
		return odResp(req, 404, `{"error":{"code":"itemNotFound","message":"The resource could not be found."}}`, nil), nil
	}}
	tn := scriptedTenant(rt, nil)
	ctx := context.Background()

	_, err := tn.graphDo(ctx, graphGet(ctx, t))

	var oe *odError
	require.True(t, errors.As(err, &oe))
	assert.Equal(t, 404, oe.Status)
	assert.Equal(t, "itemNotFound", oe.Code)
	assert.True(t, odIsNotFound(err))
}

func TestODError_A404ThatIsNotAnItemMissIsNotAMiss(t *testing.T) {
	err := &odError{tenant: "a", Status: 404, Code: "ResourceNotFound"} // the drive / user gone
	assert.False(t, odIsNotFound(err), "a missing drive is the account's failure, never an object miss")
	assert.True(t, odIsNotFound(&odError{tenant: "a", Status: 404, Code: "itemNotFound"}))
	assert.True(t, odIsNotFound(&odError{tenant: "a", Status: 404}), "a 404 with no Graph body is a miss")
}

func TestOneDriveGet_AMissEverywhereIsATypedEngineMiss(t *testing.T) {
	ctx := common.WithTenantID(context.Background(), "x")
	d, _ := stubFleet(2, zap.NewNop())

	_, err := d.Get(ctx, "bkt", "404.html")

	require.Error(t, err)
	assert.True(t, engine.IsObjectMiss(err))
	var nf engine.NotFoundError
	var nfp *engine.NotFoundError
	assert.True(t, errors.As(err, &nf) || errors.As(err, &nfp), "engine.NotFoundError by type")
}

// ensureRootFolder decided "the folder exists" from the text "409" or
// "nameAlreadyExists" anywhere in the error — a 400 whose message carried
// "409" took the exists path.
func TestEnsureRootFolder_OnlyAConflictIsTheFolderExisting(t *testing.T) {
	rt := &odScriptRT{fn: func(_ int, req *http.Request) (*http.Response, error) {
		if req.Method == http.MethodPost {
			return odResp(req, 400, `{"error":{"code":"invalidRequest","message":"quota id 409 exceeded"}}`, nil), nil
		}
		return odResp(req, 200, `{"id":"WRONG"}`, nil), nil
	}}
	tn := scriptedTenant(rt, nil)

	_, err := tn.ensureRootFolder(context.Background(), "DRIVE-a")

	require.Error(t, err, "a 400 is not a conflict")
	assert.Equal(t, 1, rt.calls(), "no folder lookup")

	rt2 := &odScriptRT{fn: func(_ int, req *http.Request) (*http.Response, error) {
		if req.Method == http.MethodPost {
			return odResp(req, 409, `{"error":{"code":"nameAlreadyExists"}}`, nil), nil
		}
		return odResp(req, 200, `{"id":"ROOT-ID"}`, nil), nil
	}}
	tn2 := scriptedTenant(rt2, nil)
	id, err := tn2.ensureRootFolder(context.Background(), "DRIVE-a")
	require.NoError(t, err)
	assert.Equal(t, "ROOT-ID", id)
}

// RP9-1 (WP-P9, assigned here): the three Graph transports had no response
// header bound. Graph's small calls are bounded like an S3-class meta call,
// a CDN range's first byte like a GET, an upload fragment's answer by the
// fragment exception (the last one commits the file).
func TestOneDriveTransports_AreBoundedByRequestClass(t *testing.T) {
	dial := odCachedDialContext()
	assert.Equal(t, odGraphHeaderTimeout, odGraphTransport(dial, odTLSSessionCache).ResponseHeaderTimeout)
	assert.Equal(t, odCDNHeaderTimeout, odCDNTransport(dial, odTLSSessionCache).ResponseHeaderTimeout)
	assert.Equal(t, odUploadHeaderTimeout, odUploadTransport(dial, odTLSSessionCache).ResponseHeaderTimeout)
	assert.Equal(t, 15*time.Second, odGraphHeaderTimeout)
	assert.Equal(t, 25*time.Second, odCDNHeaderTimeout)
	assert.LessOrEqual(t, odUploadHeaderTimeout, 2*time.Minute)
}
