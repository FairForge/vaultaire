package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// rtRangeDriver is an rtDriver with a native ranged read, like the S3-class
// drivers and Geyser.
type rtRangeDriver struct {
	rtDriver
	getRange func(ctx context.Context, container, artifact string, offset, length int64) (io.ReadCloser, error)
	ranges   []int64
}

func (d *rtRangeDriver) GetRange(ctx context.Context, container, artifact string, offset, length int64) (io.ReadCloser, error) {
	d.ranges = append(d.ranges, length)
	return d.getRange(ctx, container, artifact, offset, length)
}

// 2026-10-21, Lyve us-east-1: HEAD 200 with the right size and ETag for
// chunk blobs whose every GET was a 500 InternalError for about 45 s. The
// job's Exists said "present" for exactly those. A present row whose bytes
// cannot be read is its own verdict — unreadable — never present, and never
// missing (the bytes may well be there; the backend cannot serve them).
func TestRoutingTruth_Run_PresentButUnreadableIsItsOwnVerdict(t *testing.T) {
	// Arrange: a backend whose HEAD is 200 and whose every read is a 500,
	// beside the healthy local one.
	f := setupRoutingTruthFixture(t)
	sick := &rtRangeDriver{rtDriver: rtDriver{exists: func(context.Context, string, string) (bool, error) { return true, nil }}}
	sick.getRange = func(context.Context, string, string, int64, int64) (io.ReadCloser, error) {
		return nil, errors.New("operation error S3: GetObject, https response error StatusCode: 500, api error InternalError: We encountered an internal error, please try again")
	}
	f.eng.AddDriver("rt-sick", sick)
	f.row("b", "sick1", "rt-sick", false)
	f.row("b", "sick2", "rt-sick", false)
	f.row("b", "fine", f.hot, true)

	// Act
	res := f.run()

	// Assert: the verdict, told apart from present and missing; the sick
	// backend's breaker untouched; a bounded read (never the whole object).
	bt := res.Backends["rt-sick"]
	assert.Equal(t, 2, bt.Sampled)
	assert.Equal(t, 2, bt.Unreadable)
	assert.Zero(t, bt.Present)
	assert.Zero(t, bt.Missing)
	assert.Zero(t, bt.Errors)
	assert.InDelta(t, 1.0, bt.UnreadableRatio, 1e-9)
	assert.Zero(t, bt.MissingRatio)
	assert.Equal(t, 1, res.Backends[f.hot].Present, "the healthy backend's row reads back")
	assert.Equal(t, engine.StateClosed.String(), f.eng.GetFailoverStatus()["rt-sick"], "the job never charges a breaker")
	for _, n := range sick.ranges {
		assert.LessOrEqual(t, n, int64(routingReadBytes))
	}
	assert.Contains(t, res.Summary, "rt-sick: 2 of 2 rows sampled, 0 present, 0 missing, 2 unreadable")

	// The counter series, and the gauge a fresh collector reads from the table.
	m := prometheus.NewPedanticRegistry()
	m.MustRegister(newRoutingCollector(f.checker))
	recordJobFinish(f.db, f.checker.JobName, res.FinishedAt, "ok", res.Summary, 0, mustJSON(t, res))
	mfs, err := m.Gather()
	require.NoError(t, err)
	v, ok := gauge(mfs, "vaultaire_routing_truth_last_run_unreadable", map[string]string{"backend": "rt-sick"})
	assert.True(t, ok, "the unreadable gauge is a series per backend of the run")
	assert.Equal(t, 2.0, v)
	v, ok = gauge(mfs, "vaultaire_routing_truth_last_run_unreadable", map[string]string{"backend": f.hot})
	assert.True(t, ok)
	assert.Zero(t, v)
	v, ok = gauge(mfs, "vaultaire_routing_truth_last_run_verdicts", map[string]string{"backend": "rt-sick"})
	assert.True(t, ok)
	assert.Equal(t, 2.0, v, "an unreadable row is a verdict: the backend answered")
}

