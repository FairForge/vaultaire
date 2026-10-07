package drivers

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	promtest "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"golang.org/x/net/webdav"

	"github.com/FairForge/vaultaire/internal/engine"
)

var (
	_ engine.Driver          = (*MultiWebDAVDriver)(nil)
	_ engine.RangeGetter     = (*MultiWebDAVDriver)(nil)
	_ engine.TenantWalker    = (*MultiWebDAVDriver)(nil)
	_ engine.KeyAddresser    = (*MultiWebDAVDriver)(nil)
	_ engine.StoreIdentifier = (*MultiWebDAVDriver)(nil)
)

// Sync's bridges, live on SLC (2026-10-07): one process per device profile,
// all mounted on the same Sync folder, each with its own password. A file
// written through bridge A is visible through bridge B after 1.5–13 s (an
// overwrite ~30 s, once 310 s; a delete ~30 s); within one bridge every
// read is consistent. These tests run one x/net/webdav server per bridge —
// on one shared file system (the shared Sync folder, without the staleness)
// or each on its own (a bridge that has not seen another's writes yet).

func bridgePass(i int) string { return fmt.Sprintf("bridge-%d-s3cret", i) }

// bridgeServer is one bridge: a WebDAV server with its own password that
// records every request it serves.
type bridgeServer struct {
	srv *httptest.Server
	fs  webdav.FileSystem

	mu   sync.Mutex
	seen []string // "METHOD /path"
}

func (b *bridgeServer) requests() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.seen...)
}

func (b *bridgeServer) reset() {
	b.mu.Lock()
	b.seen = nil
	b.mu.Unlock()
}

// count is how many requests of method touched a path containing part.
func (b *bridgeServer) count(method, part string) int {
	n := 0
	for _, r := range b.requests() {
		if strings.HasPrefix(r, method+" ") && strings.Contains(r, part) {
			n++
		}
	}
	return n
}

// newBridges starts n bridges. shared = one file system behind all of them.
// wrap (optional) sits between the auth check and the WebDAV handler.
func newBridges(t *testing.T, n int, shared bool, wrap func(i int, h http.Handler) http.Handler) []*bridgeServer {
	t.Helper()
	var common webdav.FileSystem
	if shared {
		common = webdav.NewMemFS()
	}
	out := make([]*bridgeServer, n)
	for i := range out {
		b := &bridgeServer{fs: common}
		if b.fs == nil {
			b.fs = webdav.NewMemFS()
		}
		var h http.Handler = &webdav.Handler{FileSystem: b.fs, LockSystem: webdav.NewMemLS()}
		if wrap != nil {
			h = wrap(i, h)
		}
		pass := bridgePass(i)
		b.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			u, p, ok := r.BasicAuth()
			if !ok || u != davTestUser || p != pass {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			b.mu.Lock()
			b.seen = append(b.seen, r.Method+" "+r.URL.Path)
			b.mu.Unlock()
			h.ServeHTTP(w, r)
		}))
		t.Cleanup(b.srv.Close)
		out[i] = b
	}
	return out
}

func multiConfig(bs []*bridgeServer) WebDAVConfig {
	c := WebDAVConfig{User: davTestUser, Root: "vaultaire"}
	for i, b := range bs {
		c.Bridges = append(c.Bridges, WebDAVBridge{URL: b.srv.URL, Password: bridgePass(i)})
	}
	return c
}

func newMulti(t *testing.T, bs []*bridgeServer, mutate func(*WebDAVConfig), opts ...WebDAVOption) *MultiWebDAVDriver {
	t.Helper()
	c := multiConfig(bs)
	if mutate != nil {
		mutate(&c)
	}
	m, err := NewMultiWebDAVDriver("sync", c, zap.NewNop(), append([]WebDAVOption{fastRetries}, opts...)...)
	require.NoError(t, err)
	return m
}

