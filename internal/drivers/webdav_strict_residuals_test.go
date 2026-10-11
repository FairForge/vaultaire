package drivers

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/engine"
)

// Prompt 2b.5 F3.1: strictness was keyed on the RECORDED name being a
// registered driver. A head row that says `sync` on a process where the sync
// driver is not registered (SYNC_WEBDAV_URLS unset on a redeploy) fell back
// to the fan-out: the primary's older copy was served (before: "OLD", 200).
func TestSync_RowWithoutTheSyncDriverIsNeverServedFromThePrimary(t *testing.T) {
	p := newMemPrimary()
	e := engine.NewEngine(nil, zap.NewNop(), &engine.Config{DefaultBackend: "primary"})
	e.AddDriver("primary", p)
	ctx := davCtx("tenant-a")
	require.NoError(t, p.Put(ctx, "c", "doc", strings.NewReader("OLD")))
	e.HintBackend("c", "doc", "sync")

	rc, err := e.Get(ctx, "c", "doc")
	if err == nil {
		b, _ := io.ReadAll(rc)
		_ = rc.Close()
		t.Logf("served %q from the primary for a row recorded on sync (driver not registered)", b)
	}
	assert.ErrorIs(t, err, engine.ErrAllBackendsUnavailable, "a strict target that is not registered is never served from the primary")
}

// Prompt 2b.5 F3.5: a metadata FAILURE (PROPFIND 5xx) set lastFail, so three
// bridges whose data path is fine but whose PROPFIND fails were "failing",
// and a fourth bridge's single data failure was charged to the backend
// instead of partial (before: failing = 3, ErrAllBackendsUnavailable).
func TestSync_MetadataFailuresAreNoDataPathEvidence(t *testing.T) {
	var propfindDown atomic.Bool
	bs := newBridges(t, 5, true, func(i int, h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Every PROPFIND of an object path (not the probe's root).
			if propfindDown.Load() && i < 3 && r.Method == "PROPFIND" && strings.Contains(r.URL.Path, "/c/") {
				http.Error(w, "upstream unreachable", http.StatusBadGateway)
				return
			}
			h.ServeHTTP(w, r)
		})
	})
	e, m, _ := syncEngine(t, bs, nil)
	ctx := davCtx("tenant-a")
	keys := make([]string, 3)
	for i := range keys {
		keys[i] = keyOnBridge(ctx, t, m, "c", fmt.Sprintf("meta%d-", i), i)
		_, err := e.Put(ctx, "c", keys[i], strings.NewReader("x"), engine.WithStorageClass("SYNC"), engine.WithContentLength(1))
		require.NoError(t, err)
	}
	on4 := keyOnBridge(ctx, t, m, "c", "data", 4)
	_, err := e.Put(ctx, "c", on4, strings.NewReader("x"), engine.WithStorageClass("SYNC"), engine.WithContentLength(1))
	require.NoError(t, err)
	propfindDown.Store(true)
	for _, a := range keys {
		_, _ = m.Exists(ctx, "c", a) // PROPFIND 502 on bridges 0..2; their GET/PUT are fine
	}
	failing := 0
	for _, b := range m.bridges {
		if m.failing(b) {
			failing++
		}
	}
	t.Logf("failing bridges after three PROPFIND failures (data path up): %d", failing)

	// Bridge 4 alone goes away: with the others' data path fine this is
	// partial by the README's definition.
	bs[4].srv.Close()
	e.HintBackend("c", on4, "sync")
	_, err = e.Get(ctx, "c", on4)
	require.Error(t, err)
	t.Logf("err=%v", err)
	assert.ErrorIs(t, err, engine.ErrPartiallyUnavailable, "one bridge out while the rest's data path answers is partial, whatever their PROPFINDs said")
}
