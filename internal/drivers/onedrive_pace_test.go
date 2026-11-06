package drivers

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/FairForge/vaultaire/internal/common"
	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// WP-FLEET-3 PR B (D-F3-3 … D-F3-8, D-F3-5a, D-F3-10): the driver paces at
// Microsoft's published rates per account AND per tenant, a 429 holds the
// whole account for its Retry-After, a slow range is hedged, a fragment that
// exhausted its attempts resumes its session, traffic is decorated, and a
// token renewal that fails keeps the valid token without a storm.

// ── D-F3-5 / D-F3-5a: the buckets ───────────────────────────────────────────

func TestPermafrostPace_KnobsAreThePublishedRatesAndRejectedValuesKeepTheDefault(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

	assert.Equal(t, odPaceConfig{AccountRPS: 8, AccountInGBh: 50, AccountOutGBh: 100, TenantGBh: 400},
		odPaceFromEnv(env(nil), zap.NewNop()))
	assert.Equal(t, odPaceConfig{AccountRPS: 8},
		odPaceFromEnv(env(map[string]string{"PERMAFROST_ACCOUNT_IN_GBH": "0", "PERMAFROST_ACCOUNT_OUT_GBH": "off", "PERMAFROST_TENANT_GBH": "OFF"}), zap.NewNop()),
		"0 / off turns a bucket off")

	core, logs := observer.New(zap.WarnLevel)
	got := odPaceFromEnv(env(map[string]string{"PERMAFROST_ACCOUNT_RPS": "fast", "PERMAFROST_TENANT_GBH": "-3", "PERMAFROST_ACCOUNT_IN_GBH": "200"}), zap.New(core))
	assert.Equal(t, odPaceConfig{AccountRPS: 8, AccountInGBh: 200, AccountOutGBh: 100, TenantGBh: 400}, got)
	assert.Equal(t, 2, logs.Len(), "one Warn per rejected value")
}

func TestPermafrostPace_ARequestPastTheBurstWaitsItsTurn(t *testing.T) {
	// Arrange: one account at the default 8 requests/s, burst 100.
	rt := &odScriptRT{fn: func(_ int, req *http.Request) (*http.Response, error) { return odResp(req, 200, `{}`, nil), nil }}
	tn := scriptedTenant(rt, nil)
	tn.pace = newODPace("a", "dir-a", odPaceDefaults)
	ctx := context.Background()
	for i := 0; i < odRequestBurst; i++ {
		resp, err := tn.graphDo(ctx, graphGet(ctx, t))
		require.NoError(t, err)
		_ = resp.Body.Close()
	}

	// Act: the 101st within the same second.
	start := time.Now()
	resp, err := tn.graphDo(ctx, graphGet(ctx, t))

	// Assert: it waited for a token (1/8 s), it was not refused.
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.GreaterOrEqual(t, time.Since(start), 80*time.Millisecond)
	assert.Equal(t, odRequestBurst+1, rt.calls())
}

func TestPermafrostPace_TheTenantBucketIsSharedByItsAccounts(t *testing.T) {
	a := newODPace("acct-1", "dir-shared-test", odPaceDefaults)
	b := newODPace("acct-2", "dir-shared-test", odPaceDefaults)
	c := newODPace("acct-3", "dir-other-test", odPaceDefaults)

	require.NotNil(t, a.tenant)
	assert.Same(t, a.tenant, b.tenant, "two slots of one tenant share its 400 GB/h")
	assert.NotSame(t, a.tenant, c.tenant)
	assert.NotSame(t, a.out, b.out, "each account keeps its own 100 GB/h")
	assert.Nil(t, newODPace("acct-4", "dir-off-test", odPaceConfig{AccountRPS: 8}).tenant)
}

