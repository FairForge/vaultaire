package drivers

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// siaPutFake is s3d's PutObject: it keeps the last body and counts PUTs.
type siaPutFake struct {
	mu   sync.Mutex
	puts int
	last []byte
}

func (f *siaPutFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		w.WriteHeader(http.StatusNotImplemented)
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return
	}
	f.mu.Lock()
	f.puts++
	f.last = body
	f.mu.Unlock()
	w.Header().Set("ETag", `"e"`)
	w.WriteHeader(http.StatusOK)
}

func (f *siaPutFake) state() (int, []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.puts, f.last
}

func newSiaPutDriver(t *testing.T, f *siaPutFake, budget int64) *IDriveDriver {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	d, err := NewSiaDriver("ak", "sk", srv.URL, "vaultaire", zap.NewNop())
	require.NoError(t, err)
	d.setSiaWrite(budget)
	return d
}

func TestSiaWriteBudgetFromEnv(t *testing.T) {
	env := func(v string) func(string) string {
		return func(k string) string {
			if k == "SIA_WRITE_BUDGET" {
				return v
			}
			return ""
		}
	}
	assert.Equal(t, int64(0), siaWriteBudgetFromEnv(env(""), zap.NewNop()), "not paced by default")
	assert.Equal(t, int64(40<<20), siaWriteBudgetFromEnv(env("40MiB"), zap.NewNop()))
	for _, off := range []string{"0", "off", "OFF"} {
		assert.Equal(t, int64(0), siaWriteBudgetFromEnv(env(off), zap.NewNop()), off)
	}
	for _, bad := range []string{"lots", "-1MiB", "200GiB"} {
		assert.Equal(t, int64(0), siaWriteBudgetFromEnv(env(bad), zap.NewNop()), "a rejected value keeps the default: %s", bad)
	}
}

func TestSiaWriteBudget_PacesThePutBody(t *testing.T) {
	// Arrange: 2 MiB/s, a 1.5 MiB body — about 0.5 s past the burst.
	body := siaTestBody(1536 << 10)
	f := &siaPutFake{}
	d := newSiaPutDriver(t, f, 2<<20)
	waited := testutil.ToFloat64(siaWriteBudgetWait)

	// Act
	start := time.Now()
	err := d.Put(siaCtx(), "tnt_b", "k.bin", onlyReader{bytes.NewReader(body)}, engine.WithContentLength(int64(len(body))))
	took := time.Since(start)

	// Assert
	require.NoError(t, err)
	puts, got := f.state()
	assert.Equal(t, 1, puts)
	assert.True(t, bytes.Equal(body, got), "the bytes stored are the bytes sent")
	assert.GreaterOrEqual(t, took, 300*time.Millisecond, "paced at the budget")
	assert.Greater(t, testutil.ToFloat64(siaWriteBudgetWait), waited)
	assert.Equal(t, float64(2<<20), testutil.ToFloat64(siaWriteBudgetRate))
}

// The wait is on the SOURCE, before the request to s3d: a PUT deadline far
// shorter than the paced upload never fires and nothing is retried.
// Margins a loaded CI runner cannot break (RP34-11): the deadline covers
// only the fake's answer to one small loopback PUT, the pacing is several
// deadlines long. It was 1.5 MiB against 50 ms and failed 31 % of runs at
// 64 race-built copies on 10 cores; there an unpaced 256 KiB PUT took
// p99 110 ms, max 158 ms (4 MiB: max 1 s — so the body stays small). Now:
// 256 KiB at 64 KiB/s = ~3.5 s of pacing past the 32 KiB burst, against a
// 1 s deadline.
func TestSiaWriteBudget_PacingIsNeverAStalledS3d(t *testing.T) {
	const deadline = time.Second
	prev := putDeadlineFloor
	putDeadlineFloor = deadline
	t.Cleanup(func() { putDeadlineFloor = prev })
	body := siaTestBody(256 << 10)
	f := &siaPutFake{}
	d := newSiaPutDriver(t, f, 64<<10)
	d.setPutTimeout(deadline)
	require.Equal(t, deadline, s3PutDeadline(deadline, int64(len(body))), "the deadline under test")
	retries := testutil.ToFloat64(driverPutRetries.WithLabelValues("sia"))

	start := time.Now()
	err := d.Put(siaCtx(), "tnt_b", "k.bin", onlyReader{bytes.NewReader(body)}, engine.WithContentLength(int64(len(body))))
	took := time.Since(start)

	require.NoError(t, err)
	assert.GreaterOrEqual(t, took, 3*deadline, "the paced wait is far longer than the deadline")
	puts, got := f.state()
	assert.Equal(t, 1, puts, "one PUT: the deadline timed s3d, not our wait")
	assert.True(t, bytes.Equal(body, got))
	assert.Equal(t, retries, testutil.ToFloat64(driverPutRetries.WithLabelValues("sia")))
}