// keyOnBridge finds an artifact whose routed bridge is want.
func keyOnBridge(ctx context.Context, t *testing.T, m *MultiWebDAVDriver, container, prefix string, want int) string {
	t.Helper()
	for i := 0; i < 10000; i++ {
		a := fmt.Sprintf("%s-%d", prefix, i)
		idx, err := m.bridgeFor(ctx, container, a)
		require.NoError(t, err)
		if idx == want {
			return a
		}
	}
	t.Fatalf("no key routes to bridge %d", want)
	return ""
}

func putFile(t *testing.T, fs webdav.FileSystem, p, content string) {
	t.Helper()
	ctx := context.Background()
	dir := p[:strings.LastIndex(p, "/")]
	segs := strings.Split(strings.TrimPrefix(dir, "/"), "/")
	cur := ""
	for _, s := range segs {
		cur += "/" + s
		if err := fs.Mkdir(ctx, cur, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
			require.NoError(t, err)
		}
	}
	f, err := fs.OpenFile(ctx, p, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	require.NoError(t, err)
	_, err = f.Write([]byte(content))
	require.NoError(t, err)
	require.NoError(t, f.Close())
}

// --- HRW ---------------------------------------------------------------------

func TestHRW_SpreadsKeysEvenly(t *testing.T) {
	ids := []string{"http://127.0.0.1:4918", "http://127.0.0.1:4919", "http://127.0.0.1:4920", "http://127.0.0.1:4921", "http://127.0.0.1:4922"}
	const n = 10000
	counts := make([]int, len(ids))
	for i := 0; i < n; i++ {
		counts[hrwRank(ids, fmt.Sprintf("t-tenant/bucket/photos/%d.jpg", i))[0]]++
	}
	for i, c := range counts {
		assert.InDelta(t, n/len(ids), c, 0.15*n/float64(len(ids)), "bridge %d got %d of %d", i, c, n)
	}
}

func TestHRW_IsStableAndRanksEveryBridge(t *testing.T) {
	ids := []string{"a", "b", "c", "d", "e"}
	for i := 0; i < 200; i++ {
		k := fmt.Sprintf("t-x/c/%d", i)
		r1, r2 := hrwRank(ids, k), hrwRank(ids, k)
		assert.Equal(t, r1, r2, "same key, same order")
		sorted := append([]int(nil), r1...)
		sort.Ints(sorted)
		assert.Equal(t, []int{0, 1, 2, 3, 4}, sorted, "a permutation of every bridge")
	}
}

// Removing one of five bridges moves only the keys that bridge held (~1/5);
// adding a sixth takes ~1/6 and moves nothing else.
func TestHRW_RemovingABridgeMovesOnlyItsKeys(t *testing.T) {
	five := []string{"b0", "b1", "b2", "b3", "b4"}
	four := []string{"b0", "b1", "b3", "b4"} // b2 removed
	six := append(append([]string(nil), five...), "b5")
	const n = 10000
	moved, movedAdd := 0, 0
	for i := 0; i < n; i++ {
		k := fmt.Sprintf("t-tenant/bucket/%d", i)
		before := five[hrwRank(five, k)[0]]
		after := four[hrwRank(four, k)[0]]
		if before != after {
			moved++
			assert.Equal(t, "b2", before, "only the removed bridge's keys move")
		}
		if added := six[hrwRank(six, k)[0]]; added != before {
			movedAdd++
			assert.Equal(t, "b5", added, "a new bridge only takes keys")
		}
	}
	assert.InDelta(t, 0.20, float64(moved)/n, 0.03)
	assert.InDelta(t, 1.0/6, float64(movedAdd)/n, 0.03)
}

// --- routing -----------------------------------------------------------------

// Every operation on a key reaches the key's bridge and no other.
func TestMultiWebDAV_EachOpHitsTheRoutedBridge(t *testing.T) {
	bs := newBridges(t, 5, true, nil)
	m := newMulti(t, bs, nil)
	ctx := davCtx("tenant-a")
	hits := make([]int, len(bs))
	for i := 0; i < 40; i++ {
		a := fmt.Sprintf("dir/obj-%03d", i)
		idx, err := m.bridgeFor(ctx, "bucket", a)
		require.NoError(t, err)
		hits[idx]++
		for _, b := range bs {
			b.reset()
		}

		require.NoError(t, m.Put(ctx, "bucket", a, strings.NewReader("hello "+a), engine.WithContentLength(int64(len("hello "+a)))))
		assert.Equal(t, "hello "+a, string(readAllClose(t, mustMultiGet(ctx, t, m, "bucket", a))))
		rc, err := m.GetRange(ctx, "bucket", a, 6, 3)
		require.NoError(t, err)
		assert.Equal(t, "dir", string(readAllClose(t, rc)))
		ok, err := m.Exists(ctx, "bucket", a)
		require.NoError(t, err)
		assert.True(t, ok)
		require.NoError(t, m.Delete(ctx, "bucket", a))
		ok, err = m.Exists(ctx, "bucket", a)
		require.NoError(t, err)
		assert.False(t, ok, "the routed bridge's miss is a miss")

		name := "obj-" + fmt.Sprintf("%03d", i)
		for j, b := range bs {
			n := len(b.requests())
			if j == idx {
				assert.Positive(t, b.count(http.MethodPut, name), "PUT on the routed bridge")
				assert.Equal(t, 2, b.count(http.MethodGet, name), "GET + GetRange on the routed bridge")
				assert.Equal(t, 1, b.count(http.MethodDelete, name))
			} else {
				assert.Zero(t, n, "bridge %d is not %s's bridge (%d): %v", j, a, idx, b.requests())
			}
		}
	}
	spread := 0
	for _, h := range hits {
		if h > 0 {
			spread++
		}
	}
	assert.GreaterOrEqual(t, spread, 4, "40 keys land on most of 5 bridges: %v", hits)
}

func mustMultiGet(ctx context.Context, t *testing.T, m *MultiWebDAVDriver, c, a string) io.ReadCloser {
	t.Helper()
	rc, err := m.Get(ctx, c, a)
	require.NoError(t, err)
	return rc
}

// A single-bridge configuration is the old driver: everything on bridge 0.
func TestMultiWebDAV_OneBridge(t *testing.T) {
	bs := newBridges(t, 1, false, nil)
	m := newMulti(t, bs, nil)
	ctx := davCtx("tenant-a")
	require.NoError(t, m.Put(ctx, "c", "k", strings.NewReader("v"), engine.WithContentLength(1)))
	assert.Equal(t, "v", string(readAllClose(t, mustMultiGet(ctx, t, m, "c", "k"))))
	keys, err := m.List(ctx, "c", "")
	require.NoError(t, err)
	assert.Equal(t, []string{"k"}, keys)
	_, err = m.Get(ctx, "c", "missing")
	var nf engine.NotFoundError
	assert.ErrorAs(t, err, &nf, "one bridge: its miss is a miss")
	require.NoError(t, m.HealthCheck(ctx))
	assert.Equal(t, 1, m.Bridges())
	assert.Equal(t, "sync", m.Name())
	assert.Equal(t, "t-tenant-a/c/k", m.ObjectKey(ctx, "c", "k"))
}

// --- fallback reads ------------------------------------------------------------

// The routed bridge is down: a read falls back to the next bridge in HRW
// order and is served when that bridge sees the object; when it does not,
// the answer is a retryable "unavailable", never NotFound — the object may
// have been written through the dead bridge seconds ago.
func TestMultiWebDAV_FallbackReadOnADeadBridge(t *testing.T) {
	bs := newBridges(t, 2, false, nil)
	m := newMulti(t, bs, nil)
	ctx := davCtx("tenant-a")

	synced := keyOnBridge(ctx, t, m, "c", "synced", 0)
	fresh := keyOnBridge(ctx, t, m, "c", "fresh", 0)
	require.NoError(t, m.Put(ctx, "c", synced, strings.NewReader("old news"), engine.WithContentLength(8)))
	require.NoError(t, m.Put(ctx, "c", fresh, strings.NewReader("just now"), engine.WithContentLength(8)))
	// Sync has carried `synced` to the other bridge; `fresh` not yet.
	putFile(t, bs[1].fs, "/vaultaire/t-tenant-a/c/"+synced+"%o", "old news")

	bs[0].srv.Close() // connection refused from now on

	// No probe has run: the refused connection itself sends the read on.
	assert.Equal(t, "old news", string(readAllClose(t, mustMultiGet(ctx, t, m, "c", synced))))
	rc, err := m.GetRange(ctx, "c", synced, 4, 4)
	require.NoError(t, err)
	assert.Equal(t, "news", string(readAllClose(t, rc)))
	ok, err := m.Exists(ctx, "c", synced)
	require.NoError(t, err)
	assert.True(t, ok)

	_, err = m.Get(ctx, "c", fresh)
	assertStaleMiss(t, err)
	_, err = m.GetRange(ctx, "c", fresh, 0, 2)
	assertStaleMiss(t, err)
	ok, err = m.Exists(ctx, "c", fresh)
	assertStaleMiss(t, err)
	assert.False(t, ok)

	// After the probe marks it down the routed bridge is not even tried.
	require.NoError(t, m.HealthCheck(ctx), "one of two bridges is up: the backend is up")
	assert.Equal(t, "old news", string(readAllClose(t, mustMultiGet(ctx, t, m, "c", synced))))

	// Writes never fail over: a key of the dead bridge fails.
	err = m.Put(ctx, "c", fresh, strings.NewReader("again"), engine.WithContentLength(5))
	require.Error(t, err)
	assert.Zero(t, bs[1].count(http.MethodPut, fresh), "no write on the fallback bridge")
	require.Error(t, m.Delete(ctx, "c", fresh))
	assert.Zero(t, bs[1].count(http.MethodDelete, fresh))
}

func assertStaleMiss(t *testing.T, err error) {
	t.Helper()
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrWebDAVBridgeStale)
	assert.ErrorIs(t, err, engine.ErrAllBackendsUnavailable, "the API answers 503 + Retry-After")
	var nf engine.NotFoundError
	assert.False(t, errors.As(err, &nf), "never NotFound: %v", err)
}