// An object on tape answers its ranged read with ErrArchived: the bytes are
// there, behind a restore. Present, not unreadable. An empty object has
// nothing to read: present on its HEAD alone.
func TestRoutingTruth_Run_ArchivedAndEmptyObjectsArePresent(t *testing.T) {
	// Arrange
	f := setupRoutingTruthFixture(t)
	tape := &rtRangeDriver{rtDriver: rtDriver{exists: func(context.Context, string, string) (bool, error) { return true, nil }}}
	tape.getRange = func(context.Context, string, string, int64, int64) (io.ReadCloser, error) {
		return nil, engine.ErrArchived
	}
	f.eng.AddDriver("rt-tape", tape)
	f.row("b", "cold", "rt-tape", false)
	empty := &rtRangeDriver{rtDriver: rtDriver{exists: func(context.Context, string, string) (bool, error) { return true, nil }}}
	empty.getRange = func(context.Context, string, string, int64, int64) (io.ReadCloser, error) {
		return nil, errors.New("must not be read: the object is empty")
	}
	f.eng.AddDriver("rt-empty", empty)
	f.row("b", "zero", "rt-empty", false)
	_, err := f.db.Exec(`UPDATE object_head_cache SET size_bytes = 0 WHERE tenant_id = $1 AND object_key = 'zero'`, f.tenantID)
	require.NoError(t, err)

	// Act
	res := f.run()

	// Assert
	assert.Equal(t, 1, res.Backends["rt-tape"].Present)
	assert.Zero(t, res.Backends["rt-tape"].Unreadable)
	assert.Equal(t, 1, res.Backends["rt-empty"].Present)
	assert.Empty(t, empty.ranges, "an empty object is never read")
}

// A driver without a ranged read is read through Get, a bounded number of
// bytes, then closed — the body is never drained.
func TestRoutingTruth_Run_ReadProbeThroughGetIsBoundedAndClosed(t *testing.T) {
	// Arrange: an object of 1 MiB behind a driver that counts what is read.
	f := setupRoutingTruthFixture(t)
	body := &countingBody{Reader: bytes.NewReader(make([]byte, 1<<20))}
	plain := &rtDriver{exists: func(context.Context, string, string) (bool, error) { return true, nil }}
	plain.get = func(context.Context, string, string) (io.ReadCloser, error) { return body, nil }
	f.eng.AddDriver("rt-plain", plain)
	f.row("b", "big", "rt-plain", false)
	_, err := f.db.Exec(`UPDATE object_head_cache SET size_bytes = $2 WHERE tenant_id = $1 AND object_key = 'big'`, f.tenantID, 1<<20)
	require.NoError(t, err)

	// Act
	res := f.run()

	// Assert
	assert.Equal(t, 1, res.Backends["rt-plain"].Present)
	assert.LessOrEqual(t, body.read, int64(routingReadBytes)+1)
	assert.True(t, body.closed, "the probe closes what it opened")

	// And a body that fails after the status line is unreadable too.
	broken := &rtDriver{exists: func(context.Context, string, string) (bool, error) { return true, nil }}
	broken.get = func(context.Context, string, string) (io.ReadCloser, error) {
		return io.NopCloser(&rtFailingReader{}), nil
	}
	f.eng.AddDriver("rt-broken", broken)
	f.row("b", "cut", "rt-broken", false)
	res = f.run()
	assert.Equal(t, 1, res.Backends["rt-broken"].Unreadable)
}

// Chunks are read the same way: a chunk HEAD says present and its GET fails
// → unreadable, counted apart from missing, never charging the breaker.
func TestRoutingTruth_Run_AChunkThatCannotBeReadIsUnreadable(t *testing.T) {
	// Arrange: one chunked object on the fixed-bucket primary whose reads fail.
	f := setupChunkAddrFixture(t)
	chunks := f.put(f.a, "o.bin", generateTestData(8*1024))
	require.NotEmpty(t, chunks)
	f.fixed.failGet.Store(true)
	c := NewRoutingTruthChecker(f.db, f.eng, f.adapter.gci, zap.NewNop())
	c.scopeTenant = f.a.ID

	// Act
	res, err := c.RunOnce(context.Background())
	require.NoError(t, err)

	// Assert
	assert.Equal(t, len(chunks), res.Chunks.Chunks)
	assert.Equal(t, len(chunks), res.Chunks.Unreadable)
	assert.Zero(t, res.Chunks.Present)
	assert.Zero(t, res.Chunks.Missing)
	assert.Zero(t, res.Chunks.Errors)
	assert.Equal(t, engine.StateClosed.String(), f.eng.GetFailoverStatus()[f.backend])
	assert.Contains(t, res.Summary, fmt.Sprintf("%d unreadable", len(chunks)))
	assert.True(t, strings.Contains(res.Summary, "chunks:"))

	// Reads restored: present again.
	f.fixed.failGet.Store(false)
	res, err = c.RunOnce(context.Background())
	require.NoError(t, err)
	assert.Equal(t, len(chunks), res.Chunks.Present)
	assert.Zero(t, res.Chunks.Unreadable)
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	require.NoError(t, err)
	return raw
}

type countingBody struct {
	io.Reader
	read   int64
	closed bool
}

func (b *countingBody) Read(p []byte) (int, error) {
	n, err := b.Reader.Read(p)
	b.read += int64(n)
	return n, err
}
func (b *countingBody) Close() error { b.closed = true; return nil }

type rtFailingReader struct{}

func (rtFailingReader) Read([]byte) (int, error) { return 0, errors.New("unexpected EOF") }
