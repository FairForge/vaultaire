package drivers

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	promtest "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FairForge/vaultaire/internal/engine"
)

// Prompt 2b.3 D1.1 — the engine's `sync` breaker against a fleet-wide
// data-path outage while every bridge still answers its probe (the
// 2026-10-07 Route 53 episode: five bridges answering PROPFIND, every data
// call 500 for 8.5 min). `settle` judged "another bridge is healthy" from
// the probe flag, and each probe reset every bridge to healthy, so almost
// every failure was "partial" (uncharged) and the breaker never opened.
// "Before" numbers: these tests run against 9bc9d1e.

// dataPathDown answers 502 to every request but the Depth 0 PROPFIND of
// the health check while down is set on bridge i.
func dataPathDown(down []*atomic.Bool) func(i int, h http.Handler) http.Handler {
	return func(i int, h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			probe := r.Method == "PROPFIND" && r.Header.Get("Depth") == "0"
			if down[i].Load() && !probe {
				http.Error(w, "upstream unreachable", http.StatusBadGateway)
				return
			}
			h.ServeHTTP(w, r)
		})
	}
}

func TestEngine_AFleetWideDataPathOutageOpensTheSyncBreakerWhileProbesPass(t *testing.T) {
	// Arrange: five bridges; every data call fails, every probe passes.
	down := make([]*atomic.Bool, 5)
	for i := range down {
		down[i] = &atomic.Bool{}
	}
	bs := newBridges(t, 5, true, dataPathDown(down))
	e, m, _ := syncEngine(t, bs, nil)
	ctx := davCtx("tenant-a")
	partialBefore := promtest.ToFloat64(engine.PartialUnavailableCounter().WithLabelValues("sync", "get"))

	// Act: 20 reads, round-robin over the bridges (HRW ranks bridge URLs,
	// and httptest ports are random: keys are routed explicitly), a probe
	// after every fourth (the probe loop resetting the bridges' state).
	// Before (9bc9d1e): 20 partial, 0 charged, breaker closed.
	keys := make([]string, 20)
	for i := range keys {
		keys[i] = keyOnBridge(ctx, t, m, "c", fmt.Sprintf("k%02d", i), i%5)
	}
	for _, d := range down {
		d.Store(true)
	}
	charged, partial, openedAt := 0, 0, -1
	for i, a := range keys {
		if i%4 == 0 {
			require.NoError(t, m.HealthCheck(ctx), "every probe passes")
		}
		e.HintBackend("c", a, "sync")
		_, err := e.Get(ctx, "c", a)
		require.Error(t, err)
		if errorsIsPartial(err) {
			partial++
		} else {
			charged++
		}
		if openedAt < 0 && syncBreaker(e) == "open" {
			openedAt = i
		}
	}

	// Assert: the backend is down, so the breaker opens — the first two
	// failing bridges are partial, from the third every failure is charged,
	// and the engine's 5th charge (read 6) opens it: well within one probe
	// window (these reads take milliseconds; the probe runs every 30 s).
	t.Logf("partial=%d charged=%d opened at read %d", partial, charged, openedAt)
	assert.Equal(t, "open", syncBreaker(e), "every bridge's data path down is the backend down")
	assert.Equal(t, 2, partial, "only the failures before a third bridge is seen failing are partial")
	assert.Equal(t, 6, openedAt)
	assert.Equal(t, float64(partial), promtest.ToFloat64(engine.PartialUnavailableCounter().WithLabelValues("sync", "get"))-partialBefore,
		"each partial answer is counted")
}

func TestEngine_OneBridgesDataPathDownStaysPartialWhileProbesPass(t *testing.T) {
	// Arrange: bridge 0's data path fails, its probe passes; the other four
	// answer — and see no traffic at all (a quiet backend: no "recent OK"
	// evidence for them, which must not make bridge 0's failures charged).
	down := make([]*atomic.Bool, 5)
	for i := range down {
		down[i] = &atomic.Bool{}
	}
	bs := newBridges(t, 5, true, dataPathDown(down))
	e, m, _ := syncEngine(t, bs, nil)
	ctx := davCtx("tenant-a")
	var keys []string
	for i := 0; i < 12; i++ {
		keys = append(keys, keyOnBridge(ctx, t, m, "c", fmt.Sprintf("b0-%d", i), 0))
	}
	down[0].Store(true)

	// Act
	for i, a := range keys {
		if i%4 == 0 {
			require.NoError(t, m.HealthCheck(ctx))
		}
		e.HintBackend("c", a, "sync")
		_, err := e.Get(ctx, "c", a)

		// Assert
		require.Error(t, err)
		assert.ErrorIs(t, err, engine.ErrPartiallyUnavailable, "read %d", i)
	}
	assert.Equal(t, "closed", syncBreaker(e), "one bridge out of five is not the backend out")

	// Two bridges out: still partial while three answer.
	down[1].Store(true)
	for i := 0; i < 6; i++ {
		a := keyOnBridge(ctx, t, m, "c", fmt.Sprintf("b1-%d", i), 1)
		e.HintBackend("c", a, "sync")
		_, err := e.Get(ctx, "c", a)
		assert.ErrorIs(t, err, engine.ErrPartiallyUnavailable)
	}
	assert.Equal(t, "closed", syncBreaker(e))
}

func errorsIsPartial(err error) bool { return errors.Is(err, engine.ErrPartiallyUnavailable) }

// D1.3: the trial read of a bridge marked down is paid by a customer
// request, so it is one attempt with a short wait for the answer. Before:
// three attempts, each up to the 60 s idle watchdog (here: retries × hang).
func TestMultiWebDAV_ATrialReadOfAStalledBridgeIsOneShortAttempt(t *testing.T) {
	defer setBridgeTrialTimeout(setBridgeTrialTimeout(200 * time.Millisecond))
	var stall atomic.Bool
	var gets atomic.Int32
	bs := newBridges(t, 2, true, func(i int, h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if i == 0 && stall.Load() && r.Method == http.MethodGet {
				gets.Add(1)
				select {
				case <-r.Context().Done():
				case <-time.After(5 * time.Second):
				}
				return
			}
			h.ServeHTTP(w, r)
		})
	})
	m := newMulti(t, bs, nil, WithWebDAVIdleTimeout(2*time.Second))
	ctx := davCtx("tenant-a")
	a := keyOnBridge(ctx, t, m, "c", "stalled", 0)
	require.NoError(t, m.Put(ctx, "c", a, strings.NewReader("here"), engine.WithContentLength(4)))
	stall.Store(true)
	m.bridges[0].probeDown.Store(true)

	// Act
	start := time.Now()
	_, err := m.Get(ctx, "c", a)

	// Assert
	assertRoutedDown(t, err)
	assert.Less(t, time.Since(start), time.Second, "bounded by the trial timeout, not 3 × the idle watchdog")
	assert.Equal(t, int32(1), gets.Load(), "one attempt")
	assert.False(t, m.healthy(m.bridges[0]), "a trial that got no answer leaves the bridge down")
}
