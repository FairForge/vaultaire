package billing

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	dashauth "github.com/FairForge/vaultaire/internal/dashboard/auth"
	"github.com/FairForge/vaultaire/internal/dashboard/handlers"
	"github.com/FairForge/vaultaire/internal/payments"
	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/stripe/stripe-go/v75"
	"go.uber.org/zap"
)

// Stage 1a Prompt 24 Part 0.
//
// RP28-1: one refund at a provider is ONE ledger row, whichever side records
// it first — the charge.refunded webhook (keyed by the re_… id) or the admin
// refund form (the dashboard's HandleAdminRefund; its API twin calls the same
// payments.RecordRefund). A refund of a Stripe charge is Stripe's to record:
// the form refuses to CREATE one without its re_… id, and naming one already
// in the ledger is an idempotent hit — or, with "shorten", takes the refunded
// share off paid_through once.
//
// RP28-2: a refund is never lost to delivery order (charge.refunded before
// the charge's invoice.payment_succeeded) or to being pending at delivery
// (charge.refund.updated).

// adminForm posts the dashboard's admin refund form for the fixture's tenant.
func (f *houseFixture) adminForm(t *testing.T, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	r := chi.NewRouter()
	r.Post("/admin/tenants/{id}/billing/refund", handlers.HandleAdminRefund(f.db, zap.NewNop()))
	if form.Get("reason") == "" {
		form.Set("reason", "refunded in the Stripe dashboard")
	}
	req := httptest.NewRequest(http.MethodPost, "/admin/tenants/"+f.tenantID+"/billing/refund", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	sd := &dashauth.SessionData{UserID: "00000000-0000-4000-8000-00000000ad01", TenantID: "admin-tenant", Email: "admin@stored.test", Role: "admin"}
	req = req.WithContext(context.WithValue(req.Context(), dashauth.SessionKey, sd))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// chargeRow is the ledger id of the fixture's settled Stripe charge.
func (f *houseFixture) chargeRow(t *testing.T, invoiceID string) string {
	t.Helper()
	id, err := payments.ChargeOf(context.Background(), f.db, f.tenantID, payments.ProviderStripe, invoiceID)
	require.NoError(t, err)
	return id
}

func (f *houseFixture) refundSum(t *testing.T) int64 {
	t.Helper()
	var n int64
	require.NoError(t, f.db.QueryRow(
		`SELECT COALESCE(SUM(amount_cents), 0) FROM payments WHERE tenant_id = $1 AND kind = 'refund'`, f.tenantID).Scan(&n))
	return n
}

func (f *houseFixture) chargeStatus(t *testing.T, invoiceID string) string {
	t.Helper()
	var s string
	require.NoError(t, f.db.QueryRow(`SELECT status FROM payments WHERE provider = 'stripe' AND provider_id = $1`, invoiceID).Scan(&s))
	return s
}

func TestRefund_StripeWebhookThenAdminForm_OneRow(t *testing.T) {
	// Arrange: a $10.00 charge, $5.00 of it refunded in Stripe and delivered.
	f := setupHouseFixture(t)
	inv := f.paidInvoice(t, 1000)
	re := "re_wf_" + f.tenantID
	f.post(t, "refunded", "charge.refunded",
		f.refundedCharge(inv.ID, &stripe.Refund{ID: re, Amount: 500, Status: stripe.RefundStatusSucceeded}))
	require.Equal(t, 1, f.refundRows(t))

	// Act: the operator records the same refund by hand — with its re_ id,
	// then (the review's proof) with the reference left blank.
	withRef := f.adminForm(t, url.Values{"payment_id": {f.chargeRow(t, inv.ID)}, "amount": {"5.00"}, "provider_ref": {re}})
	blank := f.adminForm(t, url.Values{"payment_id": {f.chargeRow(t, inv.ID)}, "amount": {"5.00"}})

	// Assert: one row of $5.00, the charge still settled (not "refunded").
	assert.Equal(t, http.StatusOK, withRef.Code, withRef.Body.String())
	assert.Contains(t, withRef.Body.String(), "already in the ledger")
	assert.Equal(t, http.StatusBadRequest, blank.Code, blank.Body.String())
	assert.Equal(t, 1, f.refundRows(t))
	assert.Equal(t, int64(500), f.refundSum(t))
	assert.Equal(t, payments.StatusSettled, f.chargeStatus(t, inv.ID))
	assert.Equal(t, 1, f.audits(t, "billing.refund_recorded"), "the idempotent hit writes no second refund audit")
}

func TestRefund_AdminFormThenWebhook_OneRow(t *testing.T) {
	// Arrange: a $10.00 charge.
	f := setupHouseFixture(t)
	inv := f.paidInvoice(t, 1000)
	re := "re_fw_" + f.tenantID

	// Act: the form first, with the re_ id; then Stripe's delivery.
	w := f.adminForm(t, url.Values{"payment_id": {f.chargeRow(t, inv.ID)}, "amount": {"5.00"}, "provider_ref": {re}})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	code := f.postCode(t, "refunded", "charge.refunded",
		f.refundedCharge(inv.ID, &stripe.Refund{ID: re, Amount: 500, Status: stripe.RefundStatusSucceeded}))

	// Assert: the delivery takes the ErrRefundRecorded path — 200, one row.
	assert.Equal(t, http.StatusOK, code)
	assert.True(t, f.eventRecorded(t, "refunded"))
	assert.Equal(t, 1, f.refundRows(t))
	assert.Equal(t, int64(500), f.refundSum(t))
}

func TestRefund_AdminFormBlankRefOnStripeCharge_Refused(t *testing.T) {
	// Arrange: a $10.00 Stripe charge, nothing refunded yet.
	f := setupHouseFixture(t)
	inv := f.paidInvoice(t, 1000)
	numbersBefore := f.creditNotes(t)

	// Act: a blank reference, then one that is not a Stripe refund id.
	blank := f.adminForm(t, url.Values{"payment_id": {f.chargeRow(t, inv.ID)}, "amount": {"5.00"}})
	other := f.adminForm(t, url.Values{"payment_id": {f.chargeRow(t, inv.ID)}, "amount": {"5.00"}, "provider_ref": {"wire-123"}})

	// Assert: refused, no row, no credit-note number spent.
	assert.Equal(t, http.StatusBadRequest, blank.Code, blank.Body.String())
	assert.Contains(t, blank.Body.String(), "re_")
	assert.Equal(t, http.StatusBadRequest, other.Code, other.Body.String())
	assert.Zero(t, f.refundRows(t))
	assert.Equal(t, numbersBefore, f.creditNotes(t))
}

func TestRefund_ShortenAfterWebhookRefund_Once(t *testing.T) {
	// Arrange: a charge whose invoice bought a 30-day period; half refunded
	// in Stripe and delivered (the webhook never shortens — D-P23-4).
	f := setupHouseFixture(t)
	inv := f.paidInvoiceForPeriod(t, 1000, 30*24*time.Hour)
	re := "re_sh_" + f.tenantID
	f.post(t, "refunded", "charge.refunded",
		f.refundedCharge(inv.ID, &stripe.Refund{ID: re, Amount: 500, Status: stripe.RefundStatusSucceeded}))
	before, err := payments.Get(context.Background(), f.db, f.tenantID)
	require.NoError(t, err)

	// Act: the operator names the recorded refund by its re_ id and ticks
	// shorten — twice; then names it by its ledger row.
	first := f.adminForm(t, url.Values{"payment_id": {f.chargeRow(t, inv.ID)}, "provider_ref": {re}, "shorten": {"on"}})
	second := f.adminForm(t, url.Values{"payment_id": {f.chargeRow(t, inv.ID)}, "provider_ref": {re}, "shorten": {"on"}})
	var refundRow string
	require.NoError(t, f.db.QueryRow(`SELECT id::text FROM payments WHERE provider = 'stripe' AND provider_id = $1`, re).Scan(&refundRow))
	byRow := f.adminForm(t, url.Values{"payment_id": {refundRow}, "shorten": {"on"}})

	// Assert: shortened once, by half the 30-day period; no second refund
	// row; the date change audited as billing.paid_through_adjusted.
	require.Equal(t, http.StatusOK, first.Code, first.Body.String())
	assert.Contains(t, first.Body.String(), "Paid through moved back 15 days")
	assert.Equal(t, http.StatusConflict, second.Code, second.Body.String())
	assert.Equal(t, http.StatusConflict, byRow.Code, byRow.Body.String())
	after, err := payments.Get(context.Background(), f.db, f.tenantID)
	require.NoError(t, err)
	assert.WithinDuration(t, before.PaidThrough.Add(-15*24*time.Hour), after.PaidThrough, time.Second)
	assert.Equal(t, 1, f.refundRows(t))
	assert.Equal(t, 1, f.audits(t, "billing.paid_through_adjusted"))
	var shortened string
	require.NoError(t, f.db.QueryRow(`SELECT COALESCE(metadata->'shortened'->>'at', '') FROM payments WHERE id::text = $1`, refundRow).Scan(&shortened))
	assert.NotEmpty(t, shortened, "the refund row carries when it shortened the date")
}

func TestRefund_ManualChargeFormUnchanged(t *testing.T) {
	// Arrange: a manual (wire) payment of a year.
	f := setupHouseFixture(t)
	res, err := payments.RecordManualPayment(context.Background(), f.db, payments.ManualPayment{TenantID: f.tenantID, AmountCents: 5388,
		Period: payments.PeriodAnnual, Order: &payments.Order{Std: 1, Period: payments.PeriodAnnual}, Actor: "admin-1", Reason: "wire"})
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = f.db.Exec(`DELETE FROM entitlements WHERE tenant_id = $1`, f.tenantID) })

	// Act: a refund with no reference, as before.
	w := f.adminForm(t, url.Values{"payment_id": {res.PaymentID}, "amount": {"10.00"}})

	// Assert: recorded under a generated reference.
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var ref string
	require.NoError(t, f.db.QueryRow(`SELECT provider_id FROM payments WHERE tenant_id = $1 AND kind = 'refund'`, f.tenantID).Scan(&ref))
	assert.True(t, strings.HasPrefix(ref, "refund:"), ref)
}

