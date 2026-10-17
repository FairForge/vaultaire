package engine

import (
	"bytes"
	"context"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// WP-R6-3 / RP8-10: vaultaire_driver_op_seconds{driver,op} — one
// observation per driver call that goes through the engine, success or
// failure, so a backend's 60 s mode is a visible bucket and not something a
// probe finds (2026-10-17). ops: head | get_first_byte | put | delete |
// list | probe. A GET is timed to its first byte (the body streams at the
// client's pace); a PUT without the time the driver spent waiting on the
// request body (a slow uploader is not a slow backend).

type timedDriver struct {
	stubDriver
	delay time.Duration
	calls atomic.Int32
}

func (d *timedDriver) wait() {
	d.calls.Add(1)
	time.Sleep(d.delay)
}

func (d *timedDriver) Get(ctx context.Context, c, a string) (io.ReadCloser, error) {
	d.wait()
	return io.NopCloser(&slowReader{r: strings.NewReader("body"), each: 200 * time.Millisecond}), nil
}

func (d *timedDriver) GetRange(ctx context.Context, c, a string, off, n int64) (io.ReadCloser, error) {
	d.wait()
	return io.NopCloser(strings.NewReader("b")), nil
}

func (d *timedDriver) Put(ctx context.Context, c, a string, r io.Reader, _ ...PutOption) error {
	_, err := io.Copy(io.Discard, r)
	d.wait()
	return err
}

func (d *timedDriver) Delete(ctx context.Context, c, a string) error { d.wait(); return nil }

func (d *timedDriver) List(ctx context.Context, c, p string) ([]string, error) {
	d.wait()
	return nil, nil
}

func (d *timedDriver) Exists(ctx context.Context, c, a string) (bool, error) {
	d.wait()
	return true, nil
}

func (d *timedDriver) HealthCheck(ctx context.Context) error { d.wait(); return nil }

// slowReader yields its bytes one Read at a time, each after a pause: a
// client uploading (or a body being read) slowly.
type slowReader struct {
	r    io.Reader
	each time.Duration
}

func (s *slowReader) Read(p []byte) (int, error) {
	time.Sleep(s.each)
	if len(p) > 1 {
		p = p[:1]
	}
	return s.r.Read(p)
}

func opSamples(t *testing.T, driver, op string) (count uint64, sum float64) {
	t.Helper()
	m := &dto.Metric{}
	h, err := driverOpSeconds.GetMetricWithLabelValues(driver, op)
	require.NoError(t, err)
	require.NoError(t, h.(interface{ Write(*dto.Metric) error }).Write(m))
	return m.GetHistogram().GetSampleCount(), m.GetHistogram().GetSampleSum()
}

func TestDriverOpSeconds_OneObservationPerCallUnderItsOp(t *testing.T) {
	eng := NewEngine(nil, zap.NewNop(), &Config{DefaultBackend: "opsec-a"})
	d := &timedDriver{stubDriver: stubDriver{name: "opsec-a", data: map[string][]byte{}, healthy: true}}
	eng.AddDriver("opsec-a", d)
	ctx := WithRecordedBackend(tenantCtx(), "opsec-a")
	before := map[string]uint64{}
	for _, op := range []string{"head", "get_first_byte", "put", "delete", "list", "probe"} {
		before[op], _ = opSamples(t, "opsec-a", op)
	}

	rc, err := eng.Get(ctx, "c", "k")
	require.NoError(t, err)
	_ = rc.Close()
	rc, err = eng.GetRange(ctx, "c", "k", 0, 1)
	require.NoError(t, err)
	_ = rc.Close()
	rc, err = eng.GetOn(tenantCtx(), "opsec-a", "c", "k")
	require.NoError(t, err)
	_ = rc.Close()
	_, err = eng.Put(tenantCtx(), "c", "k", bytes.NewReader([]byte("x")))
	require.NoError(t, err)
	require.NoError(t, eng.PutOn(tenantCtx(), "opsec-a", "c", "k2", bytes.NewReader([]byte("y"))))
	require.NoError(t, eng.Delete(ctx, "c", "k"))
	require.NoError(t, eng.DeleteOn(tenantCtx(), "opsec-a", "c", "k2"))
	_, err = eng.ExistsOn(tenantCtx(), "opsec-a", "c", "k")
	require.NoError(t, err)
	_, err = eng.List(tenantCtx(), "c", "")
	require.NoError(t, err)
	require.NoError(t, eng.CheckDriver(context.Background(), "opsec-a"))

	want := map[string]uint64{"get_first_byte": 3, "put": 2, "delete": 2, "head": 1, "list": 1, "probe": 1}
	for op, n := range want {
		got, _ := opSamples(t, "opsec-a", op)
		assert.Equal(t, n, got-before[op], "op %s", op)
	}
}

// A GET is timed to its first byte: the driver's 300 ms before it answers
// counts, the body's slow stream after it does not.
func TestDriverOpSeconds_AGetIsTimedToItsFirstByte(t *testing.T) {
	eng := NewEngine(nil, zap.NewNop(), &Config{DefaultBackend: "opsec-b"})
	eng.AddDriver("opsec-b", &timedDriver{stubDriver: stubDriver{name: "opsec-b", data: map[string][]byte{}, healthy: true}, delay: 300 * time.Millisecond})
	n0, s0 := opSamples(t, "opsec-b", "get_first_byte")

	rc, err := eng.Get(WithRecordedBackend(tenantCtx(), "opsec-b"), "c", "k")
	require.NoError(t, err)
	_, _ = io.ReadAll(rc) // 4 bytes × 200 ms
	_ = rc.Close()

	n, s := opSamples(t, "opsec-b", "get_first_byte")
	require.Equal(t, n0+1, n)
	assert.GreaterOrEqual(t, s-s0, 0.3)
	assert.Less(t, s-s0, 0.7, "the body's 800 ms is not the backend's first byte")
}

// A PUT is timed without the request body's waits: a client uploading at a
// trickle is not a slow backend; a backend that takes 300 ms after the body
// is.
func TestDriverOpSeconds_APutIsTimedWithoutTheBodysWaits(t *testing.T) {
	eng := NewEngine(nil, zap.NewNop(), &Config{DefaultBackend: "opsec-c"})
	d := &timedDriver{stubDriver: stubDriver{name: "opsec-c", data: map[string][]byte{}, healthy: true}}
	eng.AddDriver("opsec-c", d)
	n0, s0 := opSamples(t, "opsec-c", "put")

	_, err := eng.Put(tenantCtx(), "c", "slow-client", &slowReader{r: strings.NewReader("abcd"), each: 150 * time.Millisecond})
	require.NoError(t, err)
	n1, s1 := opSamples(t, "opsec-c", "put")
	require.Equal(t, n0+1, n1)
	assert.Less(t, s1-s0, 0.3, "600 ms of a trickling body is the client's, not the backend's")

	d.delay = 300 * time.Millisecond
	_, err = eng.Put(tenantCtx(), "c", "slow-backend", bytes.NewReader([]byte("abcd")))
	require.NoError(t, err)
	n2, s2 := opSamples(t, "opsec-c", "put")
	require.Equal(t, n1+1, n2)
	assert.GreaterOrEqual(t, s2-s1, 0.3)
}

// A call refused before the driver is asked (an open breaker) is not a
// driver op: nothing to time.
func TestDriverOpSeconds_AnOpenBreakerIsNotObserved(t *testing.T) {
	eng := NewEngine(nil, zap.NewNop(), &Config{DefaultBackend: "opsec-d"})
	d := &timedDriver{stubDriver: stubDriver{name: "opsec-d", data: map[string][]byte{}, healthy: true}}
	eng.AddDriver("opsec-d", d)
	for i := 0; i < 10; i++ {
		eng.failover.breakers["opsec-d"].RecordFailure()
	}
	n0, _ := opSamples(t, "opsec-d", "head")

	_, err := eng.ExistsOn(tenantCtx(), "opsec-d", "c", "k")

	require.Error(t, err)
	n, _ := opSamples(t, "opsec-d", "head")
	assert.Equal(t, n0, n)
	assert.Zero(t, d.calls.Load())
}
