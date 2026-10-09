package drivers

import (
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FairForge/vaultaire/internal/engine"
)

// Prompt 2b.2 C2 on the real driver: a DELETE of an object whose bridge is
// down is a 503 (the head row stays; the client retries) — never the
// primary's idempotent success while the bytes stay on Sync. Before
// (2b8adc3): the engine fell back to the primary, Delete returned nil. (C1
// already ends the walk on one bridge out; the engine rule of C2 covers
// the open breaker and every other backend — internal/engine
// delete_hint_test.go.)
func TestEngine_AHintedSyncDeleteOnADownBridgeFailsAndTheRetryDeletes(t *testing.T) {
	var down atomic.Bool
	bs := newBridges(t, 3, true, func(i int, h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if i == 0 && down.Load() {
				http.Error(w, "bridge restarting", http.StatusServiceUnavailable)
				return
			}
			h.ServeHTTP(w, r)
		})
	})
	e, m, p := syncEngine(t, bs, nil)
	ctx := davCtx("tenant-a")
	a := keyOnBridge(ctx, t, m, "c", "held", 0)
	require.NoError(t, m.Put(ctx, "c", a, strings.NewReader("on bridge 0"), engine.WithContentLength(11)))
	down.Store(true)

	e.HintBackend("c", a, "sync")
	err := e.Delete(ctx, "c", a)

	require.Error(t, err)
	assert.ErrorIs(t, err, engine.ErrPartiallyUnavailable)
	assert.Zero(t, p.deletes, "the primary is never asked")
	assert.Equal(t, "closed", syncBreaker(e))

	// The bridge is back: the retry deletes the object there.
	down.Store(false)
	e.HintBackend("c", a, "sync")
	require.NoError(t, e.Delete(ctx, "c", a))
	ok, err := m.Exists(ctx, "c", a)
	require.NoError(t, err)
	assert.False(t, ok)
}
