package handlers

import (
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/FairForge/vaultaire/internal/payments"
	"github.com/stretchr/testify/assert"
	"go.uber.org/zap"
)

// Stage 1a Prompt 27 Part 0 — RP33-8b (P4): BTCPay accepted the cancel
// (the invoice is Invalid there) and our own transaction then failed. The
// page must say the invoice was cancelled and that the page catches up —
// never "Nothing was changed", which is false.
func TestCancelCryptoInvoice_MarkedButNotClosed_SaysCancelled(t *testing.T) {
	// Arrange
	db := testDashDB(t)
	t.Cleanup(func() { _ = db.Close() })
	id := houseTenant(t, db, 0, 0)
	inv := openCryptoInvoice(t, db, id, payments.StatusPending, time.Now().Add(-10*time.Minute))
	fb := newFakeHouseBilling()
	btc := &cancellingBitcoin{fakeBitcoin: &fakeBitcoin{}, db: db, closeFails: true}
	reg := cancelRegistry(db, fb, btc)
	house := HandleHouseCheckout(reg, db, houseFlags(true), "https://stored.test", zap.NewNop())

	// Act
	w := postCancel(t, HandleCancelCryptoInvoice(reg, map[string]http.Handler{"house": house}, zap.NewNop()), id,
		url.Values{"cancel_invoice": {inv}, "cancel_provider": {"btcpay"}, "cancel_then": {"house"}, "std": {"1"}, "period": {"annual"}})

	// Assert: back to billing with the truth; the checkout is not tried
	// (the guard would refuse it until the row closes).
	assert.Equal(t, http.StatusSeeOther, w.Code)
	assert.Equal(t, "/dashboard/billing", w.Header().Get("Location"))
	_, msg := flashOf(t, w)
	assert.Contains(t, msg, "was cancelled")
	assert.NotContains(t, msg, "Nothing was changed")
	assert.Zero(t, fb.liveAsked)
}
