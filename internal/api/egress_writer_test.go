package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/FairForge/vaultaire/internal/usage"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The throttle writer (WP-R10-9, part C).

// sliceRecorder is a ResponseWriter that remembers how it was written to.
type sliceRecorder struct {
	mu       sync.Mutex
	header   http.Header
	total    int64
	maxWrite int
	writes   int
	flushes  int
	status   int
}

func (s *sliceRecorder) Header() http.Header {
	if s.header == nil {
		s.header = http.Header{}
	}
	return s.header
}
func (s *sliceRecorder) WriteHeader(code int) { s.status = code }
func (s *sliceRecorder) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.total += int64(len(p))
	s.writes++
	if len(p) > s.maxWrite {
		s.maxWrite = len(p)
	}
	return len(p), nil
}
func (s *sliceRecorder) Flush() {
	s.mu.Lock()
	s.flushes++
	s.mu.Unlock()
}
func (s *sliceRecorder) written() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.total
}

// overTenant returns a meter whose tenant "t" is past its allowance with
// enforcement on, capped at bps.
func overTenant(t *testing.T, bps int64) *meterStub {
	t.Helper()
	s := newMeterStub(usage.EgressThrottle{MinBytesPerSec: bps, Factor: 1, MaxStreams: 16})
	s.m.now = time.Now // the limiter runs on the real clock
	s.base.Store(2000)
	s.allowance.Store(1000)
	s.enforced.Store(true)
	return s
}

func TestEgressWriter_ASingle64MiBWriteIsPaced(t *testing.T) {
	// Arrange: 32 MiB/s, so the bucket's burst is 8 MiB — far less than the
	// one 64 MiB Write a decrypted SSE object arrives in. WaitN fails when
	// asked for more than the burst, so the writer must cut the Write up.
	const bps = 32 << 20
	s := overTenant(t, bps)
	under := &sliceRecorder{}
	req := httptest.NewRequest(http.MethodGet, "/b/k", nil)
	w, ok := s.m.admit(under, req, "t", egressSurfaceS3)
	require.True(t, ok)
	defer w.close()
	body := make([]byte, 64<<20)
	throttledBefore := testutil.ToFloat64(egressThrottledBytes)

	// Act
	start := time.Now()
	n, err := w.Write(body)
	elapsed := time.Since(start)

	// Assert: every byte arrived, in slices no larger than 16 KiB, at the
	// capped pace: (64 − 8 burst) / 32 MiB/s = 1.75 s.
	require.NoError(t, err)
	assert.Equal(t, len(body), n)
	assert.Equal(t, int64(len(body)), under.written())
	assert.LessOrEqual(t, under.maxWrite, egressSliceBytes)
	assert.GreaterOrEqual(t, elapsed, 1500*time.Millisecond, "a paced 64 MiB at 32 MiB/s cannot finish sooner")
	assert.Less(t, elapsed, 6*time.Second)
	assert.Equal(t, float64(len(body)), testutil.ToFloat64(egressThrottledBytes)-throttledBefore)
	assert.Positive(t, under.flushes, "paced slices are pushed out, not left in a buffer")
}

func TestEgressWriter_UnderTheAllowanceIsNotCutIntoSlices(t *testing.T) {
	// Arrange: a tenant with allowance to spare.
	s := newMeterStub(usage.DefaultEgressThrottle())
	s.m.now = time.Now
	s.allowance.Store(1 << 40)
	s.enforced.Store(true)
	under := &sliceRecorder{}
	w, ok := s.m.admit(under, httptest.NewRequest(http.MethodGet, "/b/k", nil), "t", egressSurfaceS3)
	require.True(t, ok)
	defer w.close()

	// Act
	start := time.Now()
	n, err := w.Write(make([]byte, 16<<20))

	// Assert: full speed, written in re-evaluation-sized pieces.
	require.NoError(t, err)
	assert.Equal(t, 16<<20, n)
	assert.Less(t, time.Since(start), time.Second)
	assert.Equal(t, egressReevalBytes, under.maxWrite)
	assert.Equal(t, 16, under.writes)
}

func TestEgressWriter_ContextCancelReturnsAtOnce(t *testing.T) {
	// Arrange: the floor rate — 4 MiB would take a minute.
	s := overTenant(t, 65536)
	under := &sliceRecorder{}
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, "/b/k", nil).WithContext(ctx)
	w, ok := s.m.admit(under, req, "t", egressSurfaceS3)
	require.True(t, ok)
	before := runtime.NumGoroutine()
	done := make(chan error, 1)
	go func() {
		_, err := w.Write(make([]byte, 4<<20))
		done <- err
	}()
	time.Sleep(150 * time.Millisecond)

	// Act: the client disconnects.
	cancelled := time.Now()
	cancel()

	// Assert: the Write returns promptly with the context's error, well
	// short of its 4 MiB, and nothing is left running.
	select {
	case err := <-done:
		require.Error(t, err)
		assert.Less(t, time.Since(cancelled), 200*time.Millisecond)
	case <-time.After(3 * time.Second):
		t.Fatal("a paced Write must return when the request context is cancelled")
	}
	assert.Less(t, under.written(), int64(1<<20))
	w.close()
	assert.Equal(t, int32(0), s.m.tenant(context.Background(), "t").throttledStreams[0].Load())
	// (Polled by hand: require.Eventually runs its condition in a goroutine.)
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > before && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	assert.LessOrEqual(t, runtime.NumGoroutine(), before, "no goroutine may outlive the cancelled response")
}

