package api

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/FairForge/vaultaire/internal/usage"
	"go.uber.org/zap"
	"golang.org/x/time/rate"
)

// Egress allowance enforcement (WP-R10-9, decision D-25).
//
// egressMeter is the ONE counter of a tenant's egress this month: a bounded
// in-process map, loaded once per tenant from bandwidth_usage_daily (egress
// only, UTC month) and then incremented as response bytes are written, so
// responses still in flight count. The buffered DB flush (BandwidthTracker)
// is unchanged and is what a restart reloads from. Nothing sums per request.
//
// The allowance comes from internal/usage (the one definition) and is cached
// per tenant for egressAllowanceTTL, so a purchase or an admin edit lifts the
// cap within that window.
//
// Past the allowance a tenant is slowed, not refused: GetObject bodies and
// /cdn bodies share one token bucket per tenant (egressWriter). The one
// refusal is the stream guard: more than MaxStreams concurrent throttled
// responses. Uploads, listings, HEAD and errors are never slowed.

const (
	// egressSliceBytes is one paced write. At the floor rate (64 KiB/s) with
	// the maximum 16 streams each connection still sends a slice every 4 s —
	// inside HAProxy's 50 s and Cloudflare's 100 s silence timeouts.
	egressSliceBytes = 16 << 10
	// egressMinSliceBytes / egressMaxSilence: when the knobs allow more
	// streams or a lower rate, the slice shrinks (down to 1 KiB) so that with
	// every slot of both surfaces taken each stream still sends within 20 s.
	egressMinSliceBytes = 1 << 10
	egressMaxSilence    = 20 * time.Second
	// egressReevalBytes: a long response re-evaluates the tenant's position
	// this often, so a download that crosses the allowance mid-stream slows
	// down and one whose allowance was raised speeds up.
	egressReevalBytes = 1 << 20
	// egressAllowanceTTL bounds how long a purchase or an admin edit takes
	// to reach the throttle.
	egressAllowanceTTL = 30 * time.Second
	// egressAllowanceRetry is the wait after a failed allowance read.
	egressAllowanceRetry = 5 * time.Second
	// egressMaxTenants bounds the map. Only tenants that authenticated or
	// own a public bucket get an entry, so this is a ceiling, not a cache size.
	egressMaxTenants = 20000
	// egressIdleEvict: an entry with no open response that has been idle
	// this long has nothing unflushed (the flusher runs every 5 s) and can be
	// dropped and reloaded without losing a byte.
	egressIdleEvict = 2 * time.Minute
	// egressRetryAfter is what a refused stream is told to wait, in seconds.
	egressRetryAfter = "60"

	egressSurfaceS3  = "s3"
	egressSurfaceCDN = "cdn"
)

// egressSurfaceIndex maps a surface to its stream-guard counter.
func egressSurfaceIndex(surface string) int {
	if surface == egressSurfaceCDN {
		return 1
	}
	return 0
}

// egressMeter counts egress per tenant per UTC month and decides who is paced.
type egressMeter struct {
	db     *sql.DB
	logger *zap.Logger
	cfg    usage.EgressThrottle

	// enforced reports whether the egress_throttle flag is on for a tenant
	// (tenant row → global row → default off). Off = the same decision is
	// only counted.
	enforced func(tenantID string) bool
	now      func() time.Time

	// The two reads, replaceable in tests.
	loadAllowance func(ctx context.Context, tenantID string) (usage.EgressAllowance, error)
	loadUsed      func(ctx context.Context, tenantID string, monthStart time.Time) (int64, error)

	allowanceTTL time.Duration
	reevalEvery  time.Duration // a paced response re-reads its position this often
	maxTenants   int

	mu        sync.Mutex
	tenants   map[string]*egressTenant
	lastSweep time.Time
}

