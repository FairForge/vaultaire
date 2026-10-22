package btcpay

import (
	"bytes"
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/FairForge/vaultaire/internal/payments"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Stage 1a Prompt 25 Part 0 — the BTCPay webhook made safe to turn on.
//
// RP30-1: the row that decides a grant is read UNDER the tenant's
// entitlement lock, so two overlapping settle deliveries of one invoice
// (InvoiceSettled + InvoicePaymentSettled — two dedup keys) grant once, and
// an Expired delivery never writes a credit for a row a concurrent delivery
// settled. RP30-3: a verdict that applied nothing does not keep its claim,
// so BTCPay's redelivery (same originalDeliveryId) is handled fresh.
// RP30-4/5: a permanent Greenfield refusal is 200 + counted, never a 5xx
// that stalls the store's webhook queue.

// request builds the capture's delivery on the test goroutine (the claim
// row is cleaned up with the test) so it can be served from another one.
func (f *fixture) request(t *testing.T, name string) *http.Request {
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
	return req
}

// holdLock takes the tenant's entitlement lock in a transaction of the
// test's own (a checkout, another delivery) and returns it.
func (f *fixture) holdLock(t *testing.T) *sql.Tx {
	t.Helper()
	tx, err := f.db.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	require.NoError(t, payments.Lock(context.Background(), tx, f.tenantID))
	t.Cleanup(func() { _ = tx.Rollback() })
	return tx
}

// serveAll posts the requests concurrently and returns their status codes.
func (f *fixture) serveAll(reqs ...*http.Request) (wait func() []int) {
	codes := make([]int, len(reqs))
	var wg sync.WaitGroup
	for i, req := range reqs {
		wg.Add(1)
		go func(i int, req *http.Request) {
			defer wg.Done()
			w := httptest.NewRecorder()
			f.p.ServeHTTP(w, req)
			codes[i] = w.Code
		}(i, req)
	}
	return func() []int { wg.Wait(); return codes }
}

func TestBTCPayWebhook_TwoSettleDeliveriesOverlap_GrantOnce(t *testing.T) {
	// Arrange: our pending row; BTCPay answers the invoice Settled to both
	// deliveries; the tenant's lock is held (a checkout, another delivery)
	// while both arrive.
	f := newFixture(t).lab(t)
	inv := f.openRow(t, "InvoiceSettled_exact", payments.PurposeNew)
	f.serve(t, "InvoiceSettled_exact", false)
	require.Equal(t, inv, invoiceOf(t, "InvoicePaymentSettled_exact"), "two events of one invoice")
	settled := f.request(t, "InvoiceSettled_exact")
	paymentSettled := f.request(t, "InvoicePaymentSettled_exact")
	before := time.Now()
	holder := f.holdLock(t)

	// Act: both deliveries overlap, then the holder lets go.
	wait := f.serveAll(settled, paymentSettled)
	time.Sleep(1500 * time.Millisecond)
	require.NoError(t, holder.Rollback())
	codes := wait()

	// Assert: one month, one grant, one payment.
	assert.Equal(t, []int{http.StatusOK, http.StatusOK}, codes)
	e := f.entitlement(t)
	assert.WithinDuration(t, f.plusMonth(t, before), e.PaidThrough, 2*time.Minute, "ONE month for one payment")
	assert.Equal(t, 1, f.audits(t, "billing.house_granted"))
	assert.Equal(t, payments.StatusSettled, f.status(t, inv))
}

func TestBTCPayWebhook_ExpiredAfterConcurrentSettle_NoCredit(t *testing.T) {
	// Arrange: our pending row; BTCPay answers the invoice Expired with half
	// paid (what a fetch made before the settle saw); the lock is held by a
	// delivery that settles the row while the Expired one waits.
	f := newFixture(t).lab(t)
	inv := f.openRow(t, "InvoiceExpired_partial", payments.PurposeNew)
	f.serve(t, "InvoiceExpired_partial", false)
	expired := f.request(t, "InvoiceExpired_partial")
	holder := f.holdLock(t)

	// Act
	wait := f.serveAll(expired)
	time.Sleep(1500 * time.Millisecond)
	order := oneTB()
	_, err := payments.Settle(context.Background(), holder, payments.Payment{TenantID: f.tenantID, Provider: payments.ProviderBTCPay,
		ProviderID: inv, Kind: payments.KindCharge, Purpose: payments.PurposeNew, AmountCents: 1000, Order: &order}, time.Now())
	require.NoError(t, err)
	require.NoError(t, holder.Commit())
	codes := wait()

	// Assert: the settled row stays settled, and no credit was written for
	// money that paid it.
	assert.Equal(t, []int{http.StatusOK}, codes)
	assert.Equal(t, payments.StatusSettled, f.status(t, inv))
	assert.Zero(t, f.credit(t))
	_, hasPartial := f.ledger(t)["partial:"+inv]
	assert.False(t, hasPartial)
}

func TestBTCPayWebhook_ForeignThenRedeliveredAfterFix_Applies(t *testing.T) {
	// Arrange: our row; BTCPay answers 403 missing-permission to the fetch
	// (the key lost canviewinvoices, or the invoice is unknown).
	f := newFixture(t).lab(t)
	inv := f.openRow(t, "InvoiceSettled_exact", payments.PurposeNew)
	foreign := webhookCount(payments.WebhookForeign)

	// Act: the delivery is acknowledged; the operator fixes the key and
	// redelivers it from the BTCPay UI (same originalDeliveryId).
	first := f.deliver(t, "InvoiceSettled_exact")
	f.serve(t, "InvoiceSettled_exact", false)
	again := f.deliver(t, "InvoiceSettled_redelivery")

	// Assert: 200 both times; the redelivery is handled fresh and grants.
	assert.Equal(t, http.StatusOK, first)
	assert.Equal(t, foreign+1, webhookCount(payments.WebhookForeign))
	assert.Equal(t, http.StatusOK, again)
	assert.True(t, f.hasEntitlement(t), "a redelivery of a non-applied delivery is not a duplicate")
	assert.Equal(t, payments.StatusSettled, f.status(t, inv))
}

func TestBTCPayWebhook_MarkStatus422_200Counted(t *testing.T) {
	// Arrange: an invoice paid late in full; BTCPay refuses the mark (a
	// transition not in availableStatusesForManualMarking — permanent).
	f := newFixture(t).lab(t)
	inv := f.openRow(t, "InvoiceReceivedPayment_late_full", payments.PurposeNew)
	f.serve(t, "InvoicePaymentSettled_late_full", false) // confirmed (RP33-2)
	f.fake.mu.Lock()
	f.fake.markErr = http.StatusUnprocessableEntity
	f.fake.mu.Unlock()
	refused := webhookCount(payments.WebhookMarkRefused)
	errs := webhookCount(payments.WebhookError)

	// Act
	code := f.deliver(t, "InvoiceReceivedPayment_late_full")

	// Assert: acknowledged (never a 5xx that stalls the store's queue),
	// counted, nothing granted, the claim not kept.
	assert.Equal(t, http.StatusOK, code)
	assert.Equal(t, refused+1, webhookCount(payments.WebhookMarkRefused))
	assert.Equal(t, errs, webhookCount(payments.WebhookError))
	assert.False(t, f.hasEntitlement(t))
	assert.Equal(t, payments.StatusPending, f.status(t, inv))
	var claims int
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM payment_events WHERE provider = 'btcpay' AND event_id = $1`,
		invoiceDedup(t, "InvoiceReceivedPayment_late_full")).Scan(&claims))
	assert.Zero(t, claims, "a redelivery after the fix is handled fresh")

	// A Greenfield 5xx on the mark stays a retry.
	f.fake.mu.Lock()
	f.fake.markErr = http.StatusBadGateway
	f.fake.mu.Unlock()
	assert.Equal(t, http.StatusInternalServerError, f.deliver(t, "InvoiceReceivedPayment_late_full"))
}

func TestBTCPayWebhook_FetchRefused_200Counted(t *testing.T) {
	// A Greenfield 401 (a wrong key) is permanent until an operator fixes
	// it: 200 + counted, the claim not kept — never 71 minutes of 5xx.
	f := newFixture(t).lab(t)
	f.openRow(t, "InvoiceSettled_exact", payments.PurposeNew)
	f.serve(t, "InvoiceSettled_exact", false)
	f.fake.mu.Lock()
	f.fake.fetchErr = http.StatusUnauthorized
	f.fake.mu.Unlock()
	refused := webhookCount(payments.WebhookFetchRefused)

	assert.Equal(t, http.StatusOK, f.deliver(t, "InvoiceSettled_exact"))

	assert.Equal(t, refused+1, webhookCount(payments.WebhookFetchRefused))
	assert.False(t, f.hasEntitlement(t))
}

func TestBTCPayWebhook_SettledUnpaid_Counted(t *testing.T) {
	f := newFixture(t).lab(t)
	f.openRow(t, "InvoiceSettled_marked_unpaid", payments.PurposeNew)
	f.serve(t, "InvoiceSettled_marked_unpaid", false)
	unpaid := webhookCount(payments.WebhookSettledUnpaid)

	require.Equal(t, http.StatusOK, f.deliver(t, "InvoiceSettled_marked_unpaid"))

	assert.Equal(t, unpaid+1, webhookCount(payments.WebhookSettledUnpaid))
	assert.False(t, f.hasEntitlement(t))
}

func TestBTCPayWebhook_NewOutcomesExistAtZero(t *testing.T) {
	// RP14-9: a series born at 1 is never an increase a rule can see.
	for _, o := range []string{payments.WebhookSettledUnpaid, payments.WebhookMarkRefused, payments.WebhookFetchRefused} {
		assert.True(t, seriesPresent("vaultaire_payment_webhook_total", "btcpay", o), o)
	}
}

// seriesPresent reports whether a payments series with that provider and
// outcome is exported at all (at any value).
func seriesPresent(name, provider, outcome string) bool {
	reg := prometheus.NewRegistry()
	reg.MustRegister(payments.Collectors()...)
	mfs, err := reg.Gather()
	if err != nil {
		return false
	}
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			l := map[string]string{}
			for _, p := range m.GetLabel() {
				l[p.GetName()] = p.GetValue()
			}
			if l["provider"] == provider && l["outcome"] == outcome {
				return true
			}
		}
	}
	return false
}
