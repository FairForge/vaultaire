package btcpay

import (
	"context"
	"database/sql"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/FairForge/vaultaire/internal/payments"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// WP-PAY-5 (Stage 1a Prompt 25) at the BTCPay settle: a prepaid payment
// never silently takes over a house another provider renews by itself
// (RP30-2) — it is an implicit switch: the BTC period is appended, the other
// provider's renewal is stopped (durably: a renewal_stop row in the same
// transaction, the call after the commit), and the old subscription stays
// findable until the stop is confirmed.

// fakeStopper records the tenants whose pending renewal stops were run
// after a delivery's commit.
type fakeStopper struct {
	mu      sync.Mutex
	tenants []string
}

func (s *fakeStopper) StopPendingRenewals(_ context.Context, tenantID string) (payments.StopRun, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tenants = append(s.tenants, tenantID)
	return payments.StopRun{}, nil
}

func (f *fixture) grantStripe(t *testing.T, sub string, renewal payments.Renewal, paidThrough time.Time) {
	t.Helper()
	require.NoError(t, payments.InTx(context.Background(), f.db, func(tx *sql.Tx) error {
		return payments.Grant(context.Background(), tx, f.tenantID, oneTB(), paidThrough, payments.ProviderStripe, sub, renewal)
	}))
}

func TestBTCPaySettle_WhileStripeRenewsAuto_ImplicitSwitch(t *testing.T) {
	// Arrange: Stripe renews the house automatically (a checkout of Stripe
	// passed the guard while a BTCPay invoice was still open); the invoice
	// settles.
	f := newFixture(t).lab(t)
	stripePT := time.Now().Add(10 * 24 * time.Hour).UTC().Truncate(time.Second)
	sub := "sub_live_" + f.tenantID
	f.grantStripe(t, sub, payments.RenewalAuto, stripePT)
	inv := f.openRow(t, "InvoiceSettled_exact", payments.PurposeNew)
	f.serve(t, "InvoiceSettled_exact", false)
	stopper := &fakeStopper{}
	f.p.SetRenewalStopper(stopper)

	// Act
	code := f.deliver(t, "InvoiceSettled_exact")

	// Assert: the BTC month is appended to Stripe's date, the house is
	// BTCPay's now, and Stripe's renewal is being stopped — never left to
	// bill beside it.
	require.Equal(t, http.StatusOK, code)
	e := f.entitlement(t)
	assert.Equal(t, payments.ProviderBTCPay, e.Provider)
	assert.Equal(t, payments.RenewalPrepaid, e.Renewal)
	assert.Equal(t, inv, e.ProviderRef)
	assert.WithinDuration(t, f.plusMonth(t, stripePT), e.PaidThrough, time.Minute, "appended to the date Stripe was paid through")
	stops, err := payments.RenewalStops(context.Background(), f.db, f.tenantID)
	require.NoError(t, err)
	require.Len(t, stops, 1, "the Stripe subscription is remembered until its stop is confirmed")
	assert.Equal(t, payments.ProviderStripe, stops[0].Provider)
	assert.Equal(t, sub, stops[0].Ref)
	assert.Equal(t, payments.RenewalStopPending, stops[0].Status)
	assert.Equal(t, payments.ReasonImplicitSwitch, stops[0].Reason)
	stopper.mu.Lock()
	assert.Equal(t, []string{f.tenantID}, stopper.tenants, "the stop is attempted right after the commit")
	stopper.mu.Unlock()
	assert.Equal(t, 1, f.audits(t, "billing.provider_switched"))
}

func TestSwitchToCrypto_EarlyPayment_ExtendsFromPaidThrough(t *testing.T) {
	// Arrange: the customer asked to switch to crypto (Stripe's renewal
	// already stopped: prepaid, pending_provider btcpay) and pays the
	// switch invoice 20 days early.
	f := newFixture(t).lab(t)
	pt := time.Now().Add(20 * 24 * time.Hour).UTC().Truncate(time.Second)
	sub := "sub_switch_" + f.tenantID
	f.grantStripe(t, sub, payments.RenewalPrepaid, pt)
	_, err := f.db.Exec(`UPDATE entitlements SET pending_provider = 'btcpay' WHERE tenant_id = $1`, f.tenantID)
	require.NoError(t, err)
	f.openRow(t, "InvoiceSettled_exact", payments.PurposeSwitch)
	f.serve(t, "InvoiceSettled_exact", false)

	// Act
	require.Equal(t, http.StatusOK, f.deliver(t, "InvoiceSettled_exact"))

	// Assert: no gap, no overlap — a month from the paid date; BTCPay's
	// house; the pending provider cleared; the stopped subscription still
	// remembered (already stopped: nothing to call).
	e := f.entitlement(t)
	assert.WithinDuration(t, f.plusMonth(t, pt), e.PaidThrough, time.Minute)
	assert.Equal(t, payments.ProviderBTCPay, e.Provider)
	var pending sql.NullString
	require.NoError(t, f.db.QueryRow(`SELECT pending_provider FROM entitlements WHERE tenant_id = $1`, f.tenantID).Scan(&pending))
	assert.False(t, pending.Valid)
	stops, err := payments.RenewalStops(context.Background(), f.db, f.tenantID)
	require.NoError(t, err)
	require.Len(t, stops, 1)
	assert.Equal(t, sub, stops[0].Ref)
	assert.Equal(t, payments.RenewalStopStopped, stops[0].Status)
}

func TestBTCPay_InvalidateOpenInvoices_MarksAuditsAndCountsARefusal(t *testing.T) {
	// Arrange: two open invoices of the tenant, one settled one.
	f := newFixture(t).lab(t)
	open := f.openRow(t, "InvoiceSettled_exact", payments.PurposeSwitch)
	f.serve(t, "InvoiceSettled_exact", false)
	settled := f.openRow(t, "InvoiceSettled_overpaid", payments.PurposeNew)
	_, err := f.db.Exec(`UPDATE payments SET status = 'settled' WHERE provider = 'btcpay' AND provider_id = $1`, settled)
	require.NoError(t, err)

	// Act
	n, err := f.p.InvalidateOpenInvoices(context.Background(), f.tenantID, payments.ReasonPortalUncancel)

	// Assert: only the open one is marked Invalid at BTCPay, audited.
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	f.fake.mu.Lock()
	assert.Equal(t, []string{open}, f.fake.marked)
	f.fake.mu.Unlock()
	var reason, status string
	require.NoError(t, f.db.QueryRow(`SELECT metadata->>'reason', metadata->>'status' FROM audit_logs WHERE tenant_id = $1 AND action = 'billing.invoice_marked'`,
		f.tenantID).Scan(&reason, &status))
	assert.Equal(t, payments.ReasonPortalUncancel, reason)
	assert.Equal(t, StatusInvalid, status)

	// A refused mark (422) is counted, never an error for the caller.
	f.fake.mu.Lock()
	f.fake.markErr = http.StatusUnprocessableEntity
	f.fake.mu.Unlock()
	refused := webhookCount(payments.WebhookMarkRefused)
	n, err = f.p.InvalidateOpenInvoices(context.Background(), f.tenantID, payments.ReasonPortalUncancel)
	require.NoError(t, err)
	assert.Zero(t, n)
	assert.Equal(t, refused+1, webhookCount(payments.WebhookMarkRefused))
}
