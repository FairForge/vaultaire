package engine

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

type rangeInfoDriver struct{ stubDriver }

func (d *rangeInfoDriver) GetRangeInfo(_ context.Context, container, artifact string, offset, length int64) (io.ReadCloser, RangeInfo, error) {
	b := d.data[container+"/"+artifact]
	return io.NopCloser(bytes.NewReader(b[offset : offset+length])), RangeInfo{ETag: "e", Size: int64(len(b))}, nil
}

func TestParallelRangeSource_ResolvesTheRecordedBackend(t *testing.T) {
	// Arrange: the primary has no ranged reads; the object is hinted to a
	// backend that has.
	eng := NewEngine(nil, zap.NewNop(), &Config{DefaultBackend: "plain"})
	eng.AddDriver("plain", &stubDriver{name: "plain", data: map[string][]byte{}, healthy: true})
	eng.AddDriver("ranged", &rangeInfoDriver{stubDriver{name: "ranged", data: map[string][]byte{"c/a": []byte("hello")}, healthy: true}})

	// Act + Assert: not hinted → the primary → no parallel source.
	_, _, ok := eng.ParallelRangeSource(context.Background(), "c", "a")
	assert.False(t, ok, "the primary cannot do versioned ranges")

	eng.HintBackend("c", "a", "ranged")
	name, rg, ok := eng.ParallelRangeSource(context.Background(), "c", "a")
	require.True(t, ok)
	assert.Equal(t, "ranged", name)
	rc, info, err := rg.GetRangeInfo(context.Background(), "c", "a", 1, 3)
	require.NoError(t, err)
	b, _ := io.ReadAll(rc)
	assert.Equal(t, "ell", string(b))
	assert.Equal(t, int64(5), info.Size)
}

func TestParallelRangeSource_NotWhileTheBreakerIsOpen(t *testing.T) {
	eng := NewEngine(nil, zap.NewNop(), &Config{DefaultBackend: "ranged"})
	eng.AddDriver("ranged", &rangeInfoDriver{stubDriver{name: "ranged", data: map[string][]byte{}, healthy: true}})
	for i := 0; i < failureThreshold; i++ {
		eng.RecordReadOutcome("ranged", errors.New("connection reset by peer"))
	}
	_, _, ok := eng.ParallelRangeSource(context.Background(), "c", "a")
	assert.False(t, ok, "an open breaker means the single-stream path (and its failover)")
}

func TestRecordReadOutcome_ChargesOnlyBackendFailures(t *testing.T) {
	eng := NewEngine(nil, zap.NewNop(), &Config{DefaultBackend: "ranged"})
	eng.AddDriver("ranged", &rangeInfoDriver{stubDriver{name: "ranged", data: map[string][]byte{}, healthy: true}})
	for i := 0; i < 2*failureThreshold; i++ {
		eng.RecordReadOutcome("ranged", context.Canceled) // the client went away
		eng.RecordReadOutcome("ranged", nil)
	}
	assert.Equal(t, "closed", eng.failover.GetStatus("ranged"))
	eng.RecordReadOutcome("nope", errors.New("x")) // unknown backend: no panic
}