// egressTenant is one tenant's counter, cached allowance and token bucket.
type egressTenant struct {
	id string

	mu     sync.Mutex // guards the first load
	loaded atomic.Bool

	month atomic.Int64 // monthKey of `used`; 0 until loaded
	used  atomic.Int64 // egress bytes this month: loaded base + live bytes

	// unrecorded: live bytes not yet handed to the BandwidthTracker (their
	// responses have not ended). Shutdown drains it so a restart reloads
	// the whole counter.
	unrecorded atomic.Int64

	allowance   atomic.Int64 // bytes; 0 = none known
	allowanceAt atomic.Int64 // unix nanos of the next refresh
	refreshing  atomic.Bool

	limiter    *rate.Limiter // shared by every paced response of the tenant
	rateBps    atomic.Int64
	sliceBytes atomic.Int64 // one paced write; <= the limiter's burst

	streams atomic.Int32 // open object responses (pins the entry)
	// throttledStreams: of those, the ones holding a throttle slot, per
	// surface (s3, cdn). The stream guard is per surface so anonymous
	// readers of a public bucket cannot take every slot and lock the owner
	// out of the S3 API; the token bucket is still one.
	throttledStreams [2]atomic.Int32
	lastSeen         atomic.Int64
	noticedMonth     atomic.Int64 // month in which "allowance spent" was logged
}

func newEgressMeter(db *sql.DB, cfg usage.EgressThrottle, enforced func(string) bool, logger *zap.Logger) *egressMeter {
	if logger == nil {
		logger = zap.NewNop()
	}
	m := &egressMeter{
		db:           db,
		logger:       logger,
		cfg:          cfg,
		enforced:     enforced,
		now:          time.Now,
		allowanceTTL: egressAllowanceTTL,
		reevalEvery:  5 * time.Second,
		maxTenants:   egressMaxTenants,
		tenants:      make(map[string]*egressTenant),
	}
	m.loadAllowance = func(ctx context.Context, tenantID string) (usage.EgressAllowance, error) {
		if m.db == nil {
			return usage.EgressAllowance{}, sql.ErrNoRows
		}
		return usage.NewQuotaManager(m.db).EgressAllowance(ctx, tenantID)
	}
	m.loadUsed = func(ctx context.Context, tenantID string, monthStart time.Time) (int64, error) {
		if m.db == nil {
			return 0, nil
		}
		return usage.MonthEgressBytes(ctx, m.db, tenantID, monthStart)
	}
	return m
}

// egressThrottleFromEnv reads the three D-25 knobs. A rejected value is
// logged at Warn and the default kept (the R13-19 convention).
func egressThrottleFromEnv(getenv func(string) string, logger *zap.Logger) usage.EgressThrottle {
	cfg := usage.DefaultEgressThrottle()
	if v := getenv("EGRESS_THROTTLE_MIN_BYTES_PER_SEC"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n >= 1024 {
			cfg.MinBytesPerSec = n
		} else {
			logger.Warn("invalid EGRESS_THROTTLE_MIN_BYTES_PER_SEC (need an integer >= 1024), keeping default", zap.String("value", v))
		}
	}
	if v := getenv("EGRESS_THROTTLE_FACTOR"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f > 0 && f <= 1000 {
			cfg.Factor = f
		} else {
			logger.Warn("invalid EGRESS_THROTTLE_FACTOR (need 0 < f <= 1000), keeping default", zap.String("value", v))
		}
	}
	if v := getenv("EGRESS_THROTTLE_MAX_STREAMS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 1 && n <= 100000 {
			cfg.MaxStreams = n
		} else {
			logger.Warn("invalid EGRESS_THROTTLE_MAX_STREAMS (need an integer >= 1), keeping default", zap.String("value", v))
		}
	}
	// With both surfaces' slots full, the smallest slice (1 KiB) must still
	// reach each stream inside the proxies' silence timeouts.
	if worst := float64(2*cfg.MaxStreams) * egressMinSliceBytes / float64(cfg.MinBytesPerSec); worst > egressMaxSilence.Seconds() {
		logger.Warn("egress throttle: at the floor rate a paced stream can be silent longer than the proxies allow — raise EGRESS_THROTTLE_MIN_BYTES_PER_SEC or lower EGRESS_THROTTLE_MAX_STREAMS",
			zap.Int64("min_bytes_per_sec", cfg.MinBytesPerSec), zap.Int("max_streams", cfg.MaxStreams), zap.Float64("silence_seconds", worst))
	}
	return cfg
}

