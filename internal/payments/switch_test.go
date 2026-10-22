package payments

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// WP-PAY-5 item 1: "Switch to crypto at renewal".

// renewingProvider is a provider that renews by itself and records its
// renewal calls.
type renewingProvider struct {
	fakeProvider
	stops, resumes int
	stopErr        error
	onStop         func()
	// stopErrOnce fails the next stop only; panicOnStop dies inside it (a
	// process killed mid-call); refs are the references stopped by name;
	// ctxErrAtStop is the call context's state after onStop ran.
	stopErrOnce  error
	panicOnStop  bool
	refs         []string
	ctxErrAtStop error
}

func (r *renewingProvider) Renewal() Renewal { return RenewalAuto }
func (r *renewingProvider) StopRenewal(ctx context.Context, _ string) error {
	return r.stop(ctx)
}

// StopRenewalRef makes the fake a RefRenewalStopper, as Stripe is.
func (r *renewingProvider) StopRenewalRef(ctx context.Context, _, ref string) error {
	if err := r.stop(ctx); err != nil {
		return err
	}
	r.refs = append(r.refs, ref)
	return nil
}

func (r *renewingProvider) stop(ctx context.Context) error {
	r.stops++
	if r.panicOnStop {
		panic("process killed during the provider call")
	}
	if r.onStop != nil {
		r.onStop()
	}
	r.ctxErrAtStop = ctx.Err()
	if err := r.stopErrOnce; err != nil {
		r.stopErrOnce = nil
		return err
	}
	return r.stopErr
}
func (r *renewingProvider) ResumeRenewal(context.Context, string) error {
	r.resumes++
	return nil
}

func (f *fixture) switchRegistry(t *testing.T, btcpayOn bool) (*Registry, *renewingProvider) {
	t.Helper()
	stripe := &renewingProvider{fakeProvider: fakeProvider{name: ProviderStripe, state: Ready}}
	btc := &fakeProvider{name: ProviderBTCPay, state: Ready}
	r := NewRegistry(f.db, flagSet{FlagPrefix + ProviderStripe: true, FlagPrefix + ProviderBTCPay: btcpayOn}, 0, nil)
	r.Register(stripe)
	r.Register(btc)
	t.Cleanup(func() { _, _ = f.db.Exec(`DELETE FROM audit_logs WHERE tenant_id = $1`, f.tenantID) })
	return r, stripe
}

func (f *fixture) pendingProvider(t *testing.T) string {
	t.Helper()
	var p sql.NullString
	require.NoError(t, f.db.QueryRow(`SELECT pending_provider FROM entitlements WHERE tenant_id = $1`, f.tenantID).Scan(&p))
	return p.String
}

func TestSwitchToCrypto_StopsRenewal_KeepsPaidThrough(t *testing.T) {
	// Arrange: Stripe renews a house paid 20 days ahead.
	f := newFixture(t)
	pt := time.Now().Add(20 * 24 * time.Hour).UTC().Truncate(time.Second)
	f.tx(t, func(tx *sql.Tx) error {
		return Grant(context.Background(), tx, f.tenantID, Order{Std: 1, Period: PeriodMonthly}, pt, ProviderStripe, "sub_s", RenewalAuto)
	})
	r, stripe := f.switchRegistry(t, true)

	// Act
	_, err := SwitchToPrepaid(context.Background(), r, f.tenantID, ProviderBTCPay, "customer")

	// Assert: Stripe told to stop at period end; prepaid, pending btcpay;
	// the date untouched; audited.
	require.NoError(t, err)
	assert.Equal(t, 1, stripe.stops)
	e := f.entitlement(t)
	assert.Equal(t, RenewalPrepaid, e.Renewal)
	assert.Equal(t, ProviderStripe, e.Provider, "Stripe still names the house until the bitcoin invoice settles")
	assert.True(t, pt.Equal(e.PaidThrough))
	assert.Equal(t, ProviderBTCPay, f.pendingProvider(t))
	assert.Equal(t, 1, f.auditCount(t, "billing.provider_switch_requested"))
	assert.Equal(t, 1, f.auditCount(t, "billing.renewal_stopped"))

	// Again: idempotent — the renewal is already stopped, no second call.
	_, err = SwitchToPrepaid(context.Background(), r, f.tenantID, ProviderBTCPay, "customer")
	require.NoError(t, err)
	assert.Equal(t, 1, stripe.stops)
}

