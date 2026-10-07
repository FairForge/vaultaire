package drivers

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	promtest "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/engine"
)

// The live bench against Sync's bridge (2026-10-06): with 4 concurrent
// 256 MiB PUTs the bridge stopped reading one body after 62 KB and never
// answered — the client sat in io.Copy for 45+ minutes; at 32 concurrent
// requests it answered some GETs and DELETEs 500. These tests simulate each
// failure: a stalled upload, a stalled download, a request that never gets
// headers, transient 5xx/423, and more requests than the bridge can take.

const davTestIdle = 200 * time.Millisecond

// fastRetries keeps the retry backoff out of the test's wall time.
var fastRetries = WithWebDAVRetries(3, time.Millisecond)

// hang blocks a handler until the client goes away or release is called.
// A handler that has not read its request body never sees the client go,
// so the test registers release (t.Cleanup) AFTER the fixture: cleanups run
// last-in first-out, and the server's Close waits for its handlers.
func hang() (wait func(r *http.Request), release func()) {
	ch := make(chan struct{})
	return func(r *http.Request) {
			select {
			case <-r.Context().Done():
			case <-ch:
			}
		}, func() {
			close(ch)
		}
}

// A PUT whose body the server stops reading (62 KB in, then nothing) fails
// within about the idle timeout instead of hanging in io.Copy. The body is a
// 64 MiB stream that cannot be rewound, so there is no retry: the error is a
// stall and says the body was consumed (no failover either).
func TestWebDAVDriver_PutStallIsBounded(t *testing.T) {
	wait, release := hang()
	f := newDAVFixture(t, func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPut {
				_, _ = io.CopyN(io.Discard, r.Body, 62<<10)
				wait(r)
				return
			}
			h.ServeHTTP(w, r)
		})
	}, WithWebDAVIdleTimeout(davTestIdle), fastRetries)
	t.Cleanup(release)
	before := promtest.ToFloat64(webdavStalls.WithLabelValues("sync", "0", "upload"))

	const size = 64 << 20
	start := time.Now()
	err := f.drv.Put(davCtx("tenant-a"), "c", "big", onlyReader{&patternReader{n: size}}, engine.WithContentLength(size))
	elapsed := time.Since(start)

	require.Error(t, err)
	assert.ErrorIs(t, err, ErrWebDAVStalled)
	assert.ErrorIs(t, err, engine.ErrTimeout, "a stall is a timeout to the engine")
	assert.ErrorIs(t, err, engine.ErrNoFailover, "the stream was consumed")
	assert.Less(t, elapsed, 10*time.Second, "bounded by the idle timeout, not hung")
	assert.Equal(t, int32(1), f.methods.put.Load(), "a consumed stream is never replayed")
	assert.Equal(t, before+1, promtest.ToFloat64(webdavStalls.WithLabelValues("sync", "0", "upload")))
}

// A rewindable body is sent again after a stall, and the object is stored.
func TestWebDAVDriver_PutStallRetriesARewindableBody(t *testing.T) {
	wait, release := hang()
	var puts atomic.Int32
	f := newDAVFixture(t, func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPut && puts.Add(1) == 1 {
				_, _ = io.CopyN(io.Discard, r.Body, 62<<10)
				wait(r)
				return
			}
			h.ServeHTTP(w, r)
		})
	}, WithWebDAVIdleTimeout(davTestIdle), fastRetries)
	t.Cleanup(release)

	const size = 40 << 20
	payload := make([]byte, size)
	_, _ = io.ReadFull(&patternReader{n: size}, payload)
	ctx := davCtx("tenant-a")
	require.NoError(t, f.drv.Put(ctx, "c", "big", bytes.NewReader(payload), engine.WithContentLength(size)))
	assert.Equal(t, int32(2), puts.Load())
	assert.True(t, bytes.Equal(payload, readAllClose(t, mustGet(ctx, t, f.drv, "c", "big"))))
}

