package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/FairForge/vaultaire/internal/drivers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"golang.org/x/net/webdav"
)

// Prompt 2a.2 G3, end to end on the Sync leg shape: five bridges on ONE
// account (one shared file system), one of them answering PROPFIND from an
// older view (a bridge that has not seen the others' writes yet). The
// reconcile erases an orphan and an empty sibling under the digest of a real
// row, through the multi-bridge driver — and the real row's shards are
// untouched whichever bridge lags.

func syncBridges(t *testing.T, shared, stale webdav.FileSystem, lagging int, lagOn *atomic.Bool) drivers.WebDAVConfig {
	t.Helper()
	cfg := drivers.WebDAVConfig{User: "sync", Root: "vaultaire"}
	staleH := &webdav.Handler{FileSystem: stale, LockSystem: webdav.NewMemLS()}
	for i := 0; i < 5; i++ {
		h := http.Handler(&webdav.Handler{FileSystem: shared, LockSystem: webdav.NewMemLS()})
		pass := fmt.Sprintf("bridge-%d", i)
		lag := i == lagging
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if u, p, ok := r.BasicAuth(); !ok || u != "sync" || p != pass {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			if lag && lagOn.Load() && r.Method == "PROPFIND" {
				staleH.ServeHTTP(w, r)
				return
			}
			h.ServeHTTP(w, r)
		}))
		t.Cleanup(srv.Close)
		cfg.Bridges = append(cfg.Bridges, drivers.WebDAVBridge{URL: srv.URL, Password: pass})
	}
	return cfg
}

func memMkdirs(t *testing.T, fs webdav.FileSystem, dir string) {
	t.Helper()
	cur := ""
	for _, s := range strings.Split(strings.Trim(dir, "/"), "/") {
		cur += "/" + s
		if err := fs.Mkdir(context.Background(), cur, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
			require.NoError(t, err)
		}
	}
}

func memPut(t *testing.T, fs webdav.FileSystem, p string) {
	t.Helper()
	memMkdirs(t, fs, p[:strings.LastIndex(p, "/")])
	f, err := fs.OpenFile(context.Background(), p, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	require.NoError(t, err)
	_, _ = f.Write([]byte("orphan"))
	require.NoError(t, f.Close())
}

func memHas(fs webdav.FileSystem, p string) bool {
	_, err := fs.Stat(context.Background(), p)
	return err == nil
}

func TestVaultParity_SyncLegReconcileNeverCostsARealShardWhicheverBridgeLags(t *testing.T) {
	for lagging := 0; lagging < 5; lagging++ {
		t.Run(fmt.Sprintf("bridge %d lags", lagging), func(t *testing.T) {
			// Arrange: the real row's shards written through the driver; an
			// orphan and an empty sibling planted under the same digest; the
			// lagging bridge's view holds the folders but not the real shards
			// (a bridge that saw the MKCOLs, not the PUTs).
			f := setupParityFixture(t)
			f.cleanSightings()
			shared, stale := webdav.NewMemFS(), webdav.NewMemFS()
			var lagOn atomic.Bool
			sync, err := drivers.NewMultiWebDAVDriver("sync", syncBridges(t, shared, stale, lagging, &lagOn), zap.NewNop(),
				drivers.WithWebDAVRetries(2, time.Millisecond))
			require.NoError(t, err)
			f.eng.AddDriver("sync", sync)
			_, etag := f.object("real.bin", 2*f.stripe+1)
			first := f.run()
			require.Equal(t, 1, first.Protected, "%+v", first)
			require.Equal(t, "sync", first.Leg)

			base := "/vaultaire/t-" + f.tenantID + "/" + parityContainer(f.tenantID) + "/"
			real := shardPrefix(f.bucket, "real.bin", etag)
			digest, _, _ := strings.Cut(real, "/")
			for j := 0; j < 4; j++ {
				require.True(t, memHas(shared, fmt.Sprintf("%s%s/p%d%%o", base, real, j)))
			}
			memPut(t, shared, base+digest+"/e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1/p0%o")
			memMkdirs(t, shared, base+digest+"/e0e0e0e0e0e0e0e0e0e0e0e0e0e0e0e0")
			memMkdirs(t, stale, base+real)
			memMkdirs(t, stale, base+digest+"/e0e0e0e0e0e0e0e0e0e0e0e0e0e0e0e0")
			lagOn.Store(true)

			// Act: a sighting, the grace, then two reconcile slots.
			f.run()
			f.ageOrphans(2 * time.Hour)
			f.run()
			f.run()

			// Assert: the real row's four shards are all there; the orphan
			// and the empty sibling are gone; the digest stays (it holds the row).
			for j := 0; j < 4; j++ {
				assert.True(t, memHas(shared, fmt.Sprintf("%s%s/p%d%%o", base, real, j)), "real shard p%d", j)
			}
			assert.False(t, memHas(shared, base+digest+"/e0e0e0e0e0e0e0e0e0e0e0e0e0e0e0e0"))
			assert.True(t, memHas(shared, base+digest))
			state, _, _, _ := f.row("real.bin")
			assert.Equal(t, "complete", state)

			// The lag ends (Sync converges): the orphan is gone too, the real row still whole.
			lagOn.Store(false)
			f.run() // a bridge that hid the orphan: its first sighting is now
			f.ageOrphans(2 * time.Hour)
			f.run()
			assert.False(t, memHas(shared, base+digest+"/e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1"), "the orphan folder is erased")
			for j := 0; j < 4; j++ {
				assert.True(t, memHas(shared, fmt.Sprintf("%s%s/p%d%%o", base, real, j)), "real shard p%d", j)
			}
			assert.Equal(t, 0, f.orphanRows())
		})
	}
}