// egressMonthKey numbers UTC calendar months.
func egressMonthKey(t time.Time) int64 {
	u := t.UTC()
	return int64(u.Year())*12 + int64(u.Month())
}

// tenant returns the tenant's entry, loaded: the month's recorded egress and
// the allowance are read once, by whichever request arrives first; a second
// request racing it waits for that one load instead of running its own.
func (m *egressMeter) tenant(ctx context.Context, tenantID string) *egressTenant {
	now := m.now()

	m.mu.Lock()
	t, ok := m.tenants[tenantID]
	if !ok {
		t = &egressTenant{id: tenantID, limiter: rate.NewLimiter(rate.Limit(m.cfg.MinBytesPerSec), egressSliceBytes)}
		t.sliceBytes.Store(m.sliceFor(m.cfg.MinBytesPerSec))
		if len(m.tenants) >= m.maxTenants {
			m.sweepLocked(now)
		}
		if len(m.tenants) < m.maxTenants {
			m.tenants[tenantID] = t
		}
		// A full map of busy tenants: this entry serves one request and is
		// dropped (it reloads next time). Sweeps are rate-limited, so is this.
	}
	m.mu.Unlock()
	t.lastSeen.Store(now.UnixNano())

	if !t.loaded.Load() {
		m.load(ctx, t)
	} else if mk := egressMonthKey(now); t.month.Load() != mk {
		t.rollover(mk)
	}
	return t
}

// sweepLocked drops idle entries; at most once per idle period.
func (m *egressMeter) sweepLocked(now time.Time) {
	if now.Sub(m.lastSweep) < egressIdleEvict/4 {
		return
	}
	m.lastSweep = now
	cutoff := now.Add(-egressIdleEvict).UnixNano()
	for id, t := range m.tenants {
		if t.streams.Load() == 0 && t.lastSeen.Load() < cutoff {
			delete(m.tenants, id)
		}
	}
	if len(m.tenants) >= m.maxTenants {
		m.logger.Warn("egress meter is full of active tenants — new tenants are loaded per request until entries go idle",
			zap.Int("entries", len(m.tenants)))
	}
}

// load reads the month's recorded egress and the allowance, once. A failed
// read leaves the entry unloaded (never "over" — fail open, like the limit
// it replaces) and the next request retries.
func (m *egressMeter) load(ctx context.Context, t *egressTenant) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.loaded.Load() {
		return
	}
	now := m.now()
	lctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()

	used, err := m.loadUsed(lctx, t.id, usage.EgressMonthStart(now))
	if err != nil {
		m.logger.Warn("egress meter: load month egress failed — tenant is not throttled until it loads",
			zap.String("tenant_id", t.id), zap.Error(err))
		return
	}
	m.readAllowance(lctx, t, now)
	t.used.Store(used)
	t.month.Store(egressMonthKey(now))
	t.loaded.Store(true)
}

// rollover starts a new month at zero. The process saw every byte since the
// month began, so there is nothing to read back.
func (t *egressTenant) rollover(mk int64) {
	if cur := t.month.Load(); cur != mk && t.month.CompareAndSwap(cur, mk) {
		t.used.Store(0)
	}
}

// add counts bytes as they are written.
func (t *egressTenant) add(n int64, mk int64) {
	if cur := t.month.Load(); cur != mk && cur != 0 {
		t.rollover(mk)
	}
	t.used.Add(n)
	t.unrecorded.Add(n)
}

