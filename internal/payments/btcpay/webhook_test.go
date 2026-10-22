package btcpay

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/FairForge/vaultaire/internal/payments"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The webhook and the § 3.3 state table, driven by the lab's captures:
// every delivery is posted exactly as BTCPay sent it (its raw body and its
// BTCPay-Sig under the lab store's secret), and the fake Greenfield answers
// the invoice and payment methods BTCPay answered right after it. The only
// rewrite: the FETCHED invoice's metadata names this test's tenant (the lab
// used "t-lab"; a test owns its tenant) — the signed payload is untouched,
// and the handler never trusts its metadata anyway.

const labTenant = "t-lab"

// lab points the fixture's provider at the lab's secret.
func (f *fixture) lab(t *testing.T) *fixture {
	t.Helper()
	f.p.cfg.WebhookSecret = labSecret(t)
	return f
}

// invoiceOf is the capture's invoice id.
func invoiceOf(t *testing.T, name string) string {
	t.Helper()
	ev, err := ParseEvent(loadCapture(t, name).Raw)
	require.NoError(t, err)
	return ev.InvoiceID
}

// serve makes the fake answer the capture's fetched invoice (metadata
// tenant = this test's, unless keepLabTenant) and payment methods.
func (f *fixture) serve(t *testing.T, name string, keepLabTenant bool) {
	t.Helper()
	inv := string(companion(t, name, "invoice"))
	if !keepLabTenant {
		inv = strings.ReplaceAll(inv, `"`+labTenant+`"`, `"`+f.tenantID+`"`)
	}
	id := invoiceOf(t, name)
	f.fake.mu.Lock()
	defer f.fake.mu.Unlock()
	f.fake.invoices[id] = json.RawMessage(inv)
	f.fake.methods[id] = json.RawMessage(companion(t, name, "payment-methods"))
}

// openRow writes OUR pending ledger row for the capture's invoice (as
// startCheckout leaves it): $10.00, a month of 1 TB Standard.
func (f *fixture) openRow(t *testing.T, name, purpose string) string {
	t.Helper()
	id := invoiceOf(t, name)
	order := oneTB()
	require.NoError(t, payments.InTx(context.Background(), f.db, func(tx *sql.Tx) error {
		_, err := payments.RecordPending(context.Background(), tx, payments.Payment{TenantID: f.tenantID,
			Provider: payments.ProviderBTCPay, ProviderID: id, Kind: payments.KindCharge, Purpose: purpose,
			AmountCents: 1000, Order: &order})
		return err
	}))
	return id
}

// deliver posts the capture as BTCPay did; its claim rows are cleaned up
// with the test (a foreign delivery's claim names no tenant).
func (f *fixture) deliver(t *testing.T, name string) int {
	t.Helper()
	c := loadCapture(t, name)
	ev, err := ParseEvent(c.Raw)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = f.db.Exec(`DELETE FROM payment_events WHERE provider = 'btcpay' AND event_id = $1`, ev.DedupKey())
	})
	req := httptest.NewRequest(http.MethodPost, "/webhook/btcpay", bytes.NewReader(c.Raw))
	for k, v := range c.Headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	f.p.ServeHTTP(w, req)
	return w.Code
}

func (f *fixture) status(t *testing.T, invoice string) string {
	t.Helper()
	var s string
	require.NoError(t, f.db.QueryRow(`SELECT status FROM payments WHERE provider = 'btcpay' AND provider_id = $1`, invoice).Scan(&s))
	return s
}

func (f *fixture) hasEntitlement(t *testing.T) bool {
	t.Helper()
	_, err := payments.Get(context.Background(), f.db, f.tenantID)
	if err != nil {
		require.ErrorIs(t, err, payments.ErrNoEntitlement)
		return false
	}
	return true
}

