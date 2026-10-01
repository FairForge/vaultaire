// internal/usage/egress.go
//
// The egress allowance (WP-R10-9). The site sells one allowance per account
// per month: the Standard ratio × the downstairs quota plus the Vault ratio ×
// the attic quota (prices.json `egress`). This file is the ONE definition of
// it; the throttle, the alerter, the dashboard overview, the admin tenant
// page and /api/v1/user/usage all read it here.
//
// It is derived at read time from what the tenant holds — nothing is seeded
// and nothing can drift. Precedence:
//
//  1. an admin override: tenant_quotas.bandwidth_limit_bytes > 0
//  2. the house: ratio × each tenant_floor_quotas limit
//  3. no house: the Standard ratio × tenant_quotas.storage_limit_bytes
//     (the free tier's 5 GiB gives 2.5 GiB)
//
// SetHouse and ClearHouse never write bandwidth_limit_bytes, so an override
// survives a webhook replay and the end of a subscription.
//
// The month is the UTC calendar month everywhere (EgressMonthStart).
package usage

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"time"

	"github.com/FairForge/vaultaire/internal/api/landing"
)

// Where an allowance comes from.
const (
	EgressSourceOverride = "override"
	EgressSourceHouse    = "house"
	EgressSourceQuota    = "quota"
)

// EgressAllowance is a tenant's monthly egress allowance, in bytes.
type EgressAllowance struct {
	Bytes         int64  // what applies
	PlanBytes     int64  // what the plan grants (house or single quota)
	OverrideBytes int64  // the admin override; 0 = none
	Source        string // EgressSource*
}

// EgressRatios returns the allowance ratios the site prints (prices.json):
// the share of the downstairs quota and of the attic quota that may be
// downloaded per month.
func EgressRatios() (standard, vault float64) {
	p := landing.Get()
	return p.Egress.StandardFreeRatio, p.Egress.VaultRestoreFreeRatio
}

// PlanEgressAllowance is the allowance a plan grants for the given floor
// limits. A tenant without a house passes its single quota as the standard
// limit and 0 for the vault.
func PlanEgressAllowance(stdLimit, vaultLimit int64) int64 {
	std, vault := EgressRatios()
	return int64(math.Round(std*float64(stdLimit))) + int64(math.Round(vault*float64(vaultLimit)))
}

// EgressAllowance resolves the tenant's allowance with one query. An unknown
// tenant is an error wrapping sql.ErrNoRows.
func (m *QuotaManager) EgressAllowance(ctx context.Context, tenantID string) (EgressAllowance, error) {
	var total, override, stdLimit, vaultLimit int64
	var floors int
	err := m.db.QueryRowContext(ctx, `
		SELECT q.storage_limit_bytes,
		       COALESCE(q.bandwidth_limit_bytes, 0),
		       COALESCE(SUM(f.storage_limit_bytes) FILTER (WHERE f.floor = 'standard'), 0),
		       COALESCE(SUM(f.storage_limit_bytes) FILTER (WHERE f.floor = 'vault'), 0),
		       COUNT(f.floor)
		  FROM tenant_quotas q
		  LEFT JOIN tenant_floor_quotas f ON f.tenant_id = q.tenant_id
		 WHERE q.tenant_id = $1
		 GROUP BY q.storage_limit_bytes, q.bandwidth_limit_bytes`,
		tenantID).Scan(&total, &override, &stdLimit, &vaultLimit, &floors)
	if err != nil {
		return EgressAllowance{}, fmt.Errorf("egress allowance for tenant %s: %w", tenantID, err)
	}

	a := EgressAllowance{Source: EgressSourceQuota, PlanBytes: PlanEgressAllowance(total, 0)}
	if floors > 0 {
		a.Source = EgressSourceHouse
		a.PlanBytes = PlanEgressAllowance(stdLimit, vaultLimit)
	}
	a.Bytes = a.PlanBytes
	if override > 0 {
		a.Source = EgressSourceOverride
		a.OverrideBytes = override
		a.Bytes = override
	}
	return a, nil
}

// EgressMonthStart is the first instant of t's UTC calendar month — the one
// month boundary every reader of bandwidth_usage_daily uses.
func EgressMonthStart(t time.Time) time.Time {
	u := t.UTC()
	return time.Date(u.Year(), u.Month(), 1, 0, 0, 0, 0, time.UTC)
}