// A live routed bridge's miss is authoritative (one bridge is consistent with
// itself), and the fallback is never asked.
func TestMultiWebDAV_RoutedMissIsAMiss(t *testing.T) {
	bs := newBridges(t, 3, false, nil)
	m := newMulti(t, bs, nil)
	ctx := davCtx("tenant-a")
	a := keyOnBridge(ctx, t, m, "c", "k", 1)
	_, err := m.Get(ctx, "c", a)
	var nf engine.NotFoundError
	require.ErrorAs(t, err, &nf)
	assert.Empty(t, bs[0].requests())
	assert.Empty(t, bs[2].requests())
}

// A bridge that keeps answering 5xx after its retries is also passed over.
func TestMultiWebDAV_FallbackOnPersistent5xx(t *testing.T) {
	bs := newBridges(t, 2, true, func(i int, h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if i == 0 && r.Method == http.MethodGet {
				http.Error(w, "bridge cannot reach Sync", http.StatusBadGateway)
				return
			}
			h.ServeHTTP(w, r)
		})
	})
	m := newMulti(t, bs, nil)
	ctx := davCtx("tenant-a")
	a := keyOnBridge(ctx, t, m, "c", "k", 0)
	require.NoError(t, m.Put(ctx, "c", a, strings.NewReader("v"), engine.WithContentLength(1)))
	assert.Equal(t, "v", string(readAllClose(t, mustMultiGet(ctx, t, m, "c", a))))
}

