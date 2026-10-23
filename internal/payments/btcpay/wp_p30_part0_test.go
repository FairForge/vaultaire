package btcpay

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/FairForge/vaultaire/internal/payments"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// Stage 1a Prompt 30 Part 0a — RP37-1 (P2, proven) + RP37-2 (P3, proven):
// the overpaid credit of a Settled invoice is max(0, confirmed − amount),
// written on EVERY delivery of a settled invoice (SetCredit sets an
// absolute value under the invoice's id: a replay re-asserts it, a later
// confirmation raises it). RP37-6: the Settled grant gate reads the
// confirmed amount, and money that cannot be confirmed (no payments[]) is
// said out loud.
//
// The lab captured an overpaid invoice paid in ONE transaction; a second
// payment beside it is SYNTHETIC: the capture's payment methods with the
// payments array rewritten (twoPayments), and a second delivery of the same
// invoice is a SYNTHETIC body re-signed with the lab secret
// (deliverSynthetic). Rate 84675 USD/BTC: 0.0001181 BTC = $10.00,
// 0.00005905 BTC = $5.00.

// twoPayments makes the fake answer the overpaid capture's invoice with
// paidAmount paid and its BTC method holding a $10.00 payment of status
// first and, when second is not "", a $5.00 payment of status second.
func (f *fixture) twoPayments(t *testing.T, paid, first, second string) {
	t.Helper()
	const name = "InvoiceSettled_overpaid"
	f.serveAs(t, name, StatusSettled, paid)
	var pms []map[string]any
	require.NoError(t, json.Unmarshal(companion(t, name, "payment-methods"), &pms))
	pays := []any{map[string]any{"id": "aa00000000000000000000000000000000000000000000000000000000000001-0",
		"receivedDate": 1792617830, "value": "0.0001181", "fee": "0", "status": first}}
	if second != "" {
		pays = append(pays, map[string]any{"id": "bb00000000000000000000000000000000000000000000000000000000000002-0",
			"receivedDate": 1792617840, "value": "0.00005905", "fee": "0", "status": second})
	}
	pms[0]["payments"] = pays
	raw, err := json.Marshal(pms)
	require.NoError(t, err)
	f.fake.mu.Lock()
	defer f.fake.mu.Unlock()
	f.fake.methods[invoiceOf(t, name)] = raw
}

// deliverSynthetic posts a SYNTHETIC delivery of the capture's invoice: its
// body with a fresh deliveryId (a new delivery, never a redelivery) and
// type typ, signed with the provider's secret.
func (f *fixture) deliverSynthetic(t *testing.T, name, typ string) int {
	t.Helper()
	var body map[string]any
	require.NoError(t, json.Unmarshal(loadCapture(t, name).Raw, &body))
	id := fmt.Sprintf("synthetic-%d", time.Now().UnixNano())
	body["deliveryId"], body["originalDeliveryId"], body["isRedelivery"], body["type"] = id, id, false, typ
	raw, err := json.Marshal(body)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = f.db.Exec(`DELETE FROM payment_events WHERE provider = 'btcpay' AND event_id = $1`, id)
	})
	req := httptest.NewRequest(http.MethodPost, "/webhook/btcpay", bytes.NewReader(raw))
	req.Header.Set(SigHeader, sign(f.p.cfg.WebhookSecret, raw))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	f.p.ServeHTTP(w, req)
	return w.Code
}

// overpaidCredit is the invoice's overpaid credit row (0 when none).
func (f *fixture) overpaidCredit(t *testing.T, invoice string) int64 {
	t.Helper()
	var c sql.NullInt64
	err := f.db.QueryRow(`SELECT amount_cents FROM payments WHERE provider = 'btcpay' AND provider_id = $1 AND kind = 'credit'`,
		overpaidCreditID(invoice)).Scan(&c)
	if err == sql.ErrNoRows {
		return 0
	}
	require.NoError(t, err)
	return c.Int64
}