func TestPermafrostPace_AnUploadIsPacedOnItsSourceAndARetryTakesNoBytesAgain(t *testing.T) {
	// Arrange: an in-bucket of 32 MiB/s holding 32 MiB; a 96 MiB body whose
	// second fragment is refused once (503).
	ctx := common.WithTenantID(context.Background(), "x")
	const size = 96 << 20
	sink := &uploadSink{}
	var puts atomic.Int32
	flaky := odRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.Method == http.MethodPut && strings.HasPrefix(req.Header.Get("Content-Range"), fmt.Sprintf("bytes %d-", odChunkSize)) && puts.Add(1) == 1 {
			_, _ = io.Copy(io.Discard, req.Body)
			return odResp(req, 503, "", nil), nil
		}
		return sink.RoundTrip(req)
	})
	d := uploadFleet(sink)
	tn := d.tenants[0]
	tn.uploadHTTP = &http.Client{Transport: flaky}
	tn.wait = (&waitLog{}).wait
	var inWait atomic.Int64
	tn.pace = &odPace{in: newPacer("test in", 32<<20, 32<<20, func(w time.Duration) { inWait.Add(int64(w)) })}

	// Act
	start := time.Now()
	var fromSource atomic.Int64
	err := d.Put(ctx, "bkt", "big.bin", &odPatternReader{0, size, &fromSource}, engine.WithContentLength(size))

	// Assert: 96 MiB at 32 MiB/s with 32 MiB up front ≈ 2 s. The bucket sits
	// on the SOURCE, and the source was read exactly once: the refused
	// fragment was re-sent from its buffer, so its bytes were never taken
	// from the bucket again (a wall-clock upper bound flaked on a loaded
	// CI runner, 2026-11-06, and would measure the quickXorHash too).
	require.NoError(t, err)
	require.NoError(t, sink.bad)
	assert.GreaterOrEqual(t, time.Since(start), 1500*time.Millisecond, "the source was read at the bucket's rate")
	assert.Equal(t, int64(size), fromSource.Load(), "a retry re-sends the buffered fragment, never re-reads (re-paces) it")
	assert.Equal(t, int32(2), puts.Load(), "the refused fragment's two attempts")
}

// ── D-F3-6: a 429 holds the whole account ───────────────────────────────────

func TestPermafrostPace_A429HoldsTheAccountForItsRetryAfter(t *testing.T) {
	// Arrange: a range refused with Retry-After 40, then served.
	ctx := common.WithTenantID(context.Background(), "x")
	cdn := &burstCDN{size: burstObject, fail: func(s int64, n int, req *http.Request) *http.Response {
		if s == 0 && n == 0 {
			return throttled(req, "40")
		}
		return nil
	}}
	d, wl := burstFleet(ctx, t, cdn)
	tn := d.tenants[0]
	before := testutil.ToFloat64(permafrostThrottles.WithLabelValues(tn.name, "range"))

	// Act
	_, sum, err := burstRead(ctx, t, d)
	require.NoError(t, err)
	assert.Equal(t, patternSum(burstObject), sum)

	// Assert: the account is held ~40 s (every other caller waits for it),
	// and the throttle is counted under its op.
	until := time.Unix(0, tn.throttledUntil.Load())
	assert.InDelta(t, 40, time.Until(until).Seconds(), 2)
	assert.Equal(t, before+1, testutil.ToFloat64(permafrostThrottles.WithLabelValues(tn.name, "range")))
	assert.NotEmpty(t, wl.all())

	// And a Graph call on the same account now waits the hold out first.
	wl2 := &waitLog{}
	tn.wait = wl2.wait
	rt := &odScriptRT{fn: func(_ int, req *http.Request) (*http.Response, error) { return odResp(req, 200, `{}`, nil), nil }}
	tn.graphHTTP = &http.Client{Transport: rt}
	resp, err := tn.graphDo(context.Background(), graphGet(context.Background(), t))
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Len(t, wl2.all(), 1)
	assert.InDelta(t, 40, wl2.all()[0].Seconds(), 2)
}

func TestPermafrostPace_AHoldPastTheDeadlineIsAThrottleAtOnce(t *testing.T) {
	rt := &odScriptRT{fn: func(_ int, req *http.Request) (*http.Response, error) { return odResp(req, 200, `{}`, nil), nil }}
	tn := scriptedTenant(rt, &waitLog{})
	tn.hold("graph", time.Minute)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := tn.graphDo(ctx, graphGet(ctx, t))

	var thr *errODThrottled
	require.ErrorAs(t, err, &thr)
	assert.Equal(t, 0, rt.calls(), "nothing is sent while the account is held")
}

func TestPermafrostPace_AGraph429HoldsTheAccountAndIsCounted(t *testing.T) {
	rt := &odScriptRT{fn: func(n int, req *http.Request) (*http.Response, error) {
		if n == 0 {
			return throttled(req, "7"), nil
		}
		return odResp(req, 200, `{}`, nil), nil
	}}
	tn := scriptedTenant(rt, &waitLog{})
	tn.name = "graph-429-test"
	InitPermafrostPaceSeries(tn.name)
	before := testutil.ToFloat64(permafrostThrottles.WithLabelValues(tn.name, "graph"))

	resp, err := tn.graphDo(context.Background(), graphGet(context.Background(), t))

	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Equal(t, before+1, testutil.ToFloat64(permafrostThrottles.WithLabelValues(tn.name, "graph")))
	assert.Greater(t, tn.throttledUntil.Load(), time.Now().Add(5*time.Second).UnixNano(), "held for the Retry-After")
}

