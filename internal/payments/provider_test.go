package payments

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeProvider records what it was asked; state/why is its readiness (the
// zero value is Unready, as a provider with no key).
type fakeProvider struct {
	name      string
	state     Readiness
	why       string
	checkouts int
	resized   *Order
	cancelled int
}

func (f *fakeProvider) Name() string                                { return f.name }
func (f *fakeProvider) Renewal() Renewal                            { return RenewalPrepaid }
func (f *fakeProvider) Ready(context.Context) (Readiness, string)   { return f.state, f.why }
func (f *fakeProvider) StopRenewal(context.Context, string) error   { return nil }
func (f *fakeProvider) ResumeRenewal(context.Context, string) error { return nil }
func (f *fakeProvider) Cancel(context.Context, string) error {
	f.cancelled++
	return nil
}
func (f *fakeProvider) ManageURL(context.Context, string, string) (string, error) {
	return "https://manage.test/" + f.name, nil
}
func (f *fakeProvider) Webhook() http.Handler { return nil }
func (f *fakeProvider) StartCheckout(_ context.Context, c Checkout) (string, error) {
	f.checkouts++
	return "https://pay.test/" + f.name + "/" + c.TenantID, nil
}
func (f *fakeProvider) Resize(_ context.Context, _ string, o Order) (ResizeOutcome, error) {
	f.resized = &o
	return ResizeOutcome{Applied: &o}, nil
}

type flagSet map[string]bool // key → on, for every tenant

func (f flagSet) Enabled(key, _ string) bool { return f[key] }

func TestRegistry_For_FlagThenReadiness(t *testing.T) {
	// Arrange
	p := &fakeProvider{name: "fake", why: "prices not verified"}
	flags := flagSet{}
	r := NewRegistry(nil, flags, 0, nil)
	r.Register(p)
	ctx := context.Background()

	// Act + Assert: unknown, off, on-but-unready, on-and-ready.
	_, err := r.For(ctx, "t", "nope")
	assert.ErrorIs(t, err, ErrUnknownProvider)
	_, err = r.For(ctx, "t", "fake")
	assert.ErrorIs(t, err, ErrProviderDisabled)
	flags["pay_fake"] = true
	_, err = r.For(ctx, "t", "fake")
	assert.ErrorIs(t, err, ErrProviderUnavailable)
	assert.Empty(t, r.Enabled(ctx, "t"))
	p.state = Pending
	_, err = r.For(ctx, "t", "fake")
	assert.ErrorIs(t, err, ErrProviderUnavailable, "pending is not ready for new business")
	p.state = Ready
	got, err := r.For(ctx, "t", "fake")
	require.NoError(t, err)
	assert.Equal(t, p, got)
	assert.Equal(t, []string{"fake"}, r.Enabled(ctx, "t"))
	// A nil flag source: off for new business.
	_, err = NewRegistry(nil, nil, 0, nil).For(ctx, "t", "fake")
	assert.ErrorIs(t, err, ErrUnknownProvider)
}

func TestRegistry_ReadinessGaugeFromRegister(t *testing.T) {
	r := NewRegistry(nil, nil, 0, nil)
	r.Register(&fakeProvider{name: "gauge-unready"})
	r.Register(&fakeProvider{name: "gauge-ready", state: Ready})
	r.Register(&fakeProvider{name: "gauge-pending", state: Pending})

	assert.Equal(t, 0.0, testutil.ToFloat64(providerReady.WithLabelValues("gauge-unready")), "the series exists from Register")
	assert.Equal(t, 1.0, testutil.ToFloat64(providerReady.WithLabelValues("gauge-ready")))
	assert.Equal(t, 0.0, testutil.ToFloat64(providerReady.WithLabelValues("gauge-pending")), "pending exports 0")
}

func TestReadyLog_UnreadyWithin(t *testing.T) {
	// Arrange: a provider unready 2 h ago, ready since.
	now := time.Now()
	p := &fakeProvider{name: "hist", state: Ready}
	r := NewRegistry(nil, nil, 0, nil)
	r.Register(p)
	r.History().Record("hist", Unready, now.Add(-2*time.Hour))

	// Assert
	assert.True(t, r.UnreadyWithin(context.Background(), "hist", 24*time.Hour, now))
	assert.False(t, r.UnreadyWithin(context.Background(), "hist", time.Hour, now))
	assert.False(t, r.UnreadyWithin(context.Background(), "manual", 24*time.Hour, now), "an unregistered provider is never unready")
	p.state = Unready
	assert.True(t, r.UnreadyWithin(context.Background(), "hist", time.Minute, now), "unready now counts")
}

// RP22-1 (P2): a provider whose verification has not completed at boot is
// pending, not unready — the lapse job must not skip every Stripe row for
// 24 h after every deploy. Pending is never recorded; a provider pending
// NOW is not unready.
func TestReadiness_PendingAtBootIsNotUnready(t *testing.T) {
	// Arrange: pending at Register (boot), ready a minute later.
	boot := time.Now().Add(-10 * time.Hour)
	p := &fakeProvider{name: "boot", state: Pending, why: "house prices not verified yet"}
	r := NewRegistry(nil, nil, 0, nil)
	r.Register(p)
	r.RecordReadiness(context.Background(), boot)
	p.state = Ready
	r.RecordReadiness(context.Background(), boot.Add(time.Minute))

	// Act + Assert: ten hours later the job asks.
	assert.False(t, r.UnreadyWithin(context.Background(), "boot", 24*time.Hour, time.Now()),
		"a pending observation is not an outage")
	p.state = Pending
	assert.False(t, r.UnreadyWithin(context.Background(), "boot", 24*time.Hour, time.Now()),
		"pending NOW is not unready either")
	p.state = Unready
	assert.True(t, r.UnreadyWithin(context.Background(), "boot", 24*time.Hour, time.Now()))
	assert.Equal(t, "pending", Pending.String())
}

// RP22-4: the guard's transaction waits at most checkoutLockTimeout for the
// entitlement lock; a timeout is ErrProviderUnavailable, never a hang.
func TestCheckout_LockHeld_TimesOutAsUnavailable(t *testing.T) {
	// Arrange: another transaction holds the tenant's entitlement lock.
	f := newFixture(t)
	ctx := context.Background()
	holder, err := f.db.BeginTx(ctx, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = holder.Rollback() })
	require.NoError(t, Lock(ctx, holder, f.tenantID))
	prev := checkoutLockTimeout
	checkoutLockTimeout = 2 * time.Second
	t.Cleanup(func() { checkoutLockTimeout = prev })
	fake := &fakeProvider{name: "fake", state: Ready}
	r := NewRegistry(f.db, flagSet{"pay_fake": true}, 0, nil)
	r.Register(fake)

	// Act
	started := time.Now()
	_, err = r.StartCheckout(ctx, "fake", Checkout{TenantID: f.tenantID, Order: Order{Std: 1, Period: PeriodMonthly}})

	// Assert
	assert.ErrorIs(t, err, ErrProviderUnavailable, "got %v", err)
	assert.Less(t, time.Since(started), 10*time.Second)
	assert.Zero(t, fake.checkouts, "the provider was never asked")
	assert.Equal(t, 10*time.Second, prev, "the production bound")
}