func (f *fixture) audits(t *testing.T, action string) int {
	t.Helper()
	var n int
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM audit_logs WHERE tenant_id = $1 AND action = $2`, f.tenantID, action).Scan(&n))
	return n
}

// plusMonth is Postgres's month (the ledger's arithmetic: UTC, the
// month-end rule) — never Go's AddDate, which normalises 31 Oct + 1 month
// to 1 Dec.
func (f *fixture) plusMonth(t *testing.T, at time.Time) time.Time {
	t.Helper()
	var out time.Time
	require.NoError(t, f.db.QueryRow(`SELECT (($1::timestamptz AT TIME ZONE 'UTC') + INTERVAL '1 month') AT TIME ZONE 'UTC'`, at.UTC()).Scan(&out))
	return out
}

func webhookCount(outcome string) float64 {
	return counterValue("vaultaire_payment_webhook_total", "btcpay", "", outcome)
}

func TestBTCPayWebhook_Redelivery_IsDuplicate(t *testing.T) {
	// Arrange: the redelivery names the first delivery in originalDeliveryId.
	f := newFixture(t).lab(t)
	first, err := ParseEvent(loadCapture(t, "InvoiceSettled_exact").Raw)
	require.NoError(t, err)
	again, err := ParseEvent(loadCapture(t, "InvoiceSettled_redelivery").Raw)
	require.NoError(t, err)
	require.NotEqual(t, first.DeliveryID, again.DeliveryID)
	require.True(t, again.IsRedelivery)
	require.Equal(t, first.DedupKey(), again.DedupKey())
	inv := f.openRow(t, "InvoiceSettled_exact", payments.PurposeNew)
	f.serve(t, "InvoiceSettled_exact", false)
	dupBefore := webhookCount(payments.WebhookDuplicate)

	// Act
	c1 := f.deliver(t, "InvoiceSettled_exact")
	paid1 := f.entitlement(t).PaidThrough
	c2 := f.deliver(t, "InvoiceSettled_redelivery")

	// Assert: applied once — the date did not move twice.
	assert.Equal(t, http.StatusOK, c1)
	assert.Equal(t, http.StatusOK, c2)
	assert.Equal(t, payments.StatusSettled, f.status(t, inv))
	assert.True(t, paid1.Equal(f.entitlement(t).PaidThrough))
	assert.Equal(t, dupBefore+1, webhookCount(payments.WebhookDuplicate))
}

func TestBTCPayWebhook_UnknownInvoice_200NoApply(t *testing.T) {
	// No row of ours, and BTCPay does not know the invoice either (its 403
	// missing-permission — the lab: there is no 404).
	f := newFixture(t).lab(t)
	foreign := webhookCount(payments.WebhookForeign)

	code := f.deliver(t, "InvoiceSettled_exact")

	assert.Equal(t, http.StatusOK, code)
	assert.False(t, f.hasEntitlement(t))
	assert.Empty(t, f.ledger(t))
	assert.Equal(t, foreign+1, webhookCount(payments.WebhookForeign))

	// BTCPay knows it, but no row of ours names it (and its metadata names
	// no pending row of ours): still foreign, never applied.
	f.serve(t, "InvoiceSettled_speed_LowSpeed", false)
	assert.Equal(t, http.StatusOK, f.deliver(t, "InvoiceSettled_speed_LowSpeed"))
	assert.False(t, f.hasEntitlement(t))
	assert.Equal(t, foreign+2, webhookCount(payments.WebhookForeign))
}

func TestBTCPayWebhook_MetadataTenantMismatch_200NoApply(t *testing.T) {
	f := newFixture(t).lab(t)
	inv := f.openRow(t, "InvoiceSettled_exact", payments.PurposeNew)
	f.serve(t, "InvoiceSettled_exact", true) // the invoice names "t-lab"

	code := f.deliver(t, "InvoiceSettled_exact")

	assert.Equal(t, http.StatusOK, code)
	assert.False(t, f.hasEntitlement(t))
	assert.Equal(t, payments.StatusPending, f.status(t, inv))
}

func TestBTCPayWebhook_FetchError_5xx(t *testing.T) {
	// Arrange: our row, BTCPay answering 502 to the fetch.
	f := newFixture(t).lab(t)
	inv := f.openRow(t, "InvoiceSettled_exact", payments.PurposeNew)
	f.serve(t, "InvoiceSettled_exact", false)
	f.fake.mu.Lock()
	f.fake.fetchErr = http.StatusBadGateway
	f.fake.mu.Unlock()

	// Act + Assert: 500, nothing applied, the claim released — so BTCPay's
	// redelivery of the same delivery applies it.
	assert.Equal(t, http.StatusInternalServerError, f.deliver(t, "InvoiceSettled_exact"))
	assert.False(t, f.hasEntitlement(t))
	var claims int
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM payment_events WHERE provider = 'btcpay' AND event_id = $1`,
		invoiceDedup(t, "InvoiceSettled_exact")).Scan(&claims))
	assert.Zero(t, claims)

	f.fake.mu.Lock()
	f.fake.fetchErr = 0
	f.fake.mu.Unlock()
	assert.Equal(t, http.StatusOK, f.deliver(t, "InvoiceSettled_redelivery"))
	assert.True(t, f.hasEntitlement(t))
	assert.Equal(t, payments.StatusSettled, f.status(t, inv))
}