// A server that takes the whole (small) body and never answers is cut off by
// the per-request deadline; the body is held in memory (≤ 8 MiB), so the PUT
// is sent again and succeeds.
func TestWebDAVDriver_PutDeadlineThenRetry(t *testing.T) {
	wait, release := hang()
	var puts atomic.Int32
	f := newDAVFixture(t, func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPut && puts.Add(1) == 1 {
				_, _ = io.Copy(io.Discard, r.Body)
				wait(r)
				return
			}
			h.ServeHTTP(w, r)
		})
	}, WithWebDAVIdleTimeout(time.Hour), WithWebDAVPutTimeout(300*time.Millisecond), fastRetries)
	t.Cleanup(release)

	ctx := davCtx("tenant-a")
	start := time.Now()
	require.NoError(t, f.drv.Put(ctx, "c", "k", onlyReader{strings.NewReader("hello")}, engine.WithContentLength(5)))
	assert.Less(t, time.Since(start), 10*time.Second)
	assert.Equal(t, int32(2), puts.Load())
	assert.Equal(t, "hello", string(readAllClose(t, mustGet(ctx, t, f.drv, "c", "k"))))
}

func TestWebDAVPutDeadline(t *testing.T) {
	base := 2 * time.Minute
	assert.Zero(t, webdavPutDeadline(base, 0), "unknown length: the idle timeout only")
	assert.Zero(t, webdavPutDeadline(0, 1<<20), "disabled")
	assert.Equal(t, base, webdavPutDeadline(base, 1))
	assert.Equal(t, base, webdavPutDeadline(base, 64<<20))
	assert.Equal(t, 4*base, webdavPutDeadline(base, 256<<20), "base per started 64 MiB, like the fixed-bucket PUT deadline")
	assert.Equal(t, webdavPutDeadlineCap, webdavPutDeadline(base, 1<<50), "capped")
}

// A GET body the server stops sending fails the caller's read within about
// the idle timeout; a caller that is slow to read is not a stall.
func TestWebDAVDriver_GetStallIsBounded(t *testing.T) {
	wait, release := hang()
	f := newDAVFixture(t, func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet {
				w.Header().Set("Content-Length", "10000000")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write(bytes.Repeat([]byte{'x'}, 100<<10))
				w.(http.Flusher).Flush()
				wait(r)
				return
			}
			h.ServeHTTP(w, r)
		})
	}, WithWebDAVIdleTimeout(davTestIdle), fastRetries)
	t.Cleanup(release)
	before := promtest.ToFloat64(webdavStalls.WithLabelValues("sync", "0", "download"))

	rc, err := f.drv.Get(davCtx("tenant-a"), "c", "k")
	require.NoError(t, err)
	defer mustClose(rc)

	// Slower than the idle timeout between reads: the caller's pace is not
	// the server's stall.
	buf := make([]byte, 1<<10)
	_, err = io.ReadFull(rc, buf)
	require.NoError(t, err)
	time.Sleep(2 * davTestIdle)
	_, err = io.ReadFull(rc, buf)
	require.NoError(t, err)

	start := time.Now()
	_, err = io.Copy(io.Discard, rc)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrWebDAVStalled)
	assert.Less(t, time.Since(start), 10*time.Second)
	assert.Equal(t, before+1, promtest.ToFloat64(webdavStalls.WithLabelValues("sync", "0", "download")))
}

// A GET that gets no answer at all within the idle timeout is a stall
// before any byte reached the caller: it is retried.
func TestWebDAVDriver_GetHeaderStallIsRetried(t *testing.T) {
	wait, release := hang()
	var gets atomic.Int32
	f := newDAVFixture(t, func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet && gets.Add(1) == 1 {
				wait(r)
				return
			}
			h.ServeHTTP(w, r)
		})
	}, WithWebDAVIdleTimeout(davTestIdle), fastRetries)
	t.Cleanup(release)
	ctx := davCtx("tenant-a")
	require.NoError(t, f.drv.Put(ctx, "c", "k", strings.NewReader("v"), engine.WithContentLength(1)))
	assert.Equal(t, "v", string(readAllClose(t, mustGet(ctx, t, f.drv, "c", "k"))))
	assert.Equal(t, int32(2), gets.Load())
}

// flaky answers the first n requests of a method with code.
func flaky(method string, n int32, code int) (func(http.Handler) http.Handler, *atomic.Int32) {
	var seen atomic.Int32
	return func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == method && seen.Add(1) <= n {
				http.Error(w, "bridge hiccup", code)
				return
			}
			h.ServeHTTP(w, r)
		})
	}, &seen
}