// paidInvoiceForPeriod is paidInvoice with one line that bought `period`.
func (f *houseFixture) paidInvoiceForPeriod(t *testing.T, cents int64, period time.Duration) stripe.Invoice {
	t.Helper()
	start := time.Now().Add(-time.Hour).Truncate(time.Second)
	f.post(t, "created", "customer.subscription.created", f.stripeHas(f.houseSub("active", start, start.Add(period))))
	inv := invoiceFor("in_p_"+f.tenantID, "cus_"+f.tenantID, "sub_"+f.tenantID)
	inv.AmountPaid, inv.Currency = cents, "usd"
	inv.BillingReason = stripe.InvoiceBillingReasonSubscriptionCreate
	inv.Lines = &stripe.InvoiceLineItemList{Data: []*stripe.InvoiceLineItem{
		{Period: &stripe.Period{Start: start.Unix(), End: start.Add(period).Unix()}}}}
	f.post(t, "paid", "invoice.payment_succeeded", inv)
	return inv
}

// creditNotes counts the tenant's numbered refund rows (each would have
// spent a gapless number).
func (f *houseFixture) creditNotes(t *testing.T) int {
	t.Helper()
	var n int
	require.NoError(t, f.db.QueryRow(
		`SELECT COUNT(*) FROM payments WHERE tenant_id = $1 AND kind = 'refund' AND invoice_number IS NOT NULL`, f.tenantID).Scan(&n))
	return n
}

