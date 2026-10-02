package api

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/FairForge/vaultaire/internal/email"
	"github.com/FairForge/vaultaire/internal/usage"
	"go.uber.org/zap"
)

// BandwidthAlerter is the warning ladder of the egress allowance (WP-R10-9):
// every hour it reads each active tenant's position from the SAME allowance
// (internal/usage) and the SAME month counter (egressMeter) the throttle
// uses, and sends one notice per threshold per UTC month — 80 % and 95 % for
// every tenant with an allowance, and at 100 % the notice that states the
// rate and the reset date. Before WP-R10-9 it covered only tenants with an
// admin-set limit and the enforcement counted ingress too (Review R13-16).
type BandwidthAlerter struct {
	db      *sql.DB
	emailer email.Sender
	logger  *zap.Logger
	egress  usage.EgressStatusReader // the live counter; nil = the recorded month
	now     func() time.Time
}

func NewBandwidthAlerter(db *sql.DB, logger *zap.Logger) *BandwidthAlerter {
	return &BandwidthAlerter{db: db, logger: logger, now: time.Now}
}

func (a *BandwidthAlerter) SetEmailSender(s email.Sender) { a.emailer = s }

// SetEgressReader gives the alerter the server's live month counter, so its
// "used" is the throttle's "used".
func (a *BandwidthAlerter) SetEgressReader(r usage.EgressStatusReader) { a.egress = r }

// egressAlertLadder is the default ladder. A tenant's row for a step is
// created the first time the step is crossed (nothing is seeded per tenant
// per hour); a row with enabled = false silences that step.
var egressAlertLadder = []int{80, 95, 100}

// bandwidthAlertsJobName is the job's name in job_runs and on the metrics.
const bandwidthAlertsJobName = "bandwidth_alerts"

// spec is the alerter's schedule: one pass shortly after boot, then hourly
// (jobs.go). A pass fails when the candidate list could not be read; a
// tenant whose check or e-mail failed is logged and retried next pass
// (Review R13-09).
func (a *BandwidthAlerter) spec() jobSpec {
	return jobSpec{
		Name: bandwidthAlertsJobName, Every: time.Hour, BootDelay: 40 * time.Second, MaxRunTime: 30 * time.Minute,
		Run: func(ctx context.Context) (jobReport, error) {
			n, err := a.checkBandwidthAlerts(ctx)
			return jobReport{Rows: int64(n)}, err
		},
	}
}

// checkBandwidthAlerts walks the tenants with egress recorded this UTC month
// and fires any step crossed and not yet sent. It returns how many tenants
// it checked.
func (a *BandwidthAlerter) checkBandwidthAlerts(ctx context.Context) (int, error) {
	if a == nil || a.db == nil {
		return 0, nil
	}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	tenants, err := a.alertCandidates(cctx)
	cancel()
	if err != nil {
		return 0, fmt.Errorf("query tenants for egress alerts: %w", err)
	}
	for _, id := range tenants {
		// Per-tenant budget (Review R13-09): one stalled e-mail provider used
		// to eat the whole pass's 30 s and silently skip every later tenant.
		tctx, tcancel := context.WithTimeout(ctx, bandwidthAlertTenantTimeout)
		a.checkTenantAlerts(tctx, id)
		tcancel()
	}
	return len(tenants), nil
}

// alertCandidates lists, with one set-based query, the tenants that have
// egress recorded this UTC month. A tenant with none cannot have crossed a
// step.
func (a *BandwidthAlerter) alertCandidates(ctx context.Context) ([]string, error) {
	rows, err := a.db.QueryContext(ctx,
		`SELECT tenant_id FROM bandwidth_usage_daily
		  WHERE date >= $1::date
		  GROUP BY tenant_id HAVING SUM(egress_bytes) > 0`,
		usage.EgressMonthStart(a.now()).Format("2006-01-02"))
	if err != nil {
		return nil, fmt.Errorf("list tenants with egress this month: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var tenants []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan tenant with egress this month: %w", err)
		}
		tenants = append(tenants, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate tenants with egress this month: %w", err)
	}
	return tenants, nil
}

