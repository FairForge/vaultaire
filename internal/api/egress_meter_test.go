package api

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/FairForge/vaultaire/internal/usage"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// The egress month counter (WP-R10-9, part B): one load per tenant, then
// memory. These tests replace the two reads with counting stubs; the
// DB-backed behaviour is in egress_e2e_test.go.

type meterStub struct {
	m          *egressMeter
	clock      *fakeClock
	usedLoads  atomic.Int64
	allowLoads atomic.Int64
	base       atomic.Int64 // what the "database" holds for the month
	allowance  atomic.Int64
	enforced   atomic.Bool
	loadDelay  time.Duration
	loadErr    atomic.Pointer[error]
}

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) set(t time.Time) {
	c.mu.Lock()
	c.t = t
	c.mu.Unlock()
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func newMeterStub(cfg usage.EgressThrottle) *meterStub {
	s := &meterStub{clock: &fakeClock{t: time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)}}
	s.m = newEgressMeter(nil, cfg, func(string) bool { return s.enforced.Load() }, zap.NewNop())
	s.m.now = s.clock.now
	s.m.loadUsed = func(context.Context, string, time.Time) (int64, error) {
		s.usedLoads.Add(1)
		if s.loadDelay > 0 {
			time.Sleep(s.loadDelay)
		}
		if e := s.loadErr.Load(); e != nil {
			return 0, *e
		}
		return s.base.Load(), nil
	}
	s.m.loadAllowance = func(context.Context, string) (usage.EgressAllowance, error) {
		s.allowLoads.Add(1)
		a := s.allowance.Load()
		return usage.EgressAllowance{Bytes: a, PlanBytes: a, Source: usage.EgressSourceQuota}, nil
	}
	return s
}

func TestEgressMeter_OneLoadThenNoQueryPerRequest(t *testing.T) {
	// Arrange
	s := newMeterStub(usage.DefaultEgressThrottle())
	s.base.Store(1000)
	s.allowance.Store(1 << 30)
	ctx := context.Background()

	// Act: 500 requests, each counting bytes.
	for i := 0; i < 500; i++ {
		s.m.span(ctx, "tenant-a").count(10)
	}

	// Assert: the month was read once, the allowance once (inside its TTL),
	// and the counter is the loaded base plus the live bytes.
	assert.Equal(t, int64(1), s.usedLoads.Load(), "one SUM per tenant per month, never one per request")
	assert.Equal(t, int64(1), s.allowLoads.Load())
	used, err := s.m.usedBytes(ctx, "tenant-a", s.clock.now())
	require.NoError(t, err)
	assert.Equal(t, int64(1000+500*10), used)
	assert.Equal(t, int64(1), s.usedLoads.Load(), "reading the counter does not query either")
}

func TestEgressMeter_CountsBytesWhileTheResponseIsOpen(t *testing.T) {
	// Arrange: a response writer with a live span, as handleS3Request builds it.
	s := newMeterStub(usage.DefaultEgressThrottle())
	s.allowance.Store(1 << 30)
	ctx := context.Background()
	cw := &countingResponseWriter{ResponseWriter: httptest.NewRecorder(), span: s.m.span(ctx, "tenant-open")}

	// Act: half the body is written; the response has not ended.
	_, err := cw.Write(make([]byte, 4096))
	require.NoError(t, err)

	// Assert: the counter already has it.
	used, err := s.m.usedBytes(ctx, "tenant-open", s.clock.now())
	require.NoError(t, err)
	assert.Equal(t, int64(4096), used)
}

func TestCountingResponseWriter_HeadCountsNothing(t *testing.T) {
	// net/http accepts and discards a body written to a HEAD response; an
	// error document "written" to one was never sent.
	s := newMeterStub(usage.DefaultEgressThrottle())
	cw := &countingResponseWriter{ResponseWriter: httptest.NewRecorder(), head: true,
		span: s.m.span(context.Background(), "tenant-head")}

	n, err := cw.Write([]byte("<Error>NoSuchKey</Error>"))

	require.NoError(t, err)
	assert.Equal(t, 24, n)
	assert.Equal(t, int64(0), cw.bytesWritten)
	used, _ := s.m.usedBytes(context.Background(), "tenant-head", s.clock.now())
	assert.Equal(t, int64(0), used)
}

