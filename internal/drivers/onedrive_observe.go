package drivers

// What the System page's fleet card shows (WP-FLEET-10, docs/reviews/
// WP-FLEET-NEXT-PROMPT.md Part 6): one snapshot per account, from the driver
// itself — the drive it writes to, the quota its last probe read (Graph's
// drive.quota: used excludes the bin, deleted IS the bin), the last probe's
// outcome, and the 429/503s of the last hour (a per-minute ring fed by the
// account's hold). The licence is WP-FLEET-6's (empty until then).

import (
	"sync"
	"time"
)

// FleetAccount is one permafrost account on the System page.
type FleetAccount struct {
	Account   string
	DriveID   string
	Pinned    bool
	Probed    bool // a probe has run since boot
	Up        bool // the last probe answered
	LastError string

	QuotaKnown                                          bool
	QuotaTotal, QuotaUsed, QuotaDeleted, QuotaRemaining int64
	QuotaState                                          string

	Throttles1h int64
	Licence     string // WP-FLEET-6; "" = not watched yet
}

// odThrottleRing counts an account's throttles per minute for the last hour.
type odThrottleRing struct {
	mu     sync.Mutex
	minute [60]int64 // the minute (Unix / 60) a slot counts
	count  [60]int64
}

// countThrottleAt counts one throttle at t.
func (t *odTenant) countThrottleAt(at time.Time) {
	m := at.Unix() / 60
	slot := m % 60
	r := &t.throttles
	r.mu.Lock()
	if r.minute[slot] != m {
		r.minute[slot], r.count[slot] = m, 0
	}
	r.count[slot]++
	r.mu.Unlock()
}

// throttlesSince sums the throttles counted at or after since (to a minute).
func (t *odTenant) throttlesSince(since time.Time) int64 {
	from := since.Unix() / 60
	r := &t.throttles
	r.mu.Lock()
	defer r.mu.Unlock()
	var n int64
	for i := range r.minute {
		if r.minute[i] >= from {
			n += r.count[i]
		}
	}
	return n
}

// FleetAccounts is the fleet in placement order, one snapshot per account.
func (d *OneDriveDriver) FleetAccounts() []FleetAccount {
	hourAgo := time.Now().Add(-time.Hour)
	out := make([]FleetAccount, 0, len(d.tenants))
	for _, t := range d.tenants {
		a := FleetAccount{Account: t.name, Pinned: t.pinnedDriveID != "", Throttles1h: t.throttlesSince(hourAgo)}
		t.mu.Lock()
		a.DriveID = t.driveID
		t.mu.Unlock()
		if a.DriveID == "" {
			a.DriveID = t.pinnedDriveID
		}
		if o, ok := t.probeErr.Load().(odProbeOutcome); ok {
			a.Probed, a.Up = true, o.err == nil
			if o.err != nil {
				a.LastError = o.err.Error()
			}
		}
		if q, ok := t.lastQuota.Load().(odQuota); ok {
			a.QuotaKnown = true
			a.QuotaTotal, a.QuotaUsed, a.QuotaDeleted, a.QuotaRemaining, a.QuotaState = q.Total, q.Used, q.Deleted, q.Remaining, q.State
		}
		out = append(out, a)
	}
	return out
}
