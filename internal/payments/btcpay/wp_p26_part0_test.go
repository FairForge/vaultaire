package btcpay

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/FairForge/vaultaire/internal/payments"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Stage 1a Prompt 26 Part 0 — RP31-1 (the WP-P25 review, P2 proven): an
// invoice is marked Invalid only when it is provably unpaid (fetched New or
// Expired with paidAmount 0) — Processing is paid — and money that reaches an
// Invalid invoice anyway is a credit, never lost.

// serveAs makes the fake answer the capture's invoice with its status and
// paidAmount replaced.
func (f *fixture) serveAs(t *testing.T, name, status, paid string) {
	t.Helper()
	f.serve(t, name, false)
	id := invoiceOf(t, name)
	f.fake.mu.Lock()
	defer f.fake.mu.Unlock()
	var inv map[string]any
	require.NoError(t, json.Unmarshal(f.fake.invoices[id], &inv))
	inv["status"], inv["paidAmount"] = status, paid
	raw, err := json.Marshal(inv)
	require.NoError(t, err)
	f.fake.invoices[id] = raw
}

func (f *fixture) markedAtBTCPay() []string {
	f.fake.mu.Lock()
	defer f.fake.mu.Unlock()
	return append([]string(nil), f.fake.marked...)
}

func TestPortalUncancel_ProcessingInvoice_LeftToSettle(t *testing.T) {
	// Arrange: the switch invoice was paid (BTCPay answers Processing, its
	// paidAmount the whole amount) when the customer re-enabled the card
	// renewal in Stripe's portal.
	f := newFixture(t).lab(t)
	inv := f.openRow(t, "InvoiceProcessing_exact", payments.PurposeSwitch)
	f.serve(t, "InvoiceProcessing_exact", false)
	skipped := webhookCount(payments.WebhookInvalidateSkippedPaid)

	// Act
	n, err := f.p.InvalidateOpenInvoices(context.Background(), f.tenantID, payments.ReasonPortalUncancel)

	// Assert: never marked — it is left to settle (its settle is an implicit
	// switch, which re-asserts the Stripe stop); counted.
	require.NoError(t, err)
	assert.Zero(t, n)
	assert.Empty(t, f.markedAtBTCPay(), "a paid invoice is never marked Invalid")
	assert.Equal(t, skipped+1, webhookCount(payments.WebhookInvalidateSkippedPaid))
	assert.Equal(t, payments.StatusPending, f.status(t, inv), "our row is untouched")
}

func TestInvalidateOpenInvoices_PartlyPaidNew_LeftToSettle(t *testing.T) {
	// A New invoice that already received part of its amount is paid money.
	f := newFixture(t).lab(t)
	f.openRow(t, "InvoiceCreated_created", payments.PurposeSwitch)
	f.serveAs(t, "InvoiceCreated_created", StatusNew, "4.00")

	n, err := f.p.InvalidateOpenInvoices(context.Background(), f.tenantID, payments.ReasonPortalUncancel)

	require.NoError(t, err)
	assert.Zero(t, n)
	assert.Empty(t, f.markedAtBTCPay())
}

func TestInvalidateOpenInvoices_UnpaidNewAndExpired_Marked(t *testing.T) {
	f := newFixture(t).lab(t)
	newInv := f.openRow(t, "InvoiceCreated_created", payments.PurposeSwitch)
	f.serve(t, "InvoiceCreated_created", false) // New, paidAmount 0
	expInv := f.openRow(t, "InvoiceExpired_never", payments.PurposeSwitch)
	f.serveAs(t, "InvoiceExpired_never", StatusExpired, "0")

	n, err := f.p.InvalidateOpenInvoices(context.Background(), f.tenantID, payments.ReasonPortalUncancel)

	require.NoError(t, err)
	assert.Equal(t, 2, n)
	assert.ElementsMatch(t, []string{newInv, expInv}, f.markedAtBTCPay())
}

func TestInvalidateOpenInvoices_FetchFails_NothingMarked(t *testing.T) {
	// Not provably unpaid when BTCPay cannot say: nothing is marked.
	f := newFixture(t).lab(t)
	f.openRow(t, "InvoiceCreated_created", payments.PurposeSwitch)
	f.serve(t, "InvoiceCreated_created", false)
	f.fake.mu.Lock()
	f.fake.fetchErr = http.StatusBadGateway
	f.fake.mu.Unlock()

	n, err := f.p.InvalidateOpenInvoices(context.Background(), f.tenantID, payments.ReasonPortalUncancel)

	require.NoError(t, err)
	assert.Zero(t, n)
	assert.Empty(t, f.markedAtBTCPay())
}

