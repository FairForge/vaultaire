package handlers

import (
	"context"
	"database/sql"
	"math"
	"time"

	"github.com/FairForge/vaultaire/internal/usage"
)

// egressStatus reads a tenant's egress position (WP-R10-9): from the
// server's live month counter when the router was given one, from the
// database alone otherwise (tests, a dashboard without the API server).
// Either way the allowance is internal/usage's — the one definition.
func egressStatus(ctx context.Context, db *sql.DB, eg usage.EgressStatusReader, tenantID string) (usage.EgressStatus, error) {
	if eg != nil {
		return eg.EgressStatus(ctx, tenantID)
	}
	return usage.EgressStatusFromDB(ctx, db, tenantID, time.Now())
}

// populateEgress fills the egress keys the overview and the admin tenant
// page share. It leaves data untouched when the tenant has no quota row.
func populateEgress(ctx context.Context, db *sql.DB, eg usage.EgressStatusReader, tenantID string, data map[string]any) {
	st, err := egressStatus(ctx, db, eg, tenantID)
	if err != nil {
		return
	}
	a := st.Allowance
	pct := 0
	if a.Bytes > 0 {
		pct = int(math.Min(100, math.Floor(float64(st.UsedBytes)*100/float64(a.Bytes))))
	}
	data["EgressFmt"] = formatBytes(st.UsedBytes)
	data["EgressAllowanceFmt"] = formatBytes(a.Bytes)
	data["EgressPct"] = pct
	data["EgressBarClass"] = ""
	if pct >= 90 {
		data["EgressBarClass"] = "danger"
	} else if pct >= 75 {
		data["EgressBarClass"] = "warning"
	}
	data["EgressOver"] = st.Over
	data["EgressThrottled"] = st.Throttled
	data["EgressRateFmt"] = formatBytes(st.RateBytesPerSec) + "/s"
	data["EgressResetFmt"] = st.ResetAt.UTC().Format("January 2")

	// What the admin tenant page adds: where the number comes from.
	data["EgressPlanFmt"] = formatBytes(a.PlanBytes)
	data["EgressSource"] = a.Source
	data["EgressOverrideFmt"] = ""
	data["EgressOverrideGB"] = a.OverrideBytes / usage.GB
	if a.OverrideBytes > 0 {
		data["EgressOverrideFmt"] = formatBytes(a.OverrideBytes)
	}
	switch {
	case st.Throttled:
		data["EgressState"] = "throttled"
	case st.Over:
		data["EgressState"] = "over, not slowed (egress_throttle is off for this tenant)"
	default:
		data["EgressState"] = "under the allowance"
	}
}
