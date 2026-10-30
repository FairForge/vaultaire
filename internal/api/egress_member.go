package api

// A child's egress cap inside its pool (WP-ACCT-1, Stage 1a Prompt 53 PR
// A; docs/ACCOUNTS.md "caps inside the pool"). tenant_quotas.egress_cap_bytes
// > 0 on a pool's member bounds THAT member's downloads this UTC month,
// beside the pool's allowance: a member past its own cap is paced (at the
// rate its cap gives, through its own token bucket) while the pool still
// has room, and the pool's decision still applies to it. 0 = the pool
// decides alone. The member's month is its own bandwidth rows plus the
// live bytes its responses count; the cap is re-read with the allowance's
// TTL, and at once after the platform changes it (forgetMember).

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"
	"golang.org/x/time/rate"

	"github.com/FairForge/vaultaire/internal/usage"
)

// egressMember is one member of a pool, as its own cap sees it.
type egressMember struct {
	id     string
	mu     sync.Mutex
	loaded atomic.Bool
	month  atomic.Int64 // monthKey of used
	used   atomic.Int64 // the member's own egress bytes this month
	cap    atomic.Int64 // 0 = none
	capAt  atomic.Int64 // unix nanos of the next cap read

	limiter    *rate.Limiter
	sliceBytes atomic.Int64
}

// member is the member state of tenantID in its pool t, loaded; nil for
// the root itself (its own position is the pool's).
func (m *egressMeter) member(ctx context.Context, t *egressTenant, tenantID string) *egressMember {
	if tenantID == t.id {
		return nil
	}
	v, ok := t.members.Load(tenantID)
	if !ok {
		mb := &egressMember{id: tenantID, limiter: rate.NewLimiter(rate.Limit(m.cfg.MinBytesPerSec), egressSliceBytes)}
		mb.sliceBytes.Store(m.sliceFor(m.cfg.MinBytesPerSec))
		v, _ = t.members.LoadOrStore(tenantID, mb)
	}
	mb := v.(*egressMember)
	now := m.now()
	if !mb.loaded.Load() || now.UnixNano() >= mb.capAt.Load() {
		m.loadMember(ctx, mb, now)
	}
	if mk := egressMonthKey(now); mb.month.Load() != mk && mb.loaded.Load() {
		mb.month.Store(mk)
		mb.used.Store(0)
	}
	return mb
}

// loadMember reads the member's cap (and, the first time, its month). A
// failed read leaves it uncapped (fail open, as the pool's first load).
func (m *egressMeter) loadMember(ctx context.Context, mb *egressMember, now time.Time) {
	mb.mu.Lock()
	defer mb.mu.Unlock()
	if mb.loaded.Load() && now.UnixNano() < mb.capAt.Load() {
		return
	}
	lctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	limit, err := m.loadMemberCap(lctx, mb.id)
	if err != nil {
		m.logger.Warn("egress meter: read a member's egress cap failed — the pool decides alone",
			zap.String("tenant_id", mb.id), zap.Error(err))
		mb.capAt.Store(now.Add(egressAllowanceRetry).UnixNano())
		return
	}
	if !mb.loaded.Load() {
		used, err := m.loadMemberUsed(lctx, mb.id, usage.EgressMonthStart(now))
		if err != nil {
			m.logger.Warn("egress meter: read a member's month failed — the pool decides alone",
				zap.String("tenant_id", mb.id), zap.Error(err))
			mb.capAt.Store(now.Add(egressAllowanceRetry).UnixNano())
			return
		}
		mb.used.Store(used)
		mb.month.Store(egressMonthKey(now))
	}
	if mb.cap.Swap(limit) != limit && limit > 0 {
		bps := m.cfg.Rate(limit)
		burst := int(bps / 4)
		if burst < egressSliceBytes {
			burst = egressSliceBytes
		}
		mb.sliceBytes.Store(m.sliceFor(bps))
		mb.limiter.SetLimit(rate.Limit(bps))
		mb.limiter.SetBurst(burst)
	}
	mb.capAt.Store(now.Add(m.allowanceTTL).UnixNano())
	mb.loaded.Store(true)
}

// over reports whether the member has spent its own cap.
func (mb *egressMember) over() bool {
	if mb == nil || !mb.loaded.Load() {
		return false
	}
	c := mb.cap.Load()
	return c > 0 && mb.used.Load() >= c
}

// count adds a response's bytes to the member's month.
func (mb *egressMember) count(n int64, mk int64) {
	if mb == nil {
		return
	}
	if cur := mb.month.Load(); cur != mk && cur != 0 && mb.month.CompareAndSwap(cur, mk) {
		mb.used.Store(0)
	}
	mb.used.Add(n)
}

// forgetMember drops a member's state from every pool entry: its cap was
// changed and is read again on its next response.
func (m *egressMeter) forgetMember(tenantID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, t := range m.tenants {
		t.members.Delete(tenantID)
	}
}

// memberCapOf is the production read of a member's egress cap.
func memberCapOf(ctx context.Context, m *egressMeter, tenantID string) (int64, error) {
	if m.db == nil {
		return 0, nil
	}
	var c int64
	err := m.db.QueryRowContext(ctx, `SELECT egress_cap_bytes FROM tenant_quotas WHERE tenant_id = $1`, tenantID).Scan(&c)
	if err != nil {
		return 0, err
	}
	return c, nil
}

// memberMonthOf is the production read of a member's OWN month (its rows).
func memberMonthOf(ctx context.Context, m *egressMeter, tenantID string, monthStart time.Time) (int64, error) {
	if m.db == nil {
		return 0, nil
	}
	var used int64
	err := m.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(egress_bytes), 0) FROM bandwidth_usage_daily WHERE tenant_id = $1 AND date >= $2::date`,
		tenantID, monthStart.UTC().Format("2006-01-02")).Scan(&used)
	return used, err
}
