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

// Prompt 2b.2 C1 — the ENGINE's circuit breaker against the multi-bridge
// Sync driver. The 2b tests measured the bridge breaker only; the engine
// breaker one layer up charged the whole `sync` backend for what A1/A2 made
// per-bridge or per-caller: one bridge down (≈ 20 % of mutable reads) and
// clients dropping mid-body each opened it, and every `sync` read then
// answered 503 for 30 s, again and again. "Before" numbers: these tests run
// against 2b8adc3.

// memPrimary is an S3-like primary: a GET of an absent key is a miss, a
// DELETE of one succeeds (S3 DeleteObject is idempotent).
type memPrimary struct {
	mu      sync.Mutex
	objects map[string][]byte
	deletes int
}

func newMemPrimary() *memPrimary { return &memPrimary{objects: map[string][]byte{}} }

func (p *memPrimary) Name() string { return "primary" }
func (p *memPrimary) Get(_ context.Context, c, a string) (io.ReadCloser, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	b, ok := p.objects[c+"/"+a]
	if !ok {
		return nil, engine.ErrNotFound(c, a)
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}
func (p *memPrimary) Put(_ context.Context, c, a string, r io.Reader, _ ...engine.PutOption) error {
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.objects[c+"/"+a] = b
	return nil
}
func (p *memPrimary) Delete(_ context.Context, c, a string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.deletes++
	delete(p.objects, c+"/"+a)
	return nil
}
func (p *memPrimary) List(context.Context, string, string) ([]string, error) { return nil, nil }
func (p *memPrimary) Exists(_ context.Context, c, a string) (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, ok := p.objects[c+"/"+a]
	return ok, nil
}
func (p *memPrimary) HealthCheck(context.Context) error { return nil }

// syncEngine is an engine with an S3-like primary and the multi-bridge
// `sync` backend over bs.
func syncEngine(t *testing.T, bs []*bridgeServer, mutate func(*WebDAVConfig), opts ...WebDAVOption) (*engine.CoreEngine, *MultiWebDAVDriver, *memPrimary) {
	t.Helper()
	m := newMulti(t, bs, mutate, opts...)
	p := newMemPrimary()
	e := engine.NewEngine(nil, zap.NewNop(), &engine.Config{DefaultBackend: "primary"})
	e.AddDriver("primary", p)
	e.AddDriver("sync", m)
	return e, m, p
}

func syncBreaker(e *engine.CoreEngine) string { return e.GetFailoverStatus()["sync"] }

func TestEngine_OneSyncBridgeDownNeverOpensTheSyncBreaker(t *testing.T) {
	// Arrange: five bridges on one folder; objects on bridge 0 and one on
	// bridge 1; bridge 0 goes away (connection refused).
	bs := newBridges(t, 5, true, nil)
	e, m, _ := syncEngine(t, bs, nil)
	ctx := davCtx("tenant-a")
	var onDown []string
	for i := 0; i < 5; i++ {
		a := keyOnBridge(ctx, t, m, "c", fmt.Sprintf("down%d-", i), 0)
		require.NoError(t, m.Put(ctx, "c", a, strings.NewReader("on bridge 0"), engine.WithContentLength(11)))
		onDown = append(onDown, a)
	}
	healthy := keyOnBridge(ctx, t, m, "c", "up", 1)
	require.NoError(t, m.Put(ctx, "c", healthy, strings.NewReader("on bridge 1"), engine.WithContentLength(11)))
	bs[0].srv.Close()

	// Act: five reads of keys routed to the dead bridge (Before: the fifth
	// opened the engine's `sync` breaker).
	for _, a := range onDown {
		e.HintBackend("c", a, "sync")
		_, err := e.Get(ctx, "c", a)

		// Assert: a retryable 503, never a miss, never another backend.
		require.Error(t, err)
		assert.ErrorIs(t, err, engine.ErrPartiallyUnavailable)
		assert.ErrorIs(t, err, engine.ErrAllBackendsUnavailable, "the API answers 503 + Retry-After")
		var nf engine.NotFoundError
		assert.False(t, errors.As(err, &nf), "never NotFound: %v", err)
	}
	assert.Equal(t, "closed", syncBreaker(e), "one bridge down is not the backend down")

	// The object on a healthy bridge reads (Before: 503 all backends unavailable).
	e.HintBackend("c", healthy, "sync")
	rc, err := e.Get(ctx, "c", healthy)
	require.NoError(t, err)
	assert.Equal(t, "on bridge 1", string(readAllClose(t, rc)))

	// Ranged reads, existence, writes and deletes of the dead bridge's keys:
	// the same 503, the same closed breaker.
	for i := 0; i < 5; i++ {
		_, err = e.GetRange(ctx, "c", onDown[i], 0, 2)
		assert.ErrorIs(t, err, engine.ErrPartiallyUnavailable)
		_, err = e.Put(ctx, "c", onDown[i], strings.NewReader("v2"), engine.WithContentLength(2), engine.WithStorageClass("SYNC"))
		assert.ErrorIs(t, err, engine.ErrPartiallyUnavailable)
		assert.ErrorIs(t, err, engine.ErrAllBackendsUnavailable)
	}
	assert.Equal(t, "closed", syncBreaker(e))

	// Every bridge down: that IS the backend down, and the breaker opens.
	for _, b := range bs[1:] {
		b.srv.Close()
	}
	require.Error(t, m.HealthCheck(ctx))
	for i := 0; i < 5; i++ {
		_, err = e.Get(ctx, "c", healthy)
		require.Error(t, err)
		assert.NotErrorIs(t, err, engine.ErrPartiallyUnavailable)
	}
	assert.Equal(t, "open", syncBreaker(e), "every bridge down is the backend down")
}

func TestEngine_AClientAbortMidBodyNeverOpensTheSyncBreaker(t *testing.T) {
	// Arrange
	bs := newBridges(t, 2, true, nil)
	e, m, p := syncEngine(t, bs, func(c *WebDAVConfig) { c.StripeMin = -1 })
	ctx := davCtx("tenant-a")

	// Act: five uploads whose client goes away halfway (Before: bridge
	// failures 0, engine `sync` open, and while it was open a SYNC-tier
	// write landed on the primary).
	for i := 0; i < 5; i++ {
		_, err := e.Put(ctx, "c", fmt.Sprintf("abort-%d", i), &brokenSource{n: 9 << 20 / 2},
			engine.WithContentLength(9<<20), engine.WithStorageClass("SYNC"))

		// Assert
		require.Error(t, err)
		assert.ErrorIs(t, err, engine.ErrCallerAborted)
		assert.ErrorIs(t, err, io.ErrUnexpectedEOF, "the source's own error stays in the chain")
		assert.NotErrorIs(t, err, engine.ErrAllBackendsUnavailable, "the caller's error, never a 503")
	}
	assert.Equal(t, "closed", syncBreaker(e))
	for _, b := range m.bridges {
		assert.Zero(t, b.failures.Load())
	}
	used, err := e.Put(ctx, "c", "after", strings.NewReader("x"), engine.WithContentLength(1), engine.WithStorageClass("SYNC"))
	require.NoError(t, err)
	assert.Equal(t, "sync", used, "the tier's writes stay on the tier")
	assert.Empty(t, p.objects)
}

// A striped upload whose client goes away while a piece is being staged.
func TestEngine_AClientAbortWhileStagingAStripeNeverOpensTheSyncBreaker(t *testing.T) {
	bs := newBridges(t, 3, true, nil)
	e, m, _ := syncEngine(t, bs, func(c *WebDAVConfig) {
		c.StripeMin, c.StripePiece, c.StagingDir = 32<<20, 16<<20, t.TempDir()
	})
	m.stripeSettle = time.Millisecond
	ctx := davCtx("tenant-a")
	for i := 0; i < 5; i++ {
		_, err := e.Put(ctx, "c", fmt.Sprintf("big-%d", i), &brokenSource{n: 20 << 20},
			engine.WithContentLength(40<<20), engine.WithStorageClass("SYNC"))
		require.Error(t, err)
		assert.ErrorIs(t, err, engine.ErrCallerAborted)
	}
	assert.Equal(t, "closed", syncBreaker(e))
}

// Any driver: a source that fails is the caller's, wherever the driver
// wraps (or drops) its error.
type sourceSwallowingDriver struct{ memPrimary }

func (d *sourceSwallowingDriver) Name() string { return "swallow" }
func (d *sourceSwallowingDriver) Put(_ context.Context, _, _ string, r io.Reader, _ ...engine.PutOption) error {
	if _, err := io.Copy(io.Discard, r); err != nil {
		return fmt.Errorf("upload: %v", err) // %v: the SDKs do not always wrap
	}
	return nil
}

func TestEngine_ASourceFailureIsNeverABackendFailureWhateverTheDriverWraps(t *testing.T) {
	e := engine.NewEngine(nil, zap.NewNop(), &engine.Config{DefaultBackend: "swallow"})
	e.AddDriver("swallow", &sourceSwallowingDriver{memPrimary: *newMemPrimary()})
	for i := 0; i < 5; i++ {
		_, err := e.Put(context.Background(), "c", "k", &brokenSource{n: 1000}, engine.WithContentLength(5000))
		require.Error(t, err)
		assert.ErrorIs(t, err, engine.ErrCallerAborted)
		assert.ErrorIs(t, err, io.ErrUnexpectedEOF)
	}
	assert.Equal(t, "closed", e.GetFailoverStatus()["swallow"])
	assert.Zero(t, e.WriteFailures(), "not a durable-write failure")
}

// P2-a: a body of ≤ 8 MiB is read into memory before the PUT; a client that
// leaves there was io.ErrUnexpectedEOF — a bridge failure (3 opened the
// bridge's breaker), each followed by the folder-limit PROPFIND.
func TestMultiWebDAV_ASmallBodyThatBreaksIsTheCallersErrorNotTheBridges(t *testing.T) {
	bs := newBridges(t, 1, true, nil)
	m := newMulti(t, bs, func(c *WebDAVConfig) { c.StripeMin = -1 })
	ctx := davCtx("tenant-a")
	for i := 0; i < 3; i++ {
		err := m.Put(ctx, "c", fmt.Sprintf("small%d", i), &brokenSource{n: 1000}, engine.WithContentLength(1<<20))
		require.Error(t, err)
		assert.ErrorIs(t, err, engine.ErrCallerAborted)
	}
	assert.Zero(t, m.bridges[0].failures.Load())
	assert.True(t, m.healthy(m.bridges[0]))
	assert.Zero(t, bs[0].count("PROPFIND", "/c/"), "no folder check for the caller's failure")
}

// P2-b: a source that trickles below the minimum rate fails as the
// caller's, and gives the bridge's request slot back. Before: a PUT at
// 1 byte per 50 ms held the only slot for as long as the client liked and
// an Exists of another key waited behind it.
func TestMultiWebDAV_ATricklingUploadIsBoundedAndFreesTheBridge(t *testing.T) {
	bs := newBridges(t, 1, true, nil)
	m := newMulti(t, bs, func(c *WebDAVConfig) { c.StripeMin = -1 },
		WithWebDAVMaxConcurrency(1), WithWebDAVSourceMinRate(64<<10, 300*time.Millisecond))
	ctx := davCtx("tenant-a")
	require.NoError(t, m.Put(ctx, "c", "other", strings.NewReader("x"), engine.WithContentLength(1)))

	done := make(chan error, 1)
	start := time.Now()
	go func() {
		done <- m.Put(ctx, "c", "slow", &trickle{left: 9 << 20, chunk: 1, gap: 50 * time.Millisecond},
			engine.WithContentLength(9<<20))
	}()
	var err error
	select {
	case err = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("a trickling upload was never bounded")
	}
	require.Error(t, err)
	assert.ErrorIs(t, err, engine.ErrCallerAborted)
	assert.ErrorIs(t, err, engine.ErrSourceTooSlow)
	assert.Less(t, time.Since(start), 5*time.Second)
	assert.Zero(t, m.bridges[0].failures.Load(), "never a bridge charge")

	ok, err := m.Exists(ctx, "c", "other")
	require.NoError(t, err)
	assert.True(t, ok)
}

// P3: a bridge marked down that answers a real request is up again at
// once. Before: a bridge restarted at 17:09:57 kept every read of its keys
// at 503 until the probe at 17:10:35.
func TestMultiWebDAV_ABridgeMarkedDownIsUpOnItsFirstAnswer(t *testing.T) {
	bs := newBridges(t, 2, true, nil)
	m := newMulti(t, bs, nil)
	ctx := davCtx("tenant-a")
	a := keyOnBridge(ctx, t, m, "c", "back", 0)
	require.NoError(t, m.Put(ctx, "c", a, strings.NewReader("here"), engine.WithContentLength(4)))
	m.bridges[0].probeDown.Store(true) // the last probe failed; the bridge restarted since

	rc, err := m.Get(ctx, "c", a) // the trial read
	require.NoError(t, err)
	assert.Equal(t, "here", string(readAllClose(t, rc)))
	assert.True(t, m.healthy(m.bridges[0]), "its answer marked it up")
	for i := 0; i < 3; i++ {
		rc, err = m.Get(ctx, "c", a)
		require.NoError(t, err)
		_ = rc.Close()
	}

	// A bridge really down: one trial per interval, the rest refused.
	bs[0].srv.Close()
	m.bridges[0].probeDown.Store(true)
	before := bs[0].count("GET", "back")
	for i := 0; i < 5; i++ {
		_, err = m.Get(ctx, "c", a)
		assertRoutedDown(t, err)
	}
	assert.Equal(t, before, bs[0].count("GET", "back"), "a closed server records nothing")
}

// P3: the folder gauge is the largest CURRENT count, not the most ever seen.
func TestWebDAVDriver_FolderGaugeFallsAfterACleanup(t *testing.T) {
	f := newDAVFixture(t, nil)
	f.drv.recordFolder("/a", 41_000)
	f.drv.recordFolder("/b", 100)
	assert.Equal(t, 41_000.0, promtest.ToFloat64(webdavFolderFilesMax.WithLabelValues(f.drv.name, f.drv.limits.bridge)))
	f.drv.recordFolder("/a", 12) // cleaned up
	assert.Equal(t, 100.0, promtest.ToFloat64(webdavFolderFilesMax.WithLabelValues(f.drv.name, f.drv.limits.bridge)))
}

// P2: the folder-limit check runs only on the server's refusal (an HTTP
// status), never after a broken connection, a stall or a timeout. Before:
// each failed PUT was followed by a Depth 1 PROPFIND (on a stalled bridge:
// 60 s, three attempts, the key's lock held); a failed count is not
// retried for 30 s.
func TestWebDAVDriver_FolderCheckOnlyAfterARefusal(t *testing.T) {
	var refuse, cut atomic.Bool
	var depth1 atomic.Int32
	f := newDAVFixture(t, func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == "PROPFIND" && r.Header.Get("Depth") == "1" {
				depth1.Add(1)
				if refuse.Load() {
					http.Error(w, "busy", http.StatusServiceUnavailable)
					return
				}
			}
			if r.Method == http.MethodPut && cut.Load() {
				hj, _ := w.(http.Hijacker)
				conn, _, _ := hj.Hijack()
				_ = conn.Close()
				return
			}
			if r.Method == http.MethodPut && refuse.Load() {
				http.Error(w, "nope", http.StatusInsufficientStorage)
				return
			}
			h.ServeHTTP(w, r)
		})
	}, WithWebDAVRetries(1, 0))
	ctx := davCtx("tenant-a")
	require.NoError(t, f.drv.Put(ctx, "c", "k0", strings.NewReader("v"), engine.WithContentLength(1)))

	cut.Store(true)
	for i := 0; i < 3; i++ {
		require.Error(t, f.drv.Put(ctx, "c", fmt.Sprintf("k%d", i+1), strings.NewReader("v"), engine.WithContentLength(1)))
	}
	assert.Zero(t, depth1.Load(), "a broken connection says nothing about the folder")

	cut.Store(false)
	refuse.Store(true)
	for i := 0; i < 3; i++ {
		require.Error(t, f.drv.Put(ctx, "c", fmt.Sprintf("r%d", i), strings.NewReader("v"), engine.WithContentLength(1)))
	}
	assert.Equal(t, int32(1), depth1.Load(), "a refusal is checked once; the failed count is not retried for 30 s")
}

func TestSyncWebDAVConfigFromEnv_SourceMinRate(t *testing.T) {
	env := func(v string) func(string) string {
		return func(k string) string {
			switch k {
			case "SYNC_WEBDAV_PASSWORD":
				return "pw"
			case "SYNC_WEBDAV_SOURCE_MIN_RATE":
				return v
			}
			return ""
		}
	}
	for _, tc := range []struct {
		in       string
		want     int64
		warnings int
	}{{"", 0, 0}, {"64KiB", 64 << 10, 0}, {"off", -1, 0}, {"0", -1, 0}, {"fast", 0, 1}, {"-5", 0, 1}} {
		c, ok, err := SyncWebDAVConfigFromEnv(env(tc.in))
		require.NoError(t, err)
		require.True(t, ok)
		assert.Equal(t, tc.want, c.SourceMinRate, tc.in)
		assert.Len(t, c.Warnings, tc.warnings, tc.in)
	}
}
