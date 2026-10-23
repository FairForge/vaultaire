package billing

import (
	"context"
	"testing"
	"time"

	"github.com/FairForge/vaultaire/internal/payments"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// Stage 1a Prompt 30 Part 0a — RP37-3 (P3): choosing bitcoin during
// dunning never costs the customer the house sooner than Stripe's dunning
// would have. RP33-7's switch from past_due cancels the subscription now;
// Stripe then sends customer.subscription.deleted at once, and with
// entitlement_lapse OFF that event used to revoke a house whose
// paid_through (the past_due period's start) is already behind us.
// D-P30-2: the end of a subscription the customer's own switch stopped
// ends nothing — the house keeps its date and waits for the bitcoin
// invoice; the entitlement_lapse job ends it at paid_through + grace like
// any house nobody renews.

func TestSwitchToPrepaid_FromPastDue_PaidThroughInThePast_HouseKept(t *testing.T) {
	// Arrange: Stripe's house is in dunning — its period started an hour
	// ago and was never paid (paid_through stays at its start).
	f := setupHouseFixture(t)
	f.handler.SetFlags(lapseFlag{}) // entitlement_lapse OFF (the default)
	start, end := f.pastHouse(t)
	f.post(t, "past-due", "customer.subscription.updated", f.stripeHas(f.houseSub("past_due", start, end)))
	require.True(t, f.hasHouse(t))
	api := &fakeSubAPI{status: "past_due"}
	api.install(t)
	svc := NewStripeService("sk_test_fake", f.db, zap.NewNop())
	r := payments.NewRegistry(f.db, allOn{}, 0, nil)
	r.Register(svc)
	r.Register(prepaidProvider{})

	// Act: the customer chooses bitcoin — the subscription is cancelled now
	// and Stripe tells us so.
	_, err := r.SwitchToPrepaid(context.Background(), f.tenantID, payments.ProviderBTCPay, "customer")
	require.NoError(t, err)
	f.post(t, "deleted-after-switch", "customer.subscription.deleted", f.houseSub("canceled", start, end))

	// Assert: the house stands, waiting for the bitcoin invoice.
	assert.True(t, f.hasHouse(t), "choosing bitcoin during dunning must not cost the house at once")
	e := f.entitlement(t)
	assert.Nil(t, e.LapsedAt)
	assert.Equal(t, payments.RenewalPrepaid, e.Renewal)
	assert.Equal(t, payments.ProviderBTCPay, e.PendingProvider)
	assert.WithinDuration(t, end, e.PaidThrough, time.Second, "the date is not moved either")
}

func TestCancelNow_VoidsTheOpenPastDueInvoice(t *testing.T) {
	// Arrange: a past_due subscription whose open invoice Stripe's dunning
	// mails link to (payable by hand after a cancel — Stripe only turns its
	// automatic collection off).
	f := setupHouseFixture(t)
	ref := "sub_dunning_" + f.tenantID
	api := &fakeSubAPI{status: "past_due", open: []string{"in_open_" + f.tenantID}}
	api.install(t)
	svc := NewStripeService("sk_test_fake", f.db, zap.NewNop())

	// Act
	err := svc.StopRenewalRef(context.Background(), f.tenantID, ref)

	// Assert: cancelled now AND the open invoice voided — a card charge
	// after the customer chose bitcoin is the double charge § 4.4 forbids.
	assert.ErrorIs(t, err, payments.ErrRefGone)
	api.mu.Lock()
	defer api.mu.Unlock()
	require.Len(t, api.deletes, 1)
	assert.Equal(t, []string{"in_open_" + f.tenantID}, api.voided)
	require.NotEmpty(t, api.lists)
	assert.Equal(t, ref, api.lists[0].Get("subscription"))
	assert.Equal(t, "open", api.lists[0].Get("status"))
}

func TestCancelNow_VoidFailed_FinishedByTheNextPass(t *testing.T) {
	// Arrange: the cancel went through but the void did not (the process
	// died between them): Stripe now answers the subscription canceled.
	f := setupHouseFixture(t)
	ref := "sub_dunning_" + f.tenantID
	api := &fakeSubAPI{status: "canceled", open: []string{"in_left_" + f.tenantID}}
	api.install(t)
	svc := NewStripeService("sk_test_fake", f.db, zap.NewNop())

	// Act: the renewal_stops job asks again.
	err := svc.StopRenewalRef(context.Background(), f.tenantID, ref)

	// Assert: gone, and the open invoice it left is voided now.
	assert.ErrorIs(t, err, payments.ErrRefGone)
	api.mu.Lock()
	defer api.mu.Unlock()
	assert.Empty(t, api.deletes)
	assert.Equal(t, []string{"in_left_" + f.tenantID}, api.voided)
}
