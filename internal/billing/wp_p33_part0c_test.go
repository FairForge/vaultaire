package billing

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/FairForge/vaultaire/internal/payments"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/stripe/stripe-go/v75"
	"go.uber.org/zap"
)

// Stage 1a Prompt 33 Part 0c — RP42-3 (P4): "the renewal is on again at
// Stripe" was read from LIVE cancel_at_period_end = false, which cannot tell
// a portal un-cancel from a switch's stop that never landed (a 5xx or no
// answer: SwitchToPrepaid keeps the switch, the row stays pending, and Stripe
// says false because nothing was sent). Any event for the subscription
// before the job's next pass then withdrew the customer's switch and its
// stop and invalidated the open bitcoin invoices. The evidence of a real
// un-cancel is the event's previous_attributes.cancel_at_period_end = true,
// or a stop Stripe confirmed (row stopped).

// postPrev posts an event whose data.previous_attributes is prev.
func (f *houseFixture) postPrev(t *testing.T, suffix, typ string, obj any, prev map[string]any) {
	t.Helper()
	raw, err := json.Marshal(obj)
	require.NoError(t, err)
	ev := stripe.Event{ID: "evt_" + f.tenantID + "_" + suffix, APIVersion: stripe.APIVersion, Type: stripe.EventType(typ),
		Created: time.Now().Unix(), Data: &stripe.EventData{Raw: raw, PreviousAttributes: prev}}
	body, err := json.Marshal(ev)
	require.NoError(t, err)
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, signedRequest(t, testWebhookSecret, time.Now(), body))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
}

// stripeDown answers 500 to every subscription call: the switch's stop
// never lands.
func stripeDown(t *testing.T) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"type":"api_error","message":"fake outage"}}`))
	}))
	t.Cleanup(srv.Close)
	prev := stripe.GetBackend(stripe.APIBackend)
	stripe.SetBackend(stripe.APIBackend, stripe.GetBackendWithConfig(stripe.APIBackend, &stripe.BackendConfig{
		URL: stripe.String(srv.URL), MaxNetworkRetries: stripe.Int64(0), LeveledLogger: &stripe.LeveledLogger{Level: stripe.LevelNull},
	}))
	t.Cleanup(func() { stripe.SetBackend(stripe.APIBackend, prev) })
}

// switchedWithStopPending is an auto Stripe house whose customer switched
// to bitcoin while Stripe answered 500: renewal prepaid, pending_provider
// btcpay, the stop row pending — and Stripe still says
// cancel_at_period_end = false, because nothing was sent.
func switchedWithStopPending(t *testing.T) (*houseFixture, *stripe.Subscription) {
	t.Helper()
	f := setupHouseFixture(t)
	start := time.Now().Add(-24 * time.Hour).Truncate(time.Second)
	sub := f.houseSub("active", start, start.Add(30*24*time.Hour))
	f.subs[sub.ID] = sub
	f.post(t, "granted", "customer.subscription.updated", sub)
	require.Equal(t, payments.RenewalAuto, f.entitlement(t).Renewal)
	stripeDown(t)
	r := payments.NewRegistry(f.db, allOn{}, 0, nil)
	r.Register(NewStripeService("sk_test_fake", f.db, zap.NewNop()))
	r.Register(prepaidProvider{})
	_, err := r.SwitchToPrepaid(context.Background(), f.tenantID, payments.ProviderBTCPay, "customer")
	require.NoError(t, err, "a 5xx keeps the switch")
	e := f.entitlement(t)
	require.Equal(t, payments.RenewalPrepaid, e.Renewal)
	require.Equal(t, payments.ProviderBTCPay, e.PendingProvider)
	st, found, err := payments.RenewalStopOf(context.Background(), f.db, f.tenantID, payments.ProviderStripe, sub.ID)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, payments.RenewalStopPending, st.Status)
	return f, sub
}

func TestUncancel_StopNeverLanded_SwitchKept(t *testing.T) {
	// Arrange
	f, sub := switchedWithStopPending(t)
	stops := &fakeStops{}
	f.handler.stops = stops

	// Act: a redelivered event of the same outage — live false, and no
	// previous_attributes.cancel_at_period_end (nothing changed it).
	f.postPrev(t, "redelivered", "customer.subscription.updated", f.stripeHas(sub), map[string]any{"metadata": map[string]any{}})

	// Assert: the switch, its pending stop and the prepaid renewal stand.
	e := f.entitlement(t)
	assert.Equal(t, payments.RenewalPrepaid, e.Renewal, "nothing shows the customer re-enabled the renewal")
	assert.Equal(t, payments.ProviderBTCPay, e.PendingProvider, "the switch is not withdrawn")
	st, found, err := payments.RenewalStopOf(context.Background(), f.db, f.tenantID, payments.ProviderStripe, sub.ID)
	require.NoError(t, err)
	require.True(t, found, "the stop is still owed — the renewal_stops job finishes it")
	assert.Equal(t, payments.RenewalStopPending, st.Status)
}

func TestUncancel_PreviousAttributesTrue_Withdraws(t *testing.T) {
	// Arrange
	f, sub := switchedWithStopPending(t)
	stops := &fakeStops{}
	f.handler.stops = stops

	// Act: the portal's un-cancel — Stripe says what changed.
	f.postPrev(t, "uncancel", "customer.subscription.updated", f.stripeHas(sub), map[string]any{"cancel_at_period_end": true})

	// Assert
	e := f.entitlement(t)
	assert.Equal(t, payments.RenewalAuto, e.Renewal)
	assert.Empty(t, e.PendingProvider, "the switch is withdrawn")
	_, found, err := payments.RenewalStopOf(context.Background(), f.db, f.tenantID, payments.ProviderStripe, sub.ID)
	require.NoError(t, err)
	assert.False(t, found, "the withdrawn switch owes no stop")
}

// A stop Stripe CONFIRMED (row stopped): a false now is the customer's hand,
// even when the event does not say what changed.
func TestUncancel_StopConfirmed_LiveFalseWithdraws(t *testing.T) {
	f, sub := switchedWithStopPending(t)
	require.NoError(t, payments.MarkRenewalStop(context.Background(), f.db, payments.ProviderStripe, sub.ID, payments.RenewalStopStopped, ""))
	f.handler.stops = &fakeStops{}

	f.postPrev(t, "after-stop", "customer.subscription.updated", f.stripeHas(sub), nil)

	e := f.entitlement(t)
	assert.Equal(t, payments.RenewalAuto, e.Renewal)
	assert.Empty(t, e.PendingProvider)
}

// An invoice.* grant never counts as an un-cancel, even a paid one and even
// after Stripe confirmed the stop: it says nothing about the renewal setting.
func TestUncancel_InvoiceEvent_NeverWithdraws(t *testing.T) {
	for _, confirmed := range []bool{false, true} {
		t.Run(map[bool]string{false: "stop pending", true: "stop confirmed"}[confirmed], func(t *testing.T) {
			f, sub := switchedWithStopPending(t)
			if confirmed {
				require.NoError(t, payments.MarkRenewalStop(context.Background(), f.db, payments.ProviderStripe, sub.ID, payments.RenewalStopStopped, ""))
			}
			f.handler.stops = &fakeStops{}
			invs := &fakeInvoices{}
			f.handler.SetInvoiceInvalidator(invs)
			inv := invoiceFor("in_"+f.tenantID, "cus_"+f.tenantID, sub.ID)
			inv.AmountPaid, inv.Currency = 1153, "usd"
			inv.BillingReason = stripe.InvoiceBillingReasonSubscriptionUpdate

			f.post(t, "paid", "invoice.payment_succeeded", inv)

			e := f.entitlement(t)
			assert.Equal(t, payments.RenewalPrepaid, e.Renewal)
			assert.Equal(t, payments.ProviderBTCPay, e.PendingProvider, "the switch is not withdrawn")
			_, found, err := payments.RenewalStopOf(context.Background(), f.db, f.tenantID, payments.ProviderStripe, sub.ID)
			require.NoError(t, err)
			assert.True(t, found)
			invs.mu.Lock()
			defer invs.mu.Unlock()
			assert.Empty(t, invs.tenants, "no open bitcoin invoice is invalidated")
		})
	}
}

// No switch in progress and no evidence: the live state is written as it is
// (renewal auto) with none of the un-cancel's withdrawals — no open prepaid
// invoice is invalidated on an ambiguous signal.
func TestUncancel_NoSwitchNoEvidence_NothingInvalidated(t *testing.T) {
	f := setupHouseFixture(t)
	start := time.Now().Add(-24 * time.Hour).Truncate(time.Second)
	sub := f.houseSub("active", start, start.Add(30*24*time.Hour))
	sub.CancelAtPeriodEnd = true
	f.subs[sub.ID] = sub
	f.post(t, "granted", "customer.subscription.updated", sub)
	require.Equal(t, payments.RenewalPrepaid, f.entitlement(t).Renewal)
	invs := &fakeInvoices{}
	f.handler.SetInvoiceInvalidator(invs)
	on := *sub
	on.CancelAtPeriodEnd = false

	f.postPrev(t, "live-false", "customer.subscription.updated", f.stripeHas(&on), nil)

	assert.Equal(t, payments.RenewalAuto, f.entitlement(t).Renewal, "Stripe's state is written")
	invs.mu.Lock()
	defer invs.mu.Unlock()
	assert.Empty(t, invs.tenants, "no un-cancel side effect without evidence")
}
