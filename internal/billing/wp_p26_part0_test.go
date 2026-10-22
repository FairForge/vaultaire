package billing

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/FairForge/vaultaire/internal/payments"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/stripe/stripe-go/v75"
)

// Stage 1a Prompt 26 Part 0 — the WP-P25 review's findings at the Stripe
// side of WP-PAY-5.

// settleBTC writes a settled bitcoin charge of the tenant for [start, end].
func (f *houseFixture) settleBTC(t *testing.T, id, purpose string, cents int64, start, end time.Time) payments.Settled {
	t.Helper()
	ctx := context.Background()
	order := payments.Order{Std: 2, Vault: 1, Period: payments.PeriodMonthly}
	var s payments.Settled
	require.NoError(t, payments.InTx(ctx, f.db, func(tx *sql.Tx) error {
		var err error
		s, err = payments.Settle(ctx, tx, payments.Payment{TenantID: f.tenantID, Provider: payments.ProviderBTCPay,
			ProviderID: id + f.tenantID, Kind: payments.KindCharge, Purpose: purpose, AmountCents: cents,
			Order: &order, PeriodStart: start, PeriodEnd: end, Unnumbered: true}, time.Now())
		return err
	}))
	return s
}

// RP31-2 (P3, proven): a resize top-up prices the rest of a period that
// was already bought — by design inside it — so it never matches.
func TestDoublePeriod_ResizeTopUpNeverMatches(t *testing.T) {
	// Arrange: Stripe's month is paid; then a bitcoin top-up settles for the
	// rest of it (the implicit switch's resize before the card stop landed).
	f := setupHouseFixture(t)
	f.paidInvoiceForPeriod(t, 1500, 30*24*time.Hour)
	before := doublePeriods()

	// Act
	s := f.settleBTC(t, "BtcTopUp_", payments.PurposeResize, 400, time.Now(), time.Now().Add(29*24*time.Hour))
	require.NoError(t, payments.ReportDoublePeriod(context.Background(), f.db, nil, f.tenantID, s))

	// Assert
	assert.Nil(t, s.DoublePeriod)
	assert.Nil(t, s.Forfeit)
	assert.Equal(t, before, doublePeriods())
	assert.Zero(t, f.audits(t, "billing.double_period"))
}

func TestDoublePeriod_StripeResizeOverBitcoinPeriod_NeverMatches(t *testing.T) {
	// The other side: a Stripe proration invoice (subscription_update)
	// inside a bitcoin period.
	f := setupHouseFixture(t)
	start := time.Now().Add(-time.Hour).Truncate(time.Second)
	f.settleBTC(t, "BtcMonth_", payments.PurposeNew, 1000, start, start.Add(30*24*time.Hour))
	before := doublePeriods()
	sub := f.houseSub("active", start, start.Add(30*24*time.Hour))
	f.post(t, "created", "customer.subscription.created", f.stripeHas(sub))
	inv := invoiceFor("in_resize_"+f.tenantID, "cus_"+f.tenantID, sub.ID)
	inv.AmountPaid, inv.Currency = 300, "usd"
	inv.BillingReason = stripe.InvoiceBillingReasonSubscriptionUpdate
	inv.Lines = &stripe.InvoiceLineItemList{Data: []*stripe.InvoiceLineItem{
		{Period: &stripe.Period{Start: time.Now().Unix(), End: start.Add(30 * 24 * time.Hour).Unix()}}}}

	f.post(t, "resize-paid", "invoice.payment_succeeded", inv)

	assert.Equal(t, before, doublePeriods())
	assert.Zero(t, f.audits(t, "billing.double_period"))
}

func TestSwitchToCard_Inside48h_NoDoublePeriod(t *testing.T) {
	// Arrange: the bitcoin month ends in 30 h; the customer switches to
	// card — inside MinDeferredStart, so Stripe bills from now and the page
	// said the remaining prepaid hours are forfeited.
	f := setupHouseFixture(t)
	btcEnd := time.Now().Add(30 * time.Hour).Truncate(time.Second)
	f.settleBTC(t, "BtcEnding_", payments.PurposeNew, 1000, btcEnd.Add(-30*24*time.Hour), btcEnd)
	notes := &operatorNotes{}
	f.handler.SetReceiptSender(notes)
	before := doublePeriods()

	// Act: Stripe's first charge (its period starts an hour ago).
	f.paidInvoiceForPeriod(t, 1500, 30*24*time.Hour)

	// Assert: no detector hit, nobody told to refund — a known forfeit,
	// audited with the days given up.
	assert.Equal(t, before, doublePeriods())
	assert.Zero(t, f.audits(t, "billing.double_period"))
	notes.mu.Lock()
	assert.Empty(t, notes.subjects)
	notes.mu.Unlock()
	require.Equal(t, 1, f.audits(t, "billing.prepaid_forfeited"))
	var days float64
	require.NoError(t, f.db.QueryRow(`SELECT (metadata->>'days')::float FROM audit_logs WHERE tenant_id = $1 AND action = 'billing.prepaid_forfeited'`,
		f.tenantID).Scan(&days))
	assert.InDelta(t, 31.0/24, days, 0.05, "30 h ahead + the hour Stripe's period started early")
}

