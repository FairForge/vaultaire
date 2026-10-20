package billing

import (
	"context"
	"database/sql"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/FairForge/vaultaire/internal/payments"
	"github.com/FairForge/vaultaire/internal/usage"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/stripe/stripe-go/v75"
)

// WP-PAY-1: the Stripe webhook dual-writes the entitlement record (paid_through
// in our database, the payments ledger) beside the tenants columns it already
// writes. docs/PAYMENTS.md § 1.4.

func (f *houseFixture) entitlement(t *testing.T) payments.Entitlement {
	t.Helper()
	e, err := payments.Get(context.Background(), f.db, f.tenantID)
	require.NoError(t, err)
	return e
}

func (f *houseFixture) paymentRow(t *testing.T, providerID string) (status, number string, amount int64, start, end time.Time) {
	t.Helper()
	var num sql.NullString
	var ps, pe sql.NullTime
	require.NoError(t, f.db.QueryRow(
		`SELECT status, invoice_number, amount_cents, period_start, period_end FROM payments
		  WHERE provider = 'stripe' AND provider_id = $1 AND tenant_id = $2`, providerID, f.tenantID).
		Scan(&status, &num, &amount, &ps, &pe))
	return status, num.String, amount, ps.Time, pe.Time
}

// houseSub is an active monthly house subscription of the fixture's customer
// whose current period ends at end.
func (f *houseFixture) houseSub(status string, start, end time.Time) *stripe.Subscription {
	sub := subWith("sub_"+f.tenantID, status, item("si_1", "price_std_m", 2), item("si_2", "price_vault_m", 1))
	sub.Customer.ID = "cus_" + f.tenantID
	sub.CurrentPeriodStart, sub.CurrentPeriodEnd = start.Unix(), end.Unix()
	return sub
}

func TestStripeWebhook_WritesEntitlement_PaidThroughIsCurrentPeriodEnd(t *testing.T) {
	// Arrange: Stripe holds an active house whose period ends in 30 days.
	f := setupHouseFixture(t)
	start := time.Now().Add(-time.Hour).Truncate(time.Second)
	end := start.Add(30 * 24 * time.Hour)
	sub := f.houseSub("active", start, end)
	f.subs[sub.ID] = sub

	// Act: checkout completes.
	f.post(t, "checkout", "checkout.session.completed", stripe.CheckoutSession{
		ID: "cs_e", Customer: &stripe.Customer{ID: sub.Customer.ID}, Subscription: &stripe.Subscription{ID: sub.ID}})

	// Assert: the entitlement names the house, the date, the provider.
	e := f.entitlement(t)
	assert.Equal(t, payments.Order{Std: 2, Vault: 1, Period: payments.PeriodMonthly}, e.Order)
	assert.True(t, end.Equal(e.PaidThrough), "paid_through = current_period_end, got %s", e.PaidThrough)
	assert.Equal(t, payments.ProviderStripe, e.Provider)
	assert.Equal(t, sub.ID, e.ProviderRef)
	assert.Equal(t, payments.RenewalAuto, e.Renewal)
	assert.Nil(t, e.LapsedAt)
	house, ok, err := f.qm.GetHouse(context.Background(), f.tenantID)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, usage.HouseFromTB(2, 1, 0), house)

	// A renewal Stripe reports through customer.subscription.updated moves it.
	next := end.Add(31 * 24 * time.Hour)
	f.post(t, "updated", "customer.subscription.updated", f.stripeHas(f.houseSub("active", end, next)))
	assert.True(t, next.Equal(f.entitlement(t).PaidThrough))
}

// past_due: Stripe has moved the period on but nothing was paid for it — the
// date stays where the last payment left it (our grace runs; the floors stay).
func TestStripeWebhook_PastDue_KeepsTheDate(t *testing.T) {
	f := setupHouseFixture(t)
	start := time.Now().Add(-40 * 24 * time.Hour).Truncate(time.Second)
	end := start.Add(30 * 24 * time.Hour)
	f.post(t, "created", "customer.subscription.created", f.stripeHas(f.houseSub("active", start, end)))

	// The renewal fails: Stripe's period is now end → end+30d, status past_due.
	f.post(t, "past-due", "customer.subscription.updated", f.stripeHas(f.houseSub("past_due", end, end.Add(30*24*time.Hour))))

	e := f.entitlement(t)
	assert.True(t, end.Equal(e.PaidThrough), "paid through the last paid period only, got %s", e.PaidThrough)
	assert.Nil(t, e.LapsedAt)
	_, ok, err := f.qm.GetHouse(context.Background(), f.tenantID)
	require.NoError(t, err)
	assert.True(t, ok, "the house is kept while past_due")
}

