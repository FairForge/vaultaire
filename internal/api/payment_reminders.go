package api

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/FairForge/vaultaire/internal/payments"
	"go.uber.org/zap"
)

// The payment_reminders job (WP-PAY-3 item 2, docs/PAYMENTS.md § 3.8):
// hourly, 5 min ceiling, through the one scheduler. For every prepaid,
// granted entitlement (nobody renews it by itself) it sends the step that
// is due — 14 days before the date, 3 days before, the second day of
// grace, the last day of grace — once per (step, paid_through)
// (payments.DueReminders / MarkReminderSent, entitlements.reminders_sent).
// A mail that fails is a note on an ok run and the step is tried again next
// hour; vaultaire_payment_reminders_total{step,outcome}. A process with no
// mail sender sends and marks nothing (a note), so a later process sends.

const paymentRemindersJobName = "payment_reminders"

// PaymentRemindersRunner is the job.
type PaymentRemindersRunner struct {
	db     *sql.DB
	logger *zap.Logger
	// Grace: ENTITLEMENT_GRACE (the grace steps are placed on it).
	Grace time.Duration
	// Mailer sends; nil or a Mailer with no Sender = nothing is sent.
	Mailer *payments.Mailer
	// Flags gates the grace steps (RP24-2): "in grace, N days left" and
	// "your house ends tomorrow" announce an ending only the lapse job
	// performs, so they go out only where entitlement_lapse is ON for the
	// tenant (the job's own gate). Nil = no flag source = off, as
	// everywhere. The before-steps go regardless — they are true either way.
	Flags payments.FlagSource
	// JobName is the job_runs name (tests use their own: the table is shared).
	JobName    string
	MaxRunTime time.Duration

	onlyTenants func(tenantID string) bool
	now         func() time.Time
}

// PaymentRemindersResult is one run's outcome (job_runs.result).
type PaymentRemindersResult struct {
	Candidates int `json:"candidates"`
	Sent       int `json:"sent"`
	Failed     int `json:"failed"`
	// SkippedFlagOff: grace steps withheld because entitlement_lapse is
	// off for the tenant (RP24-2) — a note, never an error.
	SkippedFlagOff int      `json:"skipped_flag_off,omitempty"`
	Errors         []string `json:"errors,omitempty"`
}

// NewPaymentRemindersRunner builds the job; nil without a database.
func NewPaymentRemindersRunner(db *sql.DB, logger *zap.Logger, grace time.Duration, mailer *payments.Mailer, fl payments.FlagSource) *PaymentRemindersRunner {
	if db == nil {
		return nil
	}
	if logger == nil {
		logger = zap.NewNop()
	}
	if grace <= 0 {
		grace = payments.DefaultGrace
	}
	return &PaymentRemindersRunner{db: db, logger: logger, Grace: grace, Mailer: mailer, Flags: fl,
		JobName: paymentRemindersJobName, MaxRunTime: 5 * time.Minute, now: time.Now}
}

func (r *PaymentRemindersRunner) spec() jobSpec {
	return jobSpec{
		Name: r.JobName, Every: time.Hour, BootDelay: 3 * time.Minute, MaxRunTime: r.MaxRunTime,
		Run: func(ctx context.Context) (jobReport, error) {
			res, err := r.RunOnce(ctx)
			rep := jobReport{Rows: int64(res.Sent), Result: res}
			var notes []string
			if res.SkippedFlagOff > 0 {
				notes = append(notes, fmt.Sprintf("%d grace step(s) withheld: entitlement_lapse off for the tenant", res.SkippedFlagOff))
			}
			notes = append(notes, res.Errors...)
			if len(notes) > 0 {
				rep.Note = strings.Join(notes, "; ")
			}
			return rep, err
		},
	}
}

func (r *PaymentRemindersRunner) scoped(tenantID string) bool {
	return r.onlyTenants == nil || r.onlyTenants(tenantID)
}

// RunOnce is the work (no lock, no job_runs: the scheduler's).
func (r *PaymentRemindersRunner) RunOnce(ctx context.Context) (PaymentRemindersResult, error) {
	var res PaymentRemindersResult
	if r == nil || r.db == nil {
		return res, errors.New("payment reminders: not configured")
	}
	now := r.now()
	due, err := payments.DueReminders(ctx, r.db, r.Grace, now)
	if err != nil {
		return res, err
	}
	canSend := r.Mailer != nil && r.Mailer.Sender != nil
	for _, d := range due {
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		if !r.scoped(d.TenantID) {
			continue
		}
		res.Candidates++
		lapseOn := r.Flags != nil && r.Flags.Enabled(payments.FlagEntitlementLapse, d.TenantID)
		// RP31-7: with the flag off a Stripe house ends at Stripe's deletion
		// on the date — the copy says so, never a grace period.
		d.NoGrace = !lapseOn && d.Provider == payments.ProviderStripe
		if (d.Step == payments.StepInGrace || d.Step == payments.StepLastDay) && !lapseOn {
			// RP24-2: the mail would announce an ending the lapse job only
			// counts as would_clear while the flag is off. Not marked sent:
			// the step goes out if the flag turns on inside its window.
			res.SkippedFlagOff++
			continue
		}
		if !canSend {
			continue
		}
		if err := r.Mailer.SendReminder(ctx, d); err != nil {
			res.Failed++
			payments.CountReminder(d.Step, payments.OutcomeFailed)
			res.Errors = append(res.Errors, fmt.Sprintf("%s %s: %v", d.TenantID, d.Step, err))
			r.logger.Warn("payment reminder not sent", zap.String("tenant", d.TenantID), zap.String("step", string(d.Step)), zap.Error(err))
			continue
		}
		if err := payments.InTx(ctx, r.db, func(tx *sql.Tx) error {
			return payments.MarkReminderSent(ctx, tx, d.TenantID, d.Step, d.PaidThrough, now)
		}); err != nil {
			// Sent but not recorded: the next hour sends it again — one
			// mail too many beats one too few, and the Error line says why.
			res.Errors = append(res.Errors, fmt.Sprintf("%s %s: sent but not marked: %v", d.TenantID, d.Step, err))
			r.logger.Error("payment reminder sent but not marked", zap.String("tenant", d.TenantID), zap.String("step", string(d.Step)), zap.Error(err))
			continue
		}
		res.Sent++
		payments.CountReminder(d.Step, payments.OutcomeSent)
		r.logger.Info("payment reminder sent", zap.String("tenant", d.TenantID), zap.String("step", string(d.Step)),
			zap.Time("paid_through", d.PaidThrough), zap.Int("days_left", d.DaysLeft))
	}
	if res.Candidates > 0 && !canSend {
		res.Errors = append(res.Errors, fmt.Sprintf("%d reminder(s) due, no mail sender on this process", res.Candidates))
	}
	r.logger.Info("payment reminders run", zap.Int("candidates", res.Candidates), zap.Int("sent", res.Sent),
		zap.Int("failed", res.Failed), zap.Int("errors", len(res.Errors)))
	return res, ctx.Err()
}