func TestDoublePeriod_OverlapPast48h_StillDetected(t *testing.T) {
	// The forfeit window is MinDeferredStart, not a blanket pass: three
	// prepaid days overlapped is a double period.
	f := setupHouseFixture(t)
	btcEnd := time.Now().Add(72 * time.Hour).Truncate(time.Second)
	f.settleBTC(t, "BtcLong_", payments.PurposeNew, 1000, btcEnd.Add(-30*24*time.Hour), btcEnd)
	before := doublePeriods()

	f.paidInvoiceForPeriod(t, 1500, 30*24*time.Hour)

	assert.Equal(t, before+1, doublePeriods())
	assert.Zero(t, f.audits(t, "billing.prepaid_forfeited"))
}

// waitForLockWaiter polls pg_locks until a backend other than the test's
// waits on an advisory lock (the webhook queued behind the held
// entitlement lock).
func waitForLockWaiter(t *testing.T, db *sql.DB) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var n int
		require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM pg_locks WHERE locktype = 'advisory' AND NOT granted`).Scan(&n))
		if n > 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the webhook never waited on the entitlement lock")
}

// RP31-3 (P3): the switched-away check reads the renewal_stop row UNDER the
// entitlement lock — an implicit switch that commits while a Stripe event
// waits on the lock is seen, and the house is not taken back.
func TestStripeEvent_ImplicitSwitchCommitsWhileWaiting_NeverTakesTheHouse(t *testing.T) {
	// Arrange: Stripe renews the house; a bitcoin settle (the implicit
	// switch) holds the tenant's entitlement lock with its grant and its
	// renewal_stop row written, not committed yet.
	f := setupHouseFixture(t)
	ctx := context.Background()
	start := time.Now().Add(-24 * time.Hour).Truncate(time.Second)
	end := start.Add(30 * 24 * time.Hour)
	sub := f.houseSub("active", start, end)
	f.post(t, "granted", "customer.subscription.updated", f.stripeHas(sub))
	require.Equal(t, payments.ProviderStripe, f.entitlement(t).Provider)
	btcPT := end.Add(30 * 24 * time.Hour)
	tx, err := f.db.BeginTx(ctx, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback() })
	require.NoError(t, payments.Lock(ctx, tx, f.tenantID))
	require.NoError(t, payments.Grant(ctx, tx, f.tenantID, payments.Order{Std: 2, Vault: 1, Period: payments.PeriodMonthly}, btcPT,
		payments.ProviderBTCPay, "BtcImplicit_"+f.tenantID, payments.RenewalPrepaid))
	require.NoError(t, payments.RecordRenewalStop(ctx, tx, f.tenantID, payments.ProviderStripe, sub.ID, payments.ReasonImplicitSwitch,
		payments.RenewalStopPending, nil))
	stops := &fakeStops{}
	f.handler.stops = stops

	// Act: Stripe's update of the same subscription arrives and waits on
	// the lock; the switch commits.
	body := eventBody(t, "evt_"+f.tenantID+"_race", "customer.subscription.updated", f.stripeHas(sub))
	req := signedRequest(t, testWebhookSecret, time.Now(), body)
	done := make(chan int, 1)
	go func() {
		w := httptest.NewRecorder()
		f.handler.ServeHTTP(w, req)
		done <- w.Code
	}()
	waitForLockWaiter(t, f.db)
	require.NoError(t, tx.Commit())
	code := <-done

	// Assert: the house stays bitcoin's; the stop is asserted again.
	require.Equal(t, http.StatusOK, code)
	e := f.entitlement(t)
	assert.Equal(t, payments.ProviderBTCPay, e.Provider, "the Stripe grant must not take the house back")
	assert.True(t, btcPT.Equal(e.PaidThrough))
	stops.mu.Lock()
	assert.Equal(t, []string{sub.ID}, stops.refs)
	stops.mu.Unlock()
}