// --- RP28-2 ---

// fakeRefunds is the refund source the webhook asks (Stripe's refund list).
type fakeRefunds map[string][]*stripe.Refund

func (r fakeRefunds) ForCharge(_ context.Context, chargeID string) ([]*stripe.Refund, error) {
	return r[chargeID], nil
}

// fakeCharges is the charge source charge.refund.updated fetches.
type fakeCharges map[string]*stripe.Charge

func (c fakeCharges) Get(_ context.Context, id string) (*stripe.Charge, error) {
	ch, ok := c[id]
	if !ok {
		return nil, &stripe.Error{HTTPStatusCode: http.StatusNotFound, Code: stripe.ErrorCodeResourceMissing}
	}
	return ch, nil
}

func TestChargeRefunded_BeforeInvoicePaid_RecordedWhenChargeLands(t *testing.T) {
	// Arrange: the house's subscription exists; the charge of its first
	// invoice is refunded and Stripe delivers charge.refunded BEFORE
	// invoice.payment_succeeded (Stripe does not order deliveries).
	f := setupHouseFixture(t)
	start := time.Now().Add(-time.Hour).Truncate(time.Second)
	f.post(t, "created", "customer.subscription.created", f.stripeHas(f.houseSub("active", start, start.Add(30*24*time.Hour))))
	invID := "in_early_" + f.tenantID
	chID := "ch_" + f.tenantID
	re := &stripe.Refund{ID: "re_early_" + f.tenantID, Amount: 300, Status: stripe.RefundStatusSucceeded, Charge: &stripe.Charge{ID: chID}}
	f.handler.refunds = fakeRefunds{chID: {re}}
	code := f.postCode(t, "refunded-early", "charge.refunded", f.refundedCharge(invID, re))
	require.Equal(t, http.StatusOK, code)
	require.Zero(t, f.refundRows(t), "the ledger does not know the charge yet")

	// Act: the invoice's payment lands.
	inv := invoiceFor(invID, "cus_"+f.tenantID, "sub_"+f.tenantID)
	inv.AmountPaid, inv.Currency = 1000, "usd"
	inv.BillingReason = stripe.InvoiceBillingReasonSubscriptionCreate
	inv.Charge = &stripe.Charge{ID: chID}
	f.post(t, "paid", "invoice.payment_succeeded", inv)
	// and is redelivered.
	f.post(t, "paid-again", "invoice.payment_succeeded", inv)

	// Assert: the refund recorded once, linked to the charge.
	assert.Equal(t, 1, f.refundRows(t))
	assert.Equal(t, int64(300), f.refundSum(t))
	var refundOf string
	require.NoError(t, f.db.QueryRow(`SELECT refund_of::text FROM payments WHERE provider_id = $1`, re.ID).Scan(&refundOf))
	assert.Equal(t, f.chargeRow(t, invID), refundOf)
}

