package btcpay

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/FairForge/vaultaire/internal/payments"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Stage 1a Prompt 27 Part 0 — RP33-2 (the WP-P26 review, P2): only
// confirmed bitcoin is value. paidAmount counts a payment the moment it is
// seen (Processing = paid in full, unconfirmed); a credit and a paid-late
// mark use the CONFIRMED amount — what the invoice's Settled payments are
// worth — so an unconfirmed payment that is later double-spent away (an
// RBF replacement after a 24 h monitoring window) never becomes a credit or
// a grant.

// serveMethods makes the fake answer the payment methods of another
// capture of the same invoice id (the payments' statuses at another time).
func (f *fixture) serveMethods(t *testing.T, invoiceCapture, methodsCapture string) {
	t.Helper()
	id := invoiceOf(t, invoiceCapture)
	f.fake.mu.Lock()
	defer f.fake.mu.Unlock()
	f.fake.methods[id] = json.RawMessage(companion(t, methodsCapture, "payment-methods"))
}

// lab27 points the fixture at the Prompt 27 lab store's secret: its
// captures are in testdata/p27 (BTCPay 2.4.5 regtest, 2026-10-22 — a
// payment left unmined past the monitoring window, then mined; a late full
// payment unmined, then mined).
func (f *fixture) lab27(t *testing.T) *fixture {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "p27", "SECRET.txt"))
	require.NoError(t, err)
	f.p.cfg.WebhookSecret = strings.TrimSpace(string(raw))
	ev, err := ParseEvent(loadCapture(t, "p27/InvoicePaymentSettled_after_invalid_mined").Raw)
	require.NoError(t, err)
	f.p.cfg.StoreID = ev.StoreID // the Prompt 27 lab's store
	return f
}

func TestInvoiceInvalid_UnconfirmedPayment_NoCredit(t *testing.T) {
	// Arrange: the lab — an invoice paid in full in time and never mined;
	// its 2-minute monitoring window passed and BTCPay made it Invalid
	// (Invalid/None): paidAmount still says 10.00, the payment's own status
	// is Processing. A delivery of the invoice now fetches that state.
	f := newFixture(t).lab27(t)
	inv := f.openRow(t, "p27/InvoiceProcessing_then_monitoring_expired", payments.PurposeNew)
	f.serve(t, "p27/InvoiceProcessing_then_monitoring_expired", false)

	// Act
	require.Equal(t, http.StatusOK, f.deliver(t, "p27/InvoiceProcessing_then_monitoring_expired"))

	// Assert: closed, no grant, and no credit — nothing confirmed.
	assert.Equal(t, payments.StatusInvalid, f.status(t, inv))
	assert.False(t, f.hasEntitlement(t))
	assert.Zero(t, f.credit(t), "an unconfirmed payment is never value")

	// RP33-11, the lab: the payment is mined after the Invalid mark and
	// BTCPay still delivers InvoicePaymentSettled (the invoice stays
	// Invalid, the payment Settled) — the confirmed amount is credited.
	f.serve(t, "p27/InvoicePaymentSettled_after_invalid_mined", false)
	require.Equal(t, http.StatusOK, f.deliver(t, "p27/InvoicePaymentSettled_after_invalid_mined"))
	assert.Equal(t, int64(1000), f.credit(t))
	assert.False(t, f.hasEntitlement(t), "an Invalid invoice is never a period")
}

func TestInvoiceInvalid_MarkedOverAnUnconfirmedPayment_NoCredit(t *testing.T) {
	// The same rule for an Invalid MARK (by hand, or the customer's cancel
	// racing a payment): paidAmount 10.00, the payment Processing.
	f := newFixture(t).lab(t)
	f.openRow(t, "InvoiceInvalid_marked", payments.PurposeNew)
	f.serveAs(t, "InvoiceInvalid_marked", StatusInvalid, "10.00")
	f.serveMethods(t, "InvoiceInvalid_marked", "InvoiceProcessing_exact")

	require.Equal(t, http.StatusOK, f.deliver(t, "InvoiceInvalid_marked"))

	assert.Zero(t, f.credit(t))
}

func TestLab27_ExpiredPaidLate_UnminedThenMined(t *testing.T) {
	// The Prompt 27 lab's own late full payment: unmined = not marked;
	// mined (InvoicePaymentSettled, still Expired/PaidLate) = marked.
	f := newFixture(t).lab27(t)
	inv := f.openRow(t, "p27/InvoiceReceivedPayment_late_full_unmined", payments.PurposeNew)
	f.serve(t, "p27/InvoiceReceivedPayment_late_full_unmined", false)
	require.Equal(t, http.StatusOK, f.deliver(t, "p27/InvoiceReceivedPayment_late_full_unmined"))
	assert.Empty(t, f.markedAtBTCPay())

	f.serve(t, "p27/InvoicePaymentSettled_late_full_mined", false)
	require.Equal(t, http.StatusOK, f.deliver(t, "p27/InvoicePaymentSettled_late_full_mined"))
	assert.Equal(t, []string{inv}, f.markedAtBTCPay())
}

