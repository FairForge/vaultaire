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

	"github.com/FairForge/vaultaire/internal/flags"
	"github.com/FairForge/vaultaire/internal/payments"
	"github.com/FairForge/vaultaire/internal/usage"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

// fakeHouseBilling is a Stripe-named payments.Provider; registry puts it
// behind a real payments.Registry, so the pages exercise the flag, the
// double-billing guard and the record writes end to end.
type fakeHouseBilling struct {
	ready      bool
	why        string
	live       bool  // StartCheckout answers ErrLiveSubscription
	liveErr    error // ...or this error
	liveAsked  int
	checkout   string
	gotOrder   payments.Order
	gotPurpose string
	gotSuccess string
	gotCancel  string
	gotStartAt time.Time
	resizeErr  error
}

func newFakeHouseBilling() *fakeHouseBilling {
	return &fakeHouseBilling{ready: true, checkout: "https://checkout.stripe.test/cs_1"}
}

// registry is the payments.Registry the handlers get, with pay_stripe on
// or off for every tenant.
func (f *fakeHouseBilling) registry(db *sql.DB, payStripe bool) *payments.Registry {
	fl := flags.New(nil, nil)
	fl.Register(payments.FlagPrefix+payments.ProviderStripe, payStripe)
	reg := payments.NewRegistry(db, fl, 0, nil)
	reg.Register(f)
	return reg
}

func (f *fakeHouseBilling) Name() string              { return payments.ProviderStripe }
func (f *fakeHouseBilling) Renewal() payments.Renewal { return payments.RenewalAuto }
func (f *fakeHouseBilling) Ready(context.Context) (payments.Readiness, string) {
	if f.ready {
		return payments.Ready, ""
	}
	return payments.Unready, f.why
}
func (f *fakeHouseBilling) StartCheckout(_ context.Context, c payments.Checkout) (string, error) {
	f.liveAsked++
	if f.liveErr != nil {
		return "", f.liveErr
	}
	if f.live {
		return "", payments.ErrLiveSubscription
	}
	f.gotOrder, f.gotPurpose, f.gotSuccess, f.gotCancel, f.gotStartAt = c.Order, c.Purpose, c.ReturnURL, c.CancelURL, c.StartAt
	return f.checkout, nil
}
func (f *fakeHouseBilling) Resize(_ context.Context, _ string, o payments.Order) (payments.ResizeOutcome, error) {
	f.gotOrder = o
	if f.resizeErr != nil {
		return payments.ResizeOutcome{}, f.resizeErr
	}
	return payments.ResizeOutcome{Applied: &o}, nil
}
func (f *fakeHouseBilling) StopRenewal(context.Context, string) error   { return nil }
func (f *fakeHouseBilling) ResumeRenewal(context.Context, string) error { return nil }
func (f *fakeHouseBilling) Cancel(context.Context, string) error        { return nil }
func (f *fakeHouseBilling) ManageURL(context.Context, string, string) (string, error) {
	return "https://billing.stripe.test/portal", nil
}
func (f *fakeHouseBilling) Webhook() http.Handler { return nil }

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
	h := HandleBilling(houseTemplate(t), newFakeHouseBilling().registry(db, true), db, houseFlags(false), zap.NewNop())
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
	h := HandleBilling(houseTemplate(t), fb.registry(db, true), db, houseFlags(true), zap.NewNop())
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
	grantedHouse(t, db, id, payments.Order{Std: 3, Vault: 2, PinHot: 1, Period: payments.PeriodMonthly}, time.Now().Add(20*24*time.Hour))
	// 1.5 TB used downstairs: the stepper cannot go below 2.
	_, err := db.Exec(`UPDATE tenant_floor_quotas SET storage_used_bytes = $1 WHERE tenant_id = $2 AND floor = 'standard'`, 3*usage.TB/2, id)
	require.NoError(t, err)

	h := HandleBilling(houseTemplate(t), newFakeHouseBilling().registry(db, true), db, houseFlags(true), zap.NewNop())
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
	h := HandleHouseCheckout(fb.registry(db, true), db, houseFlags(true), "https://stored.ge", zap.NewNop())

	w := postHouse(t, h, id, url.Values{"std": {"0"}, "vault": {"2"}, "period": {"annual"}})
	require.Equal(t, http.StatusSeeOther, w.Code)
	assert.Equal(t, "https://checkout.stripe.test/cs_1", w.Header().Get("Location"))
	assert.Equal(t, payments.Order{Vault: 2, Period: payments.PeriodAnnual}, fb.gotOrder, "attic-only is sold")
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
	h = HandleHouseCheckout(fb.registry(db, true), db, houseFlags(false), "https://stored.ge", zap.NewNop())
	w = postHouse(t, h, id, url.Values{"std": {"1"}, "period": {"annual"}})
	assert.Equal(t, http.StatusNotFound, w.Code)
}

