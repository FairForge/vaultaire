package drivers

// The permafrost fleet's pacing (WP-FLEET-3 D-F3-5, D-F3-5a, D-F3-6).
//
// Microsoft's published lines (docs/FLEET.md): per user (= one fleet
// account) 3,000 requests / 5 min, 50 GB/h in, 100 GB/h out; per app per
// tenant 400 GB/h in and 400 out, whatever the user count. Above them it does
// not cap — it answers 429 BURSTS of about a minute (the 2026-11-06 soaks),
// and two drives of ONE tenant run flat out lost reads in 12 of 21 rounds:
// the tenant line bites first in a dense tenant. The good-tenant posture is
// to pace at the published rates and treat a 429 as the budget's signal.
//
// Buckets, all waited on and never refusing (the caller's context bounds the
// wait):
//
//   - per ACCOUNT (keyed on identity(): the directory + the user), one for
//     requests (PERMAFROST_ACCOUNT_RPS, default 8/s, burst 100), one for the
//     bytes in (PERMAFROST_ACCOUNT_IN_GBH, 50) and one for the bytes out
//     (PERMAFROST_ACCOUNT_OUT_GBH, 100);
//   - per TENANT (keyed on the directory id, the app registration's
//     tenant), one for each direction (PERMAFROST_TENANT_GBH, 400 each way)
//     shared by every slot in that tenant — six slots in three tenants share
//     three pairs.
//
// A bytes bucket holds a minute of its rate (at least 64 MiB): a lone read
// after a quiet spell is not slowed, a sustained one runs at the rate.
//
// WHERE: a request token before every Graph call, CDN range attempt, upload
// fragment attempt and session query; the out bytes of a range once, before
// its first attempt (a retry re-asks for bytes that never came); the in
// bytes as the fragment is READ from the source (back-pressure on the
// source, the sia_write.go pattern), so a fragment's answer bound times
// Microsoft only and a retry re-sends a buffered fragment without taking its
// bytes again.
//
// A 429/503 (any op) holds the WHOLE ACCOUNT for its Retry-After (5 s when it
// names none): every request of every caller on that account waits for the
// hold's end, and its buckets are drained so the refill does not come back
// as a stampede (D-F3-6). `RateLimit-Remaining` below 20 % of
// `RateLimit-Limit`, when Microsoft sends them, holds the account the same
// way until `RateLimit-Reset`. A hold that would outlast the caller's
// deadline is not waited for: the request fails as throttled at once.
//
// 0 / off on any knob = that bucket is off. Metrics:
// vaultaire_permafrost_throttles_total{account,op},
// vaultaire_permafrost_paced_seconds_total{account,dir}.

import (
	"context"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"
)

// odPaceConfig is the fleet's pacing knobs (odPaceFromEnv).
type odPaceConfig struct {
	AccountRPS    float64 // requests per second per account; 0 = off
	AccountInGBh  float64 // GB (10^9) per hour per account, in; 0 = off
	AccountOutGBh float64
	TenantGBh     float64 // GB per hour per tenant, each way; 0 = off
}

// odPaceDefaults are Microsoft's published rates (docs/FLEET.md).
var odPaceDefaults = odPaceConfig{AccountRPS: 8, AccountInGBh: 50, AccountOutGBh: 100, TenantGBh: 400}

const (
	odRequestBurst  = 100
	odThrottleHold  = 5 * time.Second // a 429 that names no Retry-After
	odRateLimitLow  = 0.20            // RateLimit-Remaining / RateLimit-Limit below it holds
	odBytesBurstMin = 64 << 20
)

// odPaceFromEnv reads the four knobs; a rejected value is a Warn and its
// default kept.
func odPaceFromEnv(getenv func(string) string, logger *zap.Logger) odPaceConfig {
	cfg := odPaceDefaults
	read := func(name string, def, maxV float64) float64 {
		v := strings.TrimSpace(getenv(name))
		switch {
		case v == "":
			return def
		case v == "0" || strings.EqualFold(v, "off"):
			return 0
		}
		f, err := strconv.ParseFloat(v, 64)
		if err != nil || f <= 0 || f > maxV || math.IsNaN(f) {
			logger.Warn(fmt.Sprintf("invalid %s (need a number in (0, %g], or 0/off), keeping default", name, maxV),
				zap.String("value", v), zap.Float64("default", def))
			return def
		}
		return f
	}
	cfg.AccountRPS = read("PERMAFROST_ACCOUNT_RPS", cfg.AccountRPS, 1000)
	cfg.AccountInGBh = read("PERMAFROST_ACCOUNT_IN_GBH", cfg.AccountInGBh, 100_000)
	cfg.AccountOutGBh = read("PERMAFROST_ACCOUNT_OUT_GBH", cfg.AccountOutGBh, 100_000)
	cfg.TenantGBh = read("PERMAFROST_TENANT_GBH", cfg.TenantGBh, 100_000)
	return cfg
}

