package api

// WP-R2-6 (+ R8-12), Stage 1a Prompt 14 Part C: the chunked PUT pool held
// CHUNK_PUT_CONCURRENCY × (chunk + compressed + ciphertext) per write — ≈ 72
// MiB typical, ≤ 576 MiB worst — and nothing bounded how many writes ran at
// once; the large GETs have had a process budget since Prompt 2b.3. Now a
// chunked write reserves one worker's buffers before it reads a byte (503
// SlowDown + Retry-After when the budget cannot spare them within 2 s),
// takes every chunk's buffers from the budget as it goes — waiting for its
// own chunks to finish when the budget is spent (fewer in flight, never a
// refusal) — and gives everything back on every exit.

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	promtest "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// blockBody is n distinct 16 MiB blocks, each a random 251-byte pattern
// repeated: the content-defined chunker finds no cut point in a short
// period (measured: a periodic body cuts only at the 16 MiB maximum), so
// every chunk is a whole 16 MiB block (crypto.ChunkMaxSize) with its own
// hash — and new ones every run, so no earlier run's rows are deduplicated
// onto.
func blockBody(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, 0, n<<24)
	for k := 0; k < n; k++ {
		period := randomBody(t, 251)
		b = append(b, bytes.Repeat(period, (1<<24)/251+1)[:1<<24]...)
	}
	return b
}

// budgetServer serves the fixture through the logging middleware, where a
// 5xx counts in vaultaire_errors_total unless marked a client refusal.
func budgetServer(t *testing.T, f *pendingFixture) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(f.server.loggingMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.server.handleS3Request(w, r.WithContext(s3Ctx(r.Context(), f.tenant)))
	})))
	t.Cleanup(ts.Close)
	return ts
}

func clientPut(t *testing.T, ts *httptest.Server, path string, body []byte) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPut, ts.URL+path, bytes.NewReader(body))
	require.NoError(t, err)
	req.ContentLength = int64(len(body))
	resp, err := ts.Client().Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func TestChunkPutBudget_AWriteThatCannotReserveOneWorkersBuffersIs503BeforeAnyChunk(t *testing.T) {
	// Arrange: a budget of exactly one reserve; a first write (one chunk:
	// under the 1 MiB minimum) holds it while its chunk PUT waits on the
	// backend.
	f := setupPendingFixture(t)
	f.server.chunkPutBudget = newChunkPutBudget(chunkPutReserveBytes)
	first := make(chan int, 1)
	go func() { first <- f.do(t, "PUT", "/test-bucket/first.bin", randomBody(t, 512<<10), nil).Code }()
	waitFor(t, "the first write's chunk PUT", func() bool { return f.gated.inFlight.Load() >= 1 })
	ts := budgetServer(t, f)
	puts0, refusals0, errors0 := f.gated.puts.Load(), promtest.ToFloat64(chunkPutBudgetRefusals), atomic.LoadInt64(&f.server.errorCount)
	var pending0 int
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM global_content_index WHERE backend_id = 'fixed' AND state = 'pending'`).Scan(&pending0))

	// Act: a second write.
	start := time.Now()
	resp := clientPut(t, ts, "/test-bucket/second.bin", randomBody(t, 4<<20))
	waited := time.Since(start)

	// Assert: 503 SlowDown + Retry-After after the short wait, before any
	// chunk of it was claimed or stored; counted as a refusal AND as a
	// server error (real saturation).
	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
	assert.Equal(t, "30", resp.Header.Get("Retry-After"))
	assert.GreaterOrEqual(t, waited, chunkPutBudgetWait-100*time.Millisecond, "the short wait for a reserve")
	assert.Less(t, waited, chunkPutBudgetWait+3*time.Second)
	assert.Equal(t, puts0, f.gated.puts.Load(), "no chunk of the refused write reached the backend")
	var pending int
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM global_content_index WHERE backend_id = 'fixed' AND state = 'pending'`).Scan(&pending))
	assert.Equal(t, pending0, pending, "no claim of the refused write")
	assert.Equal(t, refusals0+1, promtest.ToFloat64(chunkPutBudgetRefusals))
	assert.Equal(t, errors0+1, atomic.LoadInt64(&f.server.errorCount), "a budget refusal is a 5xx of the server's")

	// The first write finishes and gives everything back.
	close(f.gated.gate)
	require.Equal(t, http.StatusOK, <-first)
	assert.Zero(t, f.server.chunkPutBudget.inUse())
}

