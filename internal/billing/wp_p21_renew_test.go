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
	"go.uber.org/zap"
)

// RP24-1 (Stage 1a Prompt 21 Part 0): a renew by card on a date that still
// has paid time is a switch, not a top-up — the Stripe checkout defers the
// first charge to paid_through (subscription_data.trial_end), the trialing
// subscription keeps the date and flips the provider, and the trial's $0
// subscription_create invoice spends no receipt number and mails no receipt.

func TestRenew_GrantedPrepaidRow_StripeTrialEndIsPaidThrough(t *testing.T) {
	// Arrange: a checkout whose first charge may land on a date 10 days out.
	svc := NewStripeService("sk_test_fake", nil, zap.NewNop())
	svc.ConfigureHouse(testIDs, goodPrices())
	startAt := time.Now().Add(10 * 24 * time.Hour).Truncate(time.Second)

	// Act.
	params := svc.houseCheckoutParams("cus_1", "tenant-1", HouseOrder{Std: 1, Period: PeriodAnnual},
		payments.PurposeRenew, startAt, "https://x/ok", "https://x/no")

	// Assert: the subscription trials until the paid date (§ 4.2).
	require.NotNil(t, params.SubscriptionData.TrialEnd)
	assert.Equal(t, startAt.UTC().Unix(), *params.SubscriptionData.TrialEnd)
	assert.Equal(t, "renew", params.SubscriptionData.Metadata["purpose"])
}

func TestRenew_Inside48h_StartsNow(t *testing.T) {
	svc := NewStripeService("sk_test_fake", nil, zap.NewNop())
	svc.ConfigureHouse(testIDs, goodPrices())

	// Inside Stripe's 48 h trial_end minimum: no trial, the checkout
	// starts now (the page says the remaining days are forfeited).
	for _, startAt := range []time.Time{
		time.Now().Add(40 * time.Hour),
		time.Now().Add(-time.Hour), // in grace: the date has passed
		{},                         // a lapsed row carries no StartAt
	} {
		params := svc.houseCheckoutParams("cus_1", "tenant-1", HouseOrder{Std: 1, Period: PeriodAnnual},
			payments.PurposeRenew, startAt, "https://x/ok", "https://x/no")
		assert.Nil(t, params.SubscriptionData.TrialEnd, "startAt %s", startAt)
	}
}

// The trialing subscription a deferred renew opens: current_period_end is
// its trial end = paid_through, so the grant keeps the date (GREATEST) and
// flips the row to provider=stripe, renewal=auto (§ 4.2).
func TestRenew_TrialingSubscription_KeepsPaidThroughFlipsProvider(t *testing.T) {
	// Arrange: a manual prepaid house paid through a date 10 days out.
	f := setupHouseFixture(t)
	paidThrough := time.Now().Add(10 * 24 * time.Hour).Truncate(time.Second).UTC()
	require.NoError(t, payments.InTx(context.Background(), f.db, func(tx *sql.Tx) error {
		return payments.Grant(context.Background(), tx, f.tenantID,
			payments.Order{Std: 2, Vault: 1, Period: payments.PeriodMonthly},
			paidThrough, payments.ProviderManual, "", payments.RenewalPrepaid)
	}))
	t.Cleanup(func() {
		_, _ = f.db.Exec(`DELETE FROM entitlements WHERE tenant_id = $1`, f.tenantID)
		_, _ = f.db.Exec(`DELETE FROM tenant_floor_quotas WHERE tenant_id = $1`, f.tenantID)
	})
	sub := f.houseSub("trialing", time.Now(), paidThrough)

	// Act: the checkout completes.
	f.post(t, "renew", "checkout.session.completed", stripe.CheckoutSession{
		ID: "cs_renew", Customer: &stripe.Customer{ID: sub.Customer.ID}, Subscription: f.stripeHas(sub)})

	// Assert: the date unchanged, the provider flipped.
	e := f.entitlement(t)
	assert.True(t, paidThrough.Equal(e.PaidThrough), "paid_through kept, got %s", e.PaidThrough)
	assert.Equal(t, payments.ProviderStripe, e.Provider)
	assert.Equal(t, payments.RenewalAuto, e.Renewal)
	assert.Nil(t, e.LapsedAt)
}

// The $0 subscription_create invoice of a trialing subscription is settled
// for the record with NO receipt number (the gapless counter spends
// nothing on it) and mails no receipt.
func TestRecordInvoicePaid_ZeroAmount_NoReceiptNumberNoMail(t *testing.T) {
	// Arrange: a granted house and a counting receipt sender.
	f := setupHouseFixture(t)
	rc := &countingReceipts{}
	f.handler.SetReceiptSender(rc)
	f.grantedHouse(t)
	inv := invoiceFor("in_zero_"+f.tenantID, "cus_"+f.tenantID, "sub_"+f.tenantID)
	inv.AmountPaid = 0
	inv.BillingReason = stripe.InvoiceBillingReasonSubscriptionCreate

	// Act.
	f.post(t, "zero", "invoice.payment_succeeded", inv)

	// Assert: settled, unnumbered, unmailed.
	status, number, amount, _, _ := f.paymentRow(t, inv.ID)
	assert.Equal(t, payments.StatusSettled, status)
	assert.Empty(t, number, "a $0 trial invoice takes no gapless receipt number")
	assert.Zero(t, amount)
	assert.Equal(t, int32(0), rc.calls.Load(), "no receipt mail for $0")

	// A later paid invoice is numbered and mailed as before.
	paid := invoiceFor("in_paid_"+f.tenantID, "cus_"+f.tenantID, "sub_"+f.tenantID)
	paid.AmountPaid = 998
	paid.BillingReason = stripe.InvoiceBillingReasonSubscriptionCycle
	f.post(t, "paid", "invoice.payment_succeeded", paid)
	_, number, _, _, _ = f.paymentRow(t, paid.ID)
	assert.NotEmpty(t, number)
	assert.Equal(t, int32(1), rc.calls.Load())
}
