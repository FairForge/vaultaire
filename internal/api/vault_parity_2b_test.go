package api

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Prompt 2b Part 0 — the post-merge review of #648–#650. "Before" numbers
// are the same scenarios run against 0b376cf.

func TestVaultParity_TheReconcileRunsFirstAndNoProtectStartsThatCannotFinish(t *testing.T) {
	// Arrange: three objects whose protect takes 1.5 s each on a 3 s run
	// whose reconcile keeps 1 s, and an orphan to find. Before: the first
	// protect started inside the slice ran to the run's deadline, RunOnce
	// returned on ctx.Err() before the reconcile — ReconcileTenants=0 — and
	// the second protect, cut there, was left `partial` with an attempt spent.
	f := setupParityFixture(t)
	f.cleanSightings()
	f.plantOrphan("dddd0000dddd0000dddd0000/e2e2e2e2e2e2e2e2e2e2e2e2e2e2e2e2")
	for i := 0; i < 3; i++ {
		f.object(fmt.Sprintf("slow-%d.bin", i), f.stripe+3)
	}
	f.eng.AddDriver("permafrost", &slowLeg{flakyLegDriver: f.leg, delay: 1500 * time.Millisecond})
	f.svc.ReconcileSlice = time.Second
	f.svc.ProtectOverhead = 1500 * time.Millisecond // the size estimate of one of these protects
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	// Act
	res, err := f.svc.RunOnce(ctx)

	// Assert: the reconcile ran, one protect completed, none was started
	// that could not finish — nothing is left partial.
	require.NoError(t, err, "%+v", res)
	assert.Equal(t, 1, res.ReconcileTenants, "%+v", res)
	assert.Equal(t, 1, res.OrphansFound, "%+v", res)
	assert.Equal(t, 1, res.Protected, "%+v", res)
	assert.Equal(t, 0, res.Partial, "%+v", res)
	assert.True(t, res.WorkStopped, "%+v", res)
	for i := 0; i < 3; i++ {
		state, _, _, attempts := f.row(fmt.Sprintf("slow-%d.bin", i))
		assert.NotEqual(t, "partial", state, "slow-%d.bin: attempts %d", i, attempts)
	}
}

func TestVaultParity_AProtectLargerThanAWholeRunIsNamedNotStarted(t *testing.T) {
	// A protect whose estimate exceeds the whole run would be cut on every
	// run: it is never started, and the run says why.
	f := setupParityFixture(t)
	f.object("huge.bin", f.stripe+3)
	f.svc.MaxRunTime = time.Second
	f.svc.ProtectOverhead = 2 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	res, err := f.svc.RunOnce(ctx)

	require.NoError(t, err, "%+v", res)
	assert.Equal(t, 0, res.Protected)
	assert.Equal(t, 1, res.TooLarge, "%+v", res)
	state, _, _, _ := f.row("huge.bin")
	assert.Equal(t, "", state, "never started, no row, no attempt spent")
}

// failDeletesAfterLeg answers the first `after` shard deletes, then fails
// every one like a Sync 502 — a delete stopped mid-shards.
type failDeletesAfterLeg struct {
	*flakyLegDriver
	after   int32
	deletes atomic.Int32
}

func (d *failDeletesAfterLeg) Delete(ctx context.Context, container, artifact string) error {
	if d.deletes.Add(1) > d.after {
		return errors.New("sync: 502 bad gateway")
	}
	return d.flakyLegDriver.Delete(ctx, container, artifact)
}

func (f *parityFixture) headRow(key, etag string, size int) {
	f.t.Helper()
	_, err := f.db.Exec(`
		INSERT INTO object_head_cache (tenant_id, bucket, object_key, size_bytes, etag, backend_name, floor, is_chunked, content_type)
		VALUES ($1,$2,$3,$4,$5,'geyser','vault',false,'application/octet-stream')`, f.tenantID, f.bucket, key, size, etag)
	require.NoError(f.t, err)
}

func TestVaultParity_ADeleteStoppedMidShardsNeverLeavesACompleteRow(t *testing.T) {
	// Arrange: a protected object; its delete removes p0 and p1, then the leg
	// answers 502. A same-content re-upload follows before the next stale
	// pass. Before: the row stayed `complete` naming four shards with two on
	// disk; the re-upload has the same etag, so the stale pass ignored it and
	// protect skipped it (`complete`) — incomplete parity for good.
	f := setupParityFixture(t)
	size := f.stripe + 3
	_, etag := f.object("half.bin", size)
	f.run()
	require.Len(t, f.shardFiles(), 4)
	f.eng.AddDriver("permafrost", &failDeletesAfterLeg{flakyLegDriver: f.leg, after: 2})
	f.dropHead("half.bin")

	// Act: the delete, cut mid-shards; the same bytes again; three runs.
	f.svc.OnObjectDeleted(context.Background(), f.tenantID, f.bucket, "half.bin")
	state, _, _, _ := f.row("half.bin")
	assert.NotEqual(t, "complete", state, "an interrupted delete is never `complete`")
	f.headRow("half.bin", etag, size)
	f.eng.AddDriver("permafrost", f.leg)
	for i := 0; i < 3; i++ {
		f.run()
	}

	// Assert: the re-upload is protected again — four shards, row complete.
	state, _, _, _ = f.row("half.bin")
	assert.Equal(t, "complete", state)
	assert.Len(t, f.shardFiles(), 4)
}
