package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/FairForge/vaultaire/internal/common"
	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/FairForge/vaultaire/internal/flags"
	"github.com/FairForge/vaultaire/internal/parfetch"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// rangedDriver is an in-memory fixed-bucket backend whose ranged reads are
// "real and cheap" (engine.VersionedRangeGetter), with injected latency and
// failures — the shape of iDrive behind the parallel ranged GET.
type rangedDriver struct {
	mu      sync.Mutex
	objects map[string][]byte

	gets       atomic.Int32 // whole-object Get calls
	ranges     atomic.Int32 // GetRangeInfo calls
	attemptsAt sync.Map     // offset -> *atomic.Int32
	// delay returns how long the n-th attempt (1-based) at a range waits.
	delay func(offset int64, n int32) time.Duration
	// fail returns an error for the n-th attempt at a range (nil = ok).
	fail func(offset int64, n int32) error
	// etag returns the ETag a range reports (default "v1").
	etag func(offset int64) string
	// short cuts the body of a range at this many bytes (0 = whole).
	short func(offset int64) int
}

func newRangedDriver() *rangedDriver { return &rangedDriver{objects: map[string][]byte{}} }

func (d *rangedDriver) key(ctx context.Context, container, artifact string) (string, error) {
	tid := ctxTenant(ctx)
	if tid == "" {
		return "", errors.New("ranged: no tenant in context")
	}
	return tid + "/" + container + "/" + artifact, nil
}

