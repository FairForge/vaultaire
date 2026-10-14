package drivers

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FairForge/vaultaire/internal/engine"
)

// Prompt 2b.5 F1.1: the miss predicate matched the error text, and the
// webdav driver's errors embed the key ("get t-a/c/<key>") and the bridge's
// text. A key named "NotFound…" failed as a MISS when its bridge was down:
// no breaker charge, and Delete dropped the route. Before: the second Delete
// asked the primary (deletes=1).
func TestSync_KeyNamedNotFoundOnADownBridgeIsNoMiss(t *testing.T) {
	down := downFlags(5)
	bs := newBridges(t, 5, true, dataPathDown(down))
	e, m, p := syncEngine(t, bs, nil)
	ctx := davCtx("tenant-a")

	// Keys that route to bridge 0; one is named with "NotFound".
	bad := keyOnBridge(ctx, t, m, "c", "NotFound", 0)
	good := keyOnBridge(ctx, t, m, "c", "plain", 0)
	for _, a := range []string{bad, good} {
		_, err := e.Put(ctx, "c", a, strings.NewReader("x"), engine.WithStorageClass("SYNC"), engine.WithContentLength(1))
		require.NoError(t, err)
	}
	down[0].Store(true)

	// Delete of the plain key: partial 503, route kept — a second Delete
	// asks sync again (the primary is never asked).
	err := e.Delete(engine.WithRecordedBackend(ctx, "sync"), "c", good)
	require.ErrorIs(t, err, engine.ErrAllBackendsUnavailable)
	err = e.Delete(engine.WithRecordedBackend(ctx, "sync"), "c", good)
	require.ErrorIs(t, err, engine.ErrAllBackendsUnavailable)
	assert.Zero(t, p.deletes, "plain key: the route is kept, the primary never asked")

	// Delete of the "NotFound" key: the same 503 — but is the route kept?
	err = e.Delete(engine.WithRecordedBackend(ctx, "sync"), "c", bad)
	t.Logf("first delete: %v", err)
	require.ErrorIs(t, err, engine.ErrAllBackendsUnavailable, "the answer is still a 503")
	err = e.Delete(engine.WithRecordedBackend(ctx, "sync"), "c", bad)
	t.Logf("second delete: %v (primary deletes=%d)", err, p.deletes)
	assert.Zero(t, p.deletes, "the route of a key named NotFound must survive a bridge-down 503 like any other")
}

// Same text match: a fleet-wide failure on keys named "NotFound…" was never
// charged to the sync breaker (before: closed after 8 failures).
func TestSync_FleetFailureOnKeysNamedNotFoundChargesTheBreaker(t *testing.T) {
	down := downFlags(5)
	bs := newBridges(t, 5, true, dataPathDown(down))
	e, m, _ := syncEngine(t, bs, nil)
	ctx := davCtx("tenant-a")
	fleetFailing(t, m, down)
	for i := 0; i < 8; i++ {
		a := keyOnBridge(ctx, t, m, "c", fmt.Sprintf("NotFound%d-", i), i%5)
		_, err := e.Get(engine.WithRecordedBackend(ctx, "sync"), "c", a)
		require.Error(t, err)
	}
	t.Logf("breaker after 8 charged-shape failures on NotFound-named keys: %s", syncBreaker(e))
	assert.Equal(t, "open", syncBreaker(e), "a fleet-wide failure must open the breaker whatever the key is called")
}