// Idempotent requests are retried on 500/502/503/504/423.
func TestWebDAVDriver_TransientAnswersAreRetried(t *testing.T) {
	for _, code := range []int{500, 502, 503, 504, 423} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			ctx := davCtx("tenant-a")
			for _, tc := range []struct {
				method string
				op     func(f *davFixture) error
			}{
				{http.MethodGet, func(f *davFixture) error {
					rc, err := f.drv.Get(ctx, "c", "k")
					if err != nil {
						return err
					}
					return rc.Close()
				}},
				{http.MethodDelete, func(f *davFixture) error { return f.drv.Delete(ctx, "c", "k") }},
				{"PROPFIND", func(f *davFixture) error {
					ok, err := f.drv.Exists(ctx, "c", "k")
					if err == nil && !ok {
						return errors.New("not there")
					}
					return err
				}},
				{"MKCOL", func(f *davFixture) error {
					return f.drv.Put(ctx, "c", "newdir/k2", strings.NewReader("x"), engine.WithContentLength(1))
				}},
			} {
				var armed atomic.Bool
				var hits atomic.Int32
				f := newDAVFixture(t, func(h http.Handler) http.Handler {
					return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if armed.Load() && r.Method == tc.method && hits.Add(1) <= 2 {
							http.Error(w, "bridge hiccup", code)
							return
						}
						h.ServeHTTP(w, r)
					})
				}, fastRetries)
				require.NoError(t, f.drv.Put(ctx, "c", "k", strings.NewReader("v"), engine.WithContentLength(1)))
				armed.Store(true)
				assert.NoError(t, tc.op(f), tc.method)
				assert.Equal(t, int32(3), hits.Load(), "%s: two failures, then the answer", tc.method)
			}
		})
	}
}

// Retries are bounded: a persistent 500 is the caller's error after the
// configured number of attempts; a 4xx is never retried.
func TestWebDAVDriver_RetriesAreBounded(t *testing.T) {
	wrap, seen := flaky(http.MethodGet, 1<<30, http.StatusInternalServerError)
	f := newDAVFixture(t, wrap, fastRetries)
	_, err := f.drv.Get(davCtx("tenant-a"), "c", "k")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "500")
	assert.Equal(t, int32(3), seen.Load())

	wrap, seen = flaky(http.MethodGet, 1<<30, http.StatusForbidden)
	f = newDAVFixture(t, wrap, fastRetries)
	_, err = f.drv.Get(davCtx("tenant-a"), "c", "k")
	require.Error(t, err)
	assert.Equal(t, int32(1), seen.Load(), "a 403 is an answer, not a hiccup")
}

// A PUT answered 503 is sent again when its body can be replayed: a
// seekable body, or a known length ≤ 8 MiB (held in memory). A larger stream
// is never buffered and so never replayed; a 4xx is never retried.
func TestWebDAVDriver_PutRetries(t *testing.T) {
	ctx := davCtx("tenant-a")

	wrap, seen := flaky(http.MethodPut, 1, http.StatusServiceUnavailable)
	f := newDAVFixture(t, wrap, fastRetries)
	require.NoError(t, f.drv.Put(ctx, "c", "small", onlyReader{strings.NewReader("small body")}, engine.WithContentLength(10)))
	assert.Equal(t, int32(2), seen.Load())
	assert.Equal(t, "small body", string(readAllClose(t, mustGet(ctx, t, f.drv, "c", "small"))))

	const big = 9 << 20
	payload := make([]byte, big)
	_, _ = io.ReadFull(&patternReader{n: big}, payload)

	wrap, seen = flaky(http.MethodPut, 1, http.StatusServiceUnavailable)
	f = newDAVFixture(t, wrap, fastRetries)
	require.NoError(t, f.drv.Put(ctx, "c", "seekable", bytes.NewReader(payload), engine.WithContentLength(big)))
	assert.Equal(t, int32(2), seen.Load())
	assert.True(t, bytes.Equal(payload, readAllClose(t, mustGet(ctx, t, f.drv, "c", "seekable"))))

	wrap, seen = flaky(http.MethodPut, 1, http.StatusServiceUnavailable)
	f = newDAVFixture(t, wrap, fastRetries)
	err := f.drv.Put(ctx, "c", "stream", onlyReader{bytes.NewReader(payload)}, engine.WithContentLength(big))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "503")
	assert.Equal(t, int32(1), seen.Load(), "a 9 MiB stream is not buffered for a retry")

	wrap, seen = flaky(http.MethodPut, 1<<30, http.StatusForbidden)
	f = newDAVFixture(t, wrap, fastRetries)
	require.Error(t, f.drv.Put(ctx, "c", "denied", strings.NewReader("x"), engine.WithContentLength(1)))
	assert.Equal(t, int32(1), seen.Load())
}

