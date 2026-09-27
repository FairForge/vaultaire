package handlers

import (
	"context"
	"database/sql"
	"fmt"
	"html/template"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/FairForge/vaultaire/internal/billing"
	"github.com/FairForge/vaultaire/internal/flags"
	"github.com/FairForge/vaultaire/internal/usage"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/stripe/stripe-go/v75"
	"go.uber.org/zap"
)

// Whole-TB checkout on the billing page (dashboard plan, Phase 1), behind
// the quota_checkout flag.

func houseFlags(on bool) *flags.Service {
	svc := flags.New(nil, nil)
	svc.Register(FlagQuotaCheckout, on)
	return svc
}

// houseTenant creates a tenant + quota row for the house page tests.
func houseTenant(t *testing.T, db *sql.DB, intentStd, intentVault int) string {
	t.Helper()
	id := fmt.Sprintf("test-dash-house-%d", time.Now().UnixNano())
	_, err := db.Exec(`INSERT INTO tenants (id, name, email, stripe_customer_id, intent_std_tb, intent_vault_tb, intent_room)
		VALUES ($1, 'House', $2, $3, $4, $5, 'v2.abc')`, id, id+"@test.local", "cus_"+id, intentStd, intentVault)
	require.NoError(t, err)
	require.NoError(t, usage.NewQuotaManager(db).CreateTenant(context.Background(), id, "free", usage.FreeTierLimits.StorageBytes))
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM tenant_quotas WHERE tenant_id = $1`, id)
		_, _ = db.Exec(`DELETE FROM tenants WHERE id = $1`, id)
	})
	return id
}

type fakeHouseBilling struct {
	ready      bool
	why        string
	checkout   string
	gotOrder   billing.HouseOrder
	gotSuccess string
	gotCancel  string
	updated    *stripe.Subscription
	updateErr  error
	svc        *billing.StripeService // for HouseFromSubscription
}

func newFakeHouseBilling() *fakeHouseBilling {
	svc := billing.NewStripeService("sk_test_fake", nil, zap.NewNop())
	svc.ConfigureHouse(billing.HousePriceIDs{StdAnnual: "p_sy", StdMonthly: "p_sm", VaultAnnual: "p_vy",
		VaultMonthly: "p_vm", PinAnnual: "p_py", PinMonthly: "p_pm"}, nil)
	return &fakeHouseBilling{ready: true, checkout: "https://checkout.stripe.test/cs_1", svc: svc}
}
func (f *fakeHouseBilling) HouseCheckoutReady() (bool, string) { return f.ready, f.why }
func (f *fakeHouseBilling) Plans() []billing.Plan              { return nil }
func (f *fakeHouseBilling) GetCustomerID(_ context.Context, tenantID string) (string, error) {
	return "cus_" + tenantID, nil
}
func (f *fakeHouseBilling) CreateCustomer(_ context.Context, _, tenantID string) (string, error) {
	return "cus_" + tenantID, nil
}
func (f *fakeHouseBilling) CreateHouseCheckout(_ context.Context, _, _ string, o billing.HouseOrder, successURL, cancelURL string) (string, error) {
	f.gotOrder, f.gotSuccess, f.gotCancel = o, successURL, cancelURL
	return f.checkout, nil
}
func (f *fakeHouseBilling) UpdateHouseSubscription(_ context.Context, _ string, o billing.HouseOrder) (*stripe.Subscription, error) {
	f.gotOrder = o
	return f.updated, f.updateErr
}
func (f *fakeHouseBilling) HouseFromSubscription(sub *stripe.Subscription) (billing.HouseOrder, bool) {
	return f.svc.HouseFromSubscription(sub)
}

func houseTemplate(t *testing.T) *template.Template {
	t.Helper()
	tmpl := template.Must(template.New("base").Parse(`{{define "base"}}{{block "content" .}}{{end}}{{end}}`))
	template.Must(tmpl.Parse(`{{define "content"}}` +
		`{{if .House.Enabled}}<div class="house" data-ready="{{.House.Ready}}" data-why="{{.House.NotReadyWhy}}">` +
		`std={{.House.Std}} vault={{.House.Vault}} pin={{.House.PinHot}} period={{.House.Period}} ` +
		`has={{.House.HasHouse}} minstd={{.House.MinStd}} minvault={{.House.MinVault}} ` +
		`monthly={{.House.Quote.Monthly}} charge={{.House.Quote.Charge}} room={{.House.RoomURL}}</div>` +
		`{{else}}<div class="legacy">{{.Plan}}</div>{{end}}{{end}}`))
	return tmpl
}

func TestHouse_FlagOffKeepsTheLegacyPage(t *testing.T) {
	db := testDashDB(t)
	t.Cleanup(func() { _ = db.Close() }) // registered first = runs last, after the tenant cleanup
	id := houseTenant(t, db, 6, 1)
	h := HandleBilling(houseTemplate(t), newFakeHouseBilling(), db, houseFlags(false), zap.NewNop())
	req := injectSessionWithTenant(httptest.NewRequest("GET", "/dashboard/billing", nil), id)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	assert.Equal(t, 200, w.Code)
	assert.Contains(t, w.Body.String(), `class="legacy"`)
	assert.NotContains(t, w.Body.String(), `class="house"`)

	// No flag service at all (dev without DB) is the same as off.
	h = HandleBilling(houseTemplate(t), nil, nil, nil, zap.NewNop())
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	assert.Equal(t, 200, w.Code)
	assert.Contains(t, w.Body.String(), `class="legacy"`)
}

func TestHouse_FlagOnPreselectsTheIntent(t *testing.T) {
	db := testDashDB(t)
	t.Cleanup(func() { _ = db.Close() }) // registered first = runs last, after the tenant cleanup
	id := houseTenant(t, db, 6, 1)
	fb := newFakeHouseBilling()
	h := HandleBilling(houseTemplate(t), fb, db, houseFlags(true), zap.NewNop())
	req := injectSessionWithTenant(httptest.NewRequest("GET", "/dashboard/billing", nil), id)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	require.Equal(t, 200, w.Code)
	body := w.Body.String()
	assert.Contains(t, body, `data-ready="true"`)
	assert.Contains(t, body, "std=6 vault=1 pin=0 period=annual has=false")
	assert.Contains(t, body, "monthly=$28.94 charge=$347.28", "the same receipt as the site")
	assert.Contains(t, body, "room=/#room=v2.abc")

	// Prices not verified: the page says so instead of offering checkout.
	fb.ready, fb.why = false, "house prices: price_std_y is 4788 cents per unit, want 5388"
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	assert.Contains(t, w.Body.String(), `data-ready="false"`)
	assert.Contains(t, w.Body.String(), "4788")

	// No Stripe at all.
	h = HandleBilling(houseTemplate(t), nil, db, houseFlags(true), zap.NewNop())
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	assert.Contains(t, w.Body.String(), `data-ready="false"`)
}

func TestHouse_ExistingHousePreselectsItsFloors(t *testing.T) {
	db := testDashDB(t)
	t.Cleanup(func() { _ = db.Close() }) // registered first = runs last, after the tenant cleanup
	id := houseTenant(t, db, 6, 1)
	qm := usage.NewQuotaManager(db)
	require.NoError(t, qm.SetHouse(context.Background(), id, usage.HouseFromTB(3, 2, 1)))
	_, err := db.Exec(`UPDATE tenants SET plan = 'house', subscription_status = 'active', stripe_subscription_id = 'sub_1', house_period = 'monthly' WHERE id = $1`, id)
	require.NoError(t, err)
	// 1.5 TB used downstairs: the stepper cannot go below 2.
	_, err = db.Exec(`UPDATE tenant_floor_quotas SET storage_used_bytes = $1 WHERE tenant_id = $2 AND floor = 'standard'`, 3*usage.TB/2, id)
	require.NoError(t, err)

	h := HandleBilling(houseTemplate(t), newFakeHouseBilling(), db, houseFlags(true), zap.NewNop())
	req := injectSessionWithTenant(httptest.NewRequest("GET", "/dashboard/billing", nil), id)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	require.Equal(t, 200, w.Code)
	body := w.Body.String()
	assert.Contains(t, body, "std=3 vault=2 pin=1 period=monthly has=true minstd=2 minvault=0")
	assert.Contains(t, body, "monthly=$23.07", "3×4.99 + 2×2.55 + 1×3.00")
}

func postHouse(t *testing.T, h http.Handler, tenantID string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "/dashboard/billing/house", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req = injectSessionWithTenant(req, tenantID)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func flashOf(t *testing.T, w *httptest.ResponseRecorder) (category, message string) {
	t.Helper()
	for _, c := range w.Result().Cookies() {
		if c.Name == "flash" {
			v, err := url.ParseQuery(c.Value)
			require.NoError(t, err)
			for k, vals := range v {
				return k, vals[0]
			}
		}
	}
	return "", ""
}

func TestHandleHouseCheckout_StartsCheckoutWithAbsoluteURLs(t *testing.T) {
	db := testDashDB(t)
	t.Cleanup(func() { _ = db.Close() }) // registered first = runs last, after the tenant cleanup
	id := houseTenant(t, db, 0, 0)
	fb := newFakeHouseBilling()
	h := HandleHouseCheckout(fb, db, nil, houseFlags(true), "https://stored.ge", zap.NewNop())

	w := postHouse(t, h, id, url.Values{"std": {"0"}, "vault": {"2"}, "period": {"annual"}})
	require.Equal(t, http.StatusSeeOther, w.Code)
	assert.Equal(t, "https://checkout.stripe.test/cs_1", w.Header().Get("Location"))
	assert.Equal(t, billing.HouseOrder{Vault: 2, Period: billing.PeriodAnnual}, fb.gotOrder, "attic-only is sold")
	assert.Equal(t, "https://stored.ge/dashboard/billing?upgraded=1", fb.gotSuccess)
	assert.Equal(t, "https://stored.ge/dashboard/billing", fb.gotCancel)

	// Junk → back to the page with the reason.
	w = postHouse(t, h, id, url.Values{"std": {"0"}, "vault": {"0"}, "period": {"annual"}})
	require.Equal(t, http.StatusSeeOther, w.Code)
	assert.Equal(t, "/dashboard/billing", w.Header().Get("Location"))
	cat, msg := flashOf(t, w)
	assert.Equal(t, "error", cat)
	assert.Contains(t, msg, "empty")

	w = postHouse(t, h, id, url.Values{"std": {"1"}, "pin_hot": {"2"}, "period": {"monthly"}})
	cat, _ = flashOf(t, w)
	assert.Equal(t, "error", cat)

	// Not ready → no checkout, honest message.
	fb.ready, fb.why = false, "house prices not verified yet"
	w = postHouse(t, h, id, url.Values{"std": {"1"}, "period": {"annual"}})
	require.Equal(t, http.StatusSeeOther, w.Code)
	assert.Equal(t, "/dashboard/billing", w.Header().Get("Location"))
	cat, msg = flashOf(t, w)
	assert.Equal(t, "error", cat)
	assert.Contains(t, msg, "isn't open")

	// Flag off → not found.
	h = HandleHouseCheckout(fb, db, nil, houseFlags(false), "https://stored.ge", zap.NewNop())
	w = postHouse(t, h, id, url.Values{"std": {"1"}, "period": {"annual"}})
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestHandleHouseCheckout_ResizeAppliesQuotasImmediately(t *testing.T) {
	db := testDashDB(t)
	t.Cleanup(func() { _ = db.Close() }) // registered first = runs last, after the tenant cleanup
	id := houseTenant(t, db, 0, 0)
	qm := usage.NewQuotaManager(db)
	require.NoError(t, qm.SetHouse(context.Background(), id, usage.HouseFromTB(3, 2, 0)))
	_, err := db.Exec(`UPDATE tenants SET plan = 'house', subscription_status = 'active', stripe_subscription_id = 'sub_1', house_period = 'annual' WHERE id = $1`, id)
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE tenant_floor_quotas SET storage_used_bytes = $1 WHERE tenant_id = $2 AND floor = 'vault'`, usage.TB+1, id)
	require.NoError(t, err)

	fb := newFakeHouseBilling()
	fb.updated = &stripe.Subscription{ID: "sub_1", Status: "active", Items: &stripe.SubscriptionItemList{Data: []*stripe.SubscriptionItem{
		{ID: "si_1", Price: &stripe.Price{ID: "p_sy"}, Quantity: 5},
		{ID: "si_2", Price: &stripe.Price{ID: "p_vy"}, Quantity: 2},
	}}}
	h := HandleHouseCheckout(fb, db, qm, houseFlags(true), "https://stored.ge", zap.NewNop())

	// Below usage: the attic holds just over 1 TB, so it cannot shrink to 1.
	w := postHouse(t, h, id, url.Values{"std": {"5"}, "vault": {"1"}, "period": {"annual"}})
	require.Equal(t, http.StatusSeeOther, w.Code)
	cat, msg := flashOf(t, w)
	assert.Equal(t, "error", cat)
	assert.Contains(t, msg, "attic")
	house, _, _ := qm.GetHouse(context.Background(), id)
	assert.Equal(t, usage.HouseFromTB(3, 2, 0), house, "nothing changed")

	// A period switch is refused up front.
	w = postHouse(t, h, id, url.Values{"std": {"5"}, "vault": {"2"}, "period": {"monthly"}})
	cat, msg = flashOf(t, w)
	assert.Equal(t, "error", cat)
	assert.Contains(t, msg, "yearly")

	w = postHouse(t, h, id, url.Values{"std": {"5"}, "vault": {"2"}, "period": {"annual"}})
	require.Equal(t, http.StatusSeeOther, w.Code)
	assert.Equal(t, "/dashboard/billing?resized=1", w.Header().Get("Location"))
	assert.Equal(t, billing.HouseOrder{Std: 5, Vault: 2, Period: billing.PeriodAnnual}, fb.gotOrder)
	house, _, _ = qm.GetHouse(context.Background(), id)
	assert.Equal(t, usage.HouseFromTB(5, 2, 0), house, "applied from the returned subscription, before the webhook")
	cat, msg = flashOf(t, w)
	assert.Equal(t, "success", cat)
	assert.Contains(t, msg, "5 TB downstairs")
}

func TestOnboarding_PlanWaitingStep(t *testing.T) {
	db := testDashDB(t)
	t.Cleanup(func() { _ = db.Close() }) // registered first = runs last, after the tenant cleanup
	id := houseTenant(t, db, 2, 0)
	data := map[string]any{}
	req := httptest.NewRequest("GET", "/dashboard/", nil)
	populateOnboarding(context.Background(), db, id, req, data)
	st, ok := data["Onboarding"].(*OnboardingStatus)
	require.True(t, ok)
	assert.True(t, st.PlanWaiting, "a house was built on the site and nothing is bought yet")
	assert.Equal(t, "2 TB downstairs", st.PlanSummary)

	_, err := db.Exec(`UPDATE tenants SET plan = 'house', subscription_status = 'active' WHERE id = $1`, id)
	require.NoError(t, err)
	data = map[string]any{}
	populateOnboarding(context.Background(), db, id, req, data)
	st = data["Onboarding"].(*OnboardingStatus)
	assert.False(t, st.PlanWaiting)
}