func (d *rangedDriver) Name() string { return "ranged" }
func (d *rangedDriver) Get(ctx context.Context, container, artifact string) (io.ReadCloser, error) {
	d.gets.Add(1)
	k, err := d.key(ctx, container, artifact)
	if err != nil {
		return nil, err
	}
	d.mu.Lock()
	b, ok := d.objects[k]
	d.mu.Unlock()
	if !ok {
		return nil, &engine.NotFoundError{Container: container, Artifact: artifact}
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}
func (d *rangedDriver) Put(ctx context.Context, container, artifact string, data io.Reader, _ ...engine.PutOption) error {
	k, err := d.key(ctx, container, artifact)
	if err != nil {
		return err
	}
	b, err := io.ReadAll(data)
	if err != nil {
		return err
	}
	d.mu.Lock()
	d.objects[k] = b
	d.mu.Unlock()
	return nil
}
func (d *rangedDriver) Delete(context.Context, string, string) error           { return nil }
func (d *rangedDriver) List(context.Context, string, string) ([]string, error) { return nil, nil }
func (d *rangedDriver) Exists(context.Context, string, string) (bool, error)   { return true, nil }
func (d *rangedDriver) HealthCheck(context.Context) error                      { return nil }

func (d *rangedDriver) GetRangeInfo(ctx context.Context, container, artifact string, offset, length int64) (io.ReadCloser, engine.RangeInfo, error) {
	d.ranges.Add(1)
	v, _ := d.attemptsAt.LoadOrStore(offset, &atomic.Int32{})
	n := v.(*atomic.Int32).Add(1)
	if d.delay != nil {
		if wait := d.delay(offset, n); wait > 0 {
			t := time.NewTimer(wait)
			select {
			case <-t.C:
			case <-ctx.Done():
				t.Stop()
				return nil, engine.RangeInfo{}, ctx.Err()
			}
		}
	}
	if d.fail != nil {
		if err := d.fail(offset, n); err != nil {
			return nil, engine.RangeInfo{}, err
		}
	}
	k, err := d.key(ctx, container, artifact)
	if err != nil {
		return nil, engine.RangeInfo{}, err
	}
	d.mu.Lock()
	b, ok := d.objects[k]
	d.mu.Unlock()
	if !ok {
		return nil, engine.RangeInfo{}, &engine.NotFoundError{Container: container, Artifact: artifact}
	}
	end := offset + length
	if end > int64(len(b)) {
		end = int64(len(b))
	}
	part := b[offset:end]
	if d.short != nil {
		if s := d.short(offset); s > 0 && s < len(part) {
			part = part[:s]
		}
	}
	etag := "v1"
	if d.etag != nil {
		etag = d.etag(offset)
	}
	return io.NopCloser(bytes.NewReader(part)), engine.RangeInfo{ETag: etag, Size: int64(len(b))}, nil
}

func ctxTenant(ctx context.Context) string {
	v, _ := ctx.Value(common.TenantIDKey).(string)
	return v
}

// parallelFixture: the adapter fixture with the ranged backend as primary,
// small parts so a few MiB exercise many ranges, and the flag on.
type parallelFixture struct {
	*adapterTestFixture
	drv *rangedDriver
}

func setupParallelFixture(t *testing.T, flagOn bool) *parallelFixture {
	t.Helper()
	f := setupAdapterFixture(t)
	drv := newRangedDriver()
	f.eng.AddDriver("ranged", drv)
	f.eng.SetPrimary("ranged")
	f.adapter = NewS3ToEngine(f.eng, f.db, zap.NewNop())
	fl := flags.New(nil, zap.NewNop())
	fl.Register(flagParallelGet, flagOn)
	f.adapter.flags = fl
	f.adapter.largeGet.partBytes = 256 << 10
	f.adapter.largeGet.parallelParts = 4
	f.adapter.largeGet.minBytes = 1 << 20
	f.adapter.largeGet.hedgeAfter = 50 * time.Millisecond
	f.adapter.largeGet.budget = parfetch.NewBudget(64 << 20)
	return &parallelFixture{adapterTestFixture: f, drv: drv}
}

func (f *parallelFixture) put(t *testing.T, key string, content []byte) string {
	t.Helper()
	req := httptest.NewRequest("PUT", "/test-bucket/"+key, bytes.NewReader(content))
	req.ContentLength = int64(len(content))
	req = req.WithContext(s3Ctx(req.Context(), f.tenant))
	w := httptest.NewRecorder()
	f.adapter.HandlePut(w, req, "test-bucket", key)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	return w.Header().Get("ETag")
}

func (f *parallelFixture) get(t *testing.T, key string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", "/test-bucket/"+key, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	req = req.WithContext(s3Ctx(req.Context(), f.tenant))
	w := httptest.NewRecorder()
	f.adapter.HandleGet(w, req, "test-bucket", key)
	return w
}

func randomBytes(n int) []byte {
	b := make([]byte, n)
	_, _ = rand.New(rand.NewSource(int64(n))).Read(b)
	return b
}

func TestParallelGet_WholeObjectInOrderWithSameHeaders(t *testing.T) {
	// Arrange: ranges finish in random order.
	f := setupParallelFixture(t, true)
	content := randomBytes(3<<20 + 12345)
	etag := f.put(t, "big.bin", content)
	f.drv.delay = func(off int64, _ int32) time.Duration { return time.Duration((off/1024)%7) * time.Millisecond }

	// Act
	w := f.get(t, "big.bin", nil)

	// Assert
	require.Equal(t, http.StatusOK, w.Code)
	assert.True(t, bytes.Equal(content, w.Body.Bytes()), "exact bytes, in order")
	assert.Equal(t, strconv.Itoa(len(content)), w.Header().Get("Content-Length"))
	assert.Equal(t, etag, w.Header().Get("ETag"), "the object's ETag, as on the single-stream path")
	assert.Equal(t, int32(0), f.drv.gets.Load(), "no single-stream GET")
	assert.GreaterOrEqual(t, f.drv.ranges.Load(), int32(13), "one range per part")
}

func TestParallelGet_FlagOffIsTheSingleStream(t *testing.T) {
	f := setupParallelFixture(t, false)
	content := randomBytes(3 << 20)
	f.put(t, "big.bin", content)

	w := f.get(t, "big.bin", nil)

	require.Equal(t, http.StatusOK, w.Code)
	assert.True(t, bytes.Equal(content, w.Body.Bytes()))
	assert.Equal(t, int32(1), f.drv.gets.Load())
	assert.Equal(t, int32(0), f.drv.ranges.Load(), "flag off: no ranged reads")
}

func TestParallelGet_SmallObjectIsTheSingleStream(t *testing.T) {
	f := setupParallelFixture(t, true)
	content := randomBytes(512 << 10) // under minBytes
	f.put(t, "small.bin", content)

	w := f.get(t, "small.bin", nil)

	require.Equal(t, http.StatusOK, w.Code)
	assert.True(t, bytes.Equal(content, w.Body.Bytes()))
	assert.Equal(t, int32(0), f.drv.ranges.Load())
}

func TestParallelGet_RangeRequestIsServedInParallelWithinTheRange(t *testing.T) {
	f := setupParallelFixture(t, true)
	content := randomBytes(4 << 20)
	etag := f.put(t, "big.bin", content)
	start, end := int64(100_001), int64(3_500_000)

	w := f.get(t, "big.bin", map[string]string{"Range": fmt.Sprintf("bytes=%d-%d", start, end)})

	require.Equal(t, http.StatusPartialContent, w.Code)
	assert.True(t, bytes.Equal(content[start:end+1], w.Body.Bytes()), "the exact slice")
	assert.Equal(t, fmt.Sprintf("bytes %d-%d/%d", start, end, len(content)), w.Header().Get("Content-Range"))
	assert.Equal(t, strconv.FormatInt(end-start+1, 10), w.Header().Get("Content-Length"))
	assert.Equal(t, etag, w.Header().Get("ETag"))
	assert.Greater(t, f.drv.ranges.Load(), int32(1), "parallel within the range")
	assert.Equal(t, int32(0), f.drv.gets.Load())
}

func TestParallelGet_ConditionalRequestsStillAnswerFirst(t *testing.T) {
	f := setupParallelFixture(t, true)
	etag := f.put(t, "big.bin", randomBytes(2<<20))

	w := f.get(t, "big.bin", map[string]string{"If-None-Match": etag})

	assert.Equal(t, http.StatusNotModified, w.Code)
	assert.Equal(t, int32(0), f.drv.ranges.Load())
}

func TestParallelGet_SlowRangeIsHedged(t *testing.T) {
	// Arrange: the first attempt at the third range hangs (until cancelled).
	f := setupParallelFixture(t, true)
	content := randomBytes(2 << 20)
	f.put(t, "big.bin", content)
	slow := int64(2 * (256 << 10))
	f.drv.delay = func(off int64, n int32) time.Duration {
		if off == slow && n == 1 {
			return time.Hour
		}
		return 0
	}
	hedges := testutil.ToFloat64(largeGetHedges.WithLabelValues("ranged", "slow"))
	won := testutil.ToFloat64(largeGetHedgesWon.WithLabelValues("ranged"))

	// Act
	start := time.Now()
	w := f.get(t, "big.bin", nil)

	// Assert
	require.Equal(t, http.StatusOK, w.Code)
	assert.True(t, bytes.Equal(content, w.Body.Bytes()))
	assert.Less(t, time.Since(start), 5*time.Second, "the hedge, not the hung request, served the range")
	assert.Equal(t, hedges+1, testutil.ToFloat64(largeGetHedges.WithLabelValues("ranged", "slow")))
	assert.Equal(t, won+1, testutil.ToFloat64(largeGetHedgesWon.WithLabelValues("ranged")))
}

func TestParallelGet_BudgetExhaustedFallsBackToOneStream(t *testing.T) {
	f := setupParallelFixture(t, true)
	content := randomBytes(2 << 20)
	f.put(t, "big.bin", content)
	f.adapter.largeGet.budget = parfetch.NewBudget(1) // nothing to spare
	before := testutil.ToFloat64(largeGetFallbacks.WithLabelValues("budget"))

	w := f.get(t, "big.bin", nil)

	require.Equal(t, http.StatusOK, w.Code)
	assert.True(t, bytes.Equal(content, w.Body.Bytes()))
	assert.Equal(t, int32(1), f.drv.gets.Load(), "one plain stream")
	assert.Equal(t, int32(0), f.drv.ranges.Load())
	assert.Equal(t, before+1, testutil.ToFloat64(largeGetFallbacks.WithLabelValues("budget")))
}

func TestParallelGet_FirstPartFailureFallsBackCleanly(t *testing.T) {
	f := setupParallelFixture(t, true)
	content := randomBytes(2 << 20)
	f.put(t, "big.bin", content)
	f.drv.fail = func(off int64, _ int32) error {
		if off == 0 {
			return errors.New("ranged: 500 internal error")
		}
		return nil
	}

	w := f.get(t, "big.bin", nil)

	require.Equal(t, http.StatusOK, w.Code)
	assert.True(t, bytes.Equal(content, w.Body.Bytes()), "served by the single stream")
	assert.Equal(t, int32(1), f.drv.gets.Load())
}

func TestParallelGet_MidStreamFailureIsABrokenBody(t *testing.T) {
	cases := map[string]func(d *rangedDriver){
		"range error": func(d *rangedDriver) {
			d.fail = func(off int64, _ int32) error {
				if off == 5*(256<<10) {
					return errors.New("ranged: connection reset by peer")
				}
				return nil
			}
		},
		"short range": func(d *rangedDriver) {
			d.short = func(off int64) int {
				if off == 5*(256<<10) {
					return 1000
				}
				return 0
			}
		},
		"object changed under the read": func(d *rangedDriver) {
			d.etag = func(off int64) string {
				if off >= 5*(256<<10) {
					return "v2"
				}
				return "v1"
			}
		},
	}
	for name, breakIt := range cases {
		t.Run(name, func(t *testing.T) {
			f := setupParallelFixture(t, true)
			content := randomBytes(3 << 20)
			f.put(t, "big.bin", content)
			breakIt(f.drv)
			// Parts after the broken one would otherwise race ahead.
			f.drv.delay = func(off int64, _ int32) time.Duration {
				if off > 5*(256<<10) {
					return 20 * time.Millisecond
				}
				return 0
			}

			w := f.get(t, "big.bin", nil)

			require.Equal(t, http.StatusOK, w.Code, "headers were committed before the failure")
			assert.Equal(t, strconv.Itoa(len(content)), w.Header().Get("Content-Length"),
				"the promised length stays: the short body is what the client detects")
			body := w.Body.Bytes()
			require.Less(t, len(body), len(content), "never silent: the body is cut short")
			assert.True(t, bytes.Equal(content[:len(body)], body), "an exact prefix, nothing out of order")
			assert.LessOrEqual(t, len(body), 5*(256<<10), "nothing at or after the broken part")
		})
	}
}

func TestParallelGet_BreakerChargedOncePerRead(t *testing.T) {
	// Arrange: every range after the first fails. Each GET makes several
	// failing range requests; charged per request, four GETs would open
	// the breaker (threshold 5).
	f := setupParallelFixture(t, true)
	f.put(t, "big.bin", randomBytes(2<<20))
	f.drv.fail = func(off int64, _ int32) error {
		if off > 0 {
			return errors.New("ranged: connection reset by peer")
		}
		return nil
	}

	// Act
	for i := 0; i < 4; i++ {
		_ = f.get(t, "big.bin", nil)
	}

	// Assert
	assert.Equal(t, "closed", f.eng.GetFailoverStatus()["ranged"], "4 failed reads = 4 charges, under the threshold")
	_ = f.get(t, "big.bin", nil)
	assert.Equal(t, "open", f.eng.GetFailoverStatus()["ranged"], "the fifth read is the fifth charge")
}

func TestParallelGet_VaultFloorNeverRanged(t *testing.T) {
	f := setupParallelFixture(t, true)
	content := randomBytes(2 << 20)
	f.put(t, "big.bin", content)
	_, err := f.db.Exec(`UPDATE object_head_cache SET floor = 'vault' WHERE tenant_id = $1 AND object_key = 'big.bin'`, f.tenantID)
	require.NoError(t, err)

	w := f.get(t, "big.bin", nil)

	require.Equal(t, http.StatusOK, w.Code)
	assert.True(t, bytes.Equal(content, w.Body.Bytes()))
	assert.Equal(t, int32(0), f.drv.ranges.Load(), "archive-floor objects keep the single stream")
}

// egressCountingWriter counts the body bytes handed to the response writer — what
// the egress meter (which wraps the writer) sees.
type egressCountingWriter struct {
	http.ResponseWriter
	n int64
}

func (c *egressCountingWriter) Write(p []byte) (int, error) {
	n, err := c.ResponseWriter.Write(p)
	c.n += int64(n)
	return n, err
}

func TestParallelGet_EgressCountedOnceEvenWithHedges(t *testing.T) {
	f := setupParallelFixture(t, true)
	content := randomBytes(2 << 20)
	f.put(t, "big.bin", content)
	f.drv.delay = func(off int64, n int32) time.Duration {
		if n == 1 && off%(512<<10) == 0 {
			return 200 * time.Millisecond // half the ranges get hedged
		}
		return 0
	}

	req := httptest.NewRequest("GET", "/test-bucket/big.bin", nil)
	req = req.WithContext(s3Ctx(req.Context(), f.tenant))
	rec := httptest.NewRecorder()
	cw := &egressCountingWriter{ResponseWriter: rec}
	f.adapter.HandleGet(cw, req, "test-bucket", "big.bin")

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, int64(len(content)), cw.n, "every byte written to the client once")
	assert.Greater(t, f.drv.ranges.Load(), int32(8), "hedges issued extra backend reads")
}