func invoiceDedup(t *testing.T, name string) string {
	ev, err := ParseEvent(loadCapture(t, name).Raw)
	require.NoError(t, err)
	return ev.DedupKey()
}

func TestBTCPayWebhook_Fetch404_200(t *testing.T) {
	// Our row exists, BTCPay no longer has the invoice (its 403
	// missing-permission): 200, a Warn, nothing applied.
	f := newFixture(t).lab(t)
	inv := f.openRow(t, "InvoiceSettled_exact", payments.PurposeNew)

	assert.Equal(t, http.StatusOK, f.deliver(t, "InvoiceSettled_exact"))
	assert.False(t, f.hasEntitlement(t))
	assert.Equal(t, payments.StatusPending, f.status(t, inv))
}

func TestBTCPayWebhook_Settled_Grants(t *testing.T) {
	f := newFixture(t).lab(t)
	inv := f.openRow(t, "InvoiceSettled_exact", payments.PurposeNew)
	f.serve(t, "InvoiceSettled_exact", false)
	settled := counterValue("vaultaire_payments_total", "btcpay", payments.KindCharge, payments.OutcomeSettled)
	before := time.Now()

	require.Equal(t, http.StatusOK, f.deliver(t, "InvoiceSettled_exact"))

	// The house, a month from now, by BTCPay, prepaid.
	e := f.entitlement(t)
	assert.Equal(t, payments.ProviderBTCPay, e.Provider)
	assert.Equal(t, payments.RenewalPrepaid, e.Renewal)
	assert.Equal(t, inv, e.ProviderRef)
	assert.Equal(t, 1, e.Order.Std)
	assert.WithinDuration(t, f.plusMonth(t, before), e.PaidThrough, 2*time.Minute)
	// The ledger: settled, numbered, the crypto side recorded.
	var number, rate, currency, meta string
	require.NoError(t, f.db.QueryRow(`SELECT COALESCE(invoice_number,''), COALESCE(rate::text,''), COALESCE(crypto_currency,''), metadata::text
		FROM payments WHERE provider = 'btcpay' AND provider_id = $1`, inv).Scan(&number, &rate, &currency, &meta))
	assert.True(t, strings.HasPrefix(number, "ST-"), number)
	assert.NotEmpty(t, rate)
	assert.Equal(t, "BTC", currency)
	assert.Contains(t, meta, `"txids"`)
	assert.NotContains(t, meta, `-1"`, "a txid, not the payment id's <txid>-<vout>")
	assert.Equal(t, 1, f.audits(t, "billing.house_granted"))
	assert.Equal(t, settled+1, counterValue("vaultaire_payments_total", "btcpay", payments.KindCharge, payments.OutcomeSettled))
	assert.Zero(t, f.credit(t))

	// A later event of the settled invoice (its PaymentSettled, another
	// delivery id) is a replay: the date does not move again.
	paid := e.PaidThrough
	f.serve(t, "InvoicePaymentSettled_exact", false)
	require.Equal(t, http.StatusOK, f.deliver(t, "InvoicePaymentSettled_exact"))
	assert.True(t, paid.Equal(f.entitlement(t).PaidThrough))
	assert.Equal(t, 1, f.audits(t, "billing.house_granted"))
}