// The double-billing guard (§ 2.5): a granted house renewed by Stripe
// (renewal auto) refuses another provider's checkout; a prepaid one or a
// lapsed one does not; the same provider is its own guard's business.
func TestCheckout_OtherProviderLive_409(t *testing.T) {
	// Arrange
	f := newFixture(t)
	ctx := context.Background()
	require.NoError(t, InTx(ctx, f.db, func(tx *sql.Tx) error {
		return Grant(ctx, tx, f.tenantID, Order{Std: 1, Period: PeriodMonthly}, time.Now().Add(20*24*time.Hour),
			ProviderStripe, "sub_x", RenewalAuto)
	}))
	fake := &fakeProvider{name: "fake", state: Ready}
	r := NewRegistry(f.db, flagSet{"pay_fake": true}, 0, nil)
	r.Register(fake)
	c := Checkout{Email: "x@test.local", Order: Order{Std: 1, Period: PeriodMonthly}, Purpose: PurposeSwitch}

	// Act
	_, err := StartCheckout(ctx, r, f.tenantID, "fake", c)

	// Assert
	assert.True(t, errors.Is(err, ErrOtherProviderLive), "got %v", err)
	assert.Zero(t, fake.checkouts, "refused before the provider is asked")

	// Renewal stopped at Stripe: the switch may start.
	require.NoError(t, InTx(ctx, f.db, func(tx *sql.Tx) error { return StopRenewal(ctx, tx, f.tenantID) }))
	url, err := r.StartCheckout(ctx, "fake", Checkout{TenantID: f.tenantID, Order: c.Order})
	require.NoError(t, err)
	assert.Equal(t, "https://pay.test/fake/"+f.tenantID, url)
}

func TestRegistry_Resize_RecordsTheAppliedHouse(t *testing.T) {
	// Arrange: a house billed by "fake".
	f := newFixture(t)
	ctx := context.Background()
	end := time.Now().Add(20 * 24 * time.Hour).Truncate(time.Second)
	require.NoError(t, InTx(ctx, f.db, func(tx *sql.Tx) error {
		return Grant(ctx, tx, f.tenantID, Order{Std: 1, Period: PeriodMonthly}, end, "manual", "", RenewalPrepaid)
	}))
	_, err := f.db.Exec(`UPDATE entitlements SET provider = 'manual' WHERE tenant_id = $1`, f.tenantID)
	require.NoError(t, err)
	m := &fakeProvider{name: "manual", state: Ready}
	flags := flagSet{}
	r := NewRegistry(f.db, flags, 0, nil)
	r.Register(m)

	// Act + Assert: off → 409 provider_disabled; on → applied with the date kept.
	_, err = r.Resize(ctx, f.tenantID, Order{Std: 3, Period: PeriodMonthly})
	assert.ErrorIs(t, err, ErrProviderDisabled)
	flags["pay_manual"] = true
	_, err = r.Resize(ctx, f.tenantID, Order{Std: 3, Period: PeriodMonthly})
	require.NoError(t, err)
	e, err := Get(ctx, f.db, f.tenantID)
	require.NoError(t, err)
	assert.Equal(t, 3, e.Order.Std)
	assert.True(t, end.Equal(e.PaidThrough))
}
