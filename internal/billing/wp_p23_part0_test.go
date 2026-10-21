package billing

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/FairForge/vaultaire/internal/payments"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/stripe/stripe-go/v75"
)

// RP26-1 (Stage 1a Prompt 23 Part 0, decision D-P23-1): a terminal Stripe
// status never takes paid time away, entitlement_lapse ON or OFF. Since
// RP24-1 a Renew of a manually-paid house opens a TRIALING subscription with
// $0 moved; cancelling that trial in the portal used to reach the flag-OFF
// branch and Revoke the year the customer had paid by hand.

// manualYear grants a manual prepaid house paid through a date 300 days out
// and returns the date.
func (f *houseFixture) manualYear(t *testing.T) time.Time {
	t.Helper()
	paidThrough := time.Now().Add(300 * 24 * time.Hour).Truncate(time.Second).UTC()
	require.NoError(t, payments.InTx(context.Background(), f.db, func(tx *sql.Tx) error {
		return payments.Grant(context.Background(), tx, f.tenantID,
			payments.Order{Std: 2, Vault: 1, Period: payments.PeriodMonthly},
			paidThrough, payments.ProviderManual, "", payments.RenewalPrepaid)
	}))
	t.Cleanup(func() {
		_, _ = f.db.Exec(`DELETE FROM entitlements WHERE tenant_id = $1`, f.tenantID)
		_, _ = f.db.Exec(`DELETE FROM tenant_floor_quotas WHERE tenant_id = $1`, f.tenantID)
	})
	return paidThrough
}

// renewTrialing is the RP24-1 Renew: the checkout of a trialing
// subscription whose trial ends at the paid date.
func (f *houseFixture) renewTrialing(t *testing.T, paidThrough time.Time) {
	t.Helper()
	sub := f.houseSub("trialing", time.Now(), paidThrough)
	f.post(t, "renew", "checkout.session.completed", stripe.CheckoutSession{
		ID: "cs_renew", Customer: &stripe.Customer{ID: sub.Customer.ID}, Subscription: f.stripeHas(sub)})
	require.Equal(t, payments.ProviderStripe, f.entitlement(t).Provider)
}

func TestSubscriptionDeleted_FlagOff_FuturePaidThrough_KeepsHouse(t *testing.T) {
	// Arrange: a manually-paid year, renewed by card ($0 trialing), flag OFF.
	f := setupHouseFixture(t)
	f.handler.SetFlags(lapseFlag{})
	paidThrough := f.manualYear(t)
	f.renewTrialing(t, paidThrough)

	// Act: the customer cancels the trial in the portal.
	f.post(t, "deleted", "customer.subscription.deleted", f.houseSub("canceled", time.Now(), paidThrough))

	// Assert: the paid year stands — renewal stopped, nothing revoked.
	assert.True(t, f.hasHouse(t), "the floors stay: a status never takes paid time")
	e := f.entitlement(t)
	assert.Nil(t, e.LapsedAt, "not revoked")
	assert.Equal(t, payments.RenewalPrepaid, e.Renewal)
	assert.True(t, paidThrough.Equal(e.PaidThrough), "the date kept, got %s", e.PaidThrough)
	assert.Equal(t, 1, f.audits(t, "billing.renewal_stopped"))
}

func TestTerminalStatus_FlagOff_FuturePaidThrough_KeepsHouse(t *testing.T) {
	for _, status := range []string{"canceled", "unpaid", "incomplete_expired", "paused"} {
		t.Run(status, func(t *testing.T) {
			f := setupHouseFixture(t)
			f.handler.SetFlags(lapseFlag{})
			paidThrough := f.manualYear(t)
			f.renewTrialing(t, paidThrough)

			f.post(t, status, "customer.subscription.updated", f.stripeHas(f.houseSub(status, time.Now(), paidThrough)))

			assert.True(t, f.hasHouse(t))
			e := f.entitlement(t)
			assert.Nil(t, e.LapsedAt)
			assert.Equal(t, payments.RenewalPrepaid, e.Renewal)
			assert.True(t, paidThrough.Equal(e.PaidThrough))
		})
	}
}

// The boundary holds: a date already passed is revoked as before, by the
// updated status and by the deletion.
func TestTerminalStatus_FlagOff_PastDate_ClearsAsBefore(t *testing.T) {
	for _, tc := range []struct{ name, typ string }{
		{"updated", "customer.subscription.updated"},
		{"deleted", "customer.subscription.deleted"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := setupHouseFixture(t)
			f.handler.SetFlags(lapseFlag{})
			start, end := f.pastHouse(t)
			require.True(t, f.hasHouse(t))

			f.post(t, "end", tc.typ, f.stripeHas(f.houseSub("canceled", start, end)))

			assert.False(t, f.hasHouse(t), "paid_through <= now: revoked as before")
			e := f.entitlement(t)
			assert.NotNil(t, e.LapsedAt)
			assert.Equal(t, payments.RenewalPrepaid, e.Renewal)
		})
	}
}
