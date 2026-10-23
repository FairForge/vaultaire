package logstore

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// RP41-4: a behind_highwater refusal reported checkpoint_size 0 — the
// high-water check returned before the database's newest checkpoint was
// recorded. The operator reading the job's result needs both sizes.
func TestCheckpoint_BehindHighwater_ReportsTheDatabaseCheckpoint(t *testing.T) {
	ctx := context.Background()
	hw, _, live := signedLog(t) // signed at 5 and 9
	r := New(restoreOf(t, live.db, 5), live.signer).WithHighwater(hw)

	res, err := r.Checkpoint(ctx)

	require.Error(t, err)
	assert.Equal(t, RefusedBehindHighwater, res.Refused)
	assert.Equal(t, int64(5), res.Size)
	assert.Equal(t, int64(5), res.CheckpointSize, "the restored database's newest checkpoint")
	assert.Equal(t, int64(9), res.HighwaterSize)
}
