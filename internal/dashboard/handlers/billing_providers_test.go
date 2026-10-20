package handlers

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/FairForge/vaultaire/internal/payments"
	"github.com/FairForge/vaultaire/internal/usage"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// WP-PAY-2 (docs/PAYMENTS.md § 2): the dashboard reaches money through
// payments only.

// The import direction is the deliverable: payments ← billing (Stripe),
// dashboard → payments. No package of the dashboard may depend on the
// Stripe SDK, directly or through internal/billing.
func TestDashboardImportsNoStripe(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "github.com/FairForge/vaultaire/internal/dashboard/...").CombinedOutput()
	require.NoError(t, err, string(out))
	var bad []string
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, "github.com/stripe/stripe-go") || line == "github.com/FairForge/vaultaire/internal/billing" {
			bad = append(bad, line)
		}
	}
	assert.Empty(t, bad, "the dashboard depends on a provider SDK")
}

func TestCheckout_ProviderFlagOff_409(t *testing.T) {
	// Arrange: pay_stripe off for everyone.
	db := testDashDB(t)
	t.Cleanup(func() { _ = db.Close() })
	id := houseTenant(t, db, 0, 0)
	fb := newFakeHouseBilling()
	h := HandleHouseCheckout(fb.registry(db, false), db, houseFlags(true), "https://stored.test", zap.NewNop())

	// Act
	w := postHouse(t, h, id, url.Values{"std": {"1"}, "period": {"annual"}})

	// Assert
	assert.Equal(t, http.StatusConflict, w.Code)
	assert.Equal(t, "provider_disabled", w.Header().Get("X-Error-Code"))
	assert.Contains(t, w.Body.String(), "switched off")
	assert.Zero(t, fb.liveAsked, "the provider is never asked")
}

func TestResize_ProviderFlagOff_409(t *testing.T) {
	// Arrange: a granted Stripe house, pay_stripe off.
	db := testDashDB(t)
	t.Cleanup(func() { _ = db.Close() })
	id := houseTenant(t, db, 0, 0)
	grant(t, db, id, payments.RenewalAuto)
	fb := newFakeHouseBilling()
	h := HandleHouseCheckout(fb.registry(db, false), db, houseFlags(true), "https://stored.test", zap.NewNop())

	// Act
	w := postHouse(t, h, id, url.Values{"std": {"3"}, "period": {"monthly"}})

	// Assert
	assert.Equal(t, http.StatusConflict, w.Code)
	assert.Equal(t, "provider_disabled", w.Header().Get("X-Error-Code"))
	assert.Equal(t, payments.Order{}, fb.gotOrder, "nothing resized")
	house, _, err := usage.NewQuotaManager(db).GetHouse(context.Background(), id)
	require.NoError(t, err)
	assert.Equal(t, usage.HouseFromTB(2, 0, 0), house)
}

// The double-billing guard reaches the page: a house renewed by another
// provider refuses a Stripe checkout with 409 other_provider_live.
func TestCheckout_OtherProviderLive_409(t *testing.T) {
	db := testDashDB(t)
	t.Cleanup(func() { _ = db.Close() })
	id := houseTenant(t, db, 0, 0)
	grant(t, db, id, payments.RenewalAuto)
	// The row is renewed by btcpay (the only way to seed one today).
	_, err := db.Exec(`UPDATE entitlements SET provider = 'btcpay' WHERE tenant_id = $1`, id)
	require.NoError(t, err)
	fb := newFakeHouseBilling()
	reg := fb.registry(db, true)

	_, err = reg.StartCheckout(context.Background(), payments.ProviderStripe, payments.Checkout{TenantID: id,
		Order: payments.Order{Std: 1, Period: payments.PeriodAnnual}})

	assert.ErrorIs(t, err, payments.ErrOtherProviderLive)
	w := httptest.NewRecorder()
	assert.True(t, refusedForProvider(w, err))
	assert.Equal(t, http.StatusConflict, w.Code)
	assert.Equal(t, "other_provider_live", w.Header().Get("X-Error-Code"))
	assert.Zero(t, fb.liveAsked)
}

// The page renders from the entitlement record: the sentence, and the ways
// to pay that are on.
func TestBillingPage_RendersTheEntitlement(t *testing.T) {
	db := testDashDB(t)
	t.Cleanup(func() { _ = db.Close() })
	id := houseTenant(t, db, 0, 0)
	grant(t, db, id, payments.RenewalPrepaid)

	body := renderBilling(t, newFakeHouseBilling().registry(db, true), id, true)

	assert.Contains(t, body, "via card (Stripe), renewal stopped.")
	assert.Contains(t, body, "You can pay by card (Stripe).")
	assert.Contains(t, body, "Your house") // resize mode from the record
}

// grant gives the tenant a 2 TB monthly Stripe house paid 20 days ahead.
func grant(t *testing.T, db *sql.DB, tenantID string, renewal payments.Renewal) {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, payments.InTx(ctx, db, func(tx *sql.Tx) error {
		return payments.Grant(ctx, tx, tenantID, payments.Order{Std: 2, Period: payments.PeriodMonthly},
			time.Now().Add(20*24*time.Hour), payments.ProviderStripe, "sub_"+tenantID, renewal)
	}))
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM entitlements WHERE tenant_id = $1`, tenantID)
		_, _ = db.Exec(`DELETE FROM tenant_floor_quotas WHERE tenant_id = $1`, tenantID)
	})
}