// A 401 on the routed bridge is not an outage to hide: it is returned.
func TestMultiWebDAV_AuthFailureIsNotFailedOver(t *testing.T) {
	bs := newBridges(t, 2, true, nil)
	m := newMulti(t, bs, func(c *WebDAVConfig) { c.Bridges[0].Password = "wrong" })
	ctx := davCtx("tenant-a")
	a := keyOnBridge(ctx, t, m, "c", "k", 0)
	_, err := m.Get(ctx, "c", a)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "401")
	assert.Empty(t, bs[1].requests())
}

// --- listing and the erasure walk ---------------------------------------------

// WalkTenant unions every bridge's listing (an object written through any
// bridge within the staleness window is still found), each object once,
// and its Remove goes to the object's routed bridge.
func TestMultiWebDAV_WalkTenantUnionsBridges(t *testing.T) {
	bs := newBridges(t, 3, false, nil)
	m := newMulti(t, bs, nil)
	ctx := davCtx("tenant-a")
	putFile(t, bs[0].fs, "/vaultaire/t-tenant-a/c/only-on-0%o", "x")
	putFile(t, bs[1].fs, "/vaultaire/t-tenant-a/c/deep/only-on-1%o", "x")
	for _, b := range bs {
		putFile(t, b.fs, "/vaultaire/t-tenant-a/c/everywhere%o", "x")
	}
	putFile(t, bs[2].fs, "/vaultaire/t-tenant-a/top-level%o", "x")
	putFile(t, bs[2].fs, "/vaultaire/t-tenant-b/c/neighbour%o", "x")

	var got []string
	removes := map[string]func(context.Context) error{}
	require.NoError(t, m.WalkTenant(context.Background(), "tenant-a", func(o engine.TenantObject) error {
		k := o.Container + "|" + o.Artifact
		got = append(got, k)
		removes[k] = o.Remove
		return nil
	}))
	sort.Strings(got)
	assert.Equal(t, []string{"c|deep/only-on-1", "c|everywhere", "c|only-on-0", "|top-level"}, got)

	for _, b := range bs {
		b.reset()
	}
	require.NoError(t, removes["c|everywhere"](context.Background()))
	want, err := m.bridgeFor(davCtx("tenant-a"), "c", "everywhere")
	require.NoError(t, err)
	for i, b := range bs {
		if i == want {
			assert.Equal(t, 1, b.count(http.MethodDelete, "everywhere"))
		} else {
			assert.Empty(t, b.requests(), "bridge %d is not the object's bridge", i)
		}
	}
	// The top-level file (no container) is removed at its own path.
	require.NoError(t, removes["|top-level"](context.Background()))
	total := 0
	for _, b := range bs {
		total += b.count(http.MethodDelete, "/vaultaire/t-tenant-a/top-level")
	}
	assert.Equal(t, 1, total)
	_ = ctx
}