func TestPermafrostPace_RateLimitHeadersBelowTwentyPercentHoldUntilTheReset(t *testing.T) {
	rt := &odScriptRT{fn: func(_ int, req *http.Request) (*http.Response, error) {
		return odResp(req, 200, `{}`, map[string]string{"RateLimit-Limit": "1000", "RateLimit-Remaining": "150", "RateLimit-Reset": "9"}), nil
	}}
	tn := scriptedTenant(rt, &waitLog{})

	resp, err := tn.graphDo(context.Background(), graphGet(context.Background(), t))

	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.InDelta(t, 9, time.Until(time.Unix(0, tn.throttledUntil.Load())).Seconds(), 1.5)

	// 30 % left: no hold.
	tn2 := scriptedTenant(&odScriptRT{fn: func(_ int, req *http.Request) (*http.Response, error) {
		return odResp(req, 200, `{}`, map[string]string{"RateLimit-Limit": "1000", "RateLimit-Remaining": "300", "RateLimit-Reset": "9"}), nil
	}}, &waitLog{})
	resp, err = tn2.graphDo(context.Background(), graphGet(context.Background(), t))
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Equal(t, int64(0), tn2.throttledUntil.Load())
}

// ── D-F3-4: a slow range is hedged ──────────────────────────────────────────

func TestPermafrostRangeHedge_ASlowRangeIsAskedAgainAndTheFirstCopyWins(t *testing.T) {
	// Arrange: the first request for range 0 never answers.
	ctx := common.WithTenantID(context.Background(), "x")
	cdn := &burstCDN{size: burstObject, fail: func(s int64, n int, req *http.Request) *http.Response {
		if s == 0 && n == 0 {
			<-req.Context().Done()
			return odResp(req, 499, "", nil)
		}
		return nil
	}}
	d, _ := burstFleet(ctx, t, cdn)
	d.tenants[0].rangeHedgeAfter = 300 * time.Millisecond
	won := testutil.ToFloat64(permafrostRangeHedges.WithLabelValues("won"))

	// Act
	start := time.Now()
	_, sum, err := burstRead(ctx, t, d)

	// Assert
	require.NoError(t, err)
	assert.Equal(t, patternSum(burstObject), sum)
	assert.Less(t, time.Since(start), 5*time.Second)
	assert.Equal(t, 2, cdn.attemptsAt(0), "the hung range: its attempt and one hedge")
	// At least range 0's hedge won; on a loaded runner another range may
	// pass the delay too and be hedged as well (CI, 2026-11-06).
	assert.GreaterOrEqual(t, testutil.ToFloat64(permafrostRangeHedges.WithLabelValues("won")), won+1)
}

func TestPermafrostRangeHedge_OffAtZero(t *testing.T) {
	assert.Equal(t, 2*time.Second, odRangeHedgeFromEnv(func(string) string { return "" }, zap.NewNop()))
	assert.Equal(t, time.Duration(0), odRangeHedgeFromEnv(func(string) string { return "0" }, zap.NewNop()))
	assert.Equal(t, time.Duration(0), odRangeHedgeFromEnv(func(string) string { return "off" }, zap.NewNop()))
	assert.Equal(t, 2*time.Second, odRangeHedgeFromEnv(func(string) string { return "1h" }, zap.NewNop()), "rejected: default")
}

// ── D-F3-3: a fragment that exhausted its attempts resumes the session ──────

// sessionSink is uploadSink with a session query and a scripted fragment.
type sessionSink struct {
	uploadSink
	failAt   int64 // the fragment start refused on every attempt (-1 none)
	landed   bool  // the refused fragment's bytes DID land (its answer was lost)
	expected string
	status   int
	queries  atomic.Int32
	seen     sync.Map // fragment starts that reached the sink
}

