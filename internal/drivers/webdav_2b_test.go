package drivers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	promtest "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FairForge/vaultaire/internal/engine"
)

// Prompt 2b Part A — the multi-bridge driver after the review of
// #614–#632. "Before" numbers are the same tests run against 0b376cf.

// trickle is a client body arriving slowly (a slow uplink through
// Cloudflare): chunk bytes every gap, never seekable.
type trickle struct {
	left, chunk int
	gap         time.Duration
	ready       int // bytes of the current chunk not yet read
}

func (r *trickle) Read(p []byte) (int, error) {
	if r.left == 0 {
		return 0, io.EOF
	}
	if r.ready == 0 {
		time.Sleep(r.gap)
		r.ready = r.chunk
	}
	n := min(len(p), r.ready, r.left)
	r.ready -= n
	for i := range p[:n] {
		p[i] = 'x'
	}
	r.left -= n
	return n, nil
}

func TestMultiWebDAV_ASlowClientNeverRunsOutThePutDeadlineOrChargesTheBridge(t *testing.T) {
	// Arrange: a 9 MiB body (above the in-memory retry buffer: streamed
	// once) arriving in 1 MiB pieces 60 ms apart (~0.55 s) against a PUT
	// deadline of 150 ms. Before: the deadline ran out while the driver
	// waited for the client, the PUT failed (ErrNoFailover, a timeout) and
	// the bridge was charged — three such uploads opened its breaker.
	bs := newBridges(t, 2, true, nil)
	m := newMulti(t, bs, func(c *WebDAVConfig) { c.StripeMin = -1 }, WithWebDAVPutTimeout(150*time.Millisecond))
	ctx := davCtx("tenant-a")
	a := keyOnBridge(ctx, t, m, "c", "slow", 0)
	const size = 9 << 20

	// Act
	for i := 0; i < 3; i++ {
		err := m.Put(ctx, "c", a, &trickle{left: size, chunk: 1 << 20, gap: 60 * time.Millisecond}, engine.WithContentLength(size))

		// Assert
		require.NoError(t, err, "upload %d", i)
	}
	assert.Zero(t, m.bridges[0].failures.Load(), "the bridge was never charged")
	assert.True(t, m.healthy(m.bridges[0]), "its breaker stays closed")
	assert.Zero(t, bs[1].count("PUT", "slow"), "no other bridge got the write")
}

// brokenSource fails mid-body, like a client that went away.
type brokenSource struct{ n int }

func (b *brokenSource) Read(p []byte) (int, error) {
	if b.n <= 0 {
		return 0, io.ErrUnexpectedEOF
	}
	n := min(len(p), b.n)
	b.n -= n
	return n, nil
}

func TestMultiWebDAV_ABodyThatBreaksIsTheCallersErrorNotTheBridges(t *testing.T) {
	// Before: the transport's error wrapping the source's failure was a
	// transport error — the bridge was charged (3 of them opened it).
	bs := newBridges(t, 1, true, nil)
	m := newMulti(t, bs, func(c *WebDAVConfig) { c.StripeMin = -1 })
	ctx := davCtx("tenant-a")
	for i := 0; i < 3; i++ {
		// A key each: the fake server still holds an aborted PUT's lock.
		err := m.Put(ctx, "c", fmt.Sprintf("k%d", i), &brokenSource{n: 9 << 20 / 2}, engine.WithContentLength(9<<20))
		require.Error(t, err)
		assert.ErrorIs(t, err, engine.ErrNoFailover)
		assert.False(t, strings.Contains(err.Error(), "timeout"))
	}
	assert.Zero(t, m.bridges[0].failures.Load())
	assert.True(t, m.healthy(m.bridges[0]))
}

