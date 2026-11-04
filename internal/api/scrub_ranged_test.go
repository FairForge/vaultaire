package api

// Stage 1a Prompt 59 Part 0, RP96-2 (the proper fix): a blob the run cannot
// read in one piece is read as RANGES across runs. The walk's state carries
// the offset reached, the MD5 state (hash.Hash is an encoding.BinaryMarshaler)
// and the backend ETag the first range answered; every later range must
// answer the same ETag, else the blob is read again from 0 (`changed` when
// the row moved). A backend with no ranged reads keeps the stall rule.

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// resumeOf is the backend's saved range state in job_runs.result.
func (f *blobScrubFixture) resumeOf(backend string) *scrubResume {
	f.t.Helper()
	var raw []byte
	require.NoError(f.t, f.db.QueryRow(`SELECT result FROM job_runs WHERE job = $1`, f.job.JobName).Scan(&raw))
	var res BlobScrubResult
	require.NoError(f.t, json.Unmarshal(raw, &res))
	st := res.Backends[backend]
	require.NotNil(f.t, st)
	return st.Resume
}

func TestScrub_ABlobLargerThanARunIsReadAcrossRuns(t *testing.T) {
	f := setupBlobScrubFixture(t, 24*time.Hour)
	f.job.Every = 24 * time.Hour // the budget is the whole footprint: one blob a run
	f.job.rangePart = 1024
	f.job.outOfTime = func() bool { return true } // each run has time for one piece
	f.putObject("big.bin", testBytes(3*1024), nil)
	before := f.drv.ranges.Load()

	first := f.run().Backends["edgefake"]
	assert.Zero(t, first.Checked[scrubKindWhole], "one piece of three: no verdict yet")
	r := f.resumeOf("edgefake")
	require.NotNil(t, r, "the range state is in job_runs.result")
	assert.Equal(t, int64(1024), r.Offset)
	assert.Equal(t, "big.bin", r.Walk.Key)
	assert.NotEmpty(t, r.ETag)
	assert.NotEmpty(t, r.MD5)

	second := f.run().Backends["edgefake"]
	assert.Zero(t, second.Checked[scrubKindWhole])
	assert.Equal(t, int64(2048), f.resumeOf("edgefake").Offset)

	third := f.run().Backends["edgefake"]
	assert.Equal(t, 1, third.Checked[scrubKindWhole], "one ok verdict")
	assert.Zero(t, first.Failed+second.Failed+third.Failed)
	assert.Empty(t, f.findings())
	assert.Nil(t, f.resumeOf("edgefake"), "the state is dropped with the verdict")
	assert.Equal(t, int32(3), f.drv.ranges.Load()-before, "three ranged reads")
	assert.Equal(t, int64(3*1024), first.Bytes+second.Bytes+third.Bytes, "every byte read once")
}

func TestScrub_ARangedReadWhoseETagMovedStartsAgainFromZero(t *testing.T) {
	f := setupBlobScrubFixture(t, 24*time.Hour)
	f.job.Every = 24 * time.Hour
	f.job.rangePart = 1024
	f.job.outOfTime = func() bool { return true }
	f.putObject("big.bin", testBytes(3*1024), nil)
	f.run()
	require.Equal(t, int64(1024), f.resumeOf("edgefake").Offset)
	pinned := f.resumeOf("edgefake").ETag

	// The stored bytes change under an unchanged row (a rot the backend
	// re-tags): the next range answers another ETag — read again from 0.
	f.drv.mu.Lock()
	for k, b := range f.drv.objects {
		if len(b) == 3*1024 {
			c := append([]byte(nil), b...)
			c[2000] ^= 0xff
			f.drv.objects[k] = c
		}
	}
	f.drv.mu.Unlock()
	f.run()
	r := f.resumeOf("edgefake")
	require.NotNil(t, r)
	assert.Equal(t, int64(1024), r.Offset, "started again: one piece from 0")
	assert.NotEqual(t, pinned, r.ETag)
	f.run()
	res := f.run().Backends["edgefake"]
	assert.Equal(t, 1, res.Failed, "the changed bytes are a mismatch with the row")
	rows := f.findings()
	require.Len(t, rows, 1)
	assert.Equal(t, scrubMismatch, rows[0].verdict)

	// The row moved instead (an overwrite mid-read): `changed`, no finding.
	f.exec(`DELETE FROM scrub_findings WHERE tenant_id = $1`, f.tenantID)
	f.job.now = func() time.Time { return time.Now().Add(25 * time.Hour) }
	f.putObject("other.bin", testBytes(3*1024), nil)
	f.run() // big.bin again (new cycle): one piece
	f.putObject("big.bin", testBytes(3*1024+7), nil)
	for range 4 {
		f.run()
	}
	assert.Empty(t, f.findings(), "an overwrite during the ranged read is no finding")
}
