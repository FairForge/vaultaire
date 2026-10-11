package drivers

import (
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Prompt 2b.4 E1.2 — the trial mark of a read routed to a bridge marked
// down was the context of the whole open: a striped object's piece reads on
// HEALTHY bridges inherited it (one attempt, the trial's short wait), and a
// slow first byte was noted against those bridges. Two loaded bridges plus
// the down one are three failing: the trial itself charged the breaker.
// Before (eb79aaf): healthy bridges noted failing (failures=2, failing=true).
func TestMultiWebDAV_ATrialReadNeverMarksThePieceBridgesFailing(t *testing.T) {
	defer setBridgeTrialTimeout(setBridgeTrialTimeout(300 * time.Millisecond))
	// Arrange: piece GETs on every bridge but the key's answer after 700 ms
	// (loaded, healthy); the key's own bridge answers at once.
	var slow atomic.Bool
	routed := 0
	bs := newBridges(t, 3, true, func(i int, h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if slow.Load() && i != routed && r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/p0000") {
				time.Sleep(700 * time.Millisecond)
			}
			h.ServeHTTP(w, r)
		})
	})
	m := newStriped(t, bs, nil)
	ctx := davCtx("tenant-a")
	a := keyOnBridge(ctx, t, m, "c", "striped", routed)
	body := randBody(t, 3*testPiece)
	require.NoError(t, m.Put(ctx, "c", a, onlyReader{strings.NewReader(string(body))}, withLen(len(body))))
	m.mcache.drop(manifestCacheKeyOf(t, m, "c", a))
	slow.Store(true)
	m.bridges[routed].probeDown.Store(true) // the read is a trial

	// Act
	rc, err := m.Get(ctx, "c", a)

	// Assert: the pieces are read as any read (the 700 ms answer is waited
	// for), and no healthy bridge is noted failing.
	require.NoError(t, err)
	assert.Equal(t, body, readAllClose(t, rc))
	for i, b := range m.bridges {
		if i == routed {
			continue
		}
		t.Logf("bridge %d: failures=%d failing=%v", i, b.failures.Load(), m.failing(b))
		assert.Zero(t, b.failures.Load(), "bridge %d", i)
		assert.False(t, m.failing(b), "bridge %d", i)
	}
}

// Prompt 2b.4 E1.3 — a metadata answer (PROPFIND: Exists, stat, List) is
// no data-path evidence: in the Route 53 shape (PROPFIND from cache passes,
// GET/PUT 5xx) one Exists per bridge reset the rule.
// Before (eb79aaf): failing bridges after Exists: 0.
func TestMultiWebDAV_AMetadataAnswerNeverClearsFailing(t *testing.T) {
	// Arrange: every data method fails, every PROPFIND answers.
	var down atomic.Bool
	bs := newBridges(t, 5, true, func(_ int, h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if down.Load() && r.Method != "PROPFIND" {
				http.Error(w, "upstream unreachable", http.StatusBadGateway)
				return
			}
			h.ServeHTTP(w, r)
		})
	})
	m := newMulti(t, bs, nil)
	ctx := davCtx("tenant-a")
	keys := make([]string, 5)
	for i := range keys {
		keys[i] = keyOnBridge(ctx, t, m, "c", fmt.Sprintf("k%d-", i), i)
	}
	down.Store(true)
	for _, a := range keys {
		_, err := m.Get(ctx, "c", a)
		require.Error(t, err)
	}

	// Act: an Exists and a List per bridge (all PROPFIND).
	for _, a := range keys {
		_, _ = m.Exists(ctx, "c", a)
	}
	_, _ = m.List(ctx, "c", "")

	// Assert
	failing := 0
	for _, b := range m.bridges {
		if m.failing(b) {
			failing++
		}
	}
	t.Logf("failing bridges after Exists: %d", failing)
	assert.Equal(t, 5, failing)
}

func manifestCacheKeyOf(t *testing.T, m *MultiWebDAVDriver, container, artifact string) string {
	t.Helper()
	_, names, _, err := m.resolve(davCtx("tenant-a"), "Get", container, artifact)
	require.NoError(t, err)
	return manifestCacheKey(names)
}
