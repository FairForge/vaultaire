package handlers

import (
	"context"
	"database/sql"
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/FairForge/vaultaire/internal/payments"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// Stage 1a Prompt 26 Part 0 — RP31-5: the crypto_invoice_open 409 gets a
// way out. "Cancel the bitcoin invoice" withdraws an invoice nobody paid,
// then the refused checkout continues; one holding a payment keeps the 409.

// cancellingBitcoin is the bitcoin fake that can cancel an invoice: unpaid
// = our row closed (what btcpay.CancelUnpaidInvoice does after BTCPay's
// fetch and mark), paid = the provider's refusal with the date.
type cancellingBitcoin struct {
	*fakeBitcoin
	db      *sql.DB
	paid    bool
	cancels []string
}

func (c *cancellingBitcoin) CancelUnpaidInvoice(ctx context.Context, tenantID, invoiceID, _ string) error {
	c.cancels = append(c.cancels, invoiceID)
	open, err := payments.OpenInvoiceOf(ctx, c.db, tenantID, payments.ProviderBTCPay, invoiceID)
	if err != nil {
		return err
	}
	if c.paid {
		open.Paid = true
		return open
	}
	_, err = c.db.ExecContext(ctx, `UPDATE payments SET status = 'invalid' WHERE provider = 'btcpay' AND provider_id = $1`, invoiceID)
	return err
}

func cancelRegistry(db *sql.DB, stripe payments.Provider, btc payments.Provider) *payments.Registry {
	fl := houseFlags(true)
	fl.Register(payments.FlagPrefix+payments.ProviderStripe, true)
	fl.Register(payments.FlagPrefix+payments.ProviderBTCPay, true)
	reg := payments.NewRegistry(db, fl, 0, nil)
	reg.Register(stripe)
	reg.Register(btc)
	return reg
}

var hiddenField = regexp.MustCompile(`<input type="hidden" name="([^"]+)" value="([^"]*)">`)

// cancelForm is the hidden fields of the 409 page's cancel form.
func cancelForm(t *testing.T, body string) url.Values {
	t.Helper()
	i := strings.Index(body, `action="/dashboard/billing/crypto-invoice/cancel"`)
	require.GreaterOrEqual(t, i, 0, "the page offers the cancel")
	form := url.Values{}
	for _, m := range hiddenField.FindAllStringSubmatch(body[i:], -1) {
		form.Add(html.UnescapeString(m[1]), html.UnescapeString(m[2]))
	}
	return form
}

func postCancel(t *testing.T, h http.Handler, tenantID string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "/dashboard/billing/crypto-invoice/cancel", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req = injectSessionWithTenant(req, tenantID)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func TestStripeCheckout_AbandonedUnpaidInvoice_CancelThenProceeds(t *testing.T) {
	// Arrange: a bitcoin invoice opened ten minutes ago and abandoned; the
	// customer chooses card.
	db := testDashDB(t)
	t.Cleanup(func() { _ = db.Close() })
	id := houseTenant(t, db, 0, 0)
	inv := openCryptoInvoice(t, db, id, payments.StatusPending, time.Now().Add(-10*time.Minute))
	fb := newFakeHouseBilling()
	btc := &cancellingBitcoin{fakeBitcoin: &fakeBitcoin{}, db: db}
	reg := cancelRegistry(db, fb, btc)
	house := HandleHouseCheckout(reg, db, houseFlags(true), "https://stored.test", zap.NewNop())
	refused := postHouse(t, house, id, url.Values{"std": {"1"}, "period": {"annual"}, "provider": {"stripe"}})
	require.Equal(t, http.StatusConflict, refused.Code)
	form := cancelForm(t, refused.Body.String())
	assert.Equal(t, inv, form.Get("cancel_invoice"))
	assert.Equal(t, "1", form.Get("std"), "the refused form's fields travel with the cancel")

	// Act: "Cancel the bitcoin invoice and continue".
	cancel := HandleCancelCryptoInvoice(reg, map[string]http.Handler{"house": house}, zap.NewNop())
	w := postCancel(t, cancel, id, form)

	// Assert: the invoice is withdrawn and the card checkout proceeds with
	// the same order.
	assert.Equal(t, []string{inv}, btc.cancels)
	assert.Equal(t, http.StatusSeeOther, w.Code)
	assert.Equal(t, fb.checkout, w.Header().Get("Location"))
	assert.Equal(t, 1, fb.liveAsked)
	var status string
	require.NoError(t, db.QueryRow(`SELECT status FROM payments WHERE provider = 'btcpay' AND provider_id = $1`, inv).Scan(&status))
	assert.Equal(t, payments.StatusInvalid, status)
}

func TestStripeCheckout_AbandonedUnpaidInvoice_ProcessingInvoice_Still409(t *testing.T) {
	// Arrange: the bitcoin invoice holds a payment (Processing at BTCPay).
	db := testDashDB(t)
	t.Cleanup(func() { _ = db.Close() })
	id := houseTenant(t, db, 0, 0)
	opened := time.Now().Add(-10 * time.Minute).UTC()
	inv := openCryptoInvoice(t, db, id, payments.StatusProcessing, opened)
	fb := newFakeHouseBilling()
	btc := &cancellingBitcoin{fakeBitcoin: &fakeBitcoin{}, db: db, paid: true}
	reg := cancelRegistry(db, fb, btc)
	house := HandleHouseCheckout(reg, db, houseFlags(true), "https://stored.test", zap.NewNop())
	form := cancelForm(t, postHouse(t, house, id, url.Values{"std": {"1"}, "period": {"annual"}, "provider": {"stripe"}}).Body.String())

	// Act
	w := postCancel(t, HandleCancelCryptoInvoice(reg, map[string]http.Handler{"house": house}, zap.NewNop()), id, form)

	// Assert: still 409 with the date, no cancel offered again, the card
	// never asked, our row untouched.
	assert.Equal(t, http.StatusConflict, w.Code)
	assert.Equal(t, "crypto_invoice_open", w.Header().Get("X-Error-Code"))
	assert.Contains(t, w.Body.String(), opened.Add(24*time.Hour).Format("2 Jan 2006 15:04 UTC"))
	assert.Contains(t, w.Body.String(), "already holds a payment")
	assert.NotContains(t, w.Body.String(), "crypto-invoice/cancel")
	assert.Zero(t, fb.liveAsked)
	var status string
	require.NoError(t, db.QueryRow(`SELECT status FROM payments WHERE provider = 'btcpay' AND provider_id = $1`, inv).Scan(&status))
	assert.Equal(t, payments.StatusProcessing, status)
}

func TestCancelCryptoInvoice_UnknownContinuation_NothingCancelled(t *testing.T) {
	db := testDashDB(t)
	t.Cleanup(func() { _ = db.Close() })
	id := houseTenant(t, db, 0, 0)
	inv := openCryptoInvoice(t, db, id, payments.StatusPending, time.Now())
	btc := &cancellingBitcoin{fakeBitcoin: &fakeBitcoin{}, db: db}
	reg := cancelRegistry(db, newFakeHouseBilling(), btc)

	w := postCancel(t, HandleCancelCryptoInvoice(reg, map[string]http.Handler{}, zap.NewNop()), id,
		url.Values{"cancel_invoice": {inv}, "cancel_provider": {"btcpay"}, "cancel_then": {"elsewhere"}})

	assert.Equal(t, http.StatusSeeOther, w.Code)
	assert.Empty(t, btc.cancels)
}

// RP31-7: the page says a card house switching to bitcoin ends on its date
// when entitlement_lapse is off for the tenant (Stripe's deletion clears
// it — no grace).
func TestHouseSentence_PendingBitcoin_NoGrace_SaysEndsUnlessPaid(t *testing.T) {
	pt := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	st := payments.Status{Found: true, Granted: true, Entitlement: payments.Entitlement{Provider: payments.ProviderStripe,
		Renewal: payments.RenewalPrepaid, PendingProvider: payments.ProviderBTCPay, PaidThrough: pt,
		Order: payments.Order{Std: 1, Period: payments.PeriodMonthly}}}

	off := houseSentence(st, endsAtDate(st, false))
	on := houseSentence(st, endsAtDate(st, true))

	assert.Contains(t, off, "Your house ends on 20 Nov 2026 unless the bitcoin payment arrives")
	assert.NotContains(t, on, "unless the bitcoin payment arrives")
}