var (
	permafrostThrottles = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "vaultaire_permafrost_throttles_total",
		Help: "429/503 answers a permafrost account gave, by operation (graph = a Graph call, range = a CDN range, fragment = an upload fragment, session = an upload-session query). Each holds the account for its Retry-After (WP-FLEET-3 D-F3-6).",
	}, []string{"account", "op"})
	permafrostPaced = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "vaultaire_permafrost_paced_seconds_total",
		Help: "Seconds permafrost callers waited on an account's or its tenant's pacing (dir = requests, in, out, or hold — a 429's Retry-After), WP-FLEET-3 D-F3-5.",
	}, []string{"account", "dir"})
)

// odPaceOps and odPaceDirs are every label value, created at 0 per account.
var (
	odPaceOps  = []string{"graph", "range", "fragment", "session"}
	odPaceDirs = []string{"requests", "in", "out", "hold"}
)

// odBytesPacer is a bytes bucket of gbh GB per hour (nil when off).
func odBytesPacer(what string, gbh float64, waited func(time.Duration)) *pacer {
	if gbh <= 0 {
		return nil
	}
	perSec := gbh * 1e9 / 3600
	burst := int(min(max(perSec*60, odBytesBurstMin), math.MaxInt32))
	return newPacer(what, perSec, burst, waited)
}

// odTenantBuckets is one tenant's pair, shared by its accounts.
type odTenantBuckets struct{ in, out *pacer }

var (
	odTenantBucketsMu sync.Mutex
	odTenantBucketsOf = map[string]*odTenantBuckets{}
)

// tenantBuckets returns the directory's pair (made once per process; a
// config change of TenantGBh applies to a directory not yet seen — the
// driver is built once).
func tenantBuckets(tenantID string, gbh float64) *odTenantBuckets {
	odTenantBucketsMu.Lock()
	defer odTenantBucketsMu.Unlock()
	if b, ok := odTenantBucketsOf[tenantID]; ok {
		return b
	}
	b := &odTenantBuckets{
		in:  odBytesPacer("permafrost tenant in", gbh, nil),
		out: odBytesPacer("permafrost tenant out", gbh, nil),
	}
	odTenantBucketsOf[tenantID] = b
	return b
}

// odPace is one account's pacing. A nil *odPace paces nothing but the hold.
type odPace struct {
	req, in, out *pacer
	tenant       *odTenantBuckets
}

// newODPace builds the account's buckets for cfg; tenantID keys the shared
// tenant pair. Its waits are counted under account.
func newODPace(account, tenantID string, cfg odPaceConfig) *odPace {
	waited := func(dir string) func(time.Duration) {
		c := permafrostPaced.WithLabelValues(account, dir)
		return func(d time.Duration) { c.Add(d.Seconds()) }
	}
	p := &odPace{
		req: newPacer("permafrost requests", cfg.AccountRPS, odRequestBurst, waited("requests")),
		in:  odBytesPacer("permafrost account in", cfg.AccountInGBh, waited("in")),
		out: odBytesPacer("permafrost account out", cfg.AccountOutGBh, waited("out")),
	}
	if cfg.TenantGBh > 0 {
		p.tenant = tenantBuckets(tenantID, cfg.TenantGBh)
	}
	return p
}

// InitPermafrostPaceSeries creates the account's throttle and pacing series
// at 0.
func InitPermafrostPaceSeries(account string) {
	for _, op := range odPaceOps {
		permafrostThrottles.WithLabelValues(account, op)
	}
	for _, dir := range odPaceDirs {
		permafrostPaced.WithLabelValues(account, dir)
	}
	permafrostHashMismatch.WithLabelValues(account)
}

// errODThrottled is a request given up on because the account is held past
// the caller's deadline.
type errODThrottled struct {
	account string
	until   time.Time
}

func (e *errODThrottled) Error() string {
	return fmt.Sprintf("onedrive throttled [%s]: held until %s, past the caller's deadline", e.account, e.until.UTC().Format(time.RFC3339))
}