func TestInvoiceInvalid_WithPaidAmount_Credited(t *testing.T) {
	// Arrange: an invoice marked Invalid (by hand, or by us in the window
	// between the fetch and the mark) that holds a payment.
	f := newFixture(t).lab(t)
	inv := f.openRow(t, "InvoiceInvalid_marked", payments.PurposeNew)
	f.serveAs(t, "InvoiceInvalid_marked", StatusInvalid, "10.00")
	f.serveMethods(t, "InvoiceInvalid_marked", "InvoiceSettled_exact") // the payment confirmed (RP33-2)

	// Act
	require.Equal(t, http.StatusOK, f.deliver(t, "InvoiceInvalid_marked"))

	// Assert: no grant (an Invalid invoice is never a period) — the money is
	// the tenant's credit, like an expired invoice's partial payment.
	assert.False(t, f.hasEntitlement(t))
	assert.Equal(t, payments.StatusInvalid, f.status(t, inv))
	assert.Equal(t, int64(1000), f.credit(t))
	assert.Equal(t, payments.StatusSettled, f.ledger(t)["partial:"+inv])

	// A redelivery (or a later event of the invoice) credits it once.
	_, err := f.db.Exec(`DELETE FROM payment_events WHERE provider = 'btcpay' AND event_id = $1`, invoiceDedup(t, "InvoiceInvalid_marked"))
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, f.deliver(t, "InvoiceInvalid_marked"))
	assert.Equal(t, int64(1000), f.credit(t))
}

func TestInvoiceInvalid_SettledRow_NoCredit(t *testing.T) {
	// A settled row's money paid its period: an Invalid mark later credits
	// nothing (the grant stands, the operator adjusts it).
	f := newFixture(t).lab(t)
	inv := f.openRow(t, "InvoiceInvalid_marked", payments.PurposeNew)
	_, err := f.db.Exec(`UPDATE payments SET status = 'settled' WHERE provider = 'btcpay' AND provider_id = $1`, inv)
	require.NoError(t, err)
	f.serveAs(t, "InvoiceInvalid_marked", StatusInvalid, "10.00")

	require.Equal(t, http.StatusOK, f.deliver(t, "InvoiceInvalid_marked"))

	assert.Zero(t, f.credit(t))
}

// RP31-5: the customer's way out of the crypto_invoice_open 409.
func TestCancelUnpaidInvoice_UnpaidMarkedAndRowInvalid(t *testing.T) {
	f := newFixture(t).lab(t)
	inv := f.openRow(t, "InvoiceCreated_created", payments.PurposeNew)
	f.serve(t, "InvoiceCreated_created", false)

	err := f.p.CancelUnpaidInvoice(context.Background(), f.tenantID, inv, "user-1")

	require.NoError(t, err)
	assert.Equal(t, []string{inv}, f.markedAtBTCPay())
	assert.Equal(t, payments.StatusInvalid, f.status(t, inv), "our row is closed at once — the guard lets the card checkout through")
	assert.Equal(t, 1, f.audits(t, "billing.invoice_marked"))
}

func TestCancelUnpaidInvoice_ProcessingRefused(t *testing.T) {
	f := newFixture(t).lab(t)
	inv := f.openRow(t, "InvoiceProcessing_exact", payments.PurposeNew)
	f.serve(t, "InvoiceProcessing_exact", false)

	err := f.p.CancelUnpaidInvoice(context.Background(), f.tenantID, inv, "user-1")

	assert.True(t, errors.Is(err, payments.ErrInvoicePaid), "got %v", err)
	assert.Empty(t, f.markedAtBTCPay())
	assert.Equal(t, payments.StatusPending, f.status(t, inv))
}

func TestCancelUnpaidInvoice_AnotherTenantsInvoice_NotFound(t *testing.T) {
	f := newFixture(t).lab(t)
	inv := f.openRow(t, "InvoiceCreated_created", payments.PurposeNew)
	f.serve(t, "InvoiceCreated_created", false)

	err := f.p.CancelUnpaidInvoice(context.Background(), "someone-else", inv, "user-1")

	assert.True(t, errors.Is(err, payments.ErrNoSuchPayment), "got %v", err)
	assert.Empty(t, f.markedAtBTCPay())
}

func TestInvalidateSkippedPaid_ExistsAtZero(t *testing.T) {
	assert.True(t, seriesPresent("vaultaire_payment_webhook_total", "btcpay", payments.WebhookInvalidateSkippedPaid))
	assert.True(t, strings.HasPrefix(payments.WebhookInvalidateSkippedPaid, "invalidate_"))
}