func TestSiaWriteBudget_CallerGoneStopsTheWait(t *testing.T) {
	// 64 KiB/s, 4 MiB: a minute of pacing; the caller leaves after 100 ms.
	body := siaTestBody(4 << 20)
	f := &siaPutFake{}
	d := newSiaPutDriver(t, f, 64<<10)
	ctx, cancel := context.WithCancel(siaCtx())
	time.AfterFunc(100*time.Millisecond, cancel)

	start := time.Now()
	err := d.Put(ctx, "tnt_b", "k.bin", onlyReader{bytes.NewReader(body)}, engine.WithContentLength(int64(len(body))))

	require.Error(t, err)
	assert.True(t, errors.Is(err, context.Canceled), "the caller's error: %v", err)
	assert.Less(t, time.Since(start), 5*time.Second)
	puts, _ := f.state()
	assert.Equal(t, 0, puts, "nothing reached s3d")
}

func TestSiaWriteBudget_OffIsNotPaced(t *testing.T) {
	f := &siaPutFake{}
	d := newSiaPutDriver(t, f, 0)
	assert.Nil(t, d.siaWrite)
	assert.Equal(t, float64(0), testutil.ToFloat64(siaWriteBudgetRate))
	body := siaTestBody(256 << 10)
	r := onlyReader{bytes.NewReader(body)}
	assert.Equal(t, io.Reader(r), d.pacedSource(siaCtx(), r), "the body is handed on as is")
}

// Zero behaviour change for the other fixed-bucket drivers: SIA_WRITE_BUDGET
// is read by the sia constructor only.
func TestFixedBucketDrivers_OtherThanSiaAreNeverWritePaced(t *testing.T) {
	t.Setenv("SIA_WRITE_BUDGET", "1KiB")
	f := &siaPutFake{}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	body := siaTestBody(256 << 10) // 256 s at 1 KiB/s
	for _, name := range []string{"idrive", "wasabi", "lyve-shaped"} {
		t.Run(name, func(t *testing.T) {
			d, err := NewFixedBucketS3Driver(name, "ak", "sk", srv.URL, "us-east-1", "vaultaire", zap.NewNop())
			require.NoError(t, err)
			assert.Nil(t, d.siaWrite)

			ctx, cancel := context.WithTimeout(siaCtx(), 10*time.Second)
			defer cancel()
			require.NoError(t, d.Put(ctx, "tnt_b", "k.bin", onlyReader{bytes.NewReader(body)}, engine.WithContentLength(int64(len(body)))))
			_, got := f.state()
			assert.True(t, bytes.Equal(body, got))
		})
	}
	d, err := NewSiaDriver("ak", "sk", srv.URL, "vaultaire", zap.NewNop())
	require.NoError(t, err)
	require.NotNil(t, d.siaWrite, "the sia driver reads SIA_WRITE_BUDGET")
	assert.Equal(t, float64(1<<10), testutil.ToFloat64(siaWriteBudgetRate))
}