func TestChunkPutBudget_AWriteDegradesToFewerChunksInFlightRatherThanRefusing(t *testing.T) {
	// Arrange: three distinct 16 MiB chunks. Their worst-case hold is 48 MiB
	// each (chunkPutHoldBytes); once compressed (a constant block shrinks
	// to nothing) each holds its 16 MiB. With a budget of one reserve
	// (48 MiB) the write can keep ONE chunk at the backend at a time; with
	// a roomy budget the eight workers run. The gate holds every chunk PUT
	// that reaches the backend, so the write is waited for at the width
	// the budget allows (`want` PUTs at the gate — in flight can only grow
	// while the gate is closed) and never sampled on a clock: a fixed 2 s
	// settle saw two of three chunks on a loaded CI runner. The width is
	// then read from the driver's high-water mark after the whole write.
	run := func(limit int64, want int32) (widest int32, code int) {
		f := setupPendingFixture(t)
		f.server.chunkPutBudget = newChunkPutBudget(limit)
		body := blockBody(t, 3)
		gate := f.gated.gate
		var once sync.Once
		openGate := func() { once.Do(func() { close(gate) }) }
		t.Cleanup(openGate)
		done := make(chan *httptest.ResponseRecorder, 1)
		go func() {
			done <- f.do(t, "PUT", "/test-bucket/blocks.bin", body, map[string]string{"Content-Type": "application/octet-stream"})
		}()
		deadline := time.Now().Add(time.Minute)
		for f.gated.inFlight.Load() < want {
			select {
			case w := <-done:
				t.Fatalf("the write ended before %d chunk PUTs were in flight: %d %s", want, w.Code, w.Body.String())
			default:
			}
			require.True(t, time.Now().Before(deadline), "only %d of %d chunk PUTs in flight", f.gated.inFlight.Load(), want)
			time.Sleep(20 * time.Millisecond)
		}
		assert.LessOrEqual(t, f.server.chunkPutBudget.inUse(), limit, "never over the budget")
		openGate()
		code = (<-done).Code
		assert.Zero(t, f.server.chunkPutBudget.inUse(), "everything given back")
		assert.Equal(t, body, f.getObj(t, "blocks.bin"))
		return f.gated.maxInFlight.Load(), code
	}

	// Act
	narrow, codeNarrow := run(chunkPutReserveBytes, 1)
	wide, codeWide := run(1<<30, 3)

	// Assert
	assert.Equal(t, http.StatusOK, codeNarrow, "a write that holds its reserve is never refused")
	assert.Equal(t, http.StatusOK, codeWide)
	// A chunk's buffers go back to the budget only after its PUT returned,
	// so under one reserve no second PUT ever overlaps the first — over the
	// whole write, not at one sample.
	assert.Equal(t, int32(1), narrow, "one chunk at a time under a one-reserve budget")
	assert.GreaterOrEqual(t, wide, int32(3), "the workers run under a roomy budget")
}

func TestChunkPutBudget_BuffersAreReturnedOnEveryExit(t *testing.T) {
	// Arrange
	f := setupPendingFixture(t)
	budget := newChunkPutBudget(1 << 30)
	f.server.chunkPutBudget = budget
	f.server.chunkPendingTakeover = 60 * time.Second

	// Success.
	close(f.gated.gate)
	require.Equal(t, http.StatusOK, f.do(t, "PUT", "/test-bucket/ok.bin", randomBody(t, 6<<20), nil).Code)
	assert.Zero(t, budget.inUse(), "after a success")

	// A backend that answers 500 (the write aborts, the claims are dropped).
	f.fixed.failPut.Store(true)
	assert.NotEqual(t, http.StatusOK, f.do(t, "PUT", "/test-bucket/fail.bin", randomBody(t, 6<<20), nil).Code)
	f.fixed.failPut.Store(false)
	assert.Zero(t, budget.inUse(), "after an abort")

	// A client cut while chunk PUTs are in flight: the PUTs that outlive the
	// request keep their blob's share until they end (RP14-1), then it is
	// given back.
	f.gated.gate = make(chan struct{})
	f.cutUpload(t, "cut.bin", randomBody(t, 8<<20))
	assert.Greater(t, budget.inUse(), int64(0), "the detached PUTs hold their blobs' share")
	assert.Less(t, budget.inUse(), chunkPutReserveBytes, "the reserve and the rest were given back when the request ended")
	close(f.gated.gate)
	waitFor(t, "the detached PUTs to end", func() bool { return f.server.chunkPuts().count() == 0 })
	assert.Zero(t, budget.inUse(), "after the detached PUTs ended")
}

func TestChunkPutBudgetFromEnv(t *testing.T) {
	logger := zap.NewNop()
	none := func(string) string { return "" }
	// The default: a quarter of GOMEMLIMIT when one is set, else 2 GiB;
	// clamped to the range.
	assert.Equal(t, int64(2<<30), chunkPutBudgetFromEnv(none, 1<<63-1, logger), "no memory limit: 2 GiB")
	assert.Equal(t, int64(12<<30), chunkPutBudgetFromEnv(none, 48<<30, logger), "a quarter of GOMEMLIMIT")
	assert.Equal(t, chunkPutBudgetMinBytes, chunkPutBudgetFromEnv(none, 512<<20, logger), "never under the floor")
	// The knob: sizes, bytes; the range; a rejected value keeps the default.
	env := func(v string) func(string) string { return func(string) string { return v } }
	assert.Equal(t, int64(512<<20), chunkPutBudgetFromEnv(env("512MiB"), 1<<63-1, logger))
	assert.Equal(t, int64(4<<30), chunkPutBudgetFromEnv(env("4GiB"), 1<<63-1, logger))
	assert.Equal(t, int64(1<<30), chunkPutBudgetFromEnv(env("1073741824"), 1<<63-1, logger))
	for _, bad := range []string{"128MiB", "65GiB", "lots", "-1", "0"} {
		assert.Equal(t, int64(2<<30), chunkPutBudgetFromEnv(env(bad), 1<<63-1, logger), bad)
	}
	assert.True(t, strings.HasPrefix("CHUNK_PUT_BUDGET_BYTES", "CHUNK_PUT"))
}