// bandwidthAlertTenantTimeout bounds one tenant's check + e-mail.
const bandwidthAlertTenantTimeout = 10 * time.Second

// status reads the tenant's position: the server's counter when wired, the
// database otherwise.
func (a *BandwidthAlerter) status(ctx context.Context, tenantID string) (usage.EgressStatus, error) {
	if a.egress != nil {
		return a.egress.EgressStatus(ctx, tenantID)
	}
	return usage.EgressStatusFromDB(ctx, a.db, tenantID, a.now())
}

type egressAlertRow struct {
	id           string
	thresholdPct int
	alertType    string
	enabled      bool
	lastFiredAt  sql.NullTime
}

func (a *BandwidthAlerter) checkTenantAlerts(ctx context.Context, tenantID string) {
	st, err := a.status(ctx, tenantID)
	if err != nil {
		a.logger.Error("read egress status for alert",
			zap.String("tenant", tenantID), zap.Error(err))
		return
	}
	allowance, used := st.Allowance.Bytes, st.UsedBytes
	// Below the first step there is nothing to read: the ladder is the only
	// writer of alert rows, so no row can sit under it.
	if allowance <= 0 || !egressCrossed(used, allowance, egressAlertLadder[0]) {
		return
	}

	rows, err := a.db.QueryContext(ctx,
		`SELECT id, threshold_pct, alert_type, enabled, last_fired_at
		   FROM bandwidth_alerts WHERE tenant_id = $1`, tenantID)
	if err != nil {
		a.logger.Error("query egress alerts for tenant",
			zap.String("tenant", tenantID), zap.Error(err))
		return
	}
	var alerts []egressAlertRow
	have := map[int]bool{}
	for rows.Next() {
		var ar egressAlertRow
		if err := rows.Scan(&ar.id, &ar.thresholdPct, &ar.alertType, &ar.enabled, &ar.lastFiredAt); err != nil {
			a.logger.Error("scan egress alert", zap.Error(err))
			continue
		}
		alerts = append(alerts, ar)
		if ar.alertType == "email" {
			have[ar.thresholdPct] = true
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		a.logger.Error("iterate egress alerts", zap.Error(err))
		return
	}
	_ = rows.Close()

	// A ladder step crossed for the first time gets its row now.
	for _, pct := range egressAlertLadder {
		if have[pct] || !egressCrossed(used, allowance, pct) {
			continue
		}
		var id string
		err := a.db.QueryRowContext(ctx,
			`INSERT INTO bandwidth_alerts (tenant_id, threshold_pct, alert_type)
			 VALUES ($1, $2, 'email')
			 ON CONFLICT (tenant_id, threshold_pct, alert_type) DO UPDATE SET threshold_pct = EXCLUDED.threshold_pct
			 RETURNING id`, tenantID, pct).Scan(&id)
		if err != nil {
			a.logger.Error("create egress alert row",
				zap.String("tenant", tenantID), zap.Int("pct", pct), zap.Error(err))
			continue
		}
		alerts = append(alerts, egressAlertRow{id: id, thresholdPct: pct, alertType: "email", enabled: true})
	}

	monthStart := usage.EgressMonthStart(a.now())
	for _, ar := range alerts {
		if !ar.enabled || !egressCrossed(used, allowance, ar.thresholdPct) {
			continue
		}
		if ar.lastFiredAt.Valid && !ar.lastFiredAt.Time.Before(monthStart) {
			continue
		}
		// The 100 % notice says downloads are being paced. Where they are
		// not (the egress_throttle flag is off, or this tenant is exempt) it
		// would be false, so it waits — unfired — for the flag.
		if ar.thresholdPct >= 100 && !st.Throttled {
			continue
		}
		a.fireAlert(ctx, tenantID, ar, st)
	}
}

// egressCrossed reports whether used has reached pct % of the allowance.
func egressCrossed(used, allowance int64, pct int) bool {
	return float64(used)*100 >= float64(allowance)*float64(pct)
}

func (a *BandwidthAlerter) fireAlert(ctx context.Context, tenantID string, ar egressAlertRow, st usage.EgressStatus) {
	used, allowance := st.UsedBytes, st.Allowance.Bytes
	pctUsed := int64(0)
	if allowance > 0 {
		pctUsed = used * 100 / allowance
	}

	emitEvent(ctx, a.db, a.logger, "bandwidth.alert", tenantID, map[string]interface{}{
		"threshold_pct":      ar.thresholdPct,
		"used_bytes":         used,
		"limit_bytes":        allowance,
		"pct_used":           pctUsed,
		"throttled":          st.Throttled,
		"rate_bytes_per_sec": st.RateBytesPerSec,
		"resets_at":          st.ResetAt.Format(time.RFC3339),
	})

	if ar.alertType == "email" && a.emailer != nil {
		to := a.bandwidthTenantEmail(ctx, tenantID)
		if to != "" {
			subject, body := egressAlertMessage(ar.thresholdPct, st)
			if err := a.emailer.Send(ctx, to, subject, body, body); err != nil {
				// Not marked fired: the next hourly pass retries (Review
				// R13-09 — a failed send used to stamp last_fired_at and the
				// alert was lost for the rest of the month). The event row
				// above already carries it to the dashboard feed.
				a.logger.Warn("send egress alert email — will retry next pass",
					zap.String("tenant", tenantID), zap.Error(err))
				return
			}
		}
	}

	// last_fired_at is a TIMESTAMP without zone: store the UTC wall clock so
	// the comparison with the UTC month start holds in any session time zone.
	if _, err := a.db.ExecContext(ctx,
		`UPDATE bandwidth_alerts SET last_fired_at = (NOW() AT TIME ZONE 'UTC') WHERE id = $1`,
		ar.id); err != nil {
		a.logger.Error("update egress alert last_fired_at",
			zap.String("alert", ar.id), zap.Error(err))
	}
}

// egressAlertMessage words one step of the ladder.
func egressAlertMessage(thresholdPct int, st usage.EgressStatus) (subject, body string) {
	used, allowance := st.UsedBytes, st.Allowance.Bytes
	reset := st.ResetAt.UTC().Format("January 2, 2006")
	rate := formatBandwidthBytes(st.RateBytesPerSec) + "/s"
	if thresholdPct >= 100 {
		subject = "Your Stored egress allowance for this month is used up"
		body = fmt.Sprintf(
			"You have downloaded %s this month, which is all of your %s egress allowance. "+
				"Until the allowance resets on %s (UTC), downloads from your account are rate-limited to %s in total. "+
				"Nothing is billed and nothing is deleted; uploads, listings and deletes are not affected. "+
				"The allowance follows your quota: adding storage at https://stored.ge/dashboard/billing raises it within a minute.",
			formatBandwidthBytes(used), formatBandwidthBytes(allowance), reset, rate)
		return subject, body
	}
	pctUsed := used * 100 / allowance
	subject = fmt.Sprintf("You've used %d%% of your Stored egress allowance", pctUsed)
	body = fmt.Sprintf(
		"Your Stored downloads this month have reached %s of your %s egress allowance (%d%%). "+
			"Past the allowance, downloads are rate-limited to %s in total until it resets on %s (UTC) — never billed. "+
			"Uploads are not affected.",
		formatBandwidthBytes(used), formatBandwidthBytes(allowance), pctUsed, rate, reset)
	return subject, body
}

func (a *BandwidthAlerter) bandwidthTenantEmail(ctx context.Context, tenantID string) string {
	var e sql.NullString
	if err := a.db.QueryRowContext(ctx,
		`SELECT email FROM tenants WHERE id = $1`, tenantID).Scan(&e); err != nil {
		return ""
	}
	if e.Valid {
		return e.String
	}
	return ""
}

func formatBandwidthBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}