func TestEgressMeter_UTCMonthRollover(t *testing.T) {
	// Arrange: one second before the UTC month ends, seen from Denver where
	// it is still the afternoon of the 31st.
	s := newMeterStub(usage.DefaultEgressThrottle())
	denver := time.FixedZone("MDT", -6*3600)
	s.clock.set(time.Date(2026, 10, 31, 17, 59, 59, 0, denver))
	s.base.Store(900)
	s.allowance.Store(1000)
	s.enforced.Store(true)
	ctx := context.Background()
	span := s.m.span(ctx, "tenant-roll")
	span.count(200) // 1100 of 1000: over
	tn := s.m.tenant(ctx, "tenant-roll")
	require.True(t, s.m.decide(ctx, tn).over)

	// Act: the same response keeps writing after midnight UTC.
	s.clock.advance(2 * time.Second)
	s.base.Store(0) // the database holds nothing for November
	span.count(50)

	// Assert: November starts at the bytes written in November; the bytes
	// written in October are carried so the flush can date them there.
	used, err := s.m.usedBytes(ctx, "tenant-roll", s.clock.now())
	require.NoError(t, err)
	assert.Equal(t, int64(50), used)
	assert.False(t, s.m.decide(ctx, tn).over, "the allowance resets with the UTC month")
	assert.Equal(t, int64(200), span.carried)
	assert.Equal(t, int64(250), span.total)
	assert.Equal(t, int64(1), s.usedLoads.Load(), "rollover needs no reload: the process saw every byte of the new month")

	// A request that starts in the new month on an idle entry also rolls over.
	s2 := newMeterStub(usage.DefaultEgressThrottle())
	s2.clock.set(time.Date(2026, 10, 31, 23, 0, 0, 0, time.UTC))
	s2.base.Store(5000)
	s2.m.tenant(ctx, "tenant-idle")
	s2.clock.advance(2 * time.Hour)
	used, err = s2.m.usedBytes(ctx, "tenant-idle", s2.clock.now())
	require.NoError(t, err)
	assert.Equal(t, int64(0), used)
	assert.Equal(t, int64(0), s2.m.tenant(ctx, "tenant-idle").used.Load())
}

func TestBandwidthTracker_SplitsAResponseOpenAcrossTheMonthBoundary(t *testing.T) {
	// Arrange: a response that wrote 200 bytes in October and 50 in November.
	s := newMeterStub(usage.DefaultEgressThrottle())
	s.clock.set(time.Date(2026, 10, 31, 23, 59, 59, 0, time.UTC))
	cw := &countingResponseWriter{ResponseWriter: httptest.NewRecorder(), span: s.m.span(context.Background(), "tenant-split")}
	_, _ = cw.Write(make([]byte, 200))
	s.clock.advance(2 * time.Second)
	_, _ = cw.Write(make([]byte, 50))
	bt := NewBandwidthTracker(nil)

	// Act
	bt.recordResponse("tenant-split", "local", 7, cw)

	// Assert: two events — the carried bytes on the last day of the month
	// they were written in, the rest (and the ingress) today.
	bt.mu.Lock()
	defer bt.mu.Unlock()
	require.Len(t, bt.buffer, 2)
	today := time.Now().UTC()
	lastMonthEnd := time.Date(today.Year(), today.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, 0, -1)
	assert.Equal(t, lastMonthEnd.Format("2006-01-02"), bt.buffer[0].day)
	assert.Equal(t, int64(200), bt.buffer[0].egress)
	assert.Equal(t, int64(0), bt.buffer[0].ingress)
	assert.Equal(t, today.Format("2006-01-02"), bt.buffer[1].day)
	assert.Equal(t, int64(50), bt.buffer[1].egress)
	assert.Equal(t, int64(7), bt.buffer[1].ingress)
}

