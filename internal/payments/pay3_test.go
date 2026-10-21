package payments

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// WP-PAY-3 (Stage 1a Prompt 20): receipts from the ledger, the reminder
// steps, the admin money operations, the persisted readiness log, the
// house_granted audit row.

func (f *fixture) manual(t *testing.T, months Period, order *Order) ManualPaymentResult {
	t.Helper()
	res, err := RecordManualPayment(context.Background(), f.db, ManualPayment{
		TenantID: f.tenantID, AmountCents: 5388, Period: months, Order: order, Note: "wire 2026-10-20", Actor: "admin-1", Reason: "paid by wire"})
	require.NoError(t, err)
	return res
}

func (f *fixture) auditCount(t *testing.T, action string) int {
	t.Helper()
	var n int
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM audit_logs WHERE tenant_id = $1 AND action = $2`, f.tenantID, action).Scan(&n))
	return n
}

func TestManualPayment_GrantsWithoutRow_ThenExtends(t *testing.T) {
	// Arrange
	f := newFixture(t)
	t.Cleanup(func() { _, _ = f.db.Exec(`DELETE FROM audit_logs WHERE tenant_id = $1`, f.tenantID) })
	ctx := context.Background()

	// Act 1: no row → the house must be named; granted for one period.
	_, err := RecordManualPayment(ctx, f.db, ManualPayment{TenantID: f.tenantID, AmountCents: 100, Period: PeriodAnnual, Reason: "x"})
	assert.ErrorIs(t, err, ErrOrderRequired, "a manual payment on a tenant with no row names the house")
	res := f.manual(t, PeriodAnnual, &Order{Std: 1, Period: PeriodAnnual})

	// Assert 1
	assert.True(t, res.Granted)
	assert.Regexp(t, `^ST-\d{4}-\d{6}$`, res.InvoiceNumber)
	e := f.entitlement(t)
	assert.Equal(t, ProviderManual, e.Provider)
	assert.Equal(t, RenewalPrepaid, e.Renewal)
	assert.WithinDuration(t, time.Now().AddDate(1, 0, 0), e.PaidThrough, 2*time.Minute)
	assert.Equal(t, 1, f.auditCount(t, "billing.house_granted"), "one grant that changed the row")
	assert.Equal(t, 1, f.auditCount(t, "billing.payment_recorded_manual"))
	var status, number string
	var start, end sql.NullTime
	require.NoError(t, f.db.QueryRow(`SELECT status, invoice_number, period_start, period_end FROM payments WHERE id = $1`, res.PaymentID).
		Scan(&status, &number, &start, &end))
	assert.Equal(t, StatusSettled, status)
	assert.True(t, start.Valid && end.Valid, "the payment records what it bought")

	// Act 2: a row exists → Extend by one more period, no order needed.
	res2 := f.manual(t, PeriodMonthly, nil)

	// Assert 2
	assert.False(t, res2.Granted)
	assert.WithinDuration(t, e.PaidThrough.UTC().AddDate(0, 1, 0), res2.PaidThrough, time.Minute, "one month on, in UTC (the Postgres rule)")
	assert.NotEqual(t, res.InvoiceNumber, res2.InvoiceNumber)
	assert.Equal(t, 2, f.auditCount(t, "billing.house_granted"), "an extension is a grant that changed the row")
}

// Grant is idempotent: a replay writes no audit row (RETURNING tells).
func TestHouseGranted_AuditOncePerChange(t *testing.T) {
	f := newFixture(t)
	t.Cleanup(func() { _, _ = f.db.Exec(`DELETE FROM audit_logs WHERE tenant_id = $1`, f.tenantID) })
	ctx := context.Background()
	end := time.Now().Add(30 * 24 * time.Hour).Truncate(time.Second)
	for i := range 3 {
		var g Granted
		f.tx(t, func(tx *sql.Tx) error {
			var err error
			g, err = GrantRecorded(ctx, tx, f.tenantID, Order{Std: 2, Period: PeriodMonthly}, end, ProviderStripe, "sub_a", RenewalAuto)
			return err
		})
		AuditGranted(ctx, f.db, f.tenantID, g, "stripe-webhook")
		assert.Equal(t, i == 0, g.Changed, "replay %d", i)
	}
	assert.Equal(t, 1, f.auditCount(t, "billing.house_granted"))
	// A later date is a change.
	var g Granted
	f.tx(t, func(tx *sql.Tx) error {
		var err error
		g, err = GrantRecorded(ctx, tx, f.tenantID, Order{Std: 2, Period: PeriodMonthly}, end.Add(24*time.Hour), ProviderStripe, "sub_a", RenewalAuto)
		return err
	})
	assert.True(t, g.Changed)
}

func TestAdjustPaidThrough_MayShorten(t *testing.T) {
	f := newFixture(t)
	t.Cleanup(func() { _, _ = f.db.Exec(`DELETE FROM audit_logs WHERE tenant_id = $1`, f.tenantID) })
	ctx := context.Background()
	soon := time.Now().Add(time.Hour)
	_, err := AdjustEntitlement(ctx, f.db, Adjustment{TenantID: f.tenantID, PaidThrough: &soon, Reason: "x"})
	assert.ErrorIs(t, err, ErrNoEntitlement)
	f.manual(t, PeriodAnnual, &Order{Std: 1, Period: PeriodAnnual})
	sooner := time.Now().Add(48 * time.Hour).Truncate(time.Second)

	e, err := AdjustEntitlement(ctx, f.db, Adjustment{TenantID: f.tenantID, PaidThrough: &sooner, Order: &Order{Std: 3, Vault: 1, Period: PeriodAnnual},
		Actor: "admin-1", Reason: "refund agreed by mail"})
	require.NoError(t, err)

	assert.True(t, sooner.Equal(e.PaidThrough), "the one writer that may shorten a date")
	assert.Equal(t, 3, e.Order.Std)
	assert.Equal(t, 1, e.Order.Vault)
	house, ok, err := f.qm.GetHouse(ctx, f.tenantID)
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, 3, house.StdTB(), "the floors follow")
	assert.Equal(t, 1, f.auditCount(t, "billing.paid_through_adjusted"))
	_, err = AdjustEntitlement(ctx, f.db, Adjustment{TenantID: f.tenantID, PaidThrough: &sooner})
	assert.ErrorIs(t, err, ErrReasonRequired)
}

func TestRefund_ShortensWhenAsked(t *testing.T) {
	f := newFixture(t)
	t.Cleanup(func() { _, _ = f.db.Exec(`DELETE FROM audit_logs WHERE tenant_id = $1`, f.tenantID) })
	ctx := context.Background()
	paid := f.manual(t, PeriodAnnual, &Order{Std: 1, Period: PeriodAnnual}) // 5388 cents for a year
	before := f.entitlement(t).PaidThrough

	// Half the money back, the date not shortened.
	r1, err := RecordRefund(ctx, f.db, Refund{TenantID: f.tenantID, PaymentID: paid.PaymentID, AmountCents: 2694, ProviderRef: "", Actor: "admin-1", Reason: "goodwill"})
	require.NoError(t, err)
	assert.True(t, before.Equal(f.entitlement(t).PaidThrough))
	var kind, status string
	var refundOf sql.NullString
	require.NoError(t, f.db.QueryRow(`SELECT kind, status, refund_of::text FROM payments WHERE id = $1`, r1.PaymentID).Scan(&kind, &status, &refundOf))
	assert.Equal(t, KindRefund, kind)
	assert.Equal(t, StatusSettled, status)
	assert.Equal(t, paid.PaymentID, refundOf.String)

	// The other half, shortening: the whole payment is refunded → the date
	// goes back by the whole period it bought.
	r2, err := RecordRefund(ctx, f.db, Refund{TenantID: f.tenantID, PaymentID: paid.PaymentID, AmountCents: 2694, ShortenPaidThrough: true, Actor: "admin-1", Reason: "cancelled"})
	require.NoError(t, err)
	assert.NotEqual(t, r1.PaymentID, r2.PaymentID)
	after := f.entitlement(t)
	assert.WithinDuration(t, before.Add(-r2.ShortenedBy), after.PaidThrough, time.Second)
	assert.Greater(t, r2.ShortenedBy, 180*24*time.Hour, "half a year's worth, at least — got %s", r2.ShortenedBy)
	require.NoError(t, f.db.QueryRow(`SELECT status FROM payments WHERE id = $1`, paid.PaymentID).Scan(&status))
	assert.Equal(t, StatusRefunded, status, "fully refunded")
	assert.Equal(t, 2, f.auditCount(t, "billing.refund_recorded"))
	// More than was paid is refused.
	_, err = RecordRefund(ctx, f.db, Refund{TenantID: f.tenantID, PaymentID: paid.PaymentID, AmountCents: 1, Reason: "x"})
	assert.ErrorIs(t, err, ErrRefundExceeds)
}

func TestReceipt_FromTheLedger_OwnTenantOnly(t *testing.T) {
	f := newFixture(t)
	other := newFixture(t)
	ctx := context.Background()
	paid := f.manual(t, PeriodAnnual, &Order{Std: 2, Vault: 1, Period: PeriodAnnual})

	r, err := ReadReceipt(ctx, f.db, f.tenantID, paid.PaymentID)
	require.NoError(t, err)
	assert.Equal(t, paid.InvoiceNumber, r.InvoiceNumber)
	assert.Equal(t, int64(5388), r.AmountCents)
	assert.Equal(t, "$53.88", r.Amount())
	assert.Len(t, r.Lines, 2, "the house lines from the snapshot")
	assert.Equal(t, f.tenantID+"@test.local", r.Email)
	assert.Equal(t, ProviderManual, r.Provider)
	assert.False(t, r.PaidAt.IsZero())
	assert.Empty(t, r.CryptoAmount)

	_, err = ReadReceipt(ctx, f.db, other.tenantID, paid.PaymentID)
	assert.ErrorIs(t, err, ErrNoReceipt, "another tenant's id")
	_, err = ReadReceipt(ctx, f.db, f.tenantID, "not-a-uuid")
	assert.ErrorIs(t, err, ErrNoReceipt)
	list, err := ListReceipts(ctx, f.db, f.tenantID, 10)
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, paid.InvoiceNumber, list[0].InvoiceNumber)
	list, err = ListReceipts(ctx, f.db, other.tenantID, 10)
	require.NoError(t, err)
	assert.Empty(t, list)
}

// The reminder steps: which (tenant, step) are due at a given time, each
// once per period; prepaid rows only; never after a lapse; a new
// paid_through resets the steps.
func TestReminderSteps_DueOncePerPeriod(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	grace := 14 * 24 * time.Hour
	paidThrough := time.Now().Add(13 * 24 * time.Hour).Truncate(time.Second)
	f.tx(t, func(tx *sql.Tx) error {
		return Grant(ctx, tx, f.tenantID, Order{Std: 1, Period: PeriodAnnual}, paidThrough, ProviderManual, "", RenewalPrepaid)
	})
	mine := func(due []DueReminder) []DueReminder {
		var out []DueReminder
		for _, d := range due {
			if d.TenantID == f.tenantID {
				out = append(out, d)
			}
		}
		return out
	}

	// t-14d is due (13 days left), nothing else.
	due, err := DueReminders(ctx, f.db, grace, time.Now())
	require.NoError(t, err)
	require.Len(t, mine(due), 1)
	assert.Equal(t, StepBefore14d, mine(due)[0].Step)
	assert.Equal(t, 13, mine(due)[0].DaysLeft)

	// Sent → not due again.
	f.tx(t, func(tx *sql.Tx) error {
		return MarkReminderSent(ctx, tx, f.tenantID, StepBefore14d, paidThrough, time.Now())
	})
	due, err = DueReminders(ctx, f.db, grace, time.Now())
	require.NoError(t, err)
	assert.Empty(t, mine(due))

	// Two days before: t-3d. In grace, day 2: t+1d ("N days left"). The
	// last day of grace: the last-day step. After grace: nothing.
	for _, c := range []struct {
		at   time.Time
		step ReminderStep
	}{
		{paidThrough.Add(-2 * 24 * time.Hour), StepBefore3d},
		{paidThrough.Add(2 * 24 * time.Hour), StepInGrace},
		{paidThrough.Add(grace - 12*time.Hour), StepLastDay},
	} {
		due, err = DueReminders(ctx, f.db, grace, c.at)
		require.NoError(t, err)
		require.Len(t, mine(due), 1, "at %s", c.at)
		assert.Equal(t, c.step, mine(due)[0].Step)
		f.tx(t, func(tx *sql.Tx) error { return MarkReminderSent(ctx, tx, f.tenantID, c.step, paidThrough, c.at) })
	}
	due, err = DueReminders(ctx, f.db, grace, paidThrough.Add(grace+24*time.Hour))
	require.NoError(t, err)
	assert.Empty(t, mine(due), "past grace: the lapse job's business")

	// A new period resets every step.
	f.tx(t, func(tx *sql.Tx) error { _, err := Extend(ctx, tx, f.tenantID, PeriodAnnual); return err })
	e := f.entitlement(t)
	due, err = DueReminders(ctx, f.db, grace, e.PaidThrough.Add(-10*24*time.Hour))
	require.NoError(t, err)
	require.Len(t, mine(due), 1)
	assert.Equal(t, StepBefore14d, mine(due)[0].Step)

	// Not prepaid → never; lapsed → never.
	_, err = f.db.Exec(`UPDATE entitlements SET renewal = 'auto' WHERE tenant_id = $1`, f.tenantID)
	require.NoError(t, err)
	due, err = DueReminders(ctx, f.db, grace, e.PaidThrough.Add(-10*24*time.Hour))
	require.NoError(t, err)
	assert.Empty(t, mine(due))
	_, err = f.db.Exec(`UPDATE entitlements SET renewal = 'prepaid', lapsed_at = NOW() WHERE tenant_id = $1`, f.tenantID)
	require.NoError(t, err)
	due, err = DueReminders(ctx, f.db, grace, e.PaidThrough.Add(-10*24*time.Hour))
	require.NoError(t, err)
	assert.Empty(t, mine(due))
}

func TestRemindersMetric_PrecreatedAtZero(t *testing.T) {
	for _, step := range ReminderSteps {
		for _, outcome := range []string{OutcomeSent, OutcomeFailed} {
			assert.GreaterOrEqual(t, testutil.ToFloat64(remindersTotal.WithLabelValues(string(step), outcome)), 0.0)
		}
	}
	// The series exist without a first increment (the RP14-9 lesson).
	assert.Equal(t, 4*2, testutil.CollectAndCount(remindersTotal))
}

// RP21-1: an unready observation survives a new Registry over the same
// database; pending is never written.
func TestReadiness_Persisted(t *testing.T) {
	f := newFixture(t)
	name := fmt.Sprintf("persist-%d", time.Now().UnixNano())
	t.Cleanup(func() { _, _ = f.db.Exec(`DELETE FROM payment_provider_readiness WHERE provider = $1`, name) })
	p := &fakeProvider{name: name, state: Unready, why: "prices mismatch"}
	r1 := NewRegistry(f.db, nil, 0, nil)
	r1.Register(p) // observed unready at Register → written
	p.state = Pending
	r1.RecordReadiness(context.Background(), time.Now())

	r2 := NewRegistry(f.db, nil, 0, nil)
	p.state = Ready
	r2.Register(p)

	assert.True(t, r2.UnreadyWithin(context.Background(), name, 24*time.Hour, time.Now()), "the outage is in the table")
	assert.False(t, r2.UnreadyWithin(context.Background(), name, time.Millisecond, time.Now().Add(time.Second)))
	var unready, ready sql.NullTime
	var reason sql.NullString
	require.NoError(t, f.db.QueryRow(`SELECT last_unready_at, last_ready_at, last_reason FROM payment_provider_readiness WHERE provider = $1`, name).
		Scan(&unready, &ready, &reason))
	assert.True(t, unready.Valid)
	assert.True(t, ready.Valid, "the ready observation of r2 was a state change")
	assert.Equal(t, "prices mismatch", reason.String)
}

var _ = errors.New

// RP24-3: a wire recorded on a Stripe `auto` row extends that row — the
// audit reports the row's own provider and renewal, never manual/prepaid.
func TestManualPayment_ExtendAuditsTheRowsProviderAndRenewal(t *testing.T) {
	f := newFixture(t)
	t.Cleanup(func() { _, _ = f.db.Exec(`DELETE FROM audit_logs WHERE tenant_id = $1`, f.tenantID) })
	ctx := context.Background()
	end := time.Now().Add(10 * 24 * time.Hour).Truncate(time.Second)
	f.tx(t, func(tx *sql.Tx) error {
		return Grant(ctx, tx, f.tenantID, Order{Std: 1, Period: PeriodAnnual}, end, ProviderStripe, "sub_x", RenewalAuto)
	})

	f.manual(t, PeriodAnnual, nil) // the Extend branch

	var provider, renewal string
	require.NoError(t, f.db.QueryRow(`
		SELECT metadata->>'provider', metadata->>'renewal' FROM audit_logs
		 WHERE tenant_id = $1 AND action = 'billing.house_granted'`, f.tenantID).Scan(&provider, &renewal))
	assert.Equal(t, ProviderStripe, provider, "the audit says what the row is")
	assert.Equal(t, string(RenewalAuto), renewal)
	e := f.entitlement(t)
	assert.Equal(t, ProviderStripe, e.Provider, "the row keeps its own provider")
	assert.Equal(t, RenewalAuto, e.Renewal)
}

// RP24-3: a refund of a Stripe charge counts under the charge's provider.
func TestRefund_CountedByTheChargesProvider(t *testing.T) {
	f := newFixture(t)
	t.Cleanup(func() { _, _ = f.db.Exec(`DELETE FROM audit_logs WHERE tenant_id = $1`, f.tenantID) })
	ctx := context.Background()
	// A settled Stripe charge in the ledger.
	var chargeID string
	f.tx(t, func(tx *sql.Tx) error {
		s, err := Settle(ctx, tx, Payment{TenantID: f.tenantID, Provider: ProviderStripe,
			ProviderID: "in_rp243_" + f.tenantID, Kind: KindCharge, Purpose: PurposeNew, AmountCents: 1000}, time.Now())
		chargeID = s.ID
		return err
	})
	before := testutil.ToFloat64(paymentsTotal.WithLabelValues(ProviderStripe, KindRefund, OutcomeSettled))
	manualBefore := testutil.ToFloat64(paymentsTotal.WithLabelValues(ProviderManual, KindRefund, OutcomeSettled))

	_, err := RecordRefund(ctx, f.db, Refund{TenantID: f.tenantID, PaymentID: chargeID,
		AmountCents: 400, Actor: "admin-1", Reason: "partial refund"})
	require.NoError(t, err)

	assert.Equal(t, before+1, testutil.ToFloat64(paymentsTotal.WithLabelValues(ProviderStripe, KindRefund, OutcomeSettled)),
		"Stripe money refunded counts as stripe")
	assert.Equal(t, manualBefore, testutil.ToFloat64(paymentsTotal.WithLabelValues(ProviderManual, KindRefund, OutcomeSettled)))
}
