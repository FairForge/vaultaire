package handlers

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/FairForge/vaultaire/internal/payments"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// WP-PAY-5 (Stage 1a Prompt 25) on the billing page.

// openCryptoInvoice writes a pending BTCPay charge row for the tenant
// (what btcpay.startCheckout leaves while the customer pays), created at.
func openCryptoInvoice(t *testing.T, db *sql.DB, tenantID, status string, created time.Time) string {
	t.Helper()
	inv := "BtcOpen_" + tenantID
	order := payments.Order{Std: 2, Period: payments.PeriodMonthly}
	require.NoError(t, payments.InTx(context.Background(), db, func(tx *sql.Tx) error {
		_, err := payments.RecordPending(context.Background(), tx, payments.Payment{TenantID: tenantID, Provider: payments.ProviderBTCPay,
			ProviderID: inv, Kind: payments.KindCharge, Purpose: payments.PurposeNew, AmountCents: 2000, Order: &order})
		return err
	}))
	_, err := db.Exec(`UPDATE payments SET status = $2, created_at = $3 WHERE provider = 'btcpay' AND provider_id = $1`, inv, status, created)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM payments WHERE tenant_id = $1`, tenantID) })
	return inv
}

func TestStripeCheckout_RefusedWhileCryptoInvoiceOpen(t *testing.T) {
	// Arrange: no house yet; a bitcoin invoice opened 10 minutes ago that
	// BTCPay may still settle for 24 h.
	db := testDashDB(t)
	t.Cleanup(func() { _ = db.Close() })
	id := houseTenant(t, db, 0, 0)
	opened := time.Now().Add(-10 * time.Minute).UTC()
	openCryptoInvoice(t, db, id, payments.StatusPending, opened)
	fb := newFakeHouseBilling()
	h := HandleHouseCheckout(fb.registry(db, true), db, houseFlags(true), "https://stored.test", zap.NewNop())

	// Act: a card checkout.
	w := postHouse(t, h, id, url.Values{"std": {"1"}, "period": {"annual"}, "provider": {"stripe"}})

	// Assert: 409 crypto_invoice_open, the date the invoice stops being
	// payable on the page, Stripe never asked.
	assert.Equal(t, http.StatusConflict, w.Code)
	assert.Equal(t, "crypto_invoice_open", w.Header().Get("X-Error-Code"))
	assert.Contains(t, w.Body.String(), opened.Add(24*time.Hour).Format("2 Jan 2006 15:04 UTC"))
	assert.Zero(t, fb.liveAsked, "the provider is never asked")
}

func TestStripeCheckout_CryptoInvoiceExpiredPastWindow_Allowed(t *testing.T) {
	// An invoice that expired more than 24 h ago can no longer be paid.
	db := testDashDB(t)
	t.Cleanup(func() { _ = db.Close() })
	id := houseTenant(t, db, 0, 0)
	openCryptoInvoice(t, db, id, payments.StatusExpired, time.Now().Add(-25*time.Hour))
	fb := newFakeHouseBilling()
	h := HandleHouseCheckout(fb.registry(db, true), db, houseFlags(true), "https://stored.test", zap.NewNop())

	w := postHouse(t, h, id, url.Values{"std": {"1"}, "period": {"annual"}, "provider": {"stripe"}})

	assert.Equal(t, http.StatusSeeOther, w.Code)
	assert.Equal(t, 1, fb.liveAsked)
}

// fakeBitcoin is a prepaid, ready provider named btcpay.
type fakeBitcoin struct {
	payments.Provider
	checkouts []payments.Checkout
}

func (f *fakeBitcoin) Name() string                                       { return payments.ProviderBTCPay }
func (f *fakeBitcoin) Renewal() payments.Renewal                          { return payments.RenewalPrepaid }
func (f *fakeBitcoin) Ready(context.Context) (payments.Readiness, string) { return payments.Ready, "" }
func (f *fakeBitcoin) StartCheckout(_ context.Context, c payments.Checkout) (string, error) {
	f.checkouts = append(f.checkouts, c)
	return "https://pay.test/i/1", nil
}

// stopCounting is the Stripe fake with its StopRenewal counted.
type stopCounting struct {
	*fakeHouseBilling
	stops int
}

func (s *stopCounting) StopRenewal(context.Context, string) error { s.stops++; return nil }

// bothRegistry is a registry with the Stripe fake and the bitcoin fake,
// both flags on.
func bothRegistry(db *sql.DB, stripe payments.Provider, btc *fakeBitcoin) *payments.Registry {
	fl := houseFlags(true)
	fl.Register(payments.FlagPrefix+payments.ProviderStripe, true)
	fl.Register(payments.FlagPrefix+payments.ProviderBTCPay, true)
	reg := payments.NewRegistry(db, fl, 0, nil)
	reg.Register(stripe)
	reg.Register(btc)
	return reg
}

func postSwitch(t *testing.T, h http.Handler, tenantID, to string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "/dashboard/billing/switch", strings.NewReader(url.Values{"to": {to}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req = injectSessionWithTenant(req, tenantID)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

// grantBTC gives the tenant a bitcoin-paid house through pt.
func grantBTC(t *testing.T, db *sql.DB, tenantID string, pt time.Time) {
	t.Helper()
	require.NoError(t, payments.InTx(context.Background(), db, func(tx *sql.Tx) error {
		return payments.Grant(context.Background(), tx, tenantID, payments.Order{Std: 2, Period: payments.PeriodMonthly}, pt,
			payments.ProviderBTCPay, "BtcInv_"+tenantID, payments.RenewalPrepaid)
	}))
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM audit_logs WHERE tenant_id = $1`, tenantID)
		_, _ = db.Exec(`DELETE FROM entitlements WHERE tenant_id = $1`, tenantID)
		_, _ = db.Exec(`DELETE FROM tenant_floor_quotas WHERE tenant_id = $1`, tenantID)
	})
}

