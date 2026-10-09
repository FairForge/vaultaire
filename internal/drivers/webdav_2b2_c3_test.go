package drivers

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FairForge/vaultaire/internal/engine"
)

// Prompt 2b.2 C3 P3s on the multi-bridge driver. Before = 39eee79.

// The key's commit lock is waited for under the caller's context. Before:
// a plain sync.Mutex — a Delete with a 200 ms deadline waited behind a
// stalled plain PUT of the same key for as long as the PUT took.
func TestMultiWebDAV_AKeyLockWaitHonoursTheCallersDeadline(t *testing.T) {
	var stallPut atomic.Bool
	wait, rel := hang()
	var once sync.Once
	release := func() { once.Do(rel) }
	bs := newBridges(t, 2, true, func(i int, h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPut && stallPut.Load() && strings.Contains(r.URL.Path, "/busy") {
				wait(r)
				return
			}
			h.ServeHTTP(w, r)
		})
	})
	t.Cleanup(release)
	m := newMulti(t, bs, func(c *WebDAVConfig) { c.StripeMin = -1 }, WithWebDAVIdleTimeout(0), WithWebDAVPutTimeout(0))
	ctx := davCtx("tenant-a")
	stallPut.Store(true)
	putDone := make(chan error, 1)
	go func() {
		putDone <- m.Put(ctx, "c", "busy", strings.NewReader("stuck"), engine.WithContentLength(5))
	}()
	require.Eventually(t, func() bool { return bs[0].count("PUT", "busy")+bs[1].count("PUT", "busy") > 0 }, 5*time.Second, 5*time.Millisecond)

	dctx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := m.Delete(dctx, "c", "busy")

	require.Error(t, err)
	assert.Less(t, time.Since(start), time.Second, "the delete gave up at its deadline")
	assert.ErrorIs(t, err, engine.ErrPartiallyUnavailable, "the key is busy: a retryable 503, never a backend failure")
	release()
	<-putDone
}

// An overwrite that finds an unreadable manifest whose generation it can
// still name retires that generation (gone an hour later). Before: the
// manifest was removed and its pieces left to the reaper's 6 h grace.
func TestMultiWebDAV_AnUnreadableManifestsGenerationIsRetired(t *testing.T) {
	bs := newBridges(t, 3, true, nil)
	m := newMulti(t, bs, func(c *WebDAVConfig) {
		c.StripeMin, c.StripePiece, c.StagingDir = 32<<20, 16<<20, t.TempDir()
	})
	m.stripeSettle = time.Millisecond
	ctx := davCtx("tenant-a")
	body := strings.Repeat("s", 40<<20)
	require.NoError(t, m.Put(ctx, "c", "big", strings.NewReader(body), engine.WithContentLength(int64(len(body)))))
	key, names, order, err := m.resolve(ctx, "test", "c", "big")
	require.NoError(t, err)
	man, err := m.readManifestOn(ctx, m.bridges[order[0]], key, names, engine.ErrNotFound("c", "big"))
	require.NoError(t, err)

	// The manifest is corrupted (its pieces no longer sum to its size) but
	// still names its folder and generation.
	bad := *man
	bad.Size++
	raw, err := json.Marshal(&bad)
	require.NoError(t, err)
	require.NoError(t, m.bridges[order[0]].drv.putNames(ctx, "corrupt", manifestNamesOf(names), strings.NewReader(string(raw)), engine.WithContentLength(int64(len(raw)))))
	m.mcache.drop(manifestCacheKey(names))

	// Act: a small overwrite drops the manifest.
	require.NoError(t, m.Put(ctx, "c", "big", strings.NewReader("small"), engine.WithContentLength(5)))

	// Assert: the old generation is retired, not left for 6 h.
	at, retired, err := m.retiredAt(ctx, man.Dir)
	require.NoError(t, err)
	assert.True(t, retired, "the generation the corrupt manifest names is retired")
	assert.False(t, at.IsZero())
}