func TestChargeRefundUpdated_PendingThenSucceeded_RecordedOnce(t *testing.T) {
	// Arrange: a settled $10.00 charge; its refund is pending when
	// charge.refunded is delivered.
	f := setupHouseFixture(t)
	inv := f.paidInvoice(t, 1000)
	chID := "ch_" + f.tenantID
	pending := &stripe.Refund{ID: "re_pend_" + f.tenantID, Amount: 400, Status: stripe.RefundStatusPending, Charge: &stripe.Charge{ID: chID}}
	f.handler.refunds = fakeRefunds{chID: {pending}}
	f.handler.charges = fakeCharges{chID: {ID: chID, Customer: &stripe.Customer{ID: "cus_" + f.tenantID}, Invoice: &stripe.Invoice{ID: inv.ID}}}
	f.post(t, "refunded", "charge.refunded", f.refundedCharge(inv.ID, pending))
	require.Zero(t, f.refundRows(t), "a pending refund is not money given back yet")
	f.post(t, "refund-pending", "charge.refund.updated", pending)
	require.Zero(t, f.refundRows(t))

	// Act: the refund succeeds; Stripe sends charge.refund.updated (and
	// redelivers it under a new event id).
	succeeded := *pending
	succeeded.Status = stripe.RefundStatusSucceeded
	f.handler.refunds = fakeRefunds{chID: {&succeeded}}
	f.post(t, "refund-succeeded", "charge.refund.updated", &succeeded)
	f.post(t, "refund-succeeded-again", "charge.refund.updated", &succeeded)

	// Assert: recorded once, by the webhook.
	assert.Equal(t, 1, f.refundRows(t))
	assert.Equal(t, int64(400), f.refundSum(t))
	assert.Equal(t, 1, f.audits(t, "billing.refund_recorded"))
}