// A bridge whose listing fails fails the walk: the sweep defers the tenant
// rather than call it erased while a bridge may still name an object.
func TestMultiWebDAV_WalkTenantFailsWhenABridgeCannotList(t *testing.T) {
	bs := newBridges(t, 2, true, nil)
	m := newMulti(t, bs, nil)
	putFile(t, bs[0].fs, "/vaultaire/t-tenant-a/c/k", "x")
	bs[1].srv.Close()
	err := m.WalkTenant(context.Background(), "tenant-a", func(engine.TenantObject) error { return nil })
	require.Error(t, err)
	assert.Contains(t, err.Error(), "bridge 1")
}

// List goes through one bridge and falls back when it is down.
func TestMultiWebDAV_ListFallsBack(t *testing.T) {
	bs := newBridges(t, 2, true, nil)
	m := newMulti(t, bs, nil)
	ctx := davCtx("tenant-a")
	putFile(t, bs[0].fs, "/vaultaire/t-tenant-a/c/a%o", "x")
	putFile(t, bs[0].fs, "/vaultaire/t-tenant-a/c/b%o", "x")
	keys, err := m.List(ctx, "c", "")
	require.NoError(t, err)
	assert.Equal(t, []string{"a", "b"}, keys)
	listed := 0
	for _, b := range bs {
		if b.count("PROPFIND", "/t-tenant-a/c") > 0 {
			listed++
		}
	}
	assert.Equal(t, 1, listed, "one bridge lists")

	bs[0].srv.Close()
	bs[1].srv.Close()
	_, err = m.List(ctx, "c", "")
	require.Error(t, err)
}