// staleBridges: two bridges on their own file systems — bridge 1 has not
// seen bridge 0's latest writes (Sync carries an overwrite over after ~30 s,
// once 310 s; a delete after ~30 s).
func staleBridges(t *testing.T) (*MultiWebDAVDriver, []*bridgeServer, string) {
	bs := newBridges(t, 2, false, nil)
	m := newMulti(t, bs, nil)
	ctx := davCtx("tenant-a")
	a := keyOnBridge(ctx, t, m, "c", "photo", 0)
	putFile(t, bs[1].fs, "/vaultaire/t-tenant-a/c/"+a+"%o", "version-1") // bridge 1's stale view
	return m, bs, a
}

func TestMultiWebDAV_AnOverwrittenObjectIsNeverServedStaleFromAFallback(t *testing.T) {
	// Arrange: v1 everywhere, then v2 written through the routed bridge; the
	// fallback still sees v1; the routed bridge is marked down. Before: GET
	// answered 200 with "version-1" (the old bytes under v2's ETag).
	m, _, a := staleBridges(t)
	ctx := davCtx("tenant-a")
	require.NoError(t, m.Put(ctx, "c", a, strings.NewReader("version-2"), engine.WithContentLength(9)))
	m.bridges[0].probeDown.Store(true)
	m.bridges[0].trialAt.Store(time.Now().UnixNano()) // its trial read just ran (2b.2)

	// Act
	_, err := m.Get(ctx, "c", a)
	_, rerr := m.GetRange(ctx, "c", a, 0, 4)

	// Assert
	assertRoutedDown(t, err)
	assertRoutedDown(t, rerr)
}

func TestMultiWebDAV_ADeletedObjectNeverExistsOnAFallback(t *testing.T) {
	// Before: Exists answered true from the fallback that still saw it.
	m, _, a := staleBridges(t)
	ctx := davCtx("tenant-a")
	require.NoError(t, m.Put(ctx, "c", a, strings.NewReader("version-2"), engine.WithContentLength(9)))
	require.NoError(t, m.Delete(ctx, "c", a))
	m.bridges[0].probeDown.Store(true)
	m.bridges[0].trialAt.Store(time.Now().UnixNano())

	ok, err := m.Exists(ctx, "c", a)

	assertRoutedDown(t, err)
	assert.False(t, ok)
}

func TestMultiWebDAV_DeleteFindsAnObjectItsRoutedBridgeDoesNotSee(t *testing.T) {
	// Arrange: the object was written through bridge 1 (the set changed, or
	// a moment ago); it routes to bridge 0, which does not see it. Before:
	// bridge 0's 404 was a successful delete and the bytes stayed.
	bs := newBridges(t, 2, false, nil)
	m := newMulti(t, bs, nil)
	ctx := davCtx("tenant-a")
	a := keyOnBridge(ctx, t, m, "c", "moved", 0)
	putFile(t, bs[1].fs, "/vaultaire/t-tenant-a/c/"+a+"%o", "bytes")

	// Act
	require.NoError(t, m.Delete(ctx, "c", a))

	// Assert: gone from bridge 1 too.
	_, err := bs[1].fs.Stat(context.Background(), "/vaultaire/t-tenant-a/c/"+a+"%o")
	assert.ErrorIs(t, err, os.ErrNotExist, "the bytes are gone")
}

func TestMultiWebDAV_ADeleteMissedByItsBridgeFailsWhenAnotherCannotBeAsked(t *testing.T) {
	bs := newBridges(t, 2, false, nil)
	m := newMulti(t, bs, nil)
	ctx := davCtx("tenant-a")
	a := keyOnBridge(ctx, t, m, "c", "unseen", 0)
	bs[1].srv.Close()

	err := m.Delete(ctx, "c", a)

	assertRoutedDown(t, err)
}

