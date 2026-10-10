package api

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Prompt 2b.3 D2.2 — the delete-vs-protect race in the mirror ordering.
// #660 guarded finishRow with the upsert's updated_at, which catches a
// delete that marks the row AFTER the protect's upsert. deleteRecordedShards
// marked the row once, BEFORE its first delete: a protect that upserted after
// that mark wrote fresh shards and completed the row, and the resumed delete
// then removed p0/p1 and stopped on a 502. Before (9bc9d1e): run 1 left the
// row `complete` naming four shards with [p2 p3] on disk; run 2 changed
// nothing — a same-content object never re-protected.

// hookedDeleteLeg runs hook at its first Delete (after the row's mark), then
// fails every delete after `after`.
type hookedDeleteLeg struct {
	*flakyLegDriver
	hook    func()
	after   int32
	deletes atomic.Int32
}

func (d *hookedDeleteLeg) Delete(ctx context.Context, container, artifact string) error {
	if h := d.hook; h != nil {
		d.hook = nil
		h()
	}
	if d.deletes.Add(1) > d.after {
		return errors.New("sync: 502 bad gateway")
	}
	return d.flakyLegDriver.Delete(ctx, container, artifact)
}

func TestVaultParity_AProtectThatRunsInsideAStoppedDeleteIsReprotected(t *testing.T) {
	// Arrange: a protected object, deleted; while its delete is between the
	// mark and the first shard delete, the same bytes are re-uploaded and a
	// whole protect runs (upsert, four fresh shards, finish).
	f := setupParityFixture(t)
	size := f.stripe + 3
	_, etag := f.object("mirror.bin", size)
	f.run()
	require.Len(t, f.shardFiles(), 4)
	f.dropHead("mirror.bin")
	leg := &hookedDeleteLeg{flakyLegDriver: f.leg, after: 2}
	leg.hook = func() {
		f.headRow("mirror.bin", etag, size)
		f.run() // its puts go through the wrapper's leg; it deletes nothing
		st, _, _, _ := f.row("mirror.bin")
		require.Equal(t, "complete", st, "the protect inside the delete completed")
		require.Len(t, f.shardFiles(), 4)
	}
	f.eng.AddDriver("permafrost", leg)

	// Act: the delete resumes — p0, p1 deleted, then a 502.
	f.svc.OnObjectDeleted(context.Background(), f.tenantID, f.bucket, "mirror.bin")

	st0, _, le0, _ := f.row("mirror.bin")
	t.Logf("after delete: state=%q shards=%v err=%q deletes=%d", st0, f.shardFiles(), le0, leg.deletes.Load())
	// Assert: the row never claims four shards that are not there …
	state, _, _, _ := f.row("mirror.bin")
	if state == "complete" {
		assert.Len(t, f.shardFiles(), 4, "a complete row names four shards that exist")
	}
	assert.NotEqual(t, "complete", state, "the delete stopped after the protect finished: the row is flipped to partial")

	// … and the next run protects the object again.
	f.eng.AddDriver("permafrost", f.leg)
	f.run()
	state, _, _, _ = f.row("mirror.bin")
	assert.Equal(t, "complete", state)
	assert.Len(t, f.shardFiles(), 4)
}

// D2.3: a tenant whose flag is off is flag_off in the unprotected gauge, not
// "other"; the too-large objects are notes, never "failed" items. Before
// (9bc9d1e): other = 1 for the flag-off object; res.Errors held the
// "not started" line on every run (the admin page: "1 item(s) failed").
func TestVaultParity_UnprotectedReasonsAndTooLargeNotes(t *testing.T) {
	f := setupParityFixture(t)
	f.svc.MaxRunTime = time.Second
	f.svc.ProtectRate = 4 * int64(f.stripe)
	f.object("huge.bin", 8*f.stripe)

	res := f.run()

	assert.Equal(t, 1, res.TooLarge)
	assert.Empty(t, res.Errors, "a standing condition is not a failure")
	require.Len(t, res.Notes, 1)
	assert.Contains(t, res.Notes[0], "huge.bin")
	assert.Equal(t, float64(0), testutil.ToFloat64(vaultParityUnprotectedObjects.WithLabelValues("flag_off")))

	f.svc.flags = stubFlags{on: map[string]bool{flagVaultParity + "/" + f.tenantID: false}}
	res = f.run()
	assert.Equal(t, float64(1), testutil.ToFloat64(vaultParityUnprotectedObjects.WithLabelValues("flag_off")))
	assert.Equal(t, float64(0), testutil.ToFloat64(vaultParityUnprotectedObjects.WithLabelValues("too_large")))
	assert.Equal(t, float64(0), testutil.ToFloat64(vaultParityUnprotectedObjects.WithLabelValues("other")))
	assert.Empty(t, res.Notes)
}
