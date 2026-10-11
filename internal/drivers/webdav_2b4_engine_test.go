package drivers

import (
	"fmt"
	"io"
	"strings"
	"sync/atomic"
	"testing"

	promtest "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FairForge/vaultaire/internal/engine"
)

// Prompt 2b.4 E1.1 — `sync` is a strict target: once its failure is the
// backend's (three bridges failing, or the breaker open), the engine used to
// walk on to the primary. A PUT whose bridge failed before the body was read
// (every MKCOL into a folder the process had not seen, every 0-byte object)
// landed on iDrive and answered 200; a GET of a key whose older copy sat on
// the primary answered 200 with the old bytes. "Before" numbers: these tests
// run against eb79aaf.

// fleetFailing takes every bridge's data path down (probes pass) and makes
// three bridges failing by a read routed to each: the backend's failure is
// then charged, whatever the probes say (2b.3 D1.1).
func fleetFailing(t *testing.T, m *MultiWebDAVDriver, down []*atomic.Bool) {
	t.Helper()
	ctx := davCtx("tenant-a")
	keys := make([]string, 3)
	for i := range keys {
		keys[i] = keyOnBridge(ctx, t, m, "c", fmt.Sprintf("probe%d-", i), i)
	}
	for _, d := range down {
		d.Store(true)
	}
	for _, a := range keys {
		_, err := m.Get(ctx, "c", a)
		require.Error(t, err)
	}
	failing := 0
	for _, b := range m.bridges {
		if m.failing(b) {
			failing++
		}
	}
	require.GreaterOrEqual(t, failing, bridgeFleetFailing)
}

func downFlags(n int) []*atomic.Bool {
	out := make([]*atomic.Bool, n)
	for i := range out {
		out[i] = &atomic.Bool{}
	}
	return out
}

func TestEngine_ASyncPutNeverFallsOverToThePrimary(t *testing.T) {
	cases := []struct {
		name  string
		key   string
		body  string
		prime bool // write the key once while healthy: its folder is cached
	}{
		{name: "new folder", key: "fresh/obj", body: "payload"},
		{name: "known folder", key: "known/obj", body: "payload", prime: true},
		{name: "0-byte object", key: "empty", body: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Arrange
			down := downFlags(5)
			bs := newBridges(t, 5, true, dataPathDown(down))
			e, m, p := syncEngine(t, bs, nil)
			ctx := davCtx("tenant-a")
			if tc.prime {
				_, err := e.Put(ctx, "c", tc.key, strings.NewReader("old"), engine.WithStorageClass("SYNC"), engine.WithContentLength(3))
				require.NoError(t, err)
			}
			fleetFailing(t, m, down)

			// Act. Before (eb79aaf): new folder and 0-byte → used "primary",
			// err <nil>; known folder → 503.
			used, err := e.Put(ctx, "c", tc.key, onlyReader{strings.NewReader(tc.body)},
				engine.WithStorageClass("SYNC"), engine.WithContentLength(int64(len(tc.body))))

			// Assert
			t.Logf("used=%q err=%v", used, err)
			require.Error(t, err)
			assert.ErrorIs(t, err, engine.ErrAllBackendsUnavailable, "a 503 + Retry-After")
			assert.Empty(t, used)
			held, _ := p.Exists(ctx, "c", tc.key)
			assert.False(t, held, "nothing lands on the primary")
		})
	}
}

