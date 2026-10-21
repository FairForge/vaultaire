package drivers

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/FairForge/vaultaire/internal/common"
	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// siaFake is s3d as SIA-BENCH-3/5 measured it: a GET answers 200/206 and
// may then cut its body (the account's per-host read balances ran out), go
// idle for a while (the patched s3d waiting for a refill), or — between a
// cut and its resume — hold another object at the key.
type siaFake struct {
	mu   sync.Mutex
	body []byte
	etag string
	reqs []siaReq
	// cut returns how many body bytes request n (0-based) sends before the
	// connection is aborted; < 0 = the whole body.
	cut func(n int) int
	// idle pauses request n after this many bytes for idleFor (0 = none).
	idleAt  func(n int) int
	idleFor time.Duration
	// ignoreRange answers a resume (a request with If-Match) 200, whole.
	ignoreRange bool
	// skewStart shifts the Content-Range start of a resume.
	skewStart int64
	// ignoreIfMatch answers whatever the If-Match says.
	ignoreIfMatch bool
}

type siaReq struct{ rng, ifMatch string }

func (f *siaFake) setObject(body []byte, etag string) {
	f.mu.Lock()
	f.body, f.etag = body, etag
	f.mu.Unlock()
}

func (f *siaFake) requests() []siaReq {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]siaReq(nil), f.reqs...)
}