func TestSwitchToCrypto_Refusals(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	// No house.
	r, stripe := f.switchRegistry(t, true)
	_, err := SwitchToPrepaid(ctx, r, f.tenantID, ProviderBTCPay, "customer")
	assert.ErrorIs(t, err, ErrNothingToSwitch)
	// pay_btcpay off.
	f.tx(t, func(tx *sql.Tx) error {
		return Grant(ctx, tx, f.tenantID, Order{Std: 1, Period: PeriodMonthly}, time.Now().Add(48*time.Hour), ProviderStripe, "sub_s", RenewalAuto)
	})
	off, _ := f.switchRegistry(t, false)
	_, err = SwitchToPrepaid(ctx, off, f.tenantID, ProviderBTCPay, "customer")
	assert.ErrorIs(t, err, ErrProviderDisabled)
	// Stripe refuses the stop: nothing written.
	stripe.stopErr = fmt.Errorf("%w: %w", ErrProviderRefused, assert.AnError) // a permanent refusal reverts (RP33-3)
	_, err = SwitchToPrepaid(ctx, r, f.tenantID, ProviderBTCPay, "customer")
	require.Error(t, err)
	assert.Equal(t, RenewalAuto, f.entitlement(t).Renewal)
	assert.Empty(t, f.pendingProvider(t))
	// A switch to a provider that renews by itself is not "to prepaid".
	_, err = SwitchToPrepaid(ctx, r, f.tenantID, ProviderStripe, "customer")
	assert.ErrorIs(t, err, ErrNothingToSwitch)
}

func TestDueReminders_CarryThePendingSwitch(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.tx(t, func(tx *sql.Tx) error {
		return Grant(ctx, tx, f.tenantID, Order{Std: 1, Period: PeriodMonthly}, time.Now().Add(10*24*time.Hour), ProviderStripe, "sub_r", RenewalPrepaid)
	})
	_, err := f.db.Exec(`UPDATE entitlements SET pending_provider = 'btcpay' WHERE tenant_id = $1`, f.tenantID)
	require.NoError(t, err)

	due, err := DueReminders(ctx, f.db, 14*24*time.Hour, time.Now())
	require.NoError(t, err)

	var mine *DueReminder
	for i := range due {
		if due[i].TenantID == f.tenantID {
			mine = &due[i]
		}
	}
	require.NotNil(t, mine)
	assert.Equal(t, ProviderBTCPay, mine.PendingProvider)
}

func TestDoublePeriod_ZeroRowsNeverMatch_BothBoundsDo(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	order := Order{Std: 1, Period: PeriodMonthly}
	settle := func(provider, id string, cents int64, start, end time.Time) Settled {
		var s Settled
		f.tx(t, func(tx *sql.Tx) error {
			var err error
			s, err = Settle(ctx, tx, Payment{TenantID: f.tenantID, Provider: provider, ProviderID: id + f.tenantID, Kind: KindCharge,
				Purpose: PurposeNew, AmountCents: cents, Order: &order, PeriodStart: start, PeriodEnd: end, Unnumbered: true}, now)
			return err
		})
		return s
	}
	// A trialing subscription's $0 first invoice over the coming month.
	settle(ProviderStripe, "in_trial_", 0, now, now.Add(30*24*time.Hour))
	// A paid bitcoin month over the same dates: the $0 row paid nothing.
	assert.Nil(t, settle(ProviderBTCPay, "btc_a_", 1000, now, now.Add(30*24*time.Hour)).DoublePeriod)
	// A later, adjacent card period does not overlap the bitcoin month.
	assert.Nil(t, settle(ProviderStripe, "in_next_", 1000, now.Add(30*24*time.Hour), now.Add(60*24*time.Hour)).DoublePeriod)
	// A bitcoin period that ENDS before the card period starts is no match
	// either (the one-sided "other.end > this.start" would flag it).
	assert.Nil(t, settle(ProviderBTCPay, "btc_early_", 1000, now.Add(-60*24*time.Hour), now.Add(-30*24*time.Hour)).DoublePeriod)
	// An overlap on both bounds is.
	d := settle(ProviderManual, "manual_", 1000, now.Add(10*24*time.Hour), now.Add(40*24*time.Hour)).DoublePeriod
	require.NotNil(t, d)
	assert.Equal(t, ProviderManual, d.ThisProvider)
}
