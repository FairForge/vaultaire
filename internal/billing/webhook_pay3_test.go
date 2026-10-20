package billing

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/FairForge/vaultaire/internal/payments"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/stripe/stripe-go/v75"
)

// WP-PAY-3: the receipt mail on a NEWLY settled invoice, the renewal and
// grant audit rows written by the webhook, the customer key.

type countingReceipts struct {
	calls atomic.Int32
	last  string
	err   error
}

func (c *countingReceipts) SendReceipt(_ context.Context, _, paymentID string) error {
	c.calls.Add(1)
	c.last = paymentID
	return c.err
}

func (f *houseFixture) audits(t *testing.T, action string) int {
	t.Helper()
	var n int
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM audit_logs WHERE tenant_id = $1 AND action = $2`, f.tenantID, action).Scan(&n))
	return n
}

func TestReceiptEmail_OnceOnSettle(t *testing.T) {
	// Arrange: a granted house, a receipt sender that counts.
	f := setupHouseFixture(t)
	rc := &countingReceipts{}
	f.handler.SetReceiptSender(rc)
	start, end := f.grantedHouse(t)
	inv := invoiceFor("in_rcpt_"+f.tenantID, "cus_"+f.tenantID, "sub_"+f.tenantID)
	inv.AmountPaid = 5388
	inv.BillingReason = stripe.InvoiceBillingReasonSubscriptionCycle
	_ = start
	_ = end

	// Act: the paid event, then the same invoice under another event id
	// (Stripe re-sends), then the first event id again (a duplicate).
	f.post(t, "paid-1", "invoice.payment_succeeded", inv)
	f.post(t, "paid-2", "invoice.payment_succeeded", inv)
	f.post(t, "paid-1", "invoice.payment_succeeded", inv)

	// Assert: one receipt, for the settled row.
	assert.Equal(t, int32(1), rc.calls.Load(), "a replay sends nothing")
	status, _, _, _, _ := f.paymentRow(t, inv.ID)
	assert.Equal(t, payments.StatusSettled, status)
	var id string
	require.NoError(t, f.db.QueryRow(`SELECT id::text FROM payments WHERE provider_id = $1`, inv.ID).Scan(&id))
	assert.Equal(t, id, rc.last)

	// A failing sender is a Warn, never a 500.
	rc.err = errors.New("smtp down")
	inv2 := invoiceFor("in_rcpt2_"+f.tenantID, "cus_"+f.tenantID, "sub_"+f.tenantID)
	inv2.AmountPaid = 100
	f.post(t, "paid-3", "invoice.payment_succeeded", inv2)
	assert.Equal(t, int32(2), rc.calls.Load())
}

func TestWebhook_GrantAndRenewalAudits(t *testing.T) {
	f := setupHouseFixture(t)
	start := time.Now().Add(-time.Hour).Truncate(time.Second)
	end := start.Add(30 * 24 * time.Hour)

	// Created twice (a replay): one house_granted row.
	f.post(t, "c1", "customer.subscription.created", f.stripeHas(f.houseSub("active", start, end)))
	f.post(t, "c2", "customer.subscription.created", f.stripeHas(f.houseSub("active", start, end)))
	assert.Equal(t, 1, f.audits(t, "billing.house_granted"))

	// cancel_at_period_end → renewal stopped (actor: the webhook); back → resumed.
	stopped := f.houseSub("active", start, end)
	stopped.CancelAtPeriodEnd = true
	f.post(t, "u1", "customer.subscription.updated", f.stripeHas(stopped))
	assert.Equal(t, 1, f.audits(t, "billing.renewal_stopped"))
	assert.Equal(t, payments.RenewalPrepaid, f.entitlement(t).Renewal)
	f.post(t, "u2", "customer.subscription.updated", f.stripeHas(f.houseSub("active", start, end)))
	assert.Equal(t, 1, f.audits(t, "billing.renewal_resumed"))
	assert.Equal(t, payments.RenewalAuto, f.entitlement(t).Renewal)
	var actor string
	require.NoError(t, f.db.QueryRow(`SELECT metadata->>'actor' FROM audit_logs WHERE tenant_id = $1 AND action = 'billing.renewal_stopped'`, f.tenantID).Scan(&actor))
	assert.Equal(t, "stripe-webhook", actor)

	// A terminal status under the flag stops the renewal once, audited once.
	f.handler.SetFlags(lapseFlag{f.tenantID: true})
	f.post(t, "x1", "customer.subscription.updated", f.stripeHas(f.houseSub("unpaid", start, end)))
	f.post(t, "x2", "customer.subscription.updated", f.stripeHas(f.houseSub("unpaid", start, end)))
	assert.Equal(t, 2, f.audits(t, "billing.renewal_stopped"), "stopped by the terminal status once; the replay changed nothing")
}

// RP22-7: the customer idempotency key names the e-mail too.
func TestCustomerIdempotencyKey_IncludesEmail(t *testing.T) {
	a := customerIdempotencyKey("t1", "a@b.test")
	b := customerIdempotencyKey("t1", "c@d.test")
	assert.NotEqual(t, a, b)
	assert.Equal(t, a, customerIdempotencyKey("t1", " A@B.test "), "case and space do not make a new key")
	assert.Regexp(t, `^customer-t1-[0-9a-f]{8}$`, a)
}