// A second Checkout while Stripe still bills a subscription would open a
// second subscription (R10-05): refused from the local row when it says so,
// and from Stripe when the webhook has not landed yet.
func TestHandleHouseCheckout_RefusesASecondSubscription(t *testing.T) {
	db := testDashDB(t)
	t.Cleanup(func() { _ = db.Close() }) // registered first = runs last, after the tenant cleanup
	id := houseTenant(t, db, 0, 0)
	fb := newFakeHouseBilling()
	h := HandleHouseCheckout(fb.registry(db, true), db, houseFlags(true), "https://stored.ge", zap.NewNop())
	order := url.Values{"std": {"2"}, "period": {"annual"}}

	// 1. The webhook has not landed (no local subscription yet) but Stripe
	//    already has one: refused, nothing created.
	fb.live = true
	w := postHouse(t, h, id, order)
	require.Equal(t, http.StatusSeeOther, w.Code)
	assert.Equal(t, "/dashboard/billing", w.Header().Get("Location"))
	cat, msg := flashOf(t, w)
	assert.Equal(t, "error", cat)
	assert.Contains(t, msg, "already have a subscription")
	assert.Equal(t, 1, fb.liveAsked)
	assert.Equal(t, payments.Order{}, fb.gotOrder, "no checkout started")

	// 2. Stripe cannot be asked: refused too (never guess on a money path).
	fb.live, fb.liveErr = false, fmt.Errorf("stripe: 503")
	w = postHouse(t, h, id, order)
	cat, _ = flashOf(t, w)
	assert.Equal(t, "error", cat)
	assert.Equal(t, payments.Order{}, fb.gotOrder)

	// 3. A legacy pack label (a pack bought before, still billing or not —
	//    the local row no longer says; WP-PAY-7): one product (WP-PAY-0),
	//    support moves it, refused before Stripe is even asked.
	fb.liveErr = nil
	asked := fb.liveAsked
	_, err := db.Exec(`UPDATE tenants SET plan = 'vault18' WHERE id = $1`, id)
	require.NoError(t, err)
	w = postHouse(t, h, id, order)
	cat, msg = flashOf(t, w)
	assert.Equal(t, "error", cat)
	assert.Contains(t, msg, "legacy plan")
	assert.Equal(t, asked, fb.liveAsked, "refused from the local row")
	assert.Equal(t, payments.Order{}, fb.gotOrder)

	// 5. Downgraded to starter (what customer.subscription.deleted writes):
	//    a new house may be bought.
	_, err = db.Exec(`UPDATE tenants SET plan = 'starter' WHERE id = $1`, id)
	require.NoError(t, err)
	w = postHouse(t, h, id, order)
	require.Equal(t, http.StatusSeeOther, w.Code)
	assert.Equal(t, "https://checkout.stripe.test/cs_1", w.Header().Get("Location"))
	assert.Equal(t, payments.Order{Std: 2, Period: payments.PeriodAnnual}, fb.gotOrder)
}

func TestHandleHouseCheckout_ResizeAppliesQuotasImmediately(t *testing.T) {
	db := testDashDB(t)
	t.Cleanup(func() { _ = db.Close() }) // registered first = runs last, after the tenant cleanup
	id := houseTenant(t, db, 0, 0)
	qm := usage.NewQuotaManager(db)
	grantedHouseVia(t, db, id, payments.Order{Std: 3, Vault: 2, Period: payments.PeriodAnnual}, time.Now().Add(200*24*time.Hour),
		payments.ProviderStripe, "sub_1", payments.RenewalAuto)
	_, err := db.Exec(`UPDATE tenant_floor_quotas SET storage_used_bytes = $1 WHERE tenant_id = $2 AND floor = 'vault'`, usage.TB+1, id)
	require.NoError(t, err)

	fb := newFakeHouseBilling()
	h := HandleHouseCheckout(fb.registry(db, true), db, houseFlags(true), "https://stored.ge", zap.NewNop())

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
	assert.Equal(t, payments.Order{Std: 5, Vault: 2, Period: payments.PeriodAnnual}, fb.gotOrder)
	house, _, _ = qm.GetHouse(context.Background(), id)
	assert.Equal(t, usage.HouseFromTB(5, 2, 0), house, "applied from the provider's answer, before the webhook")
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

	grantedHouse(t, db, id, payments.Order{Std: 2, Period: payments.PeriodMonthly}, time.Now().Add(20*24*time.Hour))
	data = map[string]any{}
	populateOnboarding(context.Background(), db, id, req, data)
	st = data["Onboarding"].(*OnboardingStatus)
	assert.False(t, st.PlanWaiting)
}