func TestBandwidthTracker_EventsLandOnTheUTCDay(t *testing.T) {
	bt := NewBandwidthTracker(nil)
	denver := time.FixedZone("MDT", -6*3600)

	bt.recordOn(time.Date(2026, 10, 31, 19, 0, 0, 0, denver), "tenant-utc", "", 0, 10)

	bt.mu.Lock()
	defer bt.mu.Unlock()
	require.Len(t, bt.buffer, 1)
	assert.Equal(t, "2026-11-01", bt.buffer[0].day, "the row's date is the UTC date, whatever the session time zone")
}

func TestEgressMeter_BoundedMap(t *testing.T) {
	// Arrange: room for three tenants.
	s := newMeterStub(usage.DefaultEgressThrottle())
	s.m.maxTenants = 3
	ctx := context.Background()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/b/k", nil)
	held, ok := s.m.admit(rec, req, "busy", egressSurfaceS3) // an open response pins its entry
	require.True(t, ok)
	s.m.tenant(ctx, "idle-1")
	s.m.tenant(ctx, "idle-2")

	// Act 1: a fourth tenant while everything is fresh — nothing is evicted
	// (an entry younger than the flush interval may hold unflushed bytes),
	// and the map does not grow.
	s.m.span(ctx, "fourth").count(5)

	// Assert 1
	s.m.mu.Lock()
	assert.Len(t, s.m.tenants, 3)
	_, kept := s.m.tenants["fourth"]
	s.m.mu.Unlock()
	assert.False(t, kept, "a full map of fresh tenants serves the newcomer without keeping it")

	// Act 2: the idle ones age out; the newcomer takes a slot.
	s.clock.advance(egressIdleEvict + time.Second)
	s.m.span(ctx, "fourth").count(5)

	// Assert 2: idle entries were dropped, the one with an open response kept.
	s.m.mu.Lock()
	_, busyKept := s.m.tenants["busy"]
	_, idleKept := s.m.tenants["idle-1"]
	_, fourthKept := s.m.tenants["fourth"]
	size := len(s.m.tenants)
	s.m.mu.Unlock()
	assert.True(t, busyKept, "an entry with an open response is never evicted")
	assert.False(t, idleKept)
	assert.True(t, fourthKept)
	assert.LessOrEqual(t, size, 3)
	held.close()

	// Act 3: 500 more tenants never push the map past its bound.
	for i := 0; i < 500; i++ {
		s.clock.advance(time.Second)
		s.m.tenant(ctx, fmt.Sprintf("flood-%d", i))
	}
	s.m.mu.Lock()
	assert.LessOrEqual(t, len(s.m.tenants), 3)
	s.m.mu.Unlock()
}

func TestEgressMeter_RacingFirstLoadRunsOnce(t *testing.T) {
	// Arrange: a slow first load and fifty requests arriving together.
	s := newMeterStub(usage.DefaultEgressThrottle())
	s.base.Store(1_000_000)
	s.allowance.Store(1 << 40)
	s.loadDelay = 80 * time.Millisecond
	ctx := context.Background()

	// Act
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.m.span(ctx, "tenant-race").count(100)
		}()
	}
	wg.Wait()

	// Assert: one load; no byte counted before it finished was lost or
	// counted twice.
	assert.Equal(t, int64(1), s.usedLoads.Load())
	used, err := s.m.usedBytes(ctx, "tenant-race", s.clock.now())
	require.NoError(t, err)
	assert.Equal(t, int64(1_000_000+50*100), used)
}

func TestEgressMeter_AllowanceChangeArrivesWithinTheTTL(t *testing.T) {
	// Arrange: a tenant past a 1,000-byte allowance.
	s := newMeterStub(usage.DefaultEgressThrottle())
	s.base.Store(5000)
	s.allowance.Store(1000)
	ctx := context.Background()
	tn := s.m.tenant(ctx, "tenant-buy")
	require.True(t, s.m.decide(ctx, tn).over)

	// Act: they buy quota (or an admin raises the override).
	s.allowance.Store(1 << 30)
	stillOver := s.m.decide(ctx, tn).over
	s.clock.advance(egressAllowanceTTL + time.Second)
	afterTTL := s.m.decide(ctx, tn).over

	// Assert: cached for the TTL, lifted after it; the TTL is a minute or less.
	assert.True(t, stillOver)
	assert.False(t, afterTTL)
	assert.LessOrEqual(t, egressAllowanceTTL, 60*time.Second)
	assert.Equal(t, int64(2), s.allowLoads.Load())
}

