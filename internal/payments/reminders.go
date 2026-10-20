package payments

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"time"
)

// sentHas reports whether the reminders_sent document has the key.
func sentHas(doc []byte, key string) bool {
	if len(doc) == 0 {
		return false
	}
	var m map[string]any
	if err := json.Unmarshal(doc, &m); err != nil {
		return false
	}
	_, ok := m[key]
	return ok
}

// Renewal reminders (WP-PAY-3 item 2, docs/PAYMENTS.md § 3.8): for a house
// nobody renews by itself (renewal 'prepaid', lapsed_at NULL) four steps —
// 14 days before the date, 3 days before, the second day of grace ("in
// grace, N days left") and the last day of grace. Each step goes out once
// per (step, paid_through): entitlements.reminders_sent (098) keys
// "<step>@<paid_through RFC3339>" → sent_at, so a renewal (a new
// paid_through) resets every step by itself. The hourly payment_reminders
// job (internal/api/payment_reminders.go) asks DueReminders, sends, and
// MarkReminderSent inside a transaction of its own.

// ReminderStep names a step.
type ReminderStep string

// The four steps, in order.
const (
	StepBefore14d ReminderStep = "t-14d"
	StepBefore3d  ReminderStep = "t-3d"
	StepInGrace   ReminderStep = "t+1d"
	StepLastDay   ReminderStep = "last-day"
)

// ReminderSteps is every step (the metric's series are pre-created from it).
var ReminderSteps = []ReminderStep{StepBefore14d, StepBefore3d, StepInGrace, StepLastDay}

// DueReminder is one (tenant, step) to send now.
type DueReminder struct {
	TenantID    string
	Email       string
	Step        ReminderStep
	PaidThrough time.Time
	GraceEnds   time.Time
	Order       Order
	Provider    string
	// DaysLeft: until paid_through for the before-steps, until the end of
	// grace for the grace steps (rounded up).
	DaysLeft int
}

// ReminderKey is the reminders_sent key of a (step, paid_through).
func ReminderKey(step ReminderStep, paidThrough time.Time) string {
	return string(step) + "@" + paidThrough.UTC().Format(time.RFC3339)
}

// stepAt is the step due at now for a row paid through pt with the grace,
// "" when none.
func stepAt(now, pt time.Time, grace time.Duration) ReminderStep {
	const day = 24 * time.Hour
	switch {
	case now.Before(pt.Add(-14 * day)):
		return ""
	case now.Before(pt.Add(-3 * day)):
		return StepBefore14d
	case now.Before(pt):
		return StepBefore3d
	case now.Before(pt.Add(day)):
		return "" // the first day of grace: nothing (a payment may be landing)
	case now.Before(pt.Add(grace - day)):
		return StepInGrace
	case now.Before(pt.Add(grace)):
		return StepLastDay
	}
	return "" // past grace: the lapse job's business
}

func daysUp(d time.Duration) int {
	if d <= 0 {
		return 0
	}
	return int(math.Ceil(d.Hours() / 24))
}

// DueReminders lists the (tenant, step) pairs due at now and not yet sent
// for their period: prepaid, granted rows whose date is within 14 days
// ahead or within grace behind.
func DueReminders(ctx context.Context, db *sql.DB, grace time.Duration, now time.Time) ([]DueReminder, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT e.tenant_id, COALESCE(t.email, ''), e.paid_through, e.std_tb, e.vault_tb, e.pin_hot_tb, e.period, e.provider,
		       e.reminders_sent
		  FROM entitlements e JOIN tenants t ON t.id = e.tenant_id
		 WHERE e.renewal = 'prepaid' AND e.lapsed_at IS NULL
		   AND e.paid_through > $1::timestamptz - $2 * INTERVAL '1 second'
		   AND e.paid_through <= $1::timestamptz + INTERVAL '14 days'
		   AND t.suspended_at IS NULL
		 ORDER BY e.paid_through, e.tenant_id`, now.UTC(), grace.Seconds())
	if err != nil {
		return nil, fmt.Errorf("due reminders: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []DueReminder
	for rows.Next() {
		var d DueReminder
		var period string
		var sent []byte
		if err := rows.Scan(&d.TenantID, &d.Email, &d.PaidThrough, &d.Order.Std, &d.Order.Vault, &d.Order.PinHot, &period, &d.Provider, &sent); err != nil {
			return nil, fmt.Errorf("due reminders: scan: %w", err)
		}
		d.Order.Period = Period(period)
		d.GraceEnds = d.PaidThrough.Add(grace)
		step := stepAt(now, d.PaidThrough, grace)
		if step == "" || sentHas(sent, ReminderKey(step, d.PaidThrough)) {
			continue
		}
		d.Step = step
		if step == StepBefore14d || step == StepBefore3d {
			d.DaysLeft = daysUp(d.PaidThrough.Sub(now))
		} else {
			d.DaysLeft = daysUp(d.GraceEnds.Sub(now))
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("due reminders: %w", err)
	}
	return out, nil
}

// MarkReminderSent records that the step went out for the period.
func MarkReminderSent(ctx context.Context, tx *sql.Tx, tenantID string, step ReminderStep, paidThrough, at time.Time) error {
	if _, err := tx.ExecContext(ctx, `
		UPDATE entitlements SET reminders_sent = reminders_sent || jsonb_build_object($2::text, $3::text)
		 WHERE tenant_id = $1`, tenantID, ReminderKey(step, paidThrough), at.UTC().Format(time.RFC3339)); err != nil {
		return fmt.Errorf("mark reminder %s of tenant %s: %w", step, tenantID, err)
	}
	return nil
}