// takeUnrecorded claims up to n of the tenant's unrecorded bytes for a
// response that is recording itself. It returns less than n only when a
// shutdown drain already recorded them.
func (t *egressTenant) takeUnrecorded(n int64) int64 {
	for {
		have := t.unrecorded.Load()
		take := n
		if take > have {
			take = have
		}
		if take <= 0 {
			return 0
		}
		if t.unrecorded.CompareAndSwap(have, have-take) {
			return take
		}
	}
}

// drainUnrecorded hands every tenant's unrecorded live bytes to the tracker.
// Called once the HTTP server has drained, before the tracker's last flush.
func (m *egressMeter) drainUnrecorded(bt *BandwidthTracker) {
	now := m.now()
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, t := range m.tenants {
		if n := t.unrecorded.Swap(0); n > 0 {
			bt.recordOn(now, id, "", 0, n)
		}
	}
}

// readAllowance fetches the allowance and reconfigures the token bucket.
func (m *egressMeter) readAllowance(ctx context.Context, t *egressTenant, now time.Time) {
	a, err := m.loadAllowance(ctx, t.id)
	switch {
	case err == nil:
	case errors.Is(err, sql.ErrNoRows):
		// No quota row (test tenants, a tenant erased mid-request): no
		// allowance, never over.
		a = usage.EgressAllowance{}
	default:
		m.logger.Warn("egress meter: read allowance failed — keeping the last value",
			zap.String("tenant_id", t.id), zap.Error(err))
		t.allowanceAt.Store(now.Add(egressAllowanceRetry).UnixNano())
		return
	}
	bps := m.cfg.Rate(a.Bytes)
	if t.rateBps.Swap(bps) != bps {
		burst := int(bps / 4)
		if burst < egressSliceBytes {
			burst = egressSliceBytes
		}
		t.sliceBytes.Store(m.sliceFor(bps))
		t.limiter.SetLimit(rate.Limit(bps))
		t.limiter.SetBurst(burst)
	}
	t.allowance.Store(a.Bytes)
	t.allowanceAt.Store(now.Add(m.allowanceTTL).UnixNano())
}

// sliceFor sizes one paced write for a tenant capped at bps: 16 KiB, or
// less when 2 × MaxStreams streams (both surfaces full) sharing bps would
// otherwise leave a stream silent for longer than egressMaxSilence.
func (m *egressMeter) sliceFor(bps int64) int64 {
	slice := int64(float64(bps) * egressMaxSilence.Seconds() / float64(2*m.cfg.MaxStreams))
	if slice > egressSliceBytes {
		slice = egressSliceBytes
	}
	if slice < egressMinSliceBytes {
		slice = egressMinSliceBytes
	}
	return slice
}

// refresh re-reads a stale allowance. One goroutine does the read; the
// others keep the value they have.
func (m *egressMeter) refresh(ctx context.Context, t *egressTenant, now time.Time) {
	if now.UnixNano() < t.allowanceAt.Load() || !t.refreshing.CompareAndSwap(false, true) {
		return
	}
	defer t.refreshing.Store(false)
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	m.readAllowance(rctx, t, now)
}

// egressDecision is a tenant's position at one instant.
type egressDecision struct {
	used, allowance int64
	over            bool // the allowance is spent
	enforced        bool // the egress_throttle flag is on for the tenant
}

// decide reads the tenant's position, refreshing a stale allowance.
func (m *egressMeter) decide(ctx context.Context, t *egressTenant) egressDecision {
	now := m.now()
	m.refresh(ctx, t, now)
	d := egressDecision{used: t.used.Load(), allowance: t.allowance.Load()}
	if mk := egressMonthKey(now); t.month.Load() != mk {
		d.used = 0 // a new month nothing has been written in yet
	}
	d.over = t.loaded.Load() && usage.EgressOver(d.used, d.allowance)
	d.enforced = m.enforced != nil && m.enforced(t.id)
	if d.over {
		if mk := egressMonthKey(now); t.noticedMonth.Swap(mk) != mk {
			m.logger.Info("egress allowance spent",
				zap.String("tenant_id", t.id), zap.Int64("used_bytes", d.used),
				zap.Int64("allowance_bytes", d.allowance), zap.Bool("throttled", d.enforced),
				zap.Int64("rate_bytes_per_sec", t.rateBps.Load()))
		}
	}
	return d
}

