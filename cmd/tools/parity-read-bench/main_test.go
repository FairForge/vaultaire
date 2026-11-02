package main

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPlan_DryRunGeometry(t *testing.T) {
	o := opts{runs: 3, m: 4, shardBytes: 25 << 20, stripe: 1 << 20, partBytes: 16 << 20, parts: 8,
		region: "us-east-1", cases: []string{"lockstep", "ranged"}}
	require.NoError(t, o.validate())
	assert.Equal(t, 2, o.perShard(), "D-V3-1: LARGE_GET_PARALLEL_PARTS / m in flight per shard")
	p := o.plan()
	assert.Contains(t, p, "bucket stored-us-east-1, prefix t-_bench-parity/<run>/p0..p3")
	assert.Contains(t, p, "25 stripes")
	assert.Contains(t, p, "2 ranges per shard")
	assert.True(t, strings.HasSuffix(p, "cases lockstep,ranged × 3 runs\n"))

	o.cases = []string{"nope"}
	assert.Error(t, o.validate())
	o.cases = []string{"ranged"}
	o.partBytes = 1<<20 + 1
	assert.Error(t, o.validate(), "a range must be whole stripes")
}