// cancelOnWrite cancels the request after the first body write: a client
// that hangs up mid-download.
type cancelOnWrite struct {
	http.ResponseWriter
	cancel context.CancelFunc
}

func (c *cancelOnWrite) Write(p []byte) (int, error) {
	c.cancel()
	return c.ResponseWriter.Write(p)
}

func TestParallelGet_ClientGoneMidStreamLeaksNothing(t *testing.T) {
	f := setupParallelFixture(t, true)
	content := randomBytes(4 << 20)
	f.put(t, "big.bin", content)
	_ = f.get(t, "big.bin", nil) // warm: first-use allocations are not a leak
	budget := f.adapter.largeGet.budget
	f.drv.delay = func(off int64, _ int32) time.Duration {
		if off > 0 {
			return time.Hour
		}
		return 0
	}
	runtime.GC()
	before := runtime.NumGoroutine()

	ctx, cancel := context.WithCancel(s3Ctx(context.Background(), f.tenant))
	req := httptest.NewRequest("GET", "/test-bucket/big.bin", nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	f.adapter.HandleGet(&cancelOnWrite{ResponseWriter: rec, cancel: cancel}, req, "test-bucket", "big.bin")

	assert.Less(t, rec.Body.Len(), len(content))
	assert.Equal(t, int64(0), budget.InUse(), "the budget is given back")
	deadline := time.Now().Add(3 * time.Second)
	for {
		runtime.GC()
		// The fixture's fire-and-forget last_accessed touch may still run.
		if runtime.NumGoroutine() <= before+1 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if n := runtime.NumGoroutine(); n > before+1 {
		buf := make([]byte, 1<<20)
		k := runtime.Stack(buf, true)
		t.Fatalf("goroutines %d -> %d:\n%s", before, n, strings.TrimSpace(string(buf[:k])))
	}
}

func TestParallelGet_RangeRequestCountedOkAndRecordsBreakerSuccess(t *testing.T) {
	// Arrange: a Range GET is written with io.CopyN, which stops after
	// exactly the range's bytes without the read that would see EOF —
	// the stream was counted "aborted" and never recorded a success
	// (2b.4 E2.2).
	f := setupParallelFixture(t, true)
	content := randomBytes(4 << 20)
	f.put(t, "big.bin", content)
	ok := largeGetStreams.WithLabelValues("ranged", "ok")
	aborted := largeGetStreams.WithLabelValues("ranged", "aborted")
	okBefore, abortedBefore := testutil.ToFloat64(ok), testutil.ToFloat64(aborted)
	// Four charged failures: a fifth would open the breaker, a success
	// clears them.
	for i := 0; i < 4; i++ {
		f.eng.RecordReadOutcome("ranged", errors.New("ranged: connection reset by peer"))
	}

	// Act
	w := f.get(t, "big.bin", map[string]string{"Range": "bytes=0-3145727"})

	// Assert
	require.Equal(t, http.StatusPartialContent, w.Code)
	assert.True(t, bytes.Equal(content[:3<<20], w.Body.Bytes()))
	assert.Greater(t, f.drv.ranges.Load(), int32(1), "served by the parallel reader")
	assert.Equal(t, okBefore+1, testutil.ToFloat64(ok), "ok %v→%v", okBefore, testutil.ToFloat64(ok))
	assert.Equal(t, abortedBefore, testutil.ToFloat64(aborted), "aborted %v→%v", abortedBefore, testutil.ToFloat64(aborted))
	f.eng.RecordReadOutcome("ranged", errors.New("ranged: connection reset by peer"))
	assert.Equal(t, "closed", f.eng.GetFailoverStatus()["ranged"], "the read's success cleared the earlier failures")
}