// admit opens one object-body response for the tenant. When the tenant is
// being paced and already has MaxStreams paced responses open on this
// surface it answers false — the one refusal of the throttle. The caller
// must close() the writer it gets.
func (m *egressMeter) admit(w http.ResponseWriter, r *http.Request, tenantID, surface string) (*egressWriter, bool) {
	t := m.tenant(r.Context(), tenantID)
	t.streams.Add(1)
	ew := &egressWriter{ResponseWriter: w, ctx: r.Context(), m: m, t: t, surface: surface,
		slots: &t.throttledStreams[egressSurfaceIndex(surface)]}
	if d := m.decide(r.Context(), t); d.over && d.enforced {
		if int(ew.slots.Add(1)) > m.cfg.MaxStreams {
			ew.slots.Add(-1)
			t.streams.Add(-1)
			egressRejected.WithLabelValues(surface).Inc()
			return nil, false
		}
		ew.slot = true
	}
	return ew, true
}

// EgressStatus is the tenant's position for the dashboard, the admin page,
// /api/v1/user/usage and the alerter: the allowance read fresh (an admin
// edit shows at once) and the live counter when the tenant has one, the
// recorded month otherwise. It never adds an entry to the map.
func (m *egressMeter) EgressStatus(ctx context.Context, tenantID string) (usage.EgressStatus, error) {
	now := m.now()
	a, err := m.loadAllowance(ctx, tenantID)
	if err != nil {
		return usage.EgressStatus{}, err
	}
	used, err := m.usedBytes(ctx, tenantID, now)
	if err != nil {
		return usage.EgressStatus{}, err
	}
	st := usage.EgressStatus{
		Allowance:       a,
		UsedBytes:       used,
		Over:            usage.EgressOver(used, a.Bytes),
		RateBytesPerSec: m.cfg.Rate(a.Bytes),
		ResetAt:         usage.EgressResetAt(now),
	}
	st.Throttled = st.Over && m.enforced != nil && m.enforced(tenantID)
	return st, nil
}

// usedBytes is the counter's value for a tenant: live when loaded, the
// recorded month otherwise.
func (m *egressMeter) usedBytes(ctx context.Context, tenantID string, now time.Time) (int64, error) {
	m.mu.Lock()
	t := m.tenants[tenantID]
	m.mu.Unlock()
	if t != nil && t.loaded.Load() {
		if t.month.Load() != egressMonthKey(now) {
			return 0, nil
		}
		return t.used.Load(), nil
	}
	return m.loadUsed(ctx, tenantID, usage.EgressMonthStart(now))
}

// throttledTenants counts the loaded tenants that are past their allowance
// with enforcement on (the vaultaire_egress_throttled_tenants gauge).
func (m *egressMeter) throttledTenants() int {
	mk := egressMonthKey(m.now())
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for id, t := range m.tenants {
		if t.loaded.Load() && t.month.Load() == mk &&
			usage.EgressOver(t.used.Load(), t.allowance.Load()) &&
			m.enforced != nil && m.enforced(id) {
			n++
		}
	}
	return n
}

// egressSpan counts one response's bytes into the tenant's counter as they
// are written. It lives on the countingResponseWriter.
type egressSpan struct {
	m *egressMeter
	t *egressTenant

	month   int64 // month of the bytes counted so far
	total   int64
	carried int64 // bytes written before the month turned over mid-response
}

// span opens the live count for one response.
func (m *egressMeter) span(ctx context.Context, tenantID string) *egressSpan {
	return &egressSpan{m: m, t: m.tenant(ctx, tenantID)}
}

func (s *egressSpan) count(n int64) {
	mk := egressMonthKey(s.m.now())
	if s.month != mk {
		if s.month != 0 {
			s.carried = s.total
		}
		s.month = mk
	}
	s.total += n
	s.t.add(n, mk)
}