func TestSettled_UnconfirmedExtraPayment_NoCredit(t *testing.T) {
	// Arrange: the invoice settled on a confirmed $10.00; a second $5.00 was
	// seen beside it and is still Processing (a low-fee transaction the
	// sender can RBF back after the settle). paidAmount counts it: 15.00.
	f := newFixture(t).lab(t)
	inv := f.openRow(t, "InvoiceSettled_overpaid", payments.PurposeNew)
	f.twoPayments(t, "15.00", PaymentSettled, "Processing")

	// Act
	require.Equal(t, http.StatusOK, f.deliver(t, "InvoiceSettled_overpaid"))

	// Assert: granted on the confirmed $10.00; the unconfirmed $5.00 is no
	// credit (a credit that covers an order is a grant).
	assert.True(t, f.hasEntitlement(t))
	assert.Equal(t, payments.StatusSettled, f.status(t, inv))
	assert.Zero(t, f.credit(t), "an unconfirmed extra payment is never a credit")
}

func TestSettled_ExtraPaymentConfirmsLater_CreditedOnTheLaterDelivery(t *testing.T) {
	// Arrange: the invoice settled on exactly $10.00, confirmed.
	f := newFixture(t).lab(t)
	inv := f.openRow(t, "InvoiceSettled_overpaid", payments.PurposeNew)
	f.twoPayments(t, "10.00", PaymentSettled, "")
	require.Equal(t, http.StatusOK, f.deliver(t, "InvoiceSettled_overpaid"))
	require.True(t, f.hasEntitlement(t))
	paidThrough := f.entitlement(t).PaidThrough
	require.Zero(t, f.credit(t))

	// Act: the customer pays $5.00 more; BTCPay sees it (Processing) …
	f.twoPayments(t, "15.00", PaymentSettled, "Processing")
	require.Equal(t, http.StatusOK, f.deliverSynthetic(t, "InvoiceSettled_overpaid", "InvoiceReceivedPayment"))
	assert.Zero(t, f.credit(t), "seen, not confirmed: nothing yet")
	// … and confirms it (InvoicePaymentSettled).
	f.twoPayments(t, "15.00", PaymentSettled, PaymentSettled)
	require.Equal(t, http.StatusOK, f.deliverSynthetic(t, "InvoiceSettled_overpaid", "InvoicePaymentSettled"))

	// Assert: the later confirmation is credited (money is never lost,
	// § 3.4) — and only credited: the period was granted once.
	assert.Equal(t, int64(500), f.credit(t))
	assert.Equal(t, int64(500), f.overpaidCredit(t, inv))
	assert.True(t, paidThrough.Equal(f.entitlement(t).PaidThrough), "a later payment is never a second period")
	assert.Equal(t, 1, f.audits(t, "billing.house_granted"))
}

