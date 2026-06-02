package api

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/FairForge/vaultaire/internal/email"
	"go.uber.org/zap"
)

// BandwidthAlerter checks tenant egress against configured thresholds and fires
// alerts (event + email) when a threshold is crossed, once per calendar month.
type BandwidthAlerter struct {
	db      *sql.DB
	emailer email.Sender
	logger  *zap.Logger
}

func NewBandwidthAlerter(db *sql.DB, logger *zap.Logger) *BandwidthAlerter {
	return &BandwidthAlerter{db: db, logger: logger}
}

func (a *BandwidthAlerter) SetEmailSender(s email.Sender) { a.emailer = s }

// StartBandwidthAlerts launches a goroutine that seeds default alert rows and
// checks bandwidth thresholds every hour. Nil-safe on receiver and db.
func (a *BandwidthAlerter) StartBandwidthAlerts(ctx context.Context) {
	if a == nil || a.db == nil {
		return
	}
	go func() {
		a.seedDefaultAlerts(ctx)
		a.checkBandwidthAlerts(ctx)

		ticker := time.NewTicker(1 * time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				a.seedDefaultAlerts(ctx)
				a.checkBandwidthAlerts(ctx)
			}
		}
	}()
}

// seedDefaultAlerts ensures every tenant with a bandwidth limit has 80% and 95%
// email alert rows. Idempotent via ON CONFLICT DO NOTHING.
func (a *BandwidthAlerter) seedDefaultAlerts(ctx context.Context) {
	if a.db == nil {
		return
	}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	rows, err := a.db.QueryContext(cctx,
		`SELECT tenant_id FROM tenant_quotas WHERE bandwidth_limit_bytes > 0`)
	if err != nil {
		a.logger.Error("query tenants for bandwidth alert seeding", zap.Error(err))
		return
	}
	defer func() { _ = rows.Close() }()

	var tenantIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			a.logger.Error("scan tenant for bandwidth alert seeding", zap.Error(err))
			continue
		}
		tenantIDs = append(tenantIDs, id)
	}
	if err := rows.Err(); err != nil {
		a.logger.Error("iterate tenants for bandwidth alert seeding", zap.Error(err))
		return
	}

	for _, id := range tenantIDs {
		for _, pct := range []int{80, 95} {
			if _, err := a.db.ExecContext(cctx,
				`INSERT INTO bandwidth_alerts (tenant_id, threshold_pct, alert_type)
				 VALUES ($1, $2, 'email')
				 ON CONFLICT (tenant_id, threshold_pct, alert_type) DO NOTHING`,
				id, pct); err != nil {
				a.logger.Error("seed bandwidth alert",
					zap.String("tenant", id), zap.Int("pct", pct), zap.Error(err))
			}
		}
	}
}

// checkBandwidthAlerts iterates tenants with a bandwidth limit and fires alerts
// for any threshold crossed this calendar month that hasn't already been alerted.
func (a *BandwidthAlerter) checkBandwidthAlerts(ctx context.Context) {
	if a.db == nil {
		return
	}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	rows, err := a.db.QueryContext(cctx,
		`SELECT tenant_id, bandwidth_limit_bytes FROM tenant_quotas WHERE bandwidth_limit_bytes > 0`)
	if err != nil {
		a.logger.Error("query tenants for bandwidth alerts", zap.Error(err))
		return
	}
	defer func() { _ = rows.Close() }()

	type tenantLimit struct {
		tenantID   string
		limitBytes int64
	}
	var tenants []tenantLimit
	for rows.Next() {
		var tl tenantLimit
		if err := rows.Scan(&tl.tenantID, &tl.limitBytes); err != nil {
			a.logger.Error("scan tenant bandwidth limit", zap.Error(err))
			continue
		}
		tenants = append(tenants, tl)
	}
	if err := rows.Err(); err != nil {
		a.logger.Error("iterate tenants for bandwidth alerts", zap.Error(err))
		return
	}

	for _, tl := range tenants {
		a.checkTenantAlerts(cctx, tl.tenantID, tl.limitBytes)
	}
}