func TestSwitchToCrypto_Button_StopsRenewalKeepsDate(t *testing.T) {
	// Arrange: Stripe renews the house.
	db := testDashDB(t)
	t.Cleanup(func() { _ = db.Close() })
	id := houseTenant(t, db, 0, 0)
	grant(t, db, id, payments.RenewalAuto)
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM audit_logs WHERE tenant_id = $1`, id) })
	before, err := payments.Get(context.Background(), db, id)
	require.NoError(t, err)
	stripe := &stopCounting{fakeHouseBilling: newFakeHouseBilling()}
	h := HandleSwitchProvider(bothRegistry(db, stripe, &fakeBitcoin{}), db, "https://stored.test", zap.NewNop())

	// Act
	w := postSwitch(t, h, id, payments.ProviderBTCPay)

	// Assert: back to the page with the date said; Stripe stopped once.
	assert.Equal(t, http.StatusSeeOther, w.Code)
	_, msg := flashOf(t, w)
	assert.Contains(t, msg, before.PaidThrough.UTC().Format("2 Jan 2006"))
	assert.Equal(t, 1, stripe.stops)
	e, err := payments.Get(context.Background(), db, id)
	require.NoError(t, err)
	assert.Equal(t, payments.RenewalPrepaid, e.Renewal)
	assert.Equal(t, payments.ProviderBTCPay, e.PendingProvider)
	assert.True(t, before.PaidThrough.Equal(e.PaidThrough))
}

func TestSwitchToCard_TrialEndIsPaidThrough(t *testing.T) {
	// Arrange: a house paid in bitcoin for 40 more days.
	db := testDashDB(t)
	t.Cleanup(func() { _ = db.Close() })
	id := houseTenant(t, db, 0, 0)
	pt := time.Now().Add(40 * 24 * time.Hour).UTC().Truncate(time.Second)
	grantBTC(t, db, id, pt)
	stripe := newFakeHouseBilling()
	h := HandleSwitchProvider(bothRegistry(db, stripe, &fakeBitcoin{}), db, "https://stored.test", zap.NewNop())

	// Act
	w := postSwitch(t, h, id, payments.ProviderStripe)

	// Assert: a Stripe checkout for the same house whose first charge is
	// the bitcoin period's end (trial_end) — purpose switch.
	assert.Equal(t, http.StatusSeeOther, w.Code)
	assert.Equal(t, stripe.checkout, w.Header().Get("Location"))
	assert.True(t, pt.Equal(stripe.gotStartAt), "StartAt = paid_through, got %s", stripe.gotStartAt)
	assert.Equal(t, payments.PurposeSwitch, stripe.gotPurpose)
	assert.Equal(t, payments.Order{Std: 2, Period: payments.PeriodMonthly}, stripe.gotOrder)
	_, msg := flashOf(t, w)
	assert.NotContains(t, msg, "forfeited")
}

func TestSwitchToCard_Inside48h_StartsNow(t *testing.T) {
	db := testDashDB(t)
	t.Cleanup(func() { _ = db.Close() })
	id := houseTenant(t, db, 0, 0)
	grantBTC(t, db, id, time.Now().Add(20*time.Hour))
	stripe := newFakeHouseBilling()
	h := HandleSwitchProvider(bothRegistry(db, stripe, &fakeBitcoin{}), db, "https://stored.test", zap.NewNop())

	w := postSwitch(t, h, id, payments.ProviderStripe)

	assert.Equal(t, http.StatusSeeOther, w.Code)
	_, msg := flashOf(t, w)
	assert.Contains(t, msg, "forfeited", "inside 48 h the page says the remaining prepaid days are forfeited")
}

func TestBillingPage_SwitchOffers(t *testing.T) {
	db := testDashDB(t)
	t.Cleanup(func() { _ = db.Close() })
	// Stripe renews: "Switch to bitcoin at renewal".
	auto := houseTenant(t, db, 0, 0)
	grant(t, db, auto, payments.RenewalAuto)
	reg := bothRegistry(db, newFakeHouseBilling(), &fakeBitcoin{})
	body := renderBilling(t, reg, auto, true)
	assert.Contains(t, body, `action="/dashboard/billing/switch"`)
	assert.Contains(t, body, "Switch to bitcoin at renewal")
	// Paid in bitcoin: "Switch to card".
	btc := houseTenant(t, db, 0, 0)
	grantBTC(t, db, btc, time.Now().Add(40*24*time.Hour))
	body = renderBilling(t, reg, btc, true)
	assert.Contains(t, body, "Switch to card")
	// A switch asked for: the sentence says so.
	_, err := payments.SwitchToPrepaid(context.Background(), reg, auto, payments.ProviderBTCPay, "customer")
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM audit_logs WHERE tenant_id = $1`, auto) })
	body = renderBilling(t, reg, auto, true)
	assert.Contains(t, body, "you chose to pay in bitcoin")
	assert.NotContains(t, body, "Switch to bitcoin at renewal")
}