func TestBTCPayWebhook_Settled_ExtendsFromPaidThrough(t *testing.T) {
	// Arrange: a house already paid 10 days ahead (a renewal).
	f := newFixture(t).lab(t)
	ahead := time.Now().Add(10 * 24 * time.Hour).UTC().Truncate(time.Second)
	require.NoError(t, payments.InTx(context.Background(), f.db, func(tx *sql.Tx) error {
		return payments.Grant(context.Background(), tx, f.tenantID, oneTB(), ahead, payments.ProviderManual, "", payments.RenewalPrepaid)
	}))
	f.openRow(t, "InvoiceSettled_exact", payments.PurposeRenew)
	f.serve(t, "InvoiceSettled_exact", false)

	require.Equal(t, http.StatusOK, f.deliver(t, "InvoiceSettled_exact"))

	e := f.entitlement(t)
	assert.WithinDuration(t, f.plusMonth(t, ahead), e.PaidThrough, time.Minute, "a month on from the paid date, not from now")
	assert.Equal(t, payments.ProviderBTCPay, e.Provider, "the provider that paid names the row")
}

func TestBTCPayWebhook_Settled_OverPaid_Credit(t *testing.T) {
	f := newFixture(t).lab(t)
	f.openRow(t, "InvoiceSettled_overpaid", payments.PurposeNew)
	f.serve(t, "InvoiceSettled_overpaid", false)

	require.Equal(t, http.StatusOK, f.deliver(t, "InvoiceSettled_overpaid"))

	assert.True(t, f.hasEntitlement(t))
	assert.Equal(t, int64(500), f.credit(t), "$15.00 paid on $10.00: $5.00 credit")
	assert.Equal(t, payments.StatusSettled, f.ledger(t)["overpaid:"+invoiceOf(t, "InvoiceSettled_overpaid")])
}

func TestBTCPayWebhook_ExpiredPartial_Credit_NoGrant(t *testing.T) {
	f := newFixture(t).lab(t)
	inv := f.openRow(t, "InvoiceExpired_partial", payments.PurposeNew)
	f.serve(t, "InvoiceExpired_partial", false)

	require.Equal(t, http.StatusOK, f.deliver(t, "InvoiceExpired_partial"))

	assert.False(t, f.hasEntitlement(t), "never a partial grant")
	assert.Equal(t, payments.StatusExpiredPartial, f.status(t, inv))
	assert.Equal(t, int64(500), f.credit(t))
	// Never paid at all: expired, no credit.
	g := newFixture(t).lab(t)
	inv2 := g.openRow(t, "InvoiceExpired_never", payments.PurposeNew)
	g.serve(t, "InvoiceExpired_never", false)
	require.Equal(t, http.StatusOK, g.deliver(t, "InvoiceExpired_never"))
	assert.Equal(t, payments.StatusExpired, g.status(t, inv2))
	assert.Zero(t, g.credit(t))
}

func TestBTCPayWebhook_PaidLateInFull_MarksSettled(t *testing.T) {
	// Arrange: our row of an invoice that expired unpaid and was then paid
	// in full (the fetched invoice: Expired/PaidLate, paidAmount 10.00).
	f := newFixture(t).lab(t)
	inv := f.openRow(t, "InvoiceReceivedPayment_late_full", payments.PurposeNew)

	// Act: the late payment, confirmed (the fetched invoice: Expired/PaidLate,
	// its payment Settled — RP33-2); then BTCPay's Settled{manuallyMarked}.
	f.serve(t, "InvoicePaymentSettled_late_full", false)
	require.Equal(t, http.StatusOK, f.deliver(t, "InvoiceReceivedPayment_late_full"))
	marked := append([]string(nil), f.fake.marked...)
	f.serve(t, "InvoiceSettled_late_full_marked", false)
	require.Equal(t, http.StatusOK, f.deliver(t, "InvoiceSettled_late_full_marked"))

	// Assert
	assert.Contains(t, marked, inv, "marked Settled by the automation")
	assert.Equal(t, 1, f.audits(t, "billing.invoice_marked"))
	var reason string
	require.NoError(t, f.db.QueryRow(`SELECT metadata->>'reason' FROM audit_logs WHERE tenant_id = $1 AND action = 'billing.invoice_marked'`,
		f.tenantID).Scan(&reason))
	assert.Equal(t, "paid_late_in_full", reason)
	assert.True(t, f.hasEntitlement(t))
	assert.Equal(t, payments.StatusSettled, f.status(t, inv))
}