// --- health --------------------------------------------------------------------

func TestMultiWebDAV_HealthAggregates(t *testing.T) {
	bs := newBridges(t, 3, true, nil)
	backend := fmt.Sprintf("sync-health-%d", time.Now().UnixNano())
	c := multiConfig(bs)
	m, err := NewMultiWebDAVDriver(backend, c, zap.NewNop(), fastRetries)
	require.NoError(t, err)
	up := func(i int) float64 { return promtest.ToFloat64(webdavBridgeUp.WithLabelValues(backend, fmt.Sprint(i))) }

	ctx := context.Background()
	require.NoError(t, m.HealthCheck(ctx))
	for i := range bs {
		assert.Equal(t, 1.0, up(i))
	}

	bs[1].srv.Close()
	require.NoError(t, m.HealthCheck(ctx), "2 of 3 up: healthy, degraded")
	assert.Equal(t, 0.0, up(1))
	assert.Equal(t, 1.0, up(0))
	assert.Equal(t, []int{1}, m.DownBridges())

	bs[0].srv.Close()
	bs[2].srv.Close()
	err = m.HealthCheck(ctx)
	require.Error(t, err)
	for i := range bs {
		assert.Contains(t, err.Error(), fmt.Sprintf("bridge %d", i))
		assert.NotContains(t, err.Error(), bridgePass(i), "never a password")
		assert.Equal(t, 0.0, up(i))
	}
	var found bool
	for _, col := range Collectors() {
		if col == webdavBridgeUp {
			found = true
		}
	}
	assert.True(t, found, "registered through Collectors()")
}

// --- large transfers --------------------------------------------------------------

// Bodies of 16 MiB and more take one of SYNC_WEBDAV_LARGE_CONCURRENCY slots
// per bridge and direction (the bridge is one CPU-bound process: 3 large
// transfers each way saturate it); small ones only the total cap.
func TestMultiWebDAV_LargePutConcurrencyCap(t *testing.T) {
	var inFlight, peak atomic.Int32
	gate := make(chan struct{})
	bs := newBridges(t, 1, false, func(_ int, h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPut {
				h.ServeHTTP(w, r)
				return
			}
			n := inFlight.Add(1)
			for {
				p := peak.Load()
				if n <= p || peak.CompareAndSwap(p, n) {
					break
				}
			}
			<-gate
			h.ServeHTTP(w, r)
			inFlight.Add(-1)
		})
	})
	m := newMulti(t, bs, func(c *WebDAVConfig) { c.LargeConcurrency = 2 }, WithWebDAVIdleTimeout(0))
	ctx := davCtx("tenant-a")

	run := func(n int, size int64) {
		peak.Store(0)
		var wg sync.WaitGroup
		errs := make(chan error, n)
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				errs <- m.Put(ctx, "c", fmt.Sprintf("o-%d-%d", size, i), &patternReader{n: size}, engine.WithContentLength(size))
			}(i)
		}
		time.Sleep(300 * time.Millisecond)
		for i := 0; i < n; i++ {
			gate <- struct{}{}
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			require.NoError(t, err)
		}
	}
	run(5, 16<<20)
	assert.Equal(t, int32(2), peak.Load(), "large PUTs: 2 at a time")
	run(5, 1<<10)
	assert.Equal(t, int32(5), peak.Load(), "small PUTs: the total cap only")
}