func TestEgressWriter_FlushAndUnwrapPassThrough(t *testing.T) {
	s := newMeterStub(usage.DefaultEgressThrottle())
	under := &sliceRecorder{}
	w, ok := s.m.admit(under, httptest.NewRequest(http.MethodGet, "/b/k", nil), "t", egressSurfaceS3)
	require.True(t, ok)
	defer w.close()

	w.Flush()

	assert.Equal(t, 1, under.flushes)
	assert.Same(t, http.ResponseWriter(under), w.Unwrap())
	var _ http.Flusher = w
}

func TestEgressWriter_ErrorsAreNotPacedOrCounted(t *testing.T) {
	// Arrange: a tenant far past its allowance, at the floor rate.
	s := overTenant(t, 65536)
	engagedBefore := testutil.ToFloat64(egressEngaged.WithLabelValues(egressSurfaceS3))
	for _, code := range []int{http.StatusNotFound, http.StatusForbidden, http.StatusInternalServerError,
		http.StatusRequestedRangeNotSatisfiable, http.StatusPreconditionFailed} {
		under := &sliceRecorder{}
		w, ok := s.m.admit(under, httptest.NewRequest(http.MethodGet, "/b/k", nil), "t", egressSurfaceS3)
		require.True(t, ok)

		// Act: a 1 MiB error document would take 16 s through the bucket.
		start := time.Now()
		w.WriteHeader(code)
		n, err := w.Write(make([]byte, 1<<20))
		w.close()

		// Assert
		require.NoError(t, err)
		assert.Equal(t, 1<<20, n)
		assert.Less(t, time.Since(start), 500*time.Millisecond, "status %d must not be slowed", code)
		assert.Equal(t, code, under.status)
	}
	assert.Equal(t, engagedBefore, testutil.ToFloat64(egressEngaged.WithLabelValues(egressSurfaceS3)))
}

func TestEgressWriter_AllowanceRaisedMidResponseSpeedsUp(t *testing.T) {
	// Arrange: paced at the floor.
	s := overTenant(t, 65536)
	s.m.allowanceTTL = 50 * time.Millisecond
	s.m.reevalEvery = 100 * time.Millisecond
	under := &sliceRecorder{}
	w, ok := s.m.admit(under, httptest.NewRequest(http.MethodGet, "/b/k", nil), "t", egressSurfaceS3)
	require.True(t, ok)
	defer w.close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = w.Write(make([]byte, 8<<20)) // two minutes at 64 KiB/s
	}()
	time.Sleep(200 * time.Millisecond)
	require.Less(t, under.written(), int64(1<<20))

	// Act: the customer buys quota.
	s.allowance.Store(1 << 40)

	// Assert: the download finishes once the open response re-reads its
	// position — on the clock, not only every megabyte.
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatalf("still paced after the allowance was raised (%d bytes sent)", under.written())
	}
	assert.Equal(t, int64(8<<20), under.written())
}

// HAProxy drops a connection after 50 s of silence, Cloudflare after 100 s.
// With every slot of both surfaces taken, each paced stream must still send
// within egressMaxSilence — whatever the knobs are set to.
func TestEgressMeter_SliceKeepsEveryPacedStreamSending(t *testing.T) {
	for _, cfg := range []usage.EgressThrottle{
		usage.DefaultEgressThrottle(),
		{MinBytesPerSec: 65536, Factor: 1, MaxStreams: 64},
		{MinBytesPerSec: 8192, Factor: 1, MaxStreams: 16},
		{MinBytesPerSec: 32 << 20, Factor: 1, MaxStreams: 4},
	} {
		s := newMeterStub(cfg)
		tn := s.m.tenant(context.Background(), "t")

		slice := tn.sliceBytes.Load()
		worst := time.Duration(float64(2*cfg.MaxStreams) * float64(slice) / float64(cfg.MinBytesPerSec) * float64(time.Second))

		assert.LessOrEqual(t, slice, int64(egressSliceBytes), "%+v", cfg)
		assert.GreaterOrEqual(t, slice, int64(egressMinSliceBytes), "%+v", cfg)
		assert.LessOrEqual(t, worst, egressMaxSilence, "%+v: a paced stream would be silent for %s", cfg, worst)
	}
	// The defaults keep the full 16 KiB slice: 32 streams × 16 KiB at 64 KiB/s = 8 s.
	s := newMeterStub(usage.DefaultEgressThrottle())
	assert.Equal(t, int64(egressSliceBytes), s.m.tenant(context.Background(), "t").sliceBytes.Load())
}