// No more than the cap of requests reach the bridge at once, and a call
// waiting for a slot leaves when its context ends.
func TestWebDAVDriver_ConcurrencyCap(t *testing.T) {
	var inFlight, peak atomic.Int32
	f := newDAVFixture(t, func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			n := inFlight.Add(1)
			for {
				p := peak.Load()
				if n <= p || peak.CompareAndSwap(p, n) {
					break
				}
			}
			time.Sleep(10 * time.Millisecond)
			h.ServeHTTP(w, r)
			inFlight.Add(-1)
		})
	}, WithWebDAVMaxConcurrency(3))
	ctx := davCtx("tenant-a")
	require.NoError(t, f.drv.Put(ctx, "c", "k", strings.NewReader("v"), engine.WithContentLength(1)))
	var wg sync.WaitGroup
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, err := f.drv.Exists(ctx, "c", "k")
			assert.NoError(t, err)
			assert.True(t, ok)
		}()
	}
	wg.Wait()
	assert.LessOrEqual(t, peak.Load(), int32(3))
	assert.Equal(t, int32(3), peak.Load(), "the cap is used, not serialised")
}

func TestWebDAVDriver_SlotWaitHonoursContext(t *testing.T) {
	wait, release := hang()
	entered := make(chan struct{}, 1)
	f := newDAVFixture(t, func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet {
				entered <- struct{}{}
				wait(r)
				return
			}
			h.ServeHTTP(w, r)
		})
	}, WithWebDAVMaxConcurrency(1), WithWebDAVIdleTimeout(time.Hour))
	t.Cleanup(release)
	ctx := davCtx("tenant-a")

	gctx, cancelGet := context.WithCancel(ctx)
	defer cancelGet()
	go func() { _, _ = f.drv.Get(gctx, "c", "k") }()
	<-entered // the only slot is taken by a GET the server never answers

	wctx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := f.drv.Exists(wctx, "c", "k")
	require.Error(t, err)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Less(t, time.Since(start), 5*time.Second)
}

// Requests, stalls and retries are counted per backend; every series exists
// at 0 from the constructor on.
func TestWebDAVDriver_Metrics(t *testing.T) {
	backend := fmt.Sprintf("dav-metrics-%d", time.Now().UnixNano()) // global counters: unique per run
	f := newDAVFixture(t, func(h http.Handler) http.Handler {
		var n atomic.Int32
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet && n.Add(1) == 1 {
				http.Error(w, "hiccup", http.StatusInternalServerError)
				return
			}
			h.ServeHTTP(w, r)
		})
	})
	d, err := NewWebDAVDriver(backend, f.srv.URL, davTestUser, davTestPass, "vaultaire", zap.NewNop(), fastRetries)
	require.NoError(t, err)

	for _, dir := range []string{"upload", "download"} {
		assert.Zero(t, promtest.ToFloat64(webdavStalls.WithLabelValues(backend, "0", dir)))
	}
	for _, m := range []string{"GET", "PUT", "PROPFIND", "MKCOL", "DELETE"} {
		assert.Zero(t, promtest.ToFloat64(webdavRetries.WithLabelValues(backend, "0", m)))
		assert.Zero(t, promtest.ToFloat64(webdavRequests.WithLabelValues(backend, "0", m, "ok")))
	}
	assert.Positive(t, promtest.CollectAndCount(webdavRequests), "series exist before any request")

	ctx := davCtx("tenant-a")
	require.NoError(t, d.Put(ctx, "c", "k", strings.NewReader("v"), engine.WithContentLength(1)))
	assert.Equal(t, "v", string(readAllClose(t, mustGet(ctx, t, d, "c", "k"))))

	assert.Equal(t, 1.0, promtest.ToFloat64(webdavRequests.WithLabelValues(backend, "0", "GET", "http_5xx")))
	assert.Equal(t, 1.0, promtest.ToFloat64(webdavRequests.WithLabelValues(backend, "0", "GET", "ok")))
	assert.Equal(t, 1.0, promtest.ToFloat64(webdavRetries.WithLabelValues(backend, "0", "GET")))
	assert.Equal(t, 1.0, promtest.ToFloat64(webdavRequests.WithLabelValues(backend, "0", "PUT", "ok")))
	st := d.Stats()
	assert.Equal(t, int64(1), st.Retries)
	assert.Zero(t, st.UploadStalls+st.DownloadStalls)

	var found bool
	for _, c := range Collectors() {
		if c == webdavRequests {
			found = true
		}
	}
	assert.True(t, found, "registered through Collectors()")
}