func (a *BandwidthAlerter) checkTenantAlerts(ctx context.Context, tenantID string, limitBytes int64) {
	var usedBytes int64
	if err := a.db.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(egress_bytes), 0)
		 FROM bandwidth_usage_daily
		 WHERE tenant_id = $1 AND date >= date_trunc('month', CURRENT_DATE)`,
		tenantID).Scan(&usedBytes); err != nil {
		a.logger.Error("query month egress for bandwidth alert",
			zap.String("tenant", tenantID), zap.Error(err))
		return
	}

	rows, err := a.db.QueryContext(ctx,
		`SELECT id, threshold_pct, alert_type, last_fired_at
		 FROM bandwidth_alerts
		 WHERE tenant_id = $1 AND enabled = true`,
		tenantID)
	if err != nil {
		a.logger.Error("query bandwidth alerts for tenant",
			zap.String("tenant", tenantID), zap.Error(err))
		return
	}
	defer func() { _ = rows.Close() }()

	type alertRow struct {
		id           string
		thresholdPct int
		alertType    string
		lastFiredAt  sql.NullTime
	}
	var alerts []alertRow
	for rows.Next() {
		var ar alertRow
		if err := rows.Scan(&ar.id, &ar.thresholdPct, &ar.alertType, &ar.lastFiredAt); err != nil {
			a.logger.Error("scan bandwidth alert", zap.Error(err))
			continue
		}
		alerts = append(alerts, ar)
	}
	if err := rows.Err(); err != nil {
		a.logger.Error("iterate bandwidth alerts", zap.Error(err))
		return
	}

	monthStart := bandwidthAlertMonthStart()

	for _, ar := range alerts {
		if usedBytes*100 < limitBytes*int64(ar.thresholdPct) {
			continue
		}
		if ar.lastFiredAt.Valid && !ar.lastFiredAt.Time.Before(monthStart) {
			continue
		}
		a.fireAlert(ctx, tenantID, ar.id, ar.thresholdPct, ar.alertType, usedBytes, limitBytes)
	}
}

func (a *BandwidthAlerter) fireAlert(ctx context.Context, tenantID, alertID string, thresholdPct int, alertType string, usedBytes, limitBytes int64) {
	pctUsed := int64(0)
	if limitBytes > 0 {
		pctUsed = usedBytes * 100 / limitBytes
	}

	emitEvent(ctx, a.db, a.logger, "bandwidth.alert", tenantID, map[string]interface{}{
		"threshold_pct": thresholdPct,
		"used_bytes":    usedBytes,
		"limit_bytes":   limitBytes,
		"pct_used":      pctUsed,
	})

	if alertType == "email" && a.emailer != nil {
		to := a.bandwidthTenantEmail(ctx, tenantID)
		if to != "" {
			subject := fmt.Sprintf("You've used %d%% of your stored.ge bandwidth limit", pctUsed)
			body := fmt.Sprintf(
				"Your stored.ge bandwidth usage has reached %s of your %s monthly limit (%d%%).",
				formatBandwidthBytes(usedBytes), formatBandwidthBytes(limitBytes), pctUsed)
			if err := a.emailer.Send(ctx, to, subject, body, body); err != nil {
				a.logger.Warn("send bandwidth alert email",
					zap.String("tenant", tenantID), zap.Error(err))
			}
		}
	}

	if _, err := a.db.ExecContext(ctx,
		`UPDATE bandwidth_alerts SET last_fired_at = NOW() WHERE id = $1`,
		alertID); err != nil {
		a.logger.Error("update bandwidth alert last_fired_at",
			zap.String("alert", alertID), zap.Error(err))
	}
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

func bandwidthAlertMonthStart() time.Time {
	now := time.Now().UTC()
	return time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
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