func TestEngine_ASyncPutWithTheBreakerOpenNeverLandsOnThePrimary(t *testing.T) {
	// Arrange: the backend's failures open the breaker.
	down := downFlags(5)
	bs := newBridges(t, 5, true, dataPathDown(down))
	e, m, p := syncEngine(t, bs, nil)
	ctx := davCtx("tenant-a")
	fleetFailing(t, m, down)
	for i := 0; syncBreaker(e) != "open" && i < 20; i++ {
		a := keyOnBridge(ctx, t, m, "c", fmt.Sprintf("trip%d-", i), i%5)
		e.HintBackend("c", a, "sync")
		_, _ = e.Get(ctx, "c", a)
	}
	require.Equal(t, "open", syncBreaker(e))

	// Act
	used, err := e.Put(ctx, "c", "while-open", strings.NewReader("x"), engine.WithStorageClass("SYNC"), engine.WithContentLength(1))

	// Assert
	t.Logf("used=%q err=%v", used, err)
	assert.ErrorIs(t, err, engine.ErrAllBackendsUnavailable)
	held, _ := p.Exists(ctx, "c", "while-open")
	assert.False(t, held)
}

func TestEngine_ASyncReadNeverServesAnotherBackendsCopy(t *testing.T) {
	for _, ranged := range []bool{false, true} {
		t.Run(fmt.Sprintf("ranged=%v", ranged), func(t *testing.T) {
			// Arrange: the key's older copy sits on the primary (written
			// before the tier flag, or by an earlier fall-over); the current
			// one on Sync.
			down := downFlags(5)
			bs := newBridges(t, 5, true, dataPathDown(down))
			e, m, p := syncEngine(t, bs, nil)
			ctx := davCtx("tenant-a")
			a := keyOnBridge(ctx, t, m, "c", "doc", 3)
			require.NoError(t, p.Put(ctx, "c", a, strings.NewReader("OLD")))
			_, err := e.Put(ctx, "c", a, strings.NewReader("NEW"), engine.WithStorageClass("SYNC"), engine.WithContentLength(3))
			require.NoError(t, err)
			fleetFailing(t, m, down)

			// Act. Before (eb79aaf): 200 with "OLD".
			e.HintBackend("c", a, "sync")
			var rc io.ReadCloser
			if ranged {
				rc, err = e.GetRange(ctx, "c", a, 0, 3)
			} else {
				rc, err = e.Get(ctx, "c", a)
			}

			// Assert
			if rc != nil {
				b, _ := io.ReadAll(rc)
				_ = rc.Close()
				t.Logf("served %q", b)
			}
			require.Error(t, err, "never another backend's bytes")
			assert.ErrorIs(t, err, engine.ErrAllBackendsUnavailable, "a 503 + Retry-After")
		})
	}
}

func TestEngine_ASyncDeleteNeverFallsOverToThePrimary(t *testing.T) {
	// Arrange (strict since 2b.2 C2 — asserted here with the fleet failing).
	down := downFlags(5)
	bs := newBridges(t, 5, true, dataPathDown(down))
	e, m, p := syncEngine(t, bs, nil)
	ctx := davCtx("tenant-a")
	a := keyOnBridge(ctx, t, m, "c", "gone", 2)
	_, err := e.Put(ctx, "c", a, strings.NewReader("x"), engine.WithStorageClass("SYNC"), engine.WithContentLength(1))
	require.NoError(t, err)
	fleetFailing(t, m, down)

	// Act
	err = e.Delete(ctx, "c", a)

	// Assert
	assert.ErrorIs(t, err, engine.ErrAllBackendsUnavailable)
	assert.Zero(t, p.deletes, "the primary is never asked")
}

func TestEngine_ASyncFailureIsNeverCountedAsAFallover(t *testing.T) {
	// Arrange
	down := downFlags(5)
	bs := newBridges(t, 5, true, dataPathDown(down))
	e, m, _ := syncEngine(t, bs, nil)
	ctx := davCtx("tenant-a")
	fleetFailing(t, m, down)
	before := promtest.ToFloat64(engine.FalloverCounter().WithLabelValues("sync", "primary", "put"))

	// Act
	_, _ = e.Put(ctx, "c", "fresh/x", strings.NewReader("x"), engine.WithStorageClass("SYNC"), engine.WithContentLength(1))

	// Assert
	assert.Zero(t, promtest.ToFloat64(engine.FalloverCounter().WithLabelValues("sync", "primary", "put"))-before)
}