func TestStripeWebhook_PaymentSucceeded_LedgerRowAndExtend(t *testing.T) {
	// Arrange: a house paid through `end`; the renewal invoice for the next
	// period is paid and Stripe's subscription now ends at `next`.
	f := setupHouseFixture(t)
	start := time.Now().Add(-29 * 24 * time.Hour).Truncate(time.Second)
	end := start.Add(30 * 24 * time.Hour)
	f.post(t, "created", "customer.subscription.created", f.stripeHas(f.houseSub("active", start, end)))
	next := end.Add(31 * 24 * time.Hour)
	f.stripeHas(f.houseSub("active", end, next))

	inv := invoiceFor("in_"+f.tenantID, "cus_"+f.tenantID, "sub_"+f.tenantID)
	inv.AmountPaid = 1153
	inv.Currency = "usd"
	inv.BillingReason = stripe.InvoiceBillingReasonSubscriptionCycle
	inv.Lines = &stripe.InvoiceLineItemList{Data: []*stripe.InvoiceLineItem{
		{Period: &stripe.Period{Start: end.Unix(), End: next.Unix()}},
	}}

	// Act
	f.post(t, "paid", "invoice.payment_succeeded", inv)

	// Assert: one settled ledger row with our number, and the date moved.
	status, number, amount, ps, pe := f.paymentRow(t, inv.ID)
	assert.Equal(t, payments.StatusSettled, status)
	assert.Regexp(t, `^ST-\d{4}-\d{6}$`, number)
	assert.Equal(t, int64(1153), amount)
	assert.True(t, end.Equal(ps))
	assert.True(t, next.Equal(pe))
	assert.True(t, next.Equal(f.entitlement(t).PaidThrough))

	// A redelivery under a new event id is a replay: no second row, same number.
	body := eventBody(t, "evt_"+f.tenantID+"_paid_again", "invoice.payment_succeeded", inv)
	f.handler.ServeHTTP(httptest.NewRecorder(), signedRequest(t, testWebhookSecret, time.Now(), body))
	var n int
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM payments WHERE tenant_id = $1`, f.tenantID).Scan(&n))
	assert.Equal(t, 1, n)
	_, again, _, _, _ := f.paymentRow(t, inv.ID)
	assert.Equal(t, number, again)
}

func TestStripeWebhook_PaymentFailed_LedgerRowFailed_NoEntitlementChange(t *testing.T) {
	f := setupHouseFixture(t)
	start := time.Now().Add(-29 * 24 * time.Hour).Truncate(time.Second)
	end := start.Add(30 * 24 * time.Hour)
	f.post(t, "created", "customer.subscription.created", f.stripeHas(f.houseSub("active", start, end)))

	inv := invoiceFor("in_f_"+f.tenantID, "cus_"+f.tenantID, "sub_"+f.tenantID)
	inv.AmountDue = 1153
	f.post(t, "failed", "invoice.payment_failed", inv)

	status, number, _, _, _ := f.paymentRow(t, inv.ID)
	assert.Equal(t, payments.StatusFailed, status)
	assert.Empty(t, number, "a failed charge gets no receipt number")
	assert.True(t, end.Equal(f.entitlement(t).PaidThrough))
}

func TestStripeWebhook_Canceled_StillClearsWhileFlagOff_SetsPrepaid(t *testing.T) {
	// Arrange: a granted house.
	f := setupHouseFixture(t)
	start := time.Now().Add(-time.Hour).Truncate(time.Second)
	end := start.Add(30 * 24 * time.Hour)
	f.post(t, "created", "customer.subscription.created", f.stripeHas(f.houseSub("active", start, end)))

	// Act: Stripe cancels it.
	f.post(t, "canceled", "customer.subscription.updated", f.stripeHas(f.houseSub("canceled", start, end)))

	// Assert: cleared as today (the branch moves behind the flag in WP-PAY-2),
	// renewal stopped, the row marked lapsed so the reconcile never grants it
	// back, the date kept.
	_, ok, err := f.qm.GetHouse(context.Background(), f.tenantID)
	require.NoError(t, err)
	assert.False(t, ok, "the floors are cleared while entitlement_lapse is off")
	e := f.entitlement(t)
	assert.Equal(t, payments.RenewalPrepaid, e.Renewal)
	assert.NotNil(t, e.LapsedAt)
	assert.True(t, end.Equal(e.PaidThrough))
}

func TestStripeWebhook_SubscriptionDeleted_ClearsAndSetsPrepaid(t *testing.T) {
	f := setupHouseFixture(t)
	start := time.Now().Add(-time.Hour).Truncate(time.Second)
	end := start.Add(30 * 24 * time.Hour)
	sub := f.houseSub("active", start, end)
	f.post(t, "created", "customer.subscription.created", f.stripeHas(sub))

	deleted := f.houseSub("canceled", start, end)
	f.post(t, "deleted", "customer.subscription.deleted", deleted)

	_, ok, err := f.qm.GetHouse(context.Background(), f.tenantID)
	require.NoError(t, err)
	assert.False(t, ok)
	e := f.entitlement(t)
	assert.Equal(t, payments.RenewalPrepaid, e.Renewal)
	assert.NotNil(t, e.LapsedAt)
}