func TestInvoiceExpired_UnconfirmedPartial_NoCredit(t *testing.T) {
	// Arrange: the lab's late short payment, seen and not yet mined
	// (Expired/PaidPartial, paidAmount 5.00, the payment Processing).
	f := newFixture(t).lab(t)
	inv := f.openRow(t, "InvoiceReceivedPayment_late_partial", payments.PurposeNew)
	f.serve(t, "InvoiceReceivedPayment_late_partial", false)

	// Act
	require.Equal(t, http.StatusOK, f.deliver(t, "InvoiceReceivedPayment_late_partial"))

	// Assert: expired with something seen, no credit yet.
	assert.False(t, f.hasEntitlement(t))
	assert.Equal(t, payments.StatusExpiredPartial, f.status(t, inv))
	assert.Zero(t, f.credit(t), "an unconfirmed partial is never a credit")

	// Mined (the lab's InvoicePaymentSettled of the same invoice): credited.
	f.serve(t, "InvoicePaymentSettled_late_partial", false)
	require.Equal(t, http.StatusOK, f.deliver(t, "InvoicePaymentSettled_late_partial"))
	assert.Equal(t, int64(500), f.credit(t))
}

func TestInvoiceExpired_PaidLateUnconfirmed_NotMarkedSettled(t *testing.T) {
	// Arrange: the lab's late full payment, seen and not yet mined
	// (Expired/PaidLate, paidAmount 10.00, the payment Processing).
	f := newFixture(t).lab(t)
	inv := f.openRow(t, "InvoiceReceivedPayment_late_full", payments.PurposeNew)
	f.serve(t, "InvoiceReceivedPayment_late_full", false)

	// Act
	require.Equal(t, http.StatusOK, f.deliver(t, "InvoiceReceivedPayment_late_full"))

	// Assert: never marked Settled on money that may not confirm.
	assert.Empty(t, f.markedAtBTCPay(), "an unconfirmed late payment is not a reason to mark the invoice Settled")
	assert.False(t, f.hasEntitlement(t))
	assert.Zero(t, f.audits(t, "billing.invoice_marked"))
	assert.Zero(t, f.credit(t))
	assert.NotEqual(t, payments.StatusSettled, f.status(t, inv))
}

func TestPaymentSettled_ExpiredPaidLate_MarksSettled(t *testing.T) {
	// Arrange: the late full payment was seen (not marked) …
	f := newFixture(t).lab(t)
	inv := f.openRow(t, "InvoiceReceivedPayment_late_full", payments.PurposeNew)
	f.serve(t, "InvoiceReceivedPayment_late_full", false)
	require.Equal(t, http.StatusOK, f.deliver(t, "InvoiceReceivedPayment_late_full"))
	require.Empty(t, f.markedAtBTCPay())

	// Act: … then mined — BTCPay's InvoicePaymentSettled (the invoice still
	// Expired/PaidLate, the payment now Settled) — and then the
	// InvoiceSettled{manuallyMarked} that our mark causes.
	f.serve(t, "InvoicePaymentSettled_late_full", false)
	require.Equal(t, http.StatusOK, f.deliver(t, "InvoicePaymentSettled_late_full"))
	marked := f.markedAtBTCPay()
	f.serve(t, "InvoiceSettled_late_full_marked", false)
	require.Equal(t, http.StatusOK, f.deliver(t, "InvoiceSettled_late_full_marked"))

	// Assert
	assert.Equal(t, []string{inv}, marked, "marked Settled once the payment confirmed")
	assert.Equal(t, 1, f.audits(t, "billing.invoice_marked"))
	assert.True(t, f.hasEntitlement(t))
	assert.Equal(t, payments.StatusSettled, f.status(t, inv))
	assert.Zero(t, f.credit(t))
}

// RP33-8b (P4): BTCPay accepted the cancel and our transaction then
// failed — the error says so (ErrInvoiceCancelPending), never a plain
// failure the page would read as "nothing was changed".
func TestCancelUnpaidInvoice_MarkedThenCloseFails_SaysCancelPending(t *testing.T) {
	// Arrange: the tenant's entitlement lock is held, so closing our row
	// cannot finish before the caller's deadline.
	f := newFixture(t).lab(t)
	inv := f.openRow(t, "InvoiceCreated_created", payments.PurposeNew)
	f.serve(t, "InvoiceCreated_created", false)
	f.holdLock(t)
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()

	// Act
	err := f.p.CancelUnpaidInvoice(ctx, f.tenantID, inv, "user-1")

	// Assert
	assert.Equal(t, []string{inv}, f.markedAtBTCPay(), "BTCPay accepted the mark")
	assert.ErrorIs(t, err, payments.ErrInvoiceCancelPending)
}
