package api

// RP98-5 (docs/reviews/WP-STRIPE-11-REVIEW.md): the blob scrub's three-run
// stall rule (RP96-2's mitigation, #1152) on the run's own deadline. Proven
// on origin/main first by a fork's probe asserting the wrong behaviour.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/common"
	"github.com/FairForge/vaultaire/internal/drivers"
	"github.com/FairForge/vaultaire/internal/engine"
)

// scrubSlowDriver is a backend with no ranged reads whose Get, while block
// is set, takes longer than the run: it returns the run's own deadline.
type scrubSlowDriver struct {
	engine.Driver
	block bool
	reads int
}

func (d *scrubSlowDriver) Get(ctx context.Context, container, artifact string) (io.ReadCloser, error) {
	if d.block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	d.reads++
	return d.Driver.Get(ctx, container, artifact)
}

// slowScrubFixture: one healthy blob whose row and bytes are on a backend
// without ranged reads, and a run that ends after 300 ms.
func slowScrubFixture(t *testing.T) (*blobScrubFixture, *scrubSlowDriver, func() BlobScrubResult) {
	t.Helper()
	f := setupBlobScrubFixture(t, 24*time.Hour)
	f.job.Every = 24 * time.Hour
	slow := &scrubSlowDriver{Driver: drivers.NewLocalDriver(t.TempDir(), zap.NewNop()), block: true}
	f.srv.engine.AddDriver("zzslow", slow)
	_, ranged := engine.Driver(slow).(engine.VersionedRangeGetter)
	require.False(t, ranged, "the backend has no ranged reads")
	body := testBytes(4096)
	h := f.putObject("slow.bin", body, nil)
	container, blob := objectBlobAddress(f.tenantID, f.bucket, "slow.bin", h.sk)
	require.NoError(t, slow.Driver.Put(common.WithTenantID(context.Background(), f.tenantID), container, blob, bytes.NewReader(body)))
	f.exec(`UPDATE object_head_cache SET backend_name = 'zzslow' WHERE tenant_id = $1 AND bucket = $2 AND object_key = 'slow.bin'`, f.tenantID, f.bucket)
	cutRun := func() BlobScrubResult {
		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		defer cancel()
		res, err := f.job.RunOnce(ctx)
		require.NoError(t, err)
		raw, err := json.Marshal(res)
		require.NoError(t, err)
		f.exec(`INSERT INTO job_runs (job, last_outcome, result) VALUES ($1, 'ok', $2)
			ON CONFLICT (job) DO UPDATE SET result = EXCLUDED.result, last_outcome = 'ok'`, f.job.JobName, raw)
		return res
	}
	return f, slow, cutRun
}

// A backend walked after a ranged read that used the run gets the run's last
// seconds. A read the run's end cuts before it had half a run says nothing of
// the blob: it is not counted, the cursor stays before it, and a run with the
// time reads it. It used to count as "no answer"; the third such run moved
// the walk PAST the blob with no finding and the cycle completed over a blob
// no run had read.
func TestScrub_AReadCutBeforeItsShareOfTheRunIsNotCounted(t *testing.T) {
	f, slow, cutRun := slowScrubFixture(t)
	for i := 0; i < 3; i++ {
		z := cutRun().Backends["zzslow"]
		assert.Nil(t, z.Stall, "run %d: a cut read is no run without an answer", i+1)
		assert.NotEqual(t, "slow.bin", z.Cursor.Key, "run %d: the cursor stays before the blob", i+1)
		assert.True(t, z.CycleCompletedAt.IsZero(), "run %d: the cycle is not done", i+1)
	}
	assert.Empty(t, f.findings())

	slow.block = false
	z := f.run().Backends["zzslow"]
	assert.Equal(t, 1, slow.reads, "a run with the time read the blob")
	assert.Equal(t, 1, z.Checked[scrubKindWhole])
	assert.False(t, z.CycleCompletedAt.IsZero())
	assert.Empty(t, f.findings())
}

// The third run without an answer is as a rule the run's own deadline: the
// finding is written on a detached context. On the run's dead one the row's
// re-read failed, the failure read as "the row moved", and nothing was
// written.
func TestScrub_TheThirdNoAnswerAtTheRunsEndIsStillAFinding(t *testing.T) {
	f, _, cutRun := slowScrubFixture(t)
	f.job.fairShare = 0 // every cut had its share: a blob no run can read
	first := cutRun().Backends["zzslow"]
	require.NotNil(t, first.Stall)
	assert.Equal(t, 1, first.Stall.Runs)
	cutRun()
	assert.Empty(t, f.findings())

	third := cutRun().Backends["zzslow"]
	rows := f.findings()
	require.Len(t, rows, 1, "the third run without an answer is a finding, written after the run's context ended")
	assert.Equal(t, "slow.bin", rows[0].key)
	assert.Equal(t, scrubUnreadable, rows[0].verdict)
	assert.Equal(t, 1, third.Failed)
	assert.Nil(t, third.Stall)
	assert.Equal(t, "slow.bin", third.Cursor.Key, "and the walk goes on")
	var detail string
	require.NoError(t, f.db.QueryRow(`SELECT detail FROM scrub_findings WHERE tenant_id = $1 AND object_key = 'slow.bin'`, f.tenantID).Scan(&detail))
	assert.Contains(t, detail, "no answer in 3 runs")
}
