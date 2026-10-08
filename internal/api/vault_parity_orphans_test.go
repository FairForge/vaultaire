package api

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Prompt 2a, PR 5: three paths leave parity shards no row names (a row gone
// during the write, a failed deleteShard, a failed clearPriorShards) and the
// stale pass works from rows, so it never found them. The job now
// reconciles: it walks `<tenant>__parity/` on every registered leg, a
// `<digest>/<etag>/` folder no row names is a candidate, and a candidate is
// erased only once it has been one on two passes at least OrphanGrace apart
// (a row is inserted before the first shard byte, so a real write always
// has its row). The empty folders the driver's Delete leaves go too.

// plantOrphan writes four shard files under a folder no row names.
func (f *parityFixture) plantOrphan(prefix string) string {
	f.t.Helper()
	dir := filepath.Join(f.legDir, parityContainer(f.tenantID), filepath.FromSlash(prefix))
	require.NoError(f.t, os.MkdirAll(dir, 0o750))
	for j := 0; j < 4; j++ {
		require.NoError(f.t, os.WriteFile(filepath.Join(dir, "p"+string(rune('0'+j))), []byte("orphan"), 0o600))
	}
	return dir
}

func (f *parityFixture) orphanRows() int {
	var n int
	require.NoError(f.t, f.db.QueryRow(`SELECT count(*) FROM vault_parity_orphans WHERE tenant_id = $1`, f.tenantID).Scan(&n))
	return n
}

func (f *parityFixture) ageOrphans(by time.Duration) {
	_, err := f.db.Exec(`UPDATE vault_parity_orphans SET first_seen = first_seen - $2::interval WHERE tenant_id = $1`,
		f.tenantID, by.String())
	require.NoError(f.t, err)
}

func TestVaultParity_OrphanShardsAreErasedOnTheSecondPassAfterTheGrace(t *testing.T) {
	// Arrange: one protected object (row + shards) and one orphan folder.
	f := setupParityFixture(t)
	f.object("kept.bin", 2*f.stripe+5)
	first := f.run()
	require.Equal(t, 1, first.Protected)
	realShards := f.shardFiles()
	require.Len(t, realShards, 4)
	orphanDir := f.plantOrphan("0123456789abcdef01234567/deadbeefdeadbeefdeadbeefdeadbeef")
	t.Cleanup(func() { _, _ = f.db.Exec(`DELETE FROM vault_parity_orphans WHERE tenant_id = $1`, f.tenantID) })

	// Act 1: first sighting — nothing is deleted.
	res := f.run()
	assert.Equal(t, 1, res.OrphansFound, "%+v", res)
	assert.Equal(t, 0, res.OrphansErased)
	assert.Len(t, f.shardFiles(), 8, "first pass never deletes")
	assert.Equal(t, 1, f.orphanRows())

	// Act 2: a second pass inside the grace — still nothing.
	res = f.run()
	assert.Equal(t, 1, res.OrphansFound)
	assert.Equal(t, 0, res.OrphansErased)
	assert.Len(t, f.shardFiles(), 8)

	// Act 3: the grace has passed.
	f.ageOrphans(2 * time.Hour)
	res = f.run()

	// Assert: the orphan's shards and its folders are gone, the real row's shards untouched.
	assert.Equal(t, 1, res.OrphansErased, "%+v", res)
	assert.ElementsMatch(t, realShards, f.shardFiles())
	_, statErr := os.Stat(orphanDir)
	assert.True(t, os.IsNotExist(statErr), "the <etag> folder is removed")
	_, statErr = os.Stat(filepath.Dir(orphanDir))
	assert.True(t, os.IsNotExist(statErr), "the empty <digest> folder is removed")
	assert.Equal(t, 0, f.orphanRows(), "the sighting is forgotten once erased")
	state, _, _, _ := f.row("kept.bin")
	assert.Equal(t, "complete", state)

	// And a fourth pass finds nothing.
	res = f.run()
	assert.Equal(t, 0, res.OrphansFound)
	assert.Equal(t, 0, res.OrphansErased)
}

func TestVaultParity_ACandidateThatGainsItsRowIsForgotten(t *testing.T) {
	// A folder seen once without a row (a write in flight whose row this
	// pass did not see, or a stale listing) stops being a candidate when the
	// row is there: its sighting is dropped, nothing erased.
	f := setupParityFixture(t)
	t.Cleanup(func() { _, _ = f.db.Exec(`DELETE FROM vault_parity_orphans WHERE tenant_id = $1`, f.tenantID) })
	_, etag := f.object("late.bin", f.stripe+1)
	prefix := shardPrefix(f.bucket, "late.bin", etag)
	f.plantOrphan(prefix)      // the shards land before the row (never in the real flow, but a stale view looks like this)
	f.svc.MaxObjectsPerRun = 0 // the protect pass writes nothing this run
	res := f.run()
	assert.Equal(t, 1, res.OrphansFound)
	f.ageOrphans(2 * time.Hour)

	// The row appears (the protect pass runs): the folder is now named by a row.
	f.svc.MaxObjectsPerRun = 500
	res = f.run()
	assert.Equal(t, 0, res.OrphansErased)
	assert.Equal(t, 0, f.orphanRows())
	state, _, _, _ := f.row("late.bin")
	assert.Equal(t, "complete", state)
}

func TestVaultParity_ReconcileSkipsALegThatIsNotRegisteredAndNeverFailsTheRun(t *testing.T) {
	f := setupParityFixture(t)
	f.svc.Legs = []string{"sync", "permafrost", "lyve"} // sync and lyve are not registered here
	res, err := f.svc.RunOnce(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 0, res.OrphansFound)
	assert.Contains(t, res.ReconciledLegs, "permafrost")
	assert.NotContains(t, res.ReconciledLegs, "sync")
}

func TestVaultParity_ReconcileWalkIsBoundedPerRun(t *testing.T) {
	// Many orphan folders, a small per-run bound: the pass stops at the bound
	// and the rest is seen on a later run, never a run that walks forever.
	f := setupParityFixture(t)
	t.Cleanup(func() { _, _ = f.db.Exec(`DELETE FROM vault_parity_orphans WHERE tenant_id = $1`, f.tenantID) })
	for i := 0; i < 6; i++ {
		f.plantOrphan("aaaaaaaaaaaaaaaaaaaaaaaa/" + string(rune('a'+i)) + "0000000000000000000000000000000")
	}
	f.svc.MaxOrphanCandidatesPerRun = 4
	res := f.run()
	assert.Equal(t, 4, res.OrphansFound)
	assert.True(t, res.ReconcileTruncated)
}