// waitHold waits out the account's hold (a 429's Retry-After) from the
// later of now and covered — the end of a wait the caller already made for
// it — on ctx; a hold past ctx's deadline is not waited for.
func (t *odTenant) waitHold(ctx context.Context, covered time.Time) error {
	until := time.Unix(0, t.throttledUntil.Load())
	d := time.Until(until)
	if c := until.Sub(covered); !covered.IsZero() && c < d {
		d = c
	}
	if d <= 0 {
		return nil
	}
	if dl, ok := ctx.Deadline(); ok && dl.Before(until) {
		return &errODThrottled{account: t.name, until: until}
	}
	start := time.Now()
	defer func() { permafrostPaced.WithLabelValues(t.name, "hold").Add(time.Since(start).Seconds()) }()
	if err := t.sleep(ctx, d); err != nil {
		return fmt.Errorf("onedrive [%s] waiting out a throttle: %w", t.name, err)
	}
	return nil
}

// paceRequest is the gate before one request of the account: the hold, then
// a request token.
func (t *odTenant) paceRequest(ctx context.Context) error {
	return t.paceRequestFrom(ctx, time.Time{})
}

// paceRequestFrom is paceRequest for a retry whose wait covered the hold
// until covered.
func (t *odTenant) paceRequestFrom(ctx context.Context, covered time.Time) error {
	if err := t.waitHold(ctx, covered); err != nil {
		return err
	}
	if t.pace == nil {
		return nil
	}
	return t.pace.req.take(ctx, 1)
}

// paceOut takes n bytes of the account's and its tenant's egress.
func (t *odTenant) paceOut(ctx context.Context, n int64) error {
	if t.pace == nil {
		return nil
	}
	if err := t.pace.out.take(ctx, int(n)); err != nil {
		return err
	}
	if t.pace.tenant != nil {
		start := time.Now()
		defer func() { permafrostPaced.WithLabelValues(t.name, "out").Add(time.Since(start).Seconds()) }()
		return t.pace.tenant.out.take(ctx, int(n))
	}
	return nil
}

// pacedSource is the upload source read through the account's and its
// tenant's ingress (back-pressure on the source).
func (t *odTenant) pacedSource(ctx context.Context, r io.Reader) io.Reader {
	if t.pace == nil {
		return r
	}
	src := t.pace.in.reader(ctx, r)
	if t.pace.tenant != nil {
		src = t.pace.tenant.in.reader(ctx, src)
	}
	return src
}

// hold marks the account throttled for d from now (never shortening a
// longer hold), drains its buckets and counts the throttle under op.
func (t *odTenant) hold(op string, d time.Duration) {
	if d <= 0 {
		d = odThrottleHold
	}
	d = min(d, odBurstMaxWait)
	until := time.Now().Add(d).UnixNano()
	for {
		cur := t.throttledUntil.Load()
		if cur >= until || t.throttledUntil.CompareAndSwap(cur, until) {
			break
		}
	}
	if op != "" { // "" = RateLimit back-pressure, not a throttle
		t.throttleCount.Add(1)
		permafrostThrottles.WithLabelValues(t.name, op).Inc()
		t.countThrottleAt(time.Now())
	}
	if t.pace != nil {
		t.pace.req.drain()
		t.pace.in.drain()
		t.pace.out.drain()
	}
}

// ── D-F3-7: decorated traffic ───────────────────────────────────────────────

var odUserAgentValue atomic.Value // string

func init() { SetPermafrostBuild("dev") }

// SetPermafrostBuild names the build in the fleet's User-Agent,
// `ISV|Stored|permafrost/<sha>` — Microsoft's documented format for
// prioritised ISV traffic. main calls it with api.BuildSHA before the
// driver is built ("dev" unstamped).
func SetPermafrostBuild(sha string) {
	if sha = strings.TrimSpace(sha); sha == "" {
		sha = "dev"
	}
	odUserAgentValue.Store("ISV|Stored|permafrost/" + sha)
}

// odUserAgent is every fleet request's User-Agent: Graph, CDN, fragments.
func odUserAgent() string { return odUserAgentValue.Load().(string) }

// ── D-F3-4: a slow range is hedged ──────────────────────────────────────────

var (
	permafrostRangeHedges = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "vaultaire_permafrost_range_hedges_total",
		Help: "Second requests for a slow permafrost CDN range (PERMAFROST_RANGE_HEDGE_AFTER), by outcome: won (the hedge's copy was served), lost (the first answered first), failed (both failed).",
	}, []string{"outcome"})
	permafrostFragmentResumes = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "vaultaire_permafrost_fragment_resumes_total",
		Help: "Upload fragments that exhausted their attempts and were taken up from the session (D-F3-3): resumed (re-sent from where the session expects), skipped (the session already held it), restarted (the session was gone and the body could be rewound: a new session), failed.",
	}, []string{"outcome"})
)

func init() {
	for _, o := range []string{"won", "lost", "failed"} {
		permafrostRangeHedges.WithLabelValues(o)
	}
	for _, o := range []string{"resumed", "skipped", "restarted", "failed"} {
		permafrostFragmentResumes.WithLabelValues(o)
	}
}

