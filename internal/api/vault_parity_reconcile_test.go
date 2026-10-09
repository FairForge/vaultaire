package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/FairForge/vaultaire/internal/drivers"
	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Prompt 2a.2 G3 — the reconcile pass after the post-merge review of #638 /
// #640. "Before" numbers are the same scenarios run against 6be8e98.

// countingLeg counts the listings the reconcile spends on the leg.
type countingLeg struct {
	*flakyLegDriver
	mu       sync.Mutex
	lists    int
	listDirs []string
}

func (d *countingLeg) List(ctx context.Context, c, p string) ([]string, error) {
	d.mu.Lock()
	d.lists++
	d.mu.Unlock()
	return d.flakyLegDriver.List(ctx, c, p)
}

func (d *countingLeg) ListDir(ctx context.Context, c, dir string) ([]string, []string, error) {
	d.mu.Lock()
	d.listDirs = append(d.listDirs, dir)
	d.mu.Unlock()
	return d.flakyLegDriver.ListDir(ctx, c, dir)
}

func (d *countingLeg) listings() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.lists + len(d.listDirs)
}

func (f *parityFixture) cleanSightings() {
	f.t.Cleanup(func() { _, _ = f.db.Exec(`DELETE FROM vault_parity_orphans WHERE tenant_id = $1`, f.tenantID) })
}

