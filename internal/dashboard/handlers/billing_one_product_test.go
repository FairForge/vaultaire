package handlers

import (
	"context"
	"html/template"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/FairForge/vaultaire/internal/payments"
	"github.com/FairForge/vaultaire/internal/usage"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// WP-PAY-0 (= WP-R10-4, decision F-29): one product. The legacy packs and
// the metered estimate are gone; a tenant still labelled with a pack plan
// (Bench2 on prod: tenants.plan = 'vault100', no Stripe) is told to ask
// support instead of being offered a second product.

func realBillingTemplate(t *testing.T) *template.Template {
	t.Helper()
	base := template.Must(template.New("").Funcs(TemplateFuncs()).ParseFS(os.DirFS("../templates"), "layouts/base.html"))
	tmpl := template.Must(base.Clone())
	template.Must(tmpl.ParseFS(os.DirFS("../templates"), "customer/billing.html"))
	return tmpl
}

func renderBilling(t *testing.T, svc BillingService, tenantID string, on bool) string {
	t.Helper()
	db := testDashDB(t)
	t.Cleanup(func() { _ = db.Close() })
	h := HandleBilling(realBillingTemplate(t), svc, db, houseFlags(on), zap.NewNop())
	w := httptest.NewRecorder()
	h.ServeHTTP(w, injectSessionWithTenant(httptest.NewRequest("GET", "/dashboard/billing", nil), tenantID))
	require.Equal(t, 200, w.Code)
	return w.Body.String()
}

func TestBillingPage_NoAccruedEstimate(t *testing.T) {
	// Arrange: a tenant on the old metered 'standard' tier holding 2 TB.
	db := testDashDB(t)
	t.Cleanup(func() { _ = db.Close() })
	id := houseTenant(t, db, 0, 0)
	_, err := db.Exec(`UPDATE tenant_quotas SET tier = 'standard', storage_used_bytes = $2 WHERE tenant_id = $1`, id, 2*usage.TB)
	require.NoError(t, err)

	// Act
	body := renderBilling(t, nil, id, false)

	// Assert: no "≈ $X this month", no pack grid, no upgrade form.
	assert.NotContains(t, body, "Accrued This Month")
	assert.NotContains(t, body, "metered usage")
	assert.NotContains(t, body, "/dashboard/billing/upgrade")
	assert.NotContains(t, body, "Choose Plan")
}

func TestBillingPage_PackPlanTenant_ShownLiveOther(t *testing.T) {
	// Arrange: seeded the way the admin tier handler left Bench2.
	db := testDashDB(t)
	t.Cleanup(func() { _ = db.Close() })
	id := houseTenant(t, db, 0, 0)
	_, err := db.Exec(`UPDATE tenants SET plan = 'vault100' WHERE id = $1`, id)
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE tenant_quotas SET tier = 'vault100', storage_limit_bytes = $2 WHERE tenant_id = $1`, id, usage.TB)
	require.NoError(t, err)

	// Act: with the house checkout on and off.
	on := renderBilling(t, newFakeHouseBilling().registry(nil, true), id, true)
	off := renderBilling(t, nil, id, false)

	// Assert: both say to email support; neither offers a checkout.
	for _, body := range []string{on, off} {
		assert.Contains(t, body, "legacy plan")
		assert.Contains(t, body, "vault100")
		assert.NotContains(t, body, `action="/dashboard/billing/house"`)
	}
}

func TestHouseCheckout_PackPlanTenant_Refused(t *testing.T) {
	db := testDashDB(t)
	t.Cleanup(func() { _ = db.Close() })
	id := houseTenant(t, db, 0, 0)
	_, err := db.Exec(`UPDATE tenants SET plan = 'vault100' WHERE id = $1`, id)
	require.NoError(t, err)
	fb := newFakeHouseBilling()

	w := postHouse(t, HandleHouseCheckout(fb.registry(db, true), db, houseFlags(true), "https://stored.test", zap.NewNop()), id,
		map[string][]string{"std": {"2"}, "vault": {"0"}, "pin_hot": {"0"}, "period": {"annual"}})

	cat, msg := flashOf(t, w)
	assert.Equal(t, "error", cat)
	assert.Contains(t, msg, "legacy plan")
	assert.Zero(t, fb.liveAsked, "no Stripe call for a pack-plan tenant")
	assert.Empty(t, fb.gotSuccess, "no checkout started")
}

// The admin revenue page prices houses only: billing.QuoteHouse at the
// tenant's period. A free tenant and a 'vault100' tenant without Stripe
// count 0 (queryHouses is scoped to this test's tenants: the DB is shared).
func TestAdminRevenue_HousesOnly(t *testing.T) {
	// Arrange
	db := testDashDB(t)
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	house, free, pack := houseTenant(t, db, 0, 0), houseTenant(t, db, 0, 0), houseTenant(t, db, 0, 0)
	grantedHouse(t, db, house, payments.Order{Std: 3, Vault: 2, PinHot: 1, Period: payments.PeriodMonthly}, time.Now().Add(20*24*time.Hour))
	_, err := db.Exec(`UPDATE tenants SET plan = 'vault100' WHERE id = $1`, pack)
	require.NoError(t, err)
	mine := map[string]bool{house: true, free: true, pack: true}

	// Act
	var got []houseRevenue
	for _, h := range queryHouses(ctx, db, zap.NewNop()) {
		if mine[h.tenantID] {
			got = append(got, h)
		}
	}

	// Assert
	require.Len(t, got, 1, "only the house tenant earns")
	want := payments.QuoteHouse(3, 2, 1, payments.PeriodMonthly)
	assert.Equal(t, house, got[0].tenantID)
	assert.Equal(t, want.MonthlyCents, got[0].quote.MonthlyCents)
}