func TestBTCPayWebhook_PaidLateShort_Credit(t *testing.T) {
	f := newFixture(t).lab(t)
	inv := f.openRow(t, "InvoiceReceivedPayment_late_partial", payments.PurposeNew)
	// The short payment, confirmed (RP33-2: an unconfirmed one credits
	// nothing — TestInvoiceExpired_UnconfirmedPartial_NoCredit).
	f.serve(t, "InvoicePaymentSettled_late_partial", false)

	require.Equal(t, http.StatusOK, f.deliver(t, "InvoiceReceivedPayment_late_partial"))

	assert.Empty(t, f.fake.marked)
	assert.False(t, f.hasEntitlement(t))
	assert.Equal(t, payments.StatusExpiredPartial, f.status(t, inv))
	assert.Equal(t, int64(500), f.credit(t))
}

func TestBTCPayWebhook_Processing_NoGrant(t *testing.T) {
	f := newFixture(t).lab(t)
	inv := f.openRow(t, "InvoiceProcessing_exact", payments.PurposeNew)
	f.serve(t, "InvoiceProcessing_exact", false)

	require.Equal(t, http.StatusOK, f.deliver(t, "InvoiceProcessing_exact"))

	assert.False(t, f.hasEntitlement(t), "no grant on an unconfirmed payment")
	assert.Equal(t, payments.StatusProcessing, f.status(t, inv))
}

func TestBTCPayWebhook_Invalid_NoGrant(t *testing.T) {
	f := newFixture(t).lab(t)
	inv := f.openRow(t, "InvoiceInvalid_marked", payments.PurposeNew)
	f.serve(t, "InvoiceInvalid_marked", false)

	require.Equal(t, http.StatusOK, f.deliver(t, "InvoiceInvalid_marked"))

	assert.False(t, f.hasEntitlement(t))
	assert.Equal(t, payments.StatusInvalid, f.status(t, inv))
}

func TestBTCPayWebhook_Created_Noop(t *testing.T) {
	f := newFixture(t).lab(t)
	inv := f.openRow(t, "InvoiceCreated_created", payments.PurposeNew)

	require.Equal(t, http.StatusOK, f.deliver(t, "InvoiceCreated_created"))

	assert.Equal(t, payments.StatusPending, f.status(t, inv))
	f.fake.mu.Lock()
	defer f.fake.mu.Unlock()
	assert.Zero(t, f.fake.calls["GET /api/v1/stores/"+fakeStore+"/invoices/"+inv], "nothing fetched for a creation")
}

// The lab's finding 3: marking an UNPAID invoice Settled by hand sends
// InvoiceSettled{manuallyMarked} with paidAmount 0 — never a grant.
func TestBTCPayWebhook_SettledMarkedUnpaid_NoGrant(t *testing.T) {
	f := newFixture(t).lab(t)
	inv := f.openRow(t, "InvoiceSettled_marked_unpaid", payments.PurposeNew)
	f.serve(t, "InvoiceSettled_marked_unpaid", false)

	require.Equal(t, http.StatusOK, f.deliver(t, "InvoiceSettled_marked_unpaid"))

	assert.False(t, f.hasEntitlement(t))
	assert.Equal(t, payments.StatusPending, f.status(t, inv))
	var numbered int
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM payments WHERE tenant_id = $1 AND invoice_number IS NOT NULL`, f.tenantID).Scan(&numbered))
	assert.Zero(t, numbered)
}

func TestBTCPayWebhook_BadSignature_400_NothingClaimed(t *testing.T) {
	f := newFixture(t) // the fixture's own secret, not the lab's
	rejected := webhookCount(payments.WebhookRejected)
	assert.Equal(t, http.StatusBadRequest, f.deliver(t, "InvoiceSettled_exact"))
	assert.Equal(t, rejected+1, webhookCount(payments.WebhookRejected))
	var claims int
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM payment_events WHERE provider = 'btcpay' AND event_id = $1`,
		invoiceDedup(t, "InvoiceSettled_exact")).Scan(&claims))
	assert.Zero(t, claims)
}