func (s *sessionSink) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method == http.MethodGet && req.URL.Host == "upload.example.test" {
		s.queries.Add(1)
		defer func() { s.failAt = -1 }() // after the query, every send is accepted
		if s.status != 0 {
			return odResp(req, s.status, `{"error":{"code":"itemNotFound"}}`, nil), nil
		}
		return odResp(req, 200, `{"nextExpectedRanges":["`+s.expected+`"]}`, nil), nil
	}
	if req.Method == http.MethodPut && req.URL.Host == "upload.example.test" {
		var st int64
		_, _ = fmt.Sscanf(req.Header.Get("Content-Range"), "bytes %d-", &st)
		if st == s.failAt {
			if s.landed {
				s.landed = false // once: the session holds it now
				_, _ = s.uploadSink.RoundTrip(req)
			} else {
				_, _ = io.Copy(io.Discard, req.Body)
			}
			return odResp(req, 503, "", nil), nil
		}
		s.seen.Store(st, true)
	}
	return s.uploadSink.RoundTrip(req)
}

func sessionFleet(s *sessionSink) *OneDriveDriver {
	d, _ := stubFleet(1, zap.NewNop())
	tn := d.tenants[0]
	tn.graphHTTP = &http.Client{Transport: s}
	tn.uploadHTTP = &http.Client{Transport: s}
	tn.rootFolderID = "ROOT"
	tn.wait = (&waitLog{}).wait
	return d
}

func TestPermafrostResume_AFragmentThatExhaustedItsAttemptsIsResentFromTheSession(t *testing.T) {
	ctx := common.WithTenantID(context.Background(), "x")
	const size = 130 << 20
	s := &sessionSink{failAt: odChunkSize, expected: fmt.Sprintf("%d-", odChunkSize)}
	d := sessionFleet(s)
	before := testutil.ToFloat64(permafrostFragmentResumes.WithLabelValues("resumed"))

	err := d.Put(ctx, "bkt", "big.bin", &odPatternReader{0, size, nil}, engine.WithContentLength(size))

	require.NoError(t, err)
	require.NoError(t, s.bad)
	assert.Equal(t, int64(size), s.received)
	assert.Equal(t, int32(1), s.queries.Load())
	assert.Equal(t, before+1, testutil.ToFloat64(permafrostFragmentResumes.WithLabelValues("resumed")))
}

func TestPermafrostResume_AFragmentTheSessionAlreadyHoldsIsSkipped(t *testing.T) {
	ctx := common.WithTenantID(context.Background(), "x")
	const size = 130 << 20
	s := &sessionSink{failAt: odChunkSize, expected: fmt.Sprintf("%d-", 2*odChunkSize), landed: true}
	d := sessionFleet(s)

	err := d.Put(ctx, "bkt", "big.bin", &odPatternReader{0, size, nil}, engine.WithContentLength(size))

	require.NoError(t, err)
	_, resent := s.seen.Load(int64(odChunkSize))
	assert.False(t, resent, "the session's view wins: nothing re-sent after it")
	assert.Equal(t, int64(size), s.received)
}

func TestPermafrostResume_AGoneSessionRestartsOnlyARewindableBody(t *testing.T) {
	ctx := common.WithTenantID(context.Background(), "x")
	const size = 70 << 20

	t.Run("streamed: the error it was", func(t *testing.T) {
		s := &sessionSink{failAt: odChunkSize, status: 404}
		d := sessionFleet(s)
		err := d.Put(ctx, "bkt", "big.bin", &odPatternReader{0, size, nil}, engine.WithContentLength(size))
		require.Error(t, err)
	})

	t.Run("seekable: one restart", func(t *testing.T) {
		s := &sessionSink{failAt: odChunkSize, status: 404}
		d := sessionFleet(s)
		body := make([]byte, size)
		_, _ = io.ReadFull(&odPatternReader{0, size, nil}, body)
		restarted := testutil.ToFloat64(permafrostFragmentResumes.WithLabelValues("restarted"))

		// Seekable but no io.ReaderAt: a re-readable body ≤ 200 MiB would be
		// one simple PUT (WP-FLEET-4), never a session to restart.
		seekable := struct{ io.ReadSeeker }{bytes.NewReader(body)}
		err := d.Put(ctx, "bkt", "big.bin", seekable, engine.WithContentLength(size))

		require.NoError(t, err)
		require.NoError(t, s.bad)
		assert.Equal(t, restarted+1, testutil.ToFloat64(permafrostFragmentResumes.WithLabelValues("restarted")))
		assert.Equal(t, int64(size+odChunkSize), s.received, "the first session's fragment, then the whole body again")
	})
}

// ── D-F3-7: decorated traffic ───────────────────────────────────────────────