func (f *siaFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	n := len(f.reqs)
	f.reqs = append(f.reqs, siaReq{rng: r.Header.Get("Range"), ifMatch: r.Header.Get("If-Match")})
	body, etag := f.body, `"`+f.etag+`"`
	f.mu.Unlock()

	if im := r.Header.Get("If-Match"); im != "" && im != etag && !f.ignoreIfMatch {
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusPreconditionFailed)
		_, _ = io.WriteString(w, `<?xml version="1.0" encoding="UTF-8"?><Error><Code>PreconditionFailed</Code><Message>At least one of the pre-conditions you specified did not hold</Message></Error>`)
		return
	}
	start, end := int64(0), int64(len(body)-1)
	status := http.StatusOK
	if rh := r.Header.Get("Range"); rh != "" && !(f.ignoreRange && r.Header.Get("If-Match") != "") {
		var a, b int64
		if _, err := fmt.Sscanf(rh, "bytes=%d-%d", &a, &b); err == nil {
			start, end = a, min(b, int64(len(body)-1))
		} else if _, err := fmt.Sscanf(rh, "bytes=%d-", &a); err == nil {
			start = a
		} else {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		status = http.StatusPartialContent
		reported := start
		if r.Header.Get("If-Match") != "" {
			reported += f.skewStart
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", reported, end, len(body)))
	}
	part := body[start : end+1]
	w.Header().Set("ETag", etag)
	w.Header().Set("Content-Length", strconv.Itoa(len(part)))
	w.WriteHeader(status)
	if fl, ok := w.(http.Flusher); ok {
		fl.Flush() // the status line is out before any cut
	}

	limit := len(part)
	if f.cut != nil {
		if c := f.cut(n); c >= 0 && c < limit {
			limit = c
		}
	}
	idleAt := -1
	if f.idleAt != nil {
		idleAt = f.idleAt(n)
	}
	sent := 0
	for sent < limit {
		step := min(32<<10, limit-sent)
		if idleAt >= 0 && sent < idleAt && sent+step > idleAt {
			step = idleAt - sent
		}
		_, _ = w.Write(part[sent : sent+step])
		sent += step
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
		if sent == idleAt {
			time.Sleep(f.idleFor)
		}
	}
	if limit < len(part) {
		panic(http.ErrAbortHandler) // the connection drops mid-body
	}
}

func siaTestBody(n int) []byte {
	b := make([]byte, n)
	_, _ = rand.New(rand.NewSource(int64(n))).Read(b)
	return b
}

// newSiaTestDriver is the real sia driver against the fake, with backoffs
// in milliseconds.
func newSiaTestDriver(t *testing.T, f *siaFake, cfg siaReadConfig) *IDriveDriver {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	d, err := NewSiaDriver("ak", "sk", srv.URL, "vaultaire", zap.NewNop())
	require.NoError(t, err)
	if cfg.BackoffBase == 0 {
		cfg.BackoffBase = time.Millisecond
	}
	if cfg.BackoffCap == 0 {
		cfg.BackoffCap = 5 * time.Millisecond
	}
	if cfg.MaxWait == 0 {
		cfg.MaxWait = 10 * time.Second
	}
	d.setSiaRead(cfg)
	return d
}

func siaCtx() context.Context { return common.WithTenantID(context.Background(), "tnt") }

func resumeCount(outcome string) float64 {
	return testutil.ToFloat64(siaReadResumes.WithLabelValues(outcome))
}

func TestSiaGet_ResumesACutBodyFromTheReceivedOffset(t *testing.T) {
	// Arrange: the first answer is cut after 100,000 bytes.
	body := siaTestBody(1 << 20)
	f := &siaFake{cut: func(n int) int {
		if n == 0 {
			return 100_000
		}
		return -1
	}}
	f.setObject(body, "e1")
	d := newSiaTestDriver(t, f, siaReadConfig{Resumes: 8})
	before := resumeCount("resumed")

	// Act
	rc, err := d.Get(siaCtx(), "tnt_b", "k.bin")
	require.NoError(t, err)
	got, rerr := io.ReadAll(rc)
	_ = rc.Close()

	// Assert
	require.NoError(t, rerr)
	assert.True(t, bytes.Equal(body, got), "the exact object, nothing doubled")
	reqs := f.requests()
	require.Len(t, reqs, 2)
	assert.Equal(t, "", reqs[0].rng)
	assert.Equal(t, fmt.Sprintf("bytes=100000-%d", len(body)-1), reqs[1].rng, "from the received offset")
	assert.Equal(t, `"e1"`, reqs[1].ifMatch, "bound to the first answer's ETag")
	assert.Equal(t, before+1, resumeCount("resumed"))
}

func TestSiaGetRangeInfo_ResumesWithinTheRange(t *testing.T) {
	body := siaTestBody(1 << 20)
	f := &siaFake{cut: func(n int) int {
		if n < 3 {
			return 7_000 // three cuts in a row
		}
		return -1
	}}
	f.setObject(body, "e1")
	d := newSiaTestDriver(t, f, siaReadConfig{Resumes: 8})

	rc, info, err := d.GetRangeInfo(siaCtx(), "tnt_b", "k.bin", 200_000, 300_000)
	require.NoError(t, err)
	got, rerr := io.ReadAll(rc)
	_ = rc.Close()

	require.NoError(t, rerr)
	assert.True(t, bytes.Equal(body[200_000:500_000], got))
	assert.Equal(t, "e1", info.ETag)
	assert.Equal(t, int64(len(body)), info.Size)
	reqs := f.requests()
	require.Len(t, reqs, 4)
	assert.Equal(t, "bytes=200000-499999", reqs[0].rng)
	assert.Equal(t, "bytes=207000-499999", reqs[1].rng)
	assert.Equal(t, "bytes=214000-499999", reqs[2].rng)
	assert.Equal(t, "bytes=221000-499999", reqs[3].rng)
}

func TestSiaGetRange_Resumes(t *testing.T) {
	body := siaTestBody(512 << 10)
	f := &siaFake{cut: func(n int) int {
		if n == 0 {
			return 1
		}
		return -1
	}}
	f.setObject(body, "e1")
	d := newSiaTestDriver(t, f, siaReadConfig{Resumes: 8})

	rc, err := d.GetRange(siaCtx(), "tnt_b", "k.bin", 10, 100_000)
	require.NoError(t, err)
	got, rerr := io.ReadAll(rc)
	_ = rc.Close()

	require.NoError(t, rerr)
	assert.True(t, bytes.Equal(body[10:100_010], got))
}

func TestSiaGet_ObjectChangedBeforeTheResumeFailsTheBody(t *testing.T) {
	cases := map[string]bool{"412 to the If-Match": false, "another ETag answered": true}
	for name, ignoreIfMatch := range cases {
		t.Run(name, func(t *testing.T) {
			// Arrange: cut after 50,000 bytes; the key holds v2 by the resume.
			body := siaTestBody(256 << 10)
			f := &siaFake{ignoreIfMatch: ignoreIfMatch}
			f.cut = func(n int) int {
				if n == 0 {
					f.setObject(siaTestBody(256<<10 + 1)[:256<<10], "e2")
					return 50_000
				}
				return -1
			}
			f.setObject(body, "e1")
			d := newSiaTestDriver(t, f, siaReadConfig{Resumes: 8})
			before := resumeCount("changed")

			// Act
			rc, err := d.Get(siaCtx(), "tnt_b", "k.bin")
			require.NoError(t, err)
			got, rerr := io.ReadAll(rc)
			_ = rc.Close()

			// Assert
			require.Error(t, rerr)
			assert.ErrorIs(t, rerr, engine.ErrObjectChanged)
			assert.True(t, bytes.Equal(body[:len(got)], got), "only the first object's bytes")
			assert.Equal(t, 50_000, len(got))
			assert.Len(t, f.requests(), 2, "no retry once the object changed")
			assert.Equal(t, before+1, resumeCount("changed"))
		})
	}
}

func TestSiaGet_AResumeThatIsNotTheAskedRangeIsNeverServed(t *testing.T) {
	cases := map[string]func(f *siaFake){
		"200 whole object":   func(f *siaFake) { f.ignoreRange = true },
		"another start byte": func(f *siaFake) { f.skewStart = 1 },
	}
	for name, breakIt := range cases {
		t.Run(name, func(t *testing.T) {
			body := siaTestBody(256 << 10)
			f := &siaFake{cut: func(n int) int {
				if n == 0 {
					return 10_000
				}
				return -1
			}}
			breakIt(f)
			f.setObject(body, "e1")
			d := newSiaTestDriver(t, f, siaReadConfig{Resumes: 3})

			rc, err := d.Get(siaCtx(), "tnt_b", "k.bin")
			require.NoError(t, err)
			got, rerr := io.ReadAll(rc)
			_ = rc.Close()

			require.Error(t, rerr)
			assert.ErrorIs(t, rerr, ErrSiaReadResumeExhausted)
			assert.Equal(t, body[:10_000], got, "nothing from an answer that is not the asked range")
			assert.Len(t, f.requests(), 4, "the first answer + 3 resumes")
		})
	}
}

func TestSiaGet_ResumeBudgetExhaustedIsAnError(t *testing.T) {
	// Arrange: every answer is cut at once — the balances never refill.
	body := siaTestBody(128 << 10)
	f := &siaFake{cut: func(int) int { return 0 }}
	f.setObject(body, "e1")
	d := newSiaTestDriver(t, f, siaReadConfig{Resumes: 4})
	before := resumeCount("exhausted")

	// Act
	rc, err := d.Get(siaCtx(), "tnt_b", "k.bin")
	require.NoError(t, err)
	got, rerr := io.ReadAll(rc)
	_ = rc.Close()

	// Assert
	require.Error(t, rerr)
	assert.ErrorIs(t, rerr, ErrSiaReadResumeExhausted)
	assert.NotErrorIs(t, rerr, engine.ErrObjectChanged)
	assert.Empty(t, got)
	assert.Len(t, f.requests(), 5, "the first answer + SIA_READ_RESUMES resumes")
	assert.Equal(t, before+1, resumeCount("exhausted"))
}

func TestSiaGet_ResumeWaitBoundEndsTheRead(t *testing.T) {
	body := siaTestBody(128 << 10)
	f := &siaFake{cut: func(int) int { return 0 }}
	f.setObject(body, "e1")
	d := newSiaTestDriver(t, f, siaReadConfig{Resumes: 100, BackoffBase: 20 * time.Millisecond,
		BackoffCap: 20 * time.Millisecond, MaxWait: 100 * time.Millisecond})

	start := time.Now()
	rc, err := d.Get(siaCtx(), "tnt_b", "k.bin")
	require.NoError(t, err)
	_, rerr := io.ReadAll(rc)
	_ = rc.Close()

	assert.ErrorIs(t, rerr, ErrSiaReadResumeExhausted)
	assert.Less(t, time.Since(start), 2*time.Second)
	assert.Less(t, len(f.requests()), 10, "the wait bound, not the 100 attempts, ended it")
}

func TestSiaGet_AnIdleStreamIsWaitedForNotResumed(t *testing.T) {
	// The patched s3d waits for a refill instead of cutting: the body goes
	// quiet, then continues. Nothing is re-requested.
	body := siaTestBody(256 << 10)
	f := &siaFake{idleAt: func(n int) int { return 64 << 10 }, idleFor: 300 * time.Millisecond}
	f.setObject(body, "e1")
	d := newSiaTestDriver(t, f, siaReadConfig{Resumes: 8})

	rc, err := d.Get(siaCtx(), "tnt_b", "k.bin")
	require.NoError(t, err)
	got, rerr := io.ReadAll(rc)
	_ = rc.Close()

	require.NoError(t, rerr)
	assert.True(t, bytes.Equal(body, got))
	assert.Len(t, f.requests(), 1)
}

func TestSiaGet_CallerGoneDuringTheBackoffStopsAtOnce(t *testing.T) {
	body := siaTestBody(128 << 10)
	f := &siaFake{cut: func(int) int { return 1000 }}
	f.setObject(body, "e1")
	d := newSiaTestDriver(t, f, siaReadConfig{Resumes: 8, BackoffBase: time.Hour, BackoffCap: time.Hour, MaxWait: 2 * time.Hour})
	ctx, cancel := context.WithCancel(siaCtx())

	rc, err := d.Get(ctx, "tnt_b", "k.bin")
	require.NoError(t, err)
	time.AfterFunc(100*time.Millisecond, cancel)
	start := time.Now()
	_, rerr := io.ReadAll(rc)
	_ = rc.Close()

	assert.ErrorIs(t, rerr, context.Canceled)
	assert.Less(t, time.Since(start), 5*time.Second)
}

func TestSiaGet_ResumesOffKeepsTheCut(t *testing.T) {
	body := siaTestBody(128 << 10)
	f := &siaFake{cut: func(n int) int { return 1000 }}
	f.setObject(body, "e1")
	d := newSiaTestDriver(t, f, siaReadConfig{Resumes: 0})

	rc, err := d.Get(siaCtx(), "tnt_b", "k.bin")
	require.NoError(t, err)
	_, rerr := io.ReadAll(rc)
	_ = rc.Close()

	require.Error(t, rerr)
	assert.Len(t, f.requests(), 1)
}

// Zero behaviour change for the other fixed-bucket drivers: a cut body is
// the caller's error, nothing is re-requested, and their reads are hedged.
func TestFixedBucketDrivers_OtherThanSiaNeverResumeAndMayBeHedged(t *testing.T) {
	body := siaTestBody(128 << 10)
	f := &siaFake{cut: func(int) int { return 1000 }}
	f.setObject(body, "e1")
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	for _, name := range []string{"idrive", "wasabi", "lyve-shaped"} {
		t.Run(name, func(t *testing.T) {
			f.mu.Lock()
			f.reqs = nil
			f.mu.Unlock()
			d, err := NewFixedBucketS3Driver(name, "ak", "sk", srv.URL, "us-east-1", "vaultaire", zap.NewNop())
			require.NoError(t, err)
			assert.False(t, engine.IsNoHedge(d))

			rc, err := d.Get(siaCtx(), "tnt_b", "k.bin")
			require.NoError(t, err)
			_, rerr := io.ReadAll(rc)
			_ = rc.Close()

			require.Error(t, rerr)
			assert.True(t, errors.Is(rerr, io.ErrUnexpectedEOF), "the transport's own error: %v", rerr)
			assert.Len(t, f.requests(), 1)
		})
	}
	d, err := NewSiaDriver("ak", "sk", srv.URL, "vaultaire", zap.NewNop())
	require.NoError(t, err)
	assert.True(t, engine.IsNoHedge(d), "sia's reads are never hedged")
}

func TestSiaReadConfigFromEnv(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

	def := siaReadConfigFromEnv(env(nil), zap.NewNop())
	assert.Equal(t, 8, def.Resumes)
	assert.Equal(t, 10*time.Minute, def.MaxWait)
	assert.Equal(t, time.Second, def.BackoffBase)
	assert.Equal(t, time.Minute, def.BackoffCap)
	assert.Equal(t, int64(0), def.BudgetBytesPerSec, "no pacing by default")

	set := siaReadConfigFromEnv(env(map[string]string{
		"SIA_READ_RESUMES": "3", "SIA_READ_RESUME_MAX_WAIT": "90s", "SIA_READ_BUDGET": "25MiB",
	}), zap.NewNop())
	assert.Equal(t, 3, set.Resumes)
	assert.Equal(t, 90*time.Second, set.MaxWait)
	assert.Equal(t, int64(25<<20), set.BudgetBytesPerSec)

	for _, off := range []string{"0", "off", "OFF"} {
		assert.Equal(t, int64(0), siaReadConfigFromEnv(env(map[string]string{"SIA_READ_BUDGET": off}), zap.NewNop()).BudgetBytesPerSec)
	}
	assert.Equal(t, 0, siaReadConfigFromEnv(env(map[string]string{"SIA_READ_RESUMES": "0"}), zap.NewNop()).Resumes)

	bad := siaReadConfigFromEnv(env(map[string]string{
		"SIA_READ_RESUMES": "-1", "SIA_READ_RESUME_MAX_WAIT": "forever", "SIA_READ_BUDGET": "lots",
	}), zap.NewNop())
	assert.Equal(t, def, bad, "a rejected value keeps the default")
}

func TestSiaReadBudget_PacesDeliveredBytes(t *testing.T) {
	// Arrange: 2 MiB/s, a 1.5 MiB object — about 0.5 s past the burst.
	body := siaTestBody(1536 << 10)
	f := &siaFake{}
	f.setObject(body, "e1")
	d := newSiaTestDriver(t, f, siaReadConfig{Resumes: 8, BudgetBytesPerSec: 2 << 20})
	waited := testutil.ToFloat64(siaReadBudgetWait)

	// Act
	start := time.Now()
	rc, err := d.Get(siaCtx(), "tnt_b", "k.bin")
	require.NoError(t, err)
	got, rerr := io.ReadAll(rc)
	_ = rc.Close()
	took := time.Since(start)

	// Assert
	require.NoError(t, rerr)
	assert.True(t, bytes.Equal(body, got))
	assert.GreaterOrEqual(t, took, 300*time.Millisecond, "paced at the budget")
	assert.Greater(t, testutil.ToFloat64(siaReadBudgetWait), waited)
	assert.Equal(t, float64(2<<20), testutil.ToFloat64(siaReadBudgetRate))
}