func TestSettled_Replay_CreditUnchanged(t *testing.T) {
	// Arrange: settled with $5.00 over, all confirmed.
	f := newFixture(t).lab(t)
	inv := f.openRow(t, "InvoiceSettled_overpaid", payments.PurposeNew)
	f.serve(t, "InvoiceSettled_overpaid", false)
	require.Equal(t, http.StatusOK, f.deliver(t, "InvoiceSettled_overpaid"))
	require.Equal(t, int64(500), f.credit(t))

	// Act: BTCPay's redelivery, then a later event of the same invoice.
	require.Equal(t, http.StatusOK, f.deliverSynthetic(t, "InvoiceSettled_overpaid", "InvoiceSettled"))
	require.Equal(t, http.StatusOK, f.deliverSynthetic(t, "InvoiceSettled_overpaid", "InvoicePaymentSettled"))

	// Assert: one credit row, the same amount.
	assert.Equal(t, int64(500), f.credit(t))
	var rows int
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM payments WHERE tenant_id = $1 AND kind = 'credit'`, f.tenantID).Scan(&rows))
	assert.Equal(t, 1, rows)
	assert.Equal(t, int64(500), f.overpaidCredit(t, inv))
}

func TestSettled_RefundedRow_NoOverpaidCredit(t *testing.T) {
	// Arrange: settled on exactly $10.00; an operator then refunded it.
	f := newFixture(t).lab(t)
	inv := f.openRow(t, "InvoiceSettled_overpaid", payments.PurposeNew)
	f.twoPayments(t, "10.00", PaymentSettled, "")
	require.Equal(t, http.StatusOK, f.deliver(t, "InvoiceSettled_overpaid"))
	_, err := f.db.Exec(`UPDATE payments SET status = 'refunded' WHERE provider = 'btcpay' AND provider_id = $1`, inv)
	require.NoError(t, err)

	// Act: a second payment to the refunded invoice confirms.
	f.twoPayments(t, "15.00", PaymentSettled, PaymentSettled)
	require.Equal(t, http.StatusOK, f.deliverSynthetic(t, "InvoiceSettled_overpaid", "InvoicePaymentSettled"))

	// Assert: a refunded row is never credited again (D-P30-1: the operator
	// who refunded decides what is owed).
	assert.Zero(t, f.overpaidCredit(t, inv))
	assert.Zero(t, f.credit(t))
	assert.Equal(t, payments.StatusRefunded, f.status(t, inv))
}

func TestSettled_HandMarkedOverProcessing_NotGranted(t *testing.T) {
	// Arrange: an operator marks an invoice Settled while its full payment
	// is still Processing (paidAmount 10.00, nothing confirmed).
	f := newFixture(t).lab(t)
	inv := f.openRow(t, "InvoiceSettled_overpaid", payments.PurposeNew)
	f.twoPayments(t, "10.00", "Processing", "")
	unpaid := webhookCount(payments.WebhookSettledUnpaid)

	// Act
	require.Equal(t, http.StatusOK, f.deliver(t, "InvoiceSettled_overpaid"))

	// Assert: no grant on unconfirmed money; counted for a person to look.
	assert.False(t, f.hasEntitlement(t))
	assert.NotEqual(t, payments.StatusSettled, f.status(t, inv))
	assert.Equal(t, unpaid+1, webhookCount(payments.WebhookSettledUnpaid))

	// The payment confirms: BTCPay's InvoicePaymentSettled grants it then.
	f.twoPayments(t, "10.00", PaymentSettled, "")
	require.Equal(t, http.StatusOK, f.deliverSynthetic(t, "InvoiceSettled_overpaid", "InvoicePaymentSettled"))
	assert.True(t, f.hasEntitlement(t))
	assert.Equal(t, payments.StatusSettled, f.status(t, inv))
}

// unconfirmableValue reads vaultaire_btcpay_unconfirmable_total.
func unconfirmableValue(t *testing.T) float64 {
	t.Helper()
	reg := prometheus.NewRegistry()
	reg.MustRegister(payments.Collectors()...)
	mfs, err := reg.Gather()
	require.NoError(t, err)
	for _, mf := range mfs {
		if mf.GetName() == "vaultaire_btcpay_unconfirmable_total" {
			return mf.GetMetric()[0].GetCounter().GetValue()
		}
	}
	t.Fatal("vaultaire_btcpay_unconfirmable_total is not exported")
	return 0
}

func TestConfirmedCents_NoPaymentsArray_ErrorLineAndCounter(t *testing.T) {
	// Arrange: a SYNTHETIC 1.x-shaped answer — paidAmount 10.00 on an
	// Invalid invoice, its payment method listing no payments at all.
	f := newFixture(t).lab(t)
	core, logs := observer.New(zapcore.ErrorLevel)
	f.p.logger = zap.New(core)
	inv := f.openRow(t, "InvoiceInvalid_marked", payments.PurposeNew)
	f.serveAs(t, "InvoiceInvalid_marked", StatusInvalid, "10.00")
	var pms []map[string]any
	require.NoError(t, json.Unmarshal(companion(t, "InvoiceInvalid_marked", "payment-methods"), &pms))
	for _, pm := range pms {
		delete(pm, "payments")
	}
	raw, err := json.Marshal(pms)
	require.NoError(t, err)
	f.fake.mu.Lock()
	f.fake.methods[inv] = raw
	f.fake.mu.Unlock()
	before := unconfirmableValue(t)

	// Act
	require.Equal(t, http.StatusOK, f.deliver(t, "InvoiceInvalid_marked"))

	// Assert: no credit invented, one Error line naming the invoice, counted.
	assert.Zero(t, f.credit(t))
	assert.Equal(t, before+1, unconfirmableValue(t))
	var named int
	for _, e := range logs.All() {
		if e.ContextMap()["invoice"] == inv {
			named++
		}
	}
	assert.Equal(t, 1, named, "one Error line naming the invoice")
}