func TestPermafrost_EveryRequestCarriesTheISVUserAgent(t *testing.T) {
	SetPermafrostBuild("abc123def456")
	defer SetPermafrostBuild("dev")
	var mu sync.Mutex
	var agents []string
	rec := odRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		mu.Lock()
		agents = append(agents, req.Header.Get("User-Agent"))
		mu.Unlock()
		return (&burstCDN{size: burstObject}).RoundTrip(req)
	})
	ctx := common.WithTenantID(context.Background(), "x")
	d := fleetWithCDN(ctx, t, burstObject, rec)
	d.tenants[0].uploadHTTP = &http.Client{Transport: odRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		mu.Lock()
		agents = append(agents, req.Header.Get("User-Agent"))
		mu.Unlock()
		_, _ = io.Copy(io.Discard, req.Body)
		return odResp(req, 201, `{}`, nil), nil
	})}

	_, _, err := burstRead(ctx, t, d)
	require.NoError(t, err)
	require.NoError(t, func() error {
		_, err := d.tenants[0].putChunk(context.Background(), "https://upload.example.test/s", []byte("abc"), 0, 2, 3)
		return err
	}())

	mu.Lock()
	defer mu.Unlock()
	require.NotEmpty(t, agents)
	for _, a := range agents {
		assert.Equal(t, "ISV|Stored|permafrost/abc123def456", a)
	}
}

// ── D-F3-10: a renewal that fails keeps the valid token, no storm ───────────

type flakyCred struct {
	calls atomic.Int32
	fail  atomic.Bool
}

func (c *flakyCred) GetToken(context.Context, policy.TokenRequestOptions) (azcore.AccessToken, error) {
	c.calls.Add(1)
	time.Sleep(20 * time.Millisecond)
	if c.fail.Load() {
		return azcore.AccessToken{}, errors.New("AADSTS90033: a transient error has occurred")
	}
	return azcore.AccessToken{Token: "fresh", ExpiresOn: time.Now().Add(time.Hour)}, nil
}

func TestPermafrostToken_AFailedRenewalKeepsTheValidTokenAndPauses(t *testing.T) {
	// Arrange: a token inside its renewal window (3 min left), the identity
	// service failing.
	cred := &flakyCred{}
	cred.fail.Store(true)
	tn := &odTenant{name: "tok", cred: cred, cachedTok: "old", tokExpiry: time.Now().Add(-2 * time.Minute), tokValidUntil: time.Now().Add(3 * time.Minute)}

	// Act: 20 callers at once.
	var wg sync.WaitGroup
	toks := make([]string, 20)
	errs := make([]error, 20)
	for i := range toks {
		wg.Add(1)
		go func(i int) { defer wg.Done(); toks[i], errs[i] = tn.token(context.Background()) }(i)
	}
	wg.Wait()

	// Assert: one renewer, everyone kept the valid token.
	assert.Equal(t, int32(1), cred.calls.Load(), "one renewal, never a per-request storm")
	for i := range toks {
		require.NoError(t, errs[i])
		assert.Equal(t, "old", toks[i])
	}

	// And within the pause no new renewal is tried.
	_, err := tn.token(context.Background())
	require.NoError(t, err)
	assert.Equal(t, int32(1), cred.calls.Load())

	// After the pause, the next renewal succeeds.
	cred.fail.Store(false)
	tn.tokenMu.Lock()
	tn.renewPauseUntil = time.Time{}
	tn.tokenMu.Unlock()
	tok, err := tn.token(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "fresh", tok)
}

func TestPermafrostToken_AnExpiredTokenWhoseRenewalFailedIsAnErrorWithoutAStorm(t *testing.T) {
	cred := &flakyCred{}
	cred.fail.Store(true)
	tn := &odTenant{name: "tok2", cred: cred}

	_, err1 := tn.token(context.Background())
	_, err2 := tn.token(context.Background())

	require.Error(t, err1)
	require.Error(t, err2)
	assert.Equal(t, int32(1), cred.calls.Load(), "the second call inside the pause asks nobody")
}

// ── D-F3-8: the metrics are registered ──────────────────────────────────────

func TestPermafrostPace_TheMetricsAreOnCollectors(t *testing.T) {
	want := map[prometheus.Collector]string{
		permafrostThrottles:       "throttles",
		permafrostPaced:           "paced",
		permafrostRangeHedges:     "hedges",
		permafrostFragmentResumes: "resumes",
	}
	for _, c := range Collectors() {
		delete(want, c)
	}
	assert.Empty(t, want, "declared is not registered (be9d044d)")
}

// odRoundTripFunc adapts a function to http.RoundTripper.
type odRoundTripFunc func(*http.Request) (*http.Response, error)

func (f odRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }
