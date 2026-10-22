package billing

import (
	"context"
	"database/sql"
	"sync"
	"testing"
	"time"

	"github.com/FairForge/vaultaire/internal/payments"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/stripe/stripe-go/v75"
)

// WP-PAY-5 (Stage 1a Prompt 25) at the Stripe webhook: the portal race
// (docs/PAYMENTS.md § 4.1 step 5) and a subscription a switch took the
// house from (RP30-2: it never takes the house back).

// fakeInvoices records the open prepaid invoices the webhook was asked to
// invalidate.
type fakeInvoices struct {
	mu      sync.Mutex
	tenants []string
	reasons []string
}

func (f *fakeInvoices) InvalidateOpenInvoices(_ context.Context, tenantID, reason string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tenants = append(f.tenants, tenantID)
	f.reasons = append(f.reasons, reason)
	return 1, nil
}

// fakeStops records the subscriptions the webhook asked to stop again.
type fakeStops struct {
	mu   sync.Mutex
	refs []string
}

func (f *fakeStops) StopRenewalRef(_ context.Context, _, ref string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refs = append(f.refs, ref)
	return nil
}

func TestPortalUncancel_RestoresAuto_InvalidatesPendingInvoice(t *testing.T) {
	// Arrange: the customer asked to switch to crypto (renewal stopped,
	// pending_provider btcpay) and opened the switch invoice; then
	// re-enabled the renewal in Stripe's portal.
	f := setupHouseFixture(t)
	start := time.Now().Add(-24 * time.Hour).Truncate(time.Second)
	end := start.Add(30 * 24 * time.Hour)
	sub := f.houseSub("active", start, end)
	sub.CancelAtPeriodEnd = true
	f.subs[sub.ID] = sub
	f.post(t, "granted", "customer.subscription.updated", sub)
	_, err := f.db.Exec(`UPDATE entitlements SET pending_provider = 'btcpay' WHERE tenant_id = $1`, f.tenantID)
	require.NoError(t, err)
	require.Equal(t, payments.RenewalPrepaid, f.entitlement(t).Renewal)
	inv := &fakeInvoices{}
	f.handler.SetInvoiceInvalidator(inv)

	// Act: the portal's un-cancel.
	uncancelled := *sub
	uncancelled.CancelAtPeriodEnd = false
	f.post(t, "uncancel", "customer.subscription.updated", f.stripeHas(&uncancelled))

	// Assert: Stripe renews again, the switch is withdrawn, the open bitcoin
	// invoice is marked Invalid (reason portal_uncancel).
	e := f.entitlement(t)
	assert.Equal(t, payments.RenewalAuto, e.Renewal)
	assert.Equal(t, payments.ProviderStripe, e.Provider)
	var pending sql.NullString
	require.NoError(t, f.db.QueryRow(`SELECT pending_provider FROM entitlements WHERE tenant_id = $1`, f.tenantID).Scan(&pending))
	assert.False(t, pending.Valid, "the switch is withdrawn")
	inv.mu.Lock()
	defer inv.mu.Unlock()
	assert.Equal(t, []string{f.tenantID}, inv.tenants)
	assert.Equal(t, []string{payments.ReasonPortalUncancel}, inv.reasons)
}

func TestStripeEvent_SwitchedAwaySubscription_NeverTakesTheHouseBack(t *testing.T) {
	// Arrange: a BTCPay payment took the house from Stripe subscription X
	// (the renewal_stop row says it was stopped); X is un-cancelled in the
	// portal and its update arrives.
	f := setupHouseFixture(t)
	start := time.Now().Add(-24 * time.Hour).Truncate(time.Second)
	end := start.Add(30 * 24 * time.Hour)
	btcPT := end.Add(30 * 24 * time.Hour)
	sub := f.houseSub("active", start, end)
	f.subs[sub.ID] = sub
	ctx := context.Background()
	require.NoError(t, payments.InTx(ctx, f.db, func(tx *sql.Tx) error {
		if err := payments.Grant(ctx, tx, f.tenantID, payments.Order{Std: 2, Vault: 1, Period: payments.PeriodMonthly}, btcPT,
			payments.ProviderBTCPay, "BtcInv_"+f.tenantID, payments.RenewalPrepaid); err != nil {
			return err
		}
		return payments.RecordRenewalStop(ctx, tx, f.tenantID, payments.ProviderStripe, sub.ID, payments.ReasonSwitch,
			payments.RenewalStopStopped, nil)
	}))
	stops := &fakeStops{}
	f.handler.stops = stops

	// Act
	f.post(t, "uncancel-away", "customer.subscription.updated", f.stripeHas(sub))

	// Assert: the house stays BTCPay's; the stop is asserted again.
	e := f.entitlement(t)
	assert.Equal(t, payments.ProviderBTCPay, e.Provider)
	assert.Equal(t, payments.RenewalPrepaid, e.Renewal)
	assert.True(t, btcPT.Equal(e.PaidThrough))
	stops.mu.Lock()
	assert.Equal(t, []string{sub.ID}, stops.refs)
	stops.mu.Unlock()
	st, found, err := payments.RenewalStopOf(ctx, f.db, f.tenantID, payments.ProviderStripe, sub.ID)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, payments.RenewalStopStopped, st.Status)
	assert.Equal(t, 1, f.audits(t, "billing.renewal_stop_reasserted"))

	// A cancelled-at-period-end update of it is nothing at all.
	stopped := *sub
	stopped.CancelAtPeriodEnd = true
	f.post(t, "stopped-away", "customer.subscription.updated", f.stripeHas(&stopped))
	assert.Equal(t, payments.ProviderBTCPay, f.entitlement(t).Provider)
	assert.Equal(t, 1, f.audits(t, "billing.renewal_stop_reasserted"))
}

var _ = stripe.SubscriptionStatusActive

// operatorNotes is a ReceiptSender that also takes operator notes.
type operatorNotes struct {
	mu       sync.Mutex
	subjects []string
	bodies   []string
}

func (o *operatorNotes) SendReceipt(context.Context, string, string) error { return nil }
func (o *operatorNotes) NotifyOperator(_ context.Context, subject, body string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.subjects = append(o.subjects, subject)
	o.bodies = append(o.bodies, body)
	return nil
}

// doublePeriods reads vaultaire_payments_double_period_total (-1 when the
// series is not exported).
func doublePeriods() float64 {
	reg := prometheus.NewRegistry()
	reg.MustRegister(payments.Collectors()...)
	mfs, err := reg.Gather()
	if err != nil {
		return -1
	}
	for _, mf := range mfs {
		if mf.GetName() == "vaultaire_payments_double_period_total" {
			var v float64
			for _, m := range mf.GetMetric() {
				v += m.GetCounter().GetValue()
			}
			return v
		}
	}
	return -1
}

func TestDoublePeriod_Detected_Audited_StillGranted(t *testing.T) {
	// Arrange: a BTCPay payment settled for the coming month; then Stripe
	// charges a period that overlaps it (the portal race — renewal
	// re-enabled after the bitcoin invoice settled).
	f := setupHouseFixture(t)
	ctx := context.Background()
	start := time.Now().Add(-time.Hour).Truncate(time.Second)
	order := payments.Order{Std: 2, Vault: 1, Period: payments.PeriodMonthly}
	require.NoError(t, payments.InTx(ctx, f.db, func(tx *sql.Tx) error {
		_, err := payments.Settle(ctx, tx, payments.Payment{TenantID: f.tenantID, Provider: payments.ProviderBTCPay,
			ProviderID: "BtcDouble_" + f.tenantID, Kind: payments.KindCharge, Purpose: payments.PurposeSwitch, AmountCents: 1000,
			Order: &order, PeriodStart: start, PeriodEnd: start.Add(30 * 24 * time.Hour), Unnumbered: true}, time.Now())
		return err
	}))
	notes := &operatorNotes{}
	f.handler.SetReceiptSender(notes)
	before := doublePeriods()

	// Act
	inv := f.paidInvoiceForPeriod(t, 1500, 30*24*time.Hour)

	// Assert: counted, audited with both rows, the operator told — and
	// the payment still recorded and granted (paid is paid; the refund is
	// by hand).
	assert.Equal(t, before+1, doublePeriods())
	assert.Equal(t, 1, f.audits(t, "billing.double_period"))
	var meta string
	require.NoError(t, f.db.QueryRow(`SELECT metadata::text FROM audit_logs WHERE tenant_id = $1 AND action = 'billing.double_period'`,
		f.tenantID).Scan(&meta))
	assert.Contains(t, meta, inv.ID)
	assert.Contains(t, meta, "BtcDouble_"+f.tenantID)
	notes.mu.Lock()
	require.Len(t, notes.subjects, 1)
	assert.Contains(t, notes.bodies[0], f.tenantID)
	notes.mu.Unlock()
	status, _, _, _, _ := f.paymentRow(t, inv.ID)
	assert.Equal(t, payments.StatusSettled, status)
	assert.Equal(t, payments.ProviderStripe, f.entitlement(t).Provider)

	// A replay of the same invoice is not a second detection.
	f.post(t, "paid-again", "invoice.payment_succeeded", inv)
	assert.Equal(t, before+1, doublePeriods())
}

// WP-PAY-5 item 4: the card checkout of a switch from bitcoin trials until
// the bitcoin period's end; its completion flips the house to Stripe/auto
// and keeps the date — and its $0 first invoice is no double period.
func TestSwitchToCard_CompletedTrialing_FromBTCPay_KeepsPaidThrough(t *testing.T) {
	f := setupHouseFixture(t)
	ctx := context.Background()
	pt := time.Now().Add(40 * 24 * time.Hour).Truncate(time.Second).UTC()
	start := time.Now().Add(-20 * 24 * time.Hour).Truncate(time.Second).UTC()
	order := payments.Order{Std: 2, Vault: 1, Period: payments.PeriodMonthly}
	require.NoError(t, payments.InTx(ctx, f.db, func(tx *sql.Tx) error {
		if _, err := payments.Settle(ctx, tx, payments.Payment{TenantID: f.tenantID, Provider: payments.ProviderBTCPay,
			ProviderID: "BtcPaid_" + f.tenantID, Kind: payments.KindCharge, Purpose: payments.PurposeNew, AmountCents: 6000,
			Order: &order, PeriodStart: start, PeriodEnd: pt, Unnumbered: true}, time.Now()); err != nil {
			return err
		}
		return payments.Grant(ctx, tx, f.tenantID, order, pt, payments.ProviderBTCPay, "BtcPaid_"+f.tenantID, payments.RenewalPrepaid)
	}))
	params := f.handler.stripe.houseCheckoutParams("cus_"+f.tenantID, f.tenantID, order, payments.PurposeSwitch, pt, "https://x/ok", "https://x/no")
	require.NotNil(t, params.SubscriptionData.TrialEnd)
	assert.Equal(t, pt.Unix(), *params.SubscriptionData.TrialEnd, "trial_end = the bitcoin period's end")
	assert.Equal(t, payments.PurposeSwitch, params.SubscriptionData.Metadata["purpose"])
	sub := f.houseSub("trialing", time.Now(), pt)
	before := doublePeriods()

	// Act: the checkout completes; Stripe's $0 first invoice is paid.
	f.post(t, "switch-card", "checkout.session.completed", stripe.CheckoutSession{
		ID: "cs_switch", Customer: &stripe.Customer{ID: sub.Customer.ID}, Subscription: f.stripeHas(sub)})
	inv := invoiceFor("in_trial_"+f.tenantID, "cus_"+f.tenantID, sub.ID)
	inv.AmountPaid, inv.Currency = 0, "usd"
	inv.BillingReason = stripe.InvoiceBillingReasonSubscriptionCreate
	inv.Lines = &stripe.InvoiceLineItemList{Data: []*stripe.InvoiceLineItem{{Period: &stripe.Period{Start: time.Now().Unix(), End: pt.Unix()}}}}
	f.post(t, "switch-card-inv", "invoice.payment_succeeded", inv)

	// Assert
	e := f.entitlement(t)
	assert.True(t, pt.Equal(e.PaidThrough), "paid_through kept, got %s", e.PaidThrough)
	assert.Equal(t, payments.ProviderStripe, e.Provider)
	assert.Equal(t, payments.RenewalAuto, e.Renewal)
	assert.Equal(t, before, doublePeriods(), "a $0 trial invoice paid nothing twice")
}