func (f *parityFixture) parityPath(rel string) string {
	return filepath.Join(f.legDir, parityContainer(f.tenantID), filepath.FromSlash(rel))
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func TestVaultParity_AnEmptySiblingFolderNoLongerPinsASighting(t *testing.T) {
	// Arrange: an orphan <digest>/<e1>/ with shards and an empty sibling
	// <digest>/<e0>/ (an overwrite's old etag folder). Before: the digest
	// was "not empty" on every pass — after 5 passes past the grace the
	// sighting was still there (1) and the digest folder too.
	f := setupParityFixture(t)
	f.cleanSightings()
	digest := "aaaa0000aaaa0000aaaa0000"
	f.plantOrphan(digest + "/e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1")
	require.NoError(t, os.MkdirAll(f.parityPath(digest+"/e0e0e0e0e0e0e0e0e0e0e0e0e0e0e0e0"), 0o750))
	f.run()
	f.ageOrphans(2 * time.Hour)

	// Act: two reconcile slots past the grace.
	res := f.run()
	assert.Equal(t, 1, res.OrphansErased, "%+v", res)
	f.run()

	// Assert
	assert.False(t, exists(f.parityPath(digest)), "the digest folder is gone")
	assert.Equal(t, 0, f.orphanRows())
}

func TestVaultParity_ADeleteRemovesTheShardFolderItEmptied(t *testing.T) {
	// Before: the <etag> folder stayed after DeleteObject (exists = true).
	f := setupParityFixture(t)
	_, etag := f.object("del.bin", f.stripe+3)
	f.run()
	prefix := shardPrefix(f.bucket, "del.bin", etag)
	require.True(t, exists(f.parityPath(prefix)))
	_, err := f.db.Exec(`DELETE FROM object_head_cache WHERE tenant_id = $1 AND object_key = 'del.bin'`, f.tenantID)
	require.NoError(t, err)

	f.svc.OnObjectDeleted(context.Background(), f.tenantID, f.bucket, "del.bin")

	assert.False(t, exists(f.parityPath(prefix)), "the emptied <etag> folder goes with its shards")
}

func TestVaultParity_AnOverwriteRemovesTheOldEtagFolder(t *testing.T) {
	f := setupParityFixture(t)
	_, etag := f.object("ow.bin", f.stripe+3)
	f.run()
	old := f.parityPath(shardPrefix(f.bucket, "ow.bin", etag))
	require.True(t, exists(old))
	f.object("ow.bin", f.stripe+9) // new bytes, new etag
	f.run()
	assert.False(t, exists(old), "the stale pass removes the folder it emptied")
	assert.Len(t, f.shardFiles(), 4)
}

func TestVaultParity_ReconcileRunsAtMostEveryInterval(t *testing.T) {
	// Before: every 2-minute run walked every tenant's tree (2 runs = 2 walks).
	f := setupParityFixture(t)
	cl := &countingLeg{flakyLegDriver: f.leg}
	f.eng.AddDriver("permafrost", cl)
	f.svc.ReconcileEvery = 30 * time.Minute
	now := time.Now()
	f.svc.now = func() time.Time { return now }

	first := f.run()
	n := cl.listings()
	second := f.run()

	assert.False(t, first.ReconcileDeferred)
	assert.Positive(t, n)
	assert.True(t, second.ReconcileDeferred)
	assert.Equal(t, n, cl.listings(), "no listing inside the interval")
	assert.Equal(t, first.ReconcileAt, second.ReconcileAt, "the last reconcile time is carried in the result")

	now = now.Add(31 * time.Minute)
	third := f.run()
	assert.False(t, third.ReconcileDeferred)
	assert.Greater(t, cl.listings(), n)
}

func TestVaultParity_ReconcileListingsAreBoundedAndResumeFromJobRuns(t *testing.T) {
	// Arrange: six digests, a budget of four listings per run. Before: the
	// walk had no listing bound and its cursor lived in memory — a new
	// process started every walk over.
	f := setupParityFixture(t)
	f.cleanSightings()
	t.Cleanup(func() { _, _ = f.db.Exec(`DELETE FROM job_runs WHERE job = $1`, f.svc.JobName) })
	for i := 0; i < 6; i++ {
		f.plantOrphan(strings.Repeat(string(rune('a'+i)), 24) + "/e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1")
	}
	cl := &countingLeg{flakyLegDriver: f.leg}
	f.eng.AddDriver("permafrost", cl)
	f.svc.MaxReconcileListingsPerRun = 4 // the container, then one digest + its etag folder

	// Act 1
	res := f.run()
	assert.Equal(t, 4, cl.listings())
	assert.Equal(t, "listing budget", res.ReconcileStopped)
	assert.Equal(t, 1, res.OrphansFound)
	cur := res.ReconcileCursors["permafrost"]
	assert.Equal(t, f.tenantID, cur.Tenant)
	assert.Equal(t, strings.Repeat("a", 24), cur.Digest)

	// The scheduler records the result; a new process (fresh VaultParity) resumes from it.
	raw, err := json.Marshal(res)
	require.NoError(t, err)
	recordJobFinish(f.db, f.svc.JobName, time.Now(), jobOutcomeOK, "", 0, raw)
	fresh := NewVaultParity(f.db, f.eng, f.flags, f.logger)
	fresh.scopeTenant, fresh.JobName, fresh.Stripe = f.tenantID, f.svc.JobName, f.stripe
	fresh.ReconcileEvery, fresh.MaxReconcileListingsPerRun = time.Nanosecond, 4
	cl.mu.Lock()
	cl.listDirs = nil
	cl.mu.Unlock()

	// Act 2
	res2, err := fresh.RunOnce(context.Background())
	require.NoError(t, err)

	// Assert: it started at the second digest, never the first again.
	cl.mu.Lock()
	dirs := append([]string(nil), cl.listDirs...)
	cl.mu.Unlock()
	assert.NotContains(t, strings.Join(dirs, ","), strings.Repeat("a", 24))
	assert.Contains(t, dirs, strings.Repeat("b", 24))
	assert.Equal(t, 1, res2.OrphansFound)

	// A run that finishes the tenant wraps the cursor.
	fresh.MaxReconcileListingsPerRun = 100
	res3, err := fresh.RunOnce(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 4, res3.OrphansFound)
	assert.Empty(t, res3.ReconcileStopped)
	assert.Equal(t, reconcileCursor{}, res3.ReconcileCursors["permafrost"])
	assert.Equal(t, 6, f.orphanRows())
}

// slowLeg takes `delay` per shard Put (a slow leg under a protect backlog).
type slowLeg struct {
	*flakyLegDriver
	delay time.Duration
}

func (d *slowLeg) Put(ctx context.Context, container, artifact string, data io.Reader, opts ...engine.PutOption) error {
	select {
	case <-time.After(d.delay):
	case <-ctx.Done():
		return ctx.Err()
	}
	return d.flakyLegDriver.Put(ctx, container, artifact, data, opts...)
}

func TestVaultParity_ReconcileRunsWhenProtectUsesTheRun(t *testing.T) {
	// Arrange: a protect backlog longer than the run (20 objects × 4 shards
	// × 100 ms on a 3 s run) and an orphan to find. Before: the protect pass
	// ran to the run's deadline and RunOnce returned — the reconcile walked
	// 0 tenants, on every run while the backlog lasted.
	f := setupParityFixture(t)
	f.cleanSightings()
	f.plantOrphan("cccc0000cccc0000cccc0000/e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1")
	for i := 0; i < 20; i++ {
		f.object(fmt.Sprintf("backlog-%02d.bin", i), f.stripe+3)
	}
	f.eng.AddDriver("permafrost", &slowLeg{flakyLegDriver: f.leg, delay: 100 * time.Millisecond})
	f.svc.ReconcileSlice = time.Second
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	// Act
	res, err := f.svc.RunOnce(ctx)

	// Assert: the run is not a failure, the protect pass stopped at its
	// share, the reconcile had its slice.
	require.NoError(t, err)
	assert.True(t, res.WorkStopped, "%+v", res)
	assert.Equal(t, 1, res.ReconcileTenants)
	assert.Equal(t, 1, res.OrphansFound)
}

// directOnlyLeg lists like the OneDrive fleet: List = the container's
// direct children only.
type directOnlyLeg struct{ *flakyLegDriver }

func (d *directOnlyLeg) List(ctx context.Context, c, p string) ([]string, error) {
	all, err := d.flakyLegDriver.List(ctx, c, p)
	seen := map[string]bool{}
	var out []string
	for _, n := range all {
		n, _, _ = strings.Cut(n, "/")
		if !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	return out, err
}

func TestVaultParity_ReconcileWalksALegWhoseListIsOneLevel(t *testing.T) {
	// Before: on permafrost (List = direct children) the walk found 0 orphans
	// and still reported the leg reconciled.
	f := setupParityFixture(t)
	f.cleanSightings()
	f.eng.AddDriver("permafrost", &directOnlyLeg{flakyLegDriver: f.leg})
	f.plantOrphan("dddd0000dddd0000dddd0000/e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1")
	res := f.run()
	assert.Equal(t, 1, res.OrphansFound)
	assert.Contains(t, res.ReconciledLegs, "permafrost")
}

func TestVaultParity_FoundCountsNewSightingsOnly(t *testing.T) {
	// Before: one orphan seen on 3 passes inside the grace counted found 3 times.
	f := setupParityFixture(t)
	f.cleanSightings()
	f.plantOrphan("eeee0000eeee0000eeee0000/e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1")
	before := testutil.ToFloat64(vaultParityOrphans.WithLabelValues("found"))
	f.run()
	f.run()
	f.run()
	assert.Equal(t, 1.0, testutil.ToFloat64(vaultParityOrphans.WithLabelValues("found"))-before)
}

func TestVaultParity_ErasedIsNotCountedAgainForFilesAlreadyErased(t *testing.T) {
	// Arrange: the files are erased, the folder removal refused (a bridge
	// still lists them), and the next listing still shows the files (a
	// lagging listing). Before: counted erased twice (2).
	f := setupParityFixture(t)
	f.cleanSightings()
	sticky := &notEmptyLeg{flakyLegDriver: f.leg}
	f.eng.AddDriver("permafrost", sticky)
	prefix := "ffff0000ffff0000ffff0000/e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1"
	f.plantOrphan(prefix)
	f.run()
	f.ageOrphans(2 * time.Hour)
	sticky.refuse = true
	before := testutil.ToFloat64(vaultParityOrphans.WithLabelValues("erased"))
	first := f.run()
	require.Equal(t, 1, first.OrphansErased)
	f.plantOrphan(prefix) // the lagging view: the same files again

	// Act
	second := f.run()
	sticky.refuse = false
	third := f.run()

	// Assert
	assert.Equal(t, 0, second.OrphansErased)
	assert.Equal(t, 1.0, testutil.ToFloat64(vaultParityOrphans.WithLabelValues("erased"))-before)
	assert.False(t, exists(f.parityPath(prefix)))
	assert.Equal(t, 0, f.orphanRows(), "%+v", third)
}

// notEmptyLeg refuses every folder removal while refuse is set, as a Sync
// bridge that still lists deleted shards does.
type notEmptyLeg struct {
	*flakyLegDriver
	refuse bool
}

func (d *notEmptyLeg) RemoveEmptyDir(ctx context.Context, container, dir string) error {
	if d.refuse {
		return drivers.ErrDirNotEmpty
	}
	return d.LocalDriver.RemoveEmptyDir(ctx, container, dir)
}

func TestVaultParity_SkippedLegsAreNamed(t *testing.T) {
	// Before: an unregistered leg was skipped silently (no note).
	f := setupParityFixture(t)
	f.svc.Legs = []string{"sync", "permafrost", "lyve"}
	res := f.run()
	assert.Equal(t, []string{"sync: not registered", "lyve: not registered"}, res.SkippedLegs)
}

func TestVaultParity_SightingsOfAnErasedTenantOrAGoneLegArePruned(t *testing.T) {
	// Before: nothing removed them (a sighting inserted after EraseRows, or
	// on a leg no longer registered, stayed forever).
	f := setupParityFixture(t)
	f.cleanSightings()
	old := time.Now().Add(-8 * 24 * time.Hour)
	_, err := f.db.Exec(`INSERT INTO vault_parity_orphans (tenant_id, leg, prefix, first_seen, last_seen) VALUES
		($1, 'gone-leg', 'a/b', $2, $2), ($1, 'gone-leg', 'a/fresh', NOW(), NOW()), ($1, 'permafrost', 'zz/old', $2, $2)`,
		f.tenantID, old)
	require.NoError(t, err)
	res := f.run()
	assert.Equal(t, 1, res.SightingsPruned, "only the gone leg's week-old sighting")

	erased := uuid.New().String() // a tenant the deletion runner has erased
	t.Cleanup(func() { _, _ = f.db.Exec(`DELETE FROM vault_parity_orphans WHERE tenant_id = $1`, erased) })
	_, err = f.db.Exec(`INSERT INTO vault_parity_orphans (tenant_id, leg, prefix, first_seen, last_seen) VALUES ($1, 'permafrost', 'a/b', $2, $2)`, erased, old)
	require.NoError(t, err)
	f.svc.scopeTenant = erased
	res = f.run()
	assert.Equal(t, 1, res.SightingsPruned)
	var n int
	require.NoError(t, f.db.QueryRow(`SELECT count(*) FROM vault_parity_orphans WHERE tenant_id = $1`, erased).Scan(&n))
	assert.Equal(t, 0, n)
}

func TestVaultParity_ReconcileNeverTouchesARowsFolder(t *testing.T) {
	// The real row's folder: never listed (it is named before the walk),
	// never a candidate, its digest never removed — with an orphan and an
	// empty sibling under the same digest erased around it.
	f := setupParityFixture(t)
	f.cleanSightings()
	_, etag := f.object("real.bin", 2*f.stripe+1)
	f.run()
	real := shardPrefix(f.bucket, "real.bin", etag)
	digest, _, _ := strings.Cut(real, "/")
	f.plantOrphan(digest + "/e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1")
	require.NoError(t, os.MkdirAll(f.parityPath(digest+"/e0e0e0e0e0e0e0e0e0e0e0e0e0e0e0e0"), 0o750))
	cl := &countingLeg{flakyLegDriver: f.leg}
	f.eng.AddDriver("permafrost", cl)
	f.run()
	f.ageOrphans(2 * time.Hour)
	f.run()
	f.run()

	files := f.shardFiles()
	sort.Strings(files)
	require.Len(t, files, 4)
	for _, p := range files {
		assert.True(t, strings.HasPrefix(filepath.ToSlash(p), real+"/"), p)
	}
	assert.False(t, exists(f.parityPath(digest+"/e0e0e0e0e0e0e0e0e0e0e0e0e0e0e0e0")))
	assert.NotContains(t, cl.listDirs, real)
	assert.Equal(t, 0, f.orphanRows())
	state, _, _, _ := f.row("real.bin")
	assert.Equal(t, "complete", state)
}