func TestStripe_AGenerationIsLiveWhenAnyBridgesManifestNamesIt(t *testing.T) {
	// Arrange: an old generation whose commit only bridge 1 has seen — the
	// object's routed bridge 0 does not show the manifest yet (A6). Before:
	// the routed bridge's "no manifest" was authoritative, the generation
	// was judged dead and reaped under a committed object.
	bs := newBridges(t, 2, false, nil)
	m := newStriped(t, bs, nil)
	t0 := time.Date(2026, 10, 9, 6, 0, 0, 0, time.UTC)
	m.now = func() time.Time { return t0.Add(8 * time.Hour) }
	ctx := davCtx("tenant-a")
	a := keyOnBridge(ctx, t, m, "bucket", "committed", 0)
	tf, cn := davName("t-tenant-a"), davName("bucket")
	gen := newStripeGen(t0)
	dir := stripeDir(tf, cn, a, gen)
	p := "/vaultaire/" + strings.Join(dir, "/")
	for _, b := range bs {
		putFile(t, b.fs, p+"/"+pieceName(0), "x")
		putFile(t, b.fs, p+"/"+stripeKeyFile, `{"container":"bucket","artifact":"`+a+`"}`)
	}
	man, err := json.Marshal(stripeManifest{Format: stripeManifestFormat, Size: 1, PieceSize: testPiece, Gen: gen, Dir: dir,
		Pieces: []stripePiece{{Size: 1, SHA256: "2d711642b726b04401627ca9fbac32f5c8530fb1903cc4db02258717921a4881"}}})
	require.NoError(t, err)
	putFile(t, bs[1].fs, "/vaultaire/"+tf+"/"+cn+"/"+leafName(a)[:len(leafName(a))-len(WebDAVLeafMarker)]+WebDAVStripeMarker, string(man))

	// Act
	res, err := m.ReapOrphanStripes(context.Background(), time.Hour)

	// Assert
	require.NoError(t, err)
	assert.Equal(t, 1, res.Live, "%+v", res)
	assert.Zero(t, res.Reaped)
	assert.NotEmpty(t, filesWith(allFiles(t, bs[:1]), p))
}

func TestMultiWebDAV_AFullFolderIsInvalidInputNotABridgeFailure(t *testing.T) {
	// Arrange: a folder at the server's file limit (5 here; Sync: 50,000)
	// whose PUTs the server answers 500 — Sync's real answer is unknown,
	// the guard does not depend on it. Before: three such writes were
	// bridge failures and opened the breaker for every tenant (and the
	// engine would fail the write over to another backend).
	var full atomic.Bool
	bs := newBridges(t, 1, true, func(_ int, h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPut && full.Load() {
				http.Error(w, "folder full", http.StatusInternalServerError)
				return
			}
			h.ServeHTTP(w, r)
		})
	})
	m := newMulti(t, bs, nil)
	m.bridges[0].drv.folderFileLimit = 5
	ctx := davCtx("tenant-a")
	for i := 0; i < 5; i++ {
		require.NoError(t, m.Put(ctx, "flat", fmt.Sprintf("k%d", i), strings.NewReader("v"), engine.WithContentLength(1)))
	}
	full.Store(true)

	// Act + Assert
	for i := 5; i < 8; i++ {
		err := m.Put(ctx, "flat", fmt.Sprintf("k%d", i), strings.NewReader("v"), engine.WithContentLength(1))
		require.Error(t, err)
		assert.ErrorIs(t, err, engine.ErrInvalidInput, "a 400, no failover")
	}
	assert.Zero(t, m.bridges[0].failures.Load())
	assert.True(t, m.healthy(m.bridges[0]))
	assert.Equal(t, float64(5), promtest.ToFloat64(webdavFolderFilesMax.WithLabelValues("sync", "0")))
}

// reapRetired runs the reaper as it would run past the retire grace.
func reapRetired(t *testing.T, m *MultiWebDAVDriver) {
	t.Helper()
	real := m.now
	m.now = func() time.Time { return real().Add(WebDAVDefaultStripeRetireGrace + time.Minute) }
	defer func() { m.now = real }()
	res, err := m.ReapOrphanStripes(context.Background(), WebDAVDefaultStripeGrace)
	require.NoError(t, err)
	require.Empty(t, res.Errors, "%+v", res)
}