// EgressResetAt is when the allowance resets: the start of the next UTC month.
func EgressResetAt(t time.Time) time.Time {
	return EgressMonthStart(t).AddDate(0, 1, 0)
}

// MonthEgressBytes sums the tenant's recorded egress — egress only, never
// ingress (Review R13-16) — from monthStart on. The live counter in
// internal/api loads from it once per tenant per month.
func MonthEgressBytes(ctx context.Context, db *sql.DB, tenantID string, monthStart time.Time) (int64, error) {
	var used int64
	err := db.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(egress_bytes), 0) FROM bandwidth_usage_daily
		  WHERE tenant_id = $1 AND date >= $2::date`,
		tenantID, monthStart.UTC().Format("2006-01-02")).Scan(&used)
	if err != nil {
		return 0, fmt.Errorf("month egress for tenant %s: %w", tenantID, err)
	}
	return used, nil
}

// EgressMonthSeconds is the month the throttle paces against: 30 days.
const EgressMonthSeconds = 2_592_000

// EgressThrottle holds the knobs of decision D-25 (the throttle shape).
type EgressThrottle struct {
	MinBytesPerSec int64   // EGRESS_THROTTLE_MIN_BYTES_PER_SEC
	Factor         float64 // EGRESS_THROTTLE_FACTOR
	MaxStreams     int     // EGRESS_THROTTLE_MAX_STREAMS
}

// DefaultEgressThrottle is the default shape: past the allowance a tenant
// downloads at the pace that spends one more allowance in a month, never
// below 64 KiB/s, on at most 16 concurrent responses.
func DefaultEgressThrottle() EgressThrottle {
	return EgressThrottle{MinBytesPerSec: 65536, Factor: 1.0, MaxStreams: 16}
}

// Rate is the per-tenant cap, in bytes per second, that applies past the
// allowance: max(MinBytesPerSec, Factor × allowance / 30 days).
func (c EgressThrottle) Rate(allowanceBytes int64) int64 {
	paced := int64(c.Factor * float64(allowanceBytes/EgressMonthSeconds))
	if paced < c.MinBytesPerSec {
		return c.MinBytesPerSec
	}
	return paced
}

// EgressStatus is a tenant's egress position this month — what the overview,
// the admin tenant page and /api/v1/user/usage show.
type EgressStatus struct {
	Allowance EgressAllowance
	UsedBytes int64
	// Over: the allowance is spent. Throttled: over AND the egress_throttle
	// flag is on for this tenant, so downloads are being paced.
	Over      bool
	Throttled bool
	// RateBytesPerSec is the cap that applies past the allowance.
	RateBytesPerSec int64
	ResetAt         time.Time
}

// EgressStatusReader is the live reader of a tenant's egress position (the
// in-process counter in internal/api). The dashboard takes it as a dependency.
type EgressStatusReader interface {
	EgressStatus(ctx context.Context, tenantID string) (EgressStatus, error)
}

// EgressStatusFromDB builds the status from the database alone: the
// allowance and the recorded month egress, with the default throttle shape
// and no enforcement. It is what a process without the live counter (tests,
// tools) reads; the server's counter adds responses still in flight.
func EgressStatusFromDB(ctx context.Context, db *sql.DB, tenantID string, now time.Time) (EgressStatus, error) {
	a, err := NewQuotaManager(db).EgressAllowance(ctx, tenantID)
	if err != nil {
		return EgressStatus{}, err
	}
	used, err := MonthEgressBytes(ctx, db, tenantID, EgressMonthStart(now))
	if err != nil {
		return EgressStatus{}, err
	}
	return EgressStatus{
		Allowance:       a,
		UsedBytes:       used,
		Over:            EgressOver(used, a.Bytes),
		RateBytesPerSec: DefaultEgressThrottle().Rate(a.Bytes),
		ResetAt:         EgressResetAt(now),
	}, nil
}

// EgressOver reports whether used bytes have spent the allowance. A tenant
// with no quota at all (allowance 0) has nothing to download and is never
// "over".
func EgressOver(usedBytes, allowanceBytes int64) bool {
	return allowanceBytes > 0 && usedBytes >= allowanceBytes
}
