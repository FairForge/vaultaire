package api

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/FairForge/vaultaire/internal/drivers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// Prompt 2a.3 H1 — the post-merge review of #646. Folders are removed only
// by the reconcile, under the job lock: the delete, erasure and overwrite
// paths delete shards, then the row, and never a folder. "Before" numbers
// are the same scenarios run against 7bd5c5c.

// folderCallLeg counts folder removals, can make each one slow (a Sync
// RemoveEmptyDir is up to 11 PROPFIND/DELETE calls), and can end the
// caller's budget right after its n-th shard delete.
type folderCallLeg struct {
	*flakyLegDriver
	removes     atomic.Int32
	slow        time.Duration
	deletes     atomic.Int32
	cancelAfter int32
	cancel      context.CancelFunc
}

func (d *folderCallLeg) RemoveEmptyDir(ctx context.Context, container, dir string) error {
	d.removes.Add(1)
	if d.slow > 0 {
		select {
		case <-time.After(d.slow):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return d.LocalDriver.RemoveEmptyDir(ctx, container, dir)
}

func (d *folderCallLeg) Delete(ctx context.Context, container, artifact string) error {
	err := d.flakyLegDriver.Delete(ctx, container, artifact)
	if n := d.deletes.Add(1); d.cancel != nil && n == d.cancelAfter {
		d.cancel()
	}
	return err
}

func (f *parityFixture) folderLeg() *folderCallLeg {
	l := &folderCallLeg{flakyLegDriver: f.leg}
	f.eng.AddDriver("permafrost", l)
	return l
}

func (f *parityFixture) dropHead(key string) {
	f.t.Helper()
	_, err := f.db.Exec(`DELETE FROM object_head_cache WHERE tenant_id = $1 AND object_key = $2`, f.tenantID, key)
	require.NoError(f.t, err)
}

func TestVaultParity_TheDeletePathSendsNoFolderCallAndTheRowGoesWithItsShards(t *testing.T) {
	// Arrange: a protected object; folder removals take 1 s each; the delete
	// gets a 300 ms budget. Before: 1 folder call, which spent the budget, so
	// the row delete failed — the row stayed `complete` with 0 shards (a
	// same-content re-upload then skipped it: no parity, ever).
	f := setupParityFixture(t)
	f.cleanSightings()
	_, etag := f.object("del.bin", f.stripe+3)
	f.run()
	require.Len(t, f.shardFiles(), 4)
	leg := f.folderLeg()
	leg.slow = time.Second
	f.dropHead("del.bin")
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	// Act
	f.svc.OnObjectDeleted(ctx, f.tenantID, f.bucket, "del.bin")

	// Assert: shards and row gone, not one folder call.
	assert.Equal(t, int32(0), leg.removes.Load(), "the API path never removes a folder")
	assert.Empty(t, f.shardFiles())
	state, _, _, _ := f.row("del.bin")
	assert.Empty(t, state, "the row is gone with its shards")
	prefix := shardPrefix(f.bucket, "del.bin", etag)
	assert.True(t, exists(f.parityPath(prefix)), "the emptied folder is the reconcile's")

	// The next reconcile slot removes the folder and its digest.
	leg.slow = 0
	f.run()
	assert.False(t, exists(f.parityPath(prefix)))
	assert.False(t, exists(filepath.Dir(f.parityPath(prefix))))
}

func TestVaultParity_TheRowGoesOnceItsShardsAreGoneEvenWhenTheBudgetEndsThen(t *testing.T) {
	// Arrange: the caller's budget ends right after the fourth shard delete.
	// Before: the row delete ran on the dead context — row `complete`, 0 shards.
	f := setupParityFixture(t)
	f.object("cut.bin", f.stripe+3)
	f.run()
	leg := f.folderLeg()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	leg.cancel, leg.cancelAfter = cancel, 4
	f.dropHead("cut.bin")

	// Act
	f.svc.OnObjectDeleted(ctx, f.tenantID, f.bucket, "cut.bin")

	// Assert
	assert.Empty(t, f.shardFiles())
	state, _, _, _ := f.row("cut.bin")
	assert.Empty(t, state, "shards provably gone → the row goes, whatever the caller's budget")
}

func TestVaultParity_TheOverwritePathSendsNoFolderCall(t *testing.T) {
	// Before: the stale pass called RemoveEmptyDir on the old <etag> folder (1).
	f := setupParityFixture(t)
	_, etag := f.object("ow.bin", f.stripe+3)
	f.run()
	leg := f.folderLeg()
	f.svc.ReconcileEvery = time.Hour // this run: erase + protect only
	f.svc.reconcileMem.At = time.Now()
	f.object("ow.bin", f.stripe+9)

	res := f.run()

	require.True(t, res.ReconcileDeferred)
	assert.Equal(t, 1, res.Erased)
	assert.Equal(t, int32(0), leg.removes.Load())
	old := f.parityPath(shardPrefix(f.bucket, "ow.bin", etag))
	assert.True(t, exists(old), "left for the reconcile")

	// The reconcile, when due, removes it in the same slot.
	f.svc.ReconcileEvery = time.Nanosecond
	f.run()
	assert.False(t, exists(old))
	assert.Len(t, f.shardFiles(), 4)
}

func TestVaultParity_AnEmptyUnnamedFolderIsRemovedAtItsFirstSighting(t *testing.T) {
	// Only the job writes shards and only the job removes folders, under one
	// lock: an empty folder no row names holds nothing to wait for. Before:
	// first slot = a sighting, removal ≥ OrphanGrace (1 h) later.
	f := setupParityFixture(t)
	f.cleanSightings()
	digest := "bbbb0000bbbb0000bbbb0000"
	require.NoError(t, os.MkdirAll(f.parityPath(digest+"/e0e0e0e0e0e0e0e0e0e0e0e0e0e0e0e0"), 0o750))

	res := f.run()

	assert.False(t, exists(f.parityPath(digest)), "%+v", res)
	assert.Equal(t, 0, res.OrphansFound, "an empty folder is no orphan")
	assert.Equal(t, 0, f.orphanRows())
}

func TestVaultParity_AStartedProtectIsNeverCutForTheReconcileSlice(t *testing.T) {
	// Arrange: one protect taking 2.5 s on a 4 s run whose reconcile keeps
	// 2 s. Before: the protect was cut at 2 s — row partial, attempt spent
	// (a 66–79 GB object at Geyser's ~22 MB/s was cut every run, 20 times).
	f := setupParityFixture(t)
	f.cleanSightings()
	f.object("big.bin", f.stripe+3)
	f.eng.AddDriver("permafrost", &slowLeg{flakyLegDriver: f.leg, delay: 2500 * time.Millisecond})
	f.svc.ReconcileSlice = 2 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()

	// Act
	res, err := f.svc.RunOnce(ctx)

	// Assert
	require.NoError(t, err, "%+v", res)
	assert.Equal(t, 1, res.Protected, "%+v", res)
	state, _, _, attempts := f.row("big.bin")
	assert.Equal(t, "complete", state)
	assert.Equal(t, 1, attempts)
}

func TestVaultParity_ARunWithoutALegKeepsTheReconcileCursor(t *testing.T) {
	// Before: the result was empty — recorded, it wiped every leg's cursor.
	f := setupParityFixture(t)
	at := time.Now().Add(-10 * time.Minute).UTC().Truncate(time.Second)
	cur := reconcileCursor{After: "tenant-a", Tenant: "tenant-b", Digest: "dd"}
	f.svc.reconcileMem = reconcileState{At: at, Cursors: map[string]reconcileCursor{"permafrost": cur}}
	f.svc.Legs = []string{"sync"} // not registered here

	res, err := f.svc.RunOnce(context.Background())

	require.Error(t, err)
	assert.Equal(t, cur, res.ReconcileCursors["permafrost"])
	assert.True(t, res.ReconcileAt.Equal(at))
}

func TestVaultParity_AStrayCopyOnAnotherLegIsAnOrphanThere(t *testing.T) {
	// Arrange: the row's shards are on permafrost; the same <digest>/<etag>
	// folder also exists on a second leg (an old leg order, a manual copy).
	// Before: the folder matched the row's prefix whatever the leg — never
	// a candidate, kept forever.
	f := setupParityFixture(t)
	f.cleanSightings()
	_, etag := f.object("kept.bin", f.stripe+3)
	f.run()
	otherDir := t.TempDir()
	f.eng.AddDriver("sync", &flakyLegDriver{LocalDriver: drivers.NewLocalDriver(otherDir, zap.NewNop())})
	f.svc.Legs = []string{"permafrost", "sync"}
	prefix := shardPrefix(f.bucket, "kept.bin", etag)
	stray := filepath.Join(otherDir, parityContainer(f.tenantID), filepath.FromSlash(prefix))
	require.NoError(t, os.MkdirAll(stray, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(stray, "p0"), []byte("stray"), 0o600))

	// Act
	res := f.run()
	require.Equal(t, 1, res.OrphansFound, "%+v", res)
	f.ageOrphans(2 * time.Hour)
	res = f.run()

	// Assert: erased on sync, the row's shards on permafrost untouched.
	assert.Equal(t, 1, res.OrphansErased, "%+v", res)
	assert.False(t, exists(stray))
	assert.Len(t, f.shardFiles(), 4)
	state, _, _, _ := f.row("kept.bin")
	assert.Equal(t, "complete", state)
}