// odRangeHedgeDefault is PERMAFROST_RANGE_HEDGE_AFTER's default: one of
// ~10 parity reads of a 1 GiB object ran at 17 MB/s against 78–101 for the
// others (2026-11-04/05) — a straggler range; the erasure bench's hedged
// reads ran 124–131 MB/s against 97.
const odRangeHedgeDefault = 2 * time.Second

// odRangeHedgeFromEnv reads PERMAFROST_RANGE_HEDGE_AFTER: a Go duration
// 100ms–30s, 0/off = no hedge; a rejected value is a Warn and the default.
func odRangeHedgeFromEnv(getenv func(string) string, logger *zap.Logger) time.Duration {
	v := strings.TrimSpace(getenv("PERMAFROST_RANGE_HEDGE_AFTER"))
	switch {
	case v == "":
		return odRangeHedgeDefault
	case v == "0" || strings.EqualFold(v, "off"):
		return 0
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < 100*time.Millisecond || d > 30*time.Second {
		logger.Warn("invalid PERMAFROST_RANGE_HEDGE_AFTER (need a duration 100ms..30s such as 2s, or 0/off), keeping default",
			zap.String("value", v), zap.Duration("default", odRangeHedgeDefault))
		return odRangeHedgeDefault
	}
	return d
}

// odRangeTimes is one Get's recent range times: the hedge waits
// max(base, 3 × their median) — the median, so the tail it hedges does not
// push the delay out.
type odRangeTimes struct {
	mu sync.Mutex
	d  [32]time.Duration
	n  int
}

func (r *odRangeTimes) observe(d time.Duration) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.d[r.n%len(r.d)] = d
	r.n++
	r.mu.Unlock()
}

func (r *odRangeTimes) delay(base time.Duration) time.Duration {
	if r == nil {
		return base
	}
	r.mu.Lock()
	n := min(r.n, len(r.d))
	s := append([]time.Duration(nil), r.d[:n]...)
	r.mu.Unlock()
	if n == 0 {
		return base
	}
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	return max(base, 3*s[n/2])
}

// fetchRangeHedged is one attempt at a range, hedged: when it has not
// finished after the delay, the same range of the same URL is asked again
// (another connection: the first one is busy), the first verified copy
// wins and the other is cancelled. Never more than one hedge per attempt;
// the hedge waits out the account's hold and takes a request token like
// any request (a hedge during a burst waits for the burst's end). The
// answer reported for a failure is the first one (its Retry-After and
// whether to retry) — the original attempt's, when both failed.
func (t *odTenant) fetchRangeHedged(ctx context.Context, dlURL string, start, end, size, total int64, times *odRangeTimes) ([]byte, time.Duration, bool, error) {
	if t.rangeHedgeAfter <= 0 {
		return t.fetchRangeOnce(ctx, dlURL, start, end, size, total)
	}
	type answer struct {
		buf   []byte
		ra    time.Duration
		retry bool
		err   error
		hedge bool
		took  time.Duration
	}
	hctx, cancel := context.WithCancel(ctx)
	defer cancel()
	ch := make(chan answer, 2) // no send ever blocks
	launch := func(hedge bool) {
		go func() {
			if hedge {
				if err := t.paceRequest(hctx); err != nil {
					ch <- answer{err: err, hedge: true}
					return
				}
			}
			began := time.Now()
			b, ra, retry, err := t.fetchRangeOnce(hctx, dlURL, start, end, size, total)
			ch <- answer{b, ra, retry, err, hedge, time.Since(began)}
		}()
	}
	launch(false)
	timer := time.NewTimer(times.delay(t.rangeHedgeAfter))
	defer timer.Stop()
	pending, hedged := 1, false
	var first *answer
	for {
		select {
		case a := <-ch:
			pending--
			if a.err == nil {
				switch {
				case a.hedge:
					permafrostRangeHedges.WithLabelValues("won").Inc()
				case hedged:
					permafrostRangeHedges.WithLabelValues("lost").Inc()
				}
				times.observe(a.took)
				return a.buf, 0, false, nil
			}
			if first == nil || !a.hedge {
				first = &a // the original attempt's answer is the one reported
			}
			if pending == 0 {
				if hedged {
					permafrostRangeHedges.WithLabelValues("failed").Inc()
				}
				return nil, first.ra, first.retry && ctx.Err() == nil, first.err
			}
		case <-timer.C:
			if !hedged && pending == 1 && ctx.Err() == nil {
				hedged = true
				pending++
				launch(true)
			}
		}
	}
}