func TestEgressMeter_LoadFailureFailsOpenAndRetries(t *testing.T) {
	// Arrange: the database is down when the tenant's first request arrives.
	s := newMeterStub(usage.DefaultEgressThrottle())
	s.base.Store(5000)
	s.allowance.Store(1000)
	s.enforced.Store(true)
	boom := errors.New("connection refused")
	s.loadErr.Store(&boom)
	ctx := context.Background()

	// Act
	tn := s.m.tenant(ctx, "tenant-down")
	during := s.m.decide(ctx, tn)
	s.loadErr.Store(nil)
	tn = s.m.tenant(ctx, "tenant-down")
	after := s.m.decide(ctx, tn)

	// Assert: never throttled on a guess; the next request loads.
	assert.False(t, during.over)
	assert.True(t, after.over)
	assert.Equal(t, int64(2), s.usedLoads.Load())
}

func TestEgressMeter_StatusNeverGrowsTheMap(t *testing.T) {
	s := newMeterStub(usage.DefaultEgressThrottle())
	s.base.Store(300)
	s.allowance.Store(1000)

	st, err := s.m.EgressStatus(context.Background(), "tenant-peek")

	require.NoError(t, err)
	assert.Equal(t, int64(300), st.UsedBytes)
	assert.Equal(t, int64(1000), st.Allowance.Bytes)
	assert.False(t, st.Over)
	assert.Equal(t, int64(65536), st.RateBytesPerSec)
	assert.Equal(t, time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), st.ResetAt)
	s.m.mu.Lock()
	defer s.m.mu.Unlock()
	assert.Empty(t, s.m.tenants, "the dashboard and the alerter read without caching")
}

func TestEgressMeter_NoQuotaRowMeansNoAllowance(t *testing.T) {
	s := newMeterStub(usage.DefaultEgressThrottle())
	s.enforced.Store(true)
	s.m.loadAllowance = func(context.Context, string) (usage.EgressAllowance, error) {
		return usage.EgressAllowance{}, sql.ErrNoRows
	}
	ctx := context.Background()
	s.m.span(ctx, "tenant-norow").count(1 << 30)

	assert.False(t, s.m.decide(ctx, s.m.tenant(ctx, "tenant-norow")).over)
}

func TestEgressThrottleFromEnv(t *testing.T) {
	t.Run("defaults", func(t *testing.T) {
		cfg := egressThrottleFromEnv(func(string) string { return "" }, zap.NewNop())

		assert.Equal(t, usage.DefaultEgressThrottle(), cfg)
		assert.Equal(t, int64(65536), cfg.MinBytesPerSec)
		assert.Equal(t, 1.0, cfg.Factor)
		assert.Equal(t, 16, cfg.MaxStreams)
	})

	t.Run("valid values are taken", func(t *testing.T) {
		env := map[string]string{
			"EGRESS_THROTTLE_MIN_BYTES_PER_SEC": "1048576",
			"EGRESS_THROTTLE_FACTOR":            "2.5",
			"EGRESS_THROTTLE_MAX_STREAMS":       "4",
		}

		cfg := egressThrottleFromEnv(func(k string) string { return env[k] }, zap.NewNop())

		assert.Equal(t, usage.EgressThrottle{MinBytesPerSec: 1 << 20, Factor: 2.5, MaxStreams: 4}, cfg)
	})

	t.Run("a rejected value is logged at Warn and the default kept", func(t *testing.T) {
		env := map[string]string{
			"EGRESS_THROTTLE_MIN_BYTES_PER_SEC": "0",
			"EGRESS_THROTTLE_FACTOR":            "-1",
			"EGRESS_THROTTLE_MAX_STREAMS":       "many",
		}
		core, logs := observer.New(zap.WarnLevel)

		cfg := egressThrottleFromEnv(func(k string) string { return env[k] }, zap.New(core))

		assert.Equal(t, usage.DefaultEgressThrottle(), cfg)
		assert.Equal(t, 3, logs.Len())
	})
}
