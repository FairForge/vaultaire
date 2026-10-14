package engine

// Stage 1a Prompt 6, Part 0: a read or delete whose context names no
// recorded backend still asks the primary (a row written before
// backend_name), and is counted — every caller of an existing object names
// its row's backend, so an increase is a NULL row or a caller's bug.

import (
	"testing"

	promtest "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
)

func TestEngine_UnrecordedBackendCalls_AreCountedNotGuessedSilently(t *testing.T) {
	e, stores := eighteen(t)
	stores["b00"].plant("c", "k", []byte("x"))
	before := map[string]float64{}
	for _, op := range unrecordedBackendOps {
		before[op] = promtest.ToFloat64(unrecordedBackendCalls.WithLabelValues(op))
	}

	// Named: not counted.
	_, err := e.Get(WithRecordedBackend(tenantCtx(), "b00"), "c", "k")
	assert.NoError(t, err)
	// Not named: the primary, counted once per call.
	_, err = e.Get(tenantCtx(), "c", "k")
	assert.NoError(t, err)
	_, err = e.GetRange(tenantCtx(), "c", "k", 0, 1)
	assert.NoError(t, err)
	assert.NoError(t, e.Delete(tenantCtx(), "c", "k"))

	for op, want := range map[string]float64{"get": 1, "get_range": 1, "delete": 1, "parallel_get": 0} {
		assert.Equal(t, before[op]+want, promtest.ToFloat64(unrecordedBackendCalls.WithLabelValues(op)), op)
	}
}