func TestMultiWebDAV_LargeGetConcurrencyCap(t *testing.T) {
	bs := newBridges(t, 1, false, nil)
	m := newMulti(t, bs, func(c *WebDAVConfig) { c.LargeConcurrency = 2 })
	ctx := davCtx("tenant-a")
	const big = 16 << 20
	require.NoError(t, m.Put(ctx, "c", "big", &patternReader{n: big}, engine.WithContentLength(big)))
	require.NoError(t, m.Put(ctx, "c", "small", strings.NewReader("s"), engine.WithContentLength(1)))

	b1 := mustMultiGet(ctx, t, m, "c", "big")
	b2 := mustMultiGet(ctx, t, m, "c", "big")
	// Small reads are not held back by the large ones.
	assert.Equal(t, "s", string(readAllClose(t, mustMultiGet(ctx, t, m, "c", "small"))))
	rc, err := m.GetRange(ctx, "c", "big", 0, 1<<20)
	require.NoError(t, err, "a small range of a big object is small")
	mustClose(rc)

	wctx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	_, err = m.Get(wctx, "c", "big")
	cancel()
	require.Error(t, err, "a third large body waits for a slot")
	assert.ErrorIs(t, err, context.DeadlineExceeded)

	mustClose(b1)
	b3 := mustMultiGet(ctx, t, m, "c", "big")
	n, err := io.Copy(io.Discard, b3)
	require.NoError(t, err)
	assert.Equal(t, int64(big), n)
	mustClose(b3)
	mustClose(b2)
}

// --- config ----------------------------------------------------------------------

func TestSyncWebDAVConfigFromEnv_Bridges(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

	c, ok, err := SyncWebDAVConfigFromEnv(env(map[string]string{
		"SYNC_WEBDAV_URLS":      "http://127.0.0.1:4918, http://127.0.0.1:4919,http://127.0.0.1:4920",
		"SYNC_WEBDAV_PASSWORDS": "p0,p1,p2",
	}))
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, []WebDAVBridge{
		{URL: "http://127.0.0.1:4918", Password: "p0"},
		{URL: "http://127.0.0.1:4919", Password: "p1"},
		{URL: "http://127.0.0.1:4920", Password: "p2"},
	}, c.Bridges)
	assert.Equal(t, "sync", c.User)
	assert.Equal(t, "vaultaire", c.Root)

	// The single-bridge form is one bridge.
	c, ok, err = SyncWebDAVConfigFromEnv(env(map[string]string{"SYNC_WEBDAV_PASSWORD": "pw"}))
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, []WebDAVBridge{{URL: SyncWebDAVDefaultURL, Password: "pw"}}, c.Bridges)

	c, _, err = SyncWebDAVConfigFromEnv(env(map[string]string{
		"SYNC_WEBDAV_URLS": "http://127.0.0.1:4918,http://127.0.0.1:4919", "SYNC_WEBDAV_PASSWORDS": "p0,p1",
		"SYNC_WEBDAV_LARGE_CONCURRENCY": "4",
	}))
	require.NoError(t, err)
	assert.Equal(t, 4, c.LargeConcurrency)

	c, _, err = SyncWebDAVConfigFromEnv(env(map[string]string{
		"SYNC_WEBDAV_URLS": "http://127.0.0.1:4918", "SYNC_WEBDAV_PASSWORDS": "p0", "SYNC_WEBDAV_LARGE_CONCURRENCY": "0",
	}))
	require.NoError(t, err, "a bad limit keeps the default")
	assert.Zero(t, c.LargeConcurrency)
	assert.Len(t, c.Warnings, 1)

	many := make([]string, 17)
	pws := make([]string, 17)
	for i := range many {
		many[i] = fmt.Sprintf("http://127.0.0.1:%d", 5000+i)
		pws[i] = fmt.Sprintf("secret%d", i)
	}
	for name, e := range map[string]map[string]string{
		"count mismatch":         {"SYNC_WEBDAV_URLS": "http://127.0.0.1:4918,http://127.0.0.1:4919", "SYNC_WEBDAV_PASSWORDS": "secretA"},
		"passwords without urls": {"SYNC_WEBDAV_PASSWORDS": "secretA,secretB"},
		"urls without passwords": {"SYNC_WEBDAV_URLS": "http://127.0.0.1:4918", "SYNC_WEBDAV_PASSWORD": "secretA"},
		"duplicate url":          {"SYNC_WEBDAV_URLS": "http://127.0.0.1:4918,http://127.0.0.1:4918/", "SYNC_WEBDAV_PASSWORDS": "secretA,secretB"},
		"empty url":              {"SYNC_WEBDAV_URLS": "http://127.0.0.1:4918,,", "SYNC_WEBDAV_PASSWORDS": "secretA,secretB,secretC"},
		"empty password":         {"SYNC_WEBDAV_URLS": "http://127.0.0.1:4918,http://127.0.0.1:4919", "SYNC_WEBDAV_PASSWORDS": "secretA,"},
		"bad scheme":             {"SYNC_WEBDAV_URLS": "ftp://127.0.0.1:4918", "SYNC_WEBDAV_PASSWORDS": "secretA"},
		"credentials in url":     {"SYNC_WEBDAV_URLS": "http://sync:secretA@127.0.0.1:4918", "SYNC_WEBDAV_PASSWORDS": "secretA"},
		"too many":               {"SYNC_WEBDAV_URLS": strings.Join(many, ","), "SYNC_WEBDAV_PASSWORDS": strings.Join(pws, ",")},
	} {
		_, ok, err := SyncWebDAVConfigFromEnv(env(e))
		assert.Error(t, err, name)
		assert.True(t, ok, "%s: configured (wrongly) — the boot says so loudly", name)
		if err != nil {
			assert.NotContains(t, err.Error(), "secret", "%s: a password is never in an error", name)
		}
	}

	_, ok, err = SyncWebDAVConfigFromEnv(env(nil))
	assert.NoError(t, err)
	assert.False(t, ok)
}

