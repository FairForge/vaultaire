package billing

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/FairForge/vaultaire/internal/payments"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/stripe/stripe-go/v75"
)

// RP20-4: a subscription edited at Stripe into an invalid house (pin-hot
// with no Standard under it) is answered as handled — 200, recorded, never a
// retry — and leaves the house and the entitlement as they were.

func TestStripeWebhook_InvalidHouseIsHandledNotRetried(t *testing.T) {
	// Arrange: Stripe holds vault_m 1 + pin_m 1 and no Standard item.
	f := setupHouseFixture(t)
	start := time.Now().Add(-time.Hour).Truncate(time.Second)
	sub := subWith("sub_"+f.tenantID, "active", item("si_v", "price_vault_m", 1), item("si_p", "price_pin_m", 1))
	sub.Customer.ID = "cus_" + f.tenantID
	sub.CurrentPeriodStart, sub.CurrentPeriodEnd = start.Unix(), start.Add(30*24*time.Hour).Unix()
	f.subs[sub.ID] = sub
	before := testutil.ToFloat64(houseInvalidTotal)
	body := eventBody(t, "evt_"+f.tenantID+"_invalid", "customer.subscription.created", sub)

	// Act: two deliveries of the same event.
	var codes []int
	for range 2 {
		w := httptest.NewRecorder()
		f.handler.ServeHTTP(w, signedRequest(t, testWebhookSecret, time.Now(), body))
		codes = append(codes, w.Code)
	}

	// Assert: handled on the first delivery, the second is the duplicate.
	assert.Equal(t, []int{http.StatusOK, http.StatusOK}, codes)
	var claimed int
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM stripe_events WHERE event_id = $1`,
		"evt_"+f.tenantID+"_invalid").Scan(&claimed))
	assert.Equal(t, 1, claimed, "the event is recorded")
	_, err := payments.Get(context.Background(), f.db, f.tenantID)
	assert.True(t, errors.Is(err, payments.ErrNoEntitlement), "no entitlement row, got %v", err)
	_, hasHouse, err := f.qm.GetHouse(context.Background(), f.tenantID)
	require.NoError(t, err)
	assert.False(t, hasHouse, "the floors are untouched")
	assert.Equal(t, before+1, testutil.ToFloat64(houseInvalidTotal), "counted once")
	var audited int
	require.NoError(t, f.db.QueryRow(
		`SELECT COUNT(*) FROM audit_logs WHERE tenant_id = $1 AND action = 'billing.house_invalid'`, f.tenantID).Scan(&audited))
	assert.Equal(t, 1, audited)
}

// A paid invoice whose subscription is an invalid house still settles its
// ledger row; the house is not touched.
func TestStripeWebhook_PaidInvoiceOfInvalidHouse_SettlesLedgerOnly(t *testing.T) {
	// Arrange: a granted house, then Stripe's subscription loses its Standard item.
	f := setupHouseFixture(t)
	start := time.Now().Add(-29 * 24 * time.Hour).Truncate(time.Second)
	end := start.Add(30 * 24 * time.Hour)
	f.post(t, "created", "customer.subscription.created", f.stripeHas(f.houseSub("active", start, end)))
	bad := subWith("sub_"+f.tenantID, "active", item("si_v", "price_vault_m", 1), item("si_p", "price_pin_m", 1))
	bad.Customer.ID = "cus_" + f.tenantID
	bad.CurrentPeriodStart, bad.CurrentPeriodEnd = end.Unix(), end.Add(31*24*time.Hour).Unix()
	f.subs[bad.ID] = bad
	inv := invoiceFor("in_bad_"+f.tenantID, "cus_"+f.tenantID, bad.ID)
	inv.AmountPaid = 500
	inv.BillingReason = stripe.InvoiceBillingReasonSubscriptionCycle

	// Act
	f.post(t, "paid", "invoice.payment_succeeded", inv)

	// Assert: settled; the entitlement as the first grant left it.
	status, number, _, _, _ := f.paymentRow(t, inv.ID)
	assert.Equal(t, payments.StatusSettled, status)
	assert.NotEmpty(t, number)
	e := f.entitlement(t)
	assert.Equal(t, payments.Order{Std: 2, Vault: 1, Period: payments.PeriodMonthly}, e.Order)
	assert.True(t, end.Equal(e.PaidThrough), "the date did not move, got %s", e.PaidThrough)
}

// RP22-3: the subscription acknowledged as an invalid house is recorded
// (outside the rolled-back grant) so the deletion runner finds it and
// cancels it — a payment_events row since WP-PAY-7, one per subscription.
func TestInvalidHouse_SubscriptionRecordedForRunner(t *testing.T) {
	// Arrange
	f := setupHouseFixture(t)
	sub := subWith("sub_"+f.tenantID, "active", item("si_v", "price_vault_m", 1), item("si_p", "price_pin_m", 1))
	sub.Customer.ID = "cus_" + f.tenantID
	sub.CurrentPeriodStart, sub.CurrentPeriodEnd = time.Now().Add(-time.Hour).Unix(), time.Now().Add(29*24*time.Hour).Unix()
	f.subs[sub.ID] = sub

	// Act: three deliveries.
	for i := range 3 {
		assert.Equal(t, http.StatusOK, f.postCode(t, fmt.Sprintf("invalid-%d", i), "customer.subscription.updated", sub))
	}

	// Assert: no entitlement, the subscription recorded once for the runner.
	_, err := payments.Get(context.Background(), f.db, f.tenantID)
	assert.ErrorIs(t, err, payments.ErrNoEntitlement)
	refs, err := payments.InvalidHouseRefs(context.Background(), f.db, f.tenantID)
	require.NoError(t, err)
	assert.Equal(t, []string{sub.ID}, refs, "recorded for the deletion runner, once")
}