func TestMultiWebDAV_StoreIDAndValidation(t *testing.T) {
	c := WebDAVConfig{User: "sync", Root: "vaultaire", Bridges: []WebDAVBridge{
		{URL: "http://127.0.0.1:4919", Password: "b"}, {URL: "http://127.0.0.1:4918", Password: "a"},
	}}
	m, err := NewMultiWebDAVDriver("sync", c, nil)
	require.NoError(t, err)
	c.Bridges[0], c.Bridges[1] = c.Bridges[1], c.Bridges[0]
	m2, err := NewMultiWebDAVDriver("sync", c, nil)
	require.NoError(t, err)
	assert.Equal(t, m.StoreID(), m2.StoreID(), "the bridges' order is not the store")
	assert.Contains(t, m.StoreID(), "127.0.0.1:4918")

	_, err = NewMultiWebDAVDriver("sync", WebDAVConfig{User: "sync"}, nil)
	assert.Error(t, err, "no bridge")
	_, err = NewMultiWebDAVDriver("sync", WebDAVConfig{User: "sync", Bridges: []WebDAVBridge{
		{URL: "http://127.0.0.1:4918", Password: "a"}, {URL: "http://127.0.0.1:4918/", Password: "b"},
	}}, nil)
	assert.Error(t, err, "the same bridge twice")
}

// Sync's bridge reports a wrong getlastmodified for every file (live,
// 2026-10-07: a file uploaded now lists as 1970-01-21T17:35:39Z — rclone
// --min-age retention deleted fresh files because of it). The driver never
// asks for it, so no listing, walk or health decision can rest on it.
func TestWebDAV_NeverAsksForModTimes(t *testing.T) {
	assert.NotContains(t, strings.ToLower(propfindBody), "lastmodified")
	assert.NotContains(t, strings.ToLower(propfindBody), "creationdate")
}
