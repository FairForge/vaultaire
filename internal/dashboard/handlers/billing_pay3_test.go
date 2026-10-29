package handlers

import (
	"context"
	"database/sql"
	"fmt"
	"html/template"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	dashauth "github.com/FairForge/vaultaire/internal/dashboard/auth"
	"github.com/FairForge/vaultaire/internal/hosts"
	"github.com/FairForge/vaultaire/internal/payments"
	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// WP-PAY-3 (Stage 1a Prompt 20): the receipt page, the receipts list and
// the Renew button on the billing page, the provider refusals as a page,
// the admin tenant page's Billing panel.

func manualPaid(t *testing.T, db *sql.DB, tenantID string, period payments.Period) payments.ManualPaymentResult {
	t.Helper()
	res, err := payments.RecordManualPayment(context.Background(), db, payments.ManualPayment{TenantID: tenantID, AmountCents: 5388,
		Period: period, Order: &payments.Order{Std: 1, Period: period}, Actor: "admin", Reason: "test"})
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM audit_logs WHERE tenant_id = $1`, tenantID)
		_, _ = db.Exec(`DELETE FROM payments WHERE tenant_id = $1`, tenantID)
		_, _ = db.Exec(`DELETE FROM entitlements WHERE tenant_id = $1`, tenantID)
		_, _ = db.Exec(`DELETE FROM tenant_floor_quotas WHERE tenant_id = $1`, tenantID)
	})
	return res
}

func receiptTemplate(t *testing.T) *template.Template {
	t.Helper()
	return template.Must(template.New("").Funcs(TemplateFuncs()).ParseFS(os.DirFS("../templates"), "customer/receipt.html"))
}

func receiptRouter(db *sql.DB) http.Handler {
	r := chi.NewRouter()
	r.Get("/dashboard/billing/receipts/{id}", HandleReceipt(nil, db, hosts.Defaults(8000).Legal(), zap.NewNop()))
	return r
}

func TestReceiptPage_RendersFromLedger(t *testing.T) {
	db := testDashDB(t)
	t.Cleanup(func() { _ = db.Close() })
	id := houseTenant(t, db, 0, 0)
	paid := manualPaid(t, db, id, payments.PeriodAnnual)
	r := chi.NewRouter()
	r.Get("/dashboard/billing/receipts/{id}", HandleReceipt(receiptTemplate(t), db, hosts.Defaults(8000).Legal(), zap.NewNop()))

	w := httptest.NewRecorder()
	r.ServeHTTP(w, injectSessionWithTenant(httptest.NewRequest("GET", "/dashboard/billing/receipts/"+paid.PaymentID, nil), id))

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	body := w.Body.String()
	for _, want := range []string{paid.InvoiceNumber, "FairForge LLC", id + "@test.local", "$53.88", "Standard", "1 TB", "Paid",
		"State of Utah", "@media print"} {
		assert.Contains(t, body, want)
	}
	assert.Contains(t, w.Header().Get("Content-Type"), "text/html")
	assert.Equal(t, "no-store", w.Header().Get("Cache-Control"))
}

func TestReceiptPage_OtherTenant_404(t *testing.T) {
	db := testDashDB(t)
	t.Cleanup(func() { _ = db.Close() })
	mine := houseTenant(t, db, 0, 0)
	other := houseTenant(t, db, 0, 0)
	paid := manualPaid(t, db, mine, payments.PeriodAnnual)
	r := receiptRouter(db)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, injectSessionWithTenant(httptest.NewRequest("GET", "/dashboard/billing/receipts/"+paid.PaymentID, nil), other))
	assert.Equal(t, http.StatusNotFound, w.Code, "the tenant comes from the session, never the URL")

	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/dashboard/billing/receipts/"+paid.PaymentID, nil))
	assert.Equal(t, http.StatusSeeOther, w.Code, "no session: login")
}

func TestBillingPage_ReceiptsListAndRenewButton(t *testing.T) {
	db := testDashDB(t)
	t.Cleanup(func() { _ = db.Close() })
	render := func(id string, fb *fakeHouseBilling, payStripe bool) string {
		t.Helper()
		h := HandleBilling(realBillingTemplate(t), fb.registry(db, payStripe), db, houseFlags(false), zap.NewNop())
		w := httptest.NewRecorder()
		h.ServeHTTP(w, injectSessionWithTenant(httptest.NewRequest("GET", "/dashboard/billing", nil), id))
		require.Equal(t, 200, w.Code)
		return w.Body.String()
	}

	// Free: no button, no receipts.
	free := houseTenant(t, db, 0, 0)
	body := render(free, newFakeHouseBilling(), true)
	assert.NotContains(t, body, `action="/dashboard/billing/renew"`)
	assert.NotContains(t, body, "Receipts")

	// Prepaid (a manual payment) inside the renew window (RP24-1: ≤ 14 d
	// before the date): the receipt listed, Renew offered on the enabled
	// provider.
	prepaid := houseTenant(t, db, 0, 0)
	paid := manualPaid(t, db, prepaid, payments.PeriodAnnual)
	_, err0 := db.Exec(`UPDATE entitlements SET paid_through = NOW() + INTERVAL '10 days' WHERE tenant_id = $1`, prepaid)
	require.NoError(t, err0)
	body = render(prepaid, newFakeHouseBilling(), true)
	assert.Contains(t, body, paid.InvoiceNumber)
	assert.Contains(t, body, "/dashboard/billing/receipts/"+paid.PaymentID)
	assert.Contains(t, body, `action="/dashboard/billing/renew"`)
	assert.Contains(t, body, "Add a year")
	assert.Contains(t, body, "renewal stopped")
	// … but not when no provider may take the money.
	body = render(prepaid, newFakeHouseBilling(), false)
	assert.NotContains(t, body, `action="/dashboard/billing/renew"`)

	// Lapsed: Renew offered (RP21-2's way out).
	lapsed := houseTenant(t, db, 0, 0)
	manualPaid(t, db, lapsed, payments.PeriodMonthly)
	_, err := db.Exec(`UPDATE entitlements SET lapsed_at = NOW(), paid_through = NOW() - INTERVAL '20 days' WHERE tenant_id = $1`, lapsed)
	require.NoError(t, err)
	body = render(lapsed, newFakeHouseBilling(), true)
	assert.Contains(t, body, `action="/dashboard/billing/renew"`)
	assert.Contains(t, body, "Renew now")

	// Auto (a live Stripe subscription): no button.
	auto := houseTenant(t, db, 0, 0)
	grant(t, db, auto, payments.RenewalAuto)
	body = render(auto, newFakeHouseBilling(), true)
	assert.NotContains(t, body, `action="/dashboard/billing/renew"`)
}

// RP24-1: the Renew button shows only inside the reminder window — within
// 14 days of the date, in grace, or lapsed. "Add a year" on a freshly paid
// row is WP-PAY-5's switch, not a renew.
func TestBillingPage_RenewOnlyInsideTheWindow(t *testing.T) {
	db := testDashDB(t)
	t.Cleanup(func() { _ = db.Close() })
	id := houseTenant(t, db, 0, 0)
	manualPaid(t, db, id, payments.PeriodAnnual) // paid through a year out
	render := func() string {
		t.Helper()
		h := HandleBilling(realBillingTemplate(t), newFakeHouseBilling().registry(db, true), db, houseFlags(false), zap.NewNop())
		w := httptest.NewRecorder()
		h.ServeHTTP(w, injectSessionWithTenant(httptest.NewRequest("GET", "/dashboard/billing", nil), id))
		require.Equal(t, 200, w.Code)
		return w.Body.String()
	}

	// A year out: the sentence, no button.
	body := render()
	assert.Contains(t, body, "renewal stopped")
	assert.NotContains(t, body, `action="/dashboard/billing/renew"`, "outside the window the switch is WP-PAY-5's")

	// 10 days out: the button.
	_, err := db.Exec(`UPDATE entitlements SET paid_through = NOW() + INTERVAL '10 days' WHERE tenant_id = $1`, id)
	require.NoError(t, err)
	body = render()
	assert.Contains(t, body, `action="/dashboard/billing/renew"`)
	assert.Contains(t, body, "Add a year")

	// In grace (date passed, row granted): "Renew now".
	_, err = db.Exec(`UPDATE entitlements SET paid_through = NOW() - INTERVAL '2 days' WHERE tenant_id = $1`, id)
	require.NoError(t, err)
	body = render()
	assert.Contains(t, body, `action="/dashboard/billing/renew"`)
	assert.Contains(t, body, "Renew now")

	// And the POST outside the window is refused with a flash.
	_, err = db.Exec(`UPDATE entitlements SET paid_through = NOW() + INTERVAL '300 days' WHERE tenant_id = $1`, id)
	require.NoError(t, err)
	fb := newFakeHouseBilling()
	h := HandleRenewHouse(fb.registry(db, true), db, "https://stored.test", zap.NewNop())
	w := httptest.NewRecorder()
	h.ServeHTTP(w, injectSessionWithTenant(httptest.NewRequest("POST", "/dashboard/billing/renew", nil), id))
	assert.Equal(t, http.StatusSeeOther, w.Code)
	cat, _ := flashOf(t, w)
	assert.Equal(t, "error", cat)
	assert.Zero(t, fb.gotPurpose, "no checkout started outside the window")
}

// RP24-5: a provider form value that is not an enabled provider is a flash
// refusal logged at Warn — never Registry.For's ErrUnknownProvider at Error
// with a user-chosen string.
func TestRenew_UnknownProviderForm_IsAFlashNotAnErrorLine(t *testing.T) {
	db := testDashDB(t)
	t.Cleanup(func() { _ = db.Close() })
	id := houseTenant(t, db, 0, 0)
	manualPaid(t, db, id, payments.PeriodAnnual)
	_, err := db.Exec(`UPDATE entitlements SET paid_through = NOW() + INTERVAL '10 days' WHERE tenant_id = $1`, id)
	require.NoError(t, err)
	core, logs := observer.New(zapcore.WarnLevel)
	fb := newFakeHouseBilling()
	h := HandleRenewHouse(fb.registry(db, true), db, "https://stored.test", zap.New(core))

	form := url.Values{"provider": {"'; DROP TABLE payments; --"}}
	req := httptest.NewRequest("POST", "/dashboard/billing/renew", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, injectSessionWithTenant(req, id))

	assert.Equal(t, http.StatusSeeOther, w.Code)
	cat, _ := flashOf(t, w)
	assert.Equal(t, "error", cat)
	assert.Zero(t, fb.gotPurpose, "no checkout was started")
	for _, e := range logs.All() {
		assert.Less(t, e.Level, zapcore.ErrorLevel, "logged at Warn, never Error: %s", e.Message)
		for _, f := range e.Context {
			assert.NotContains(t, fmt.Sprint(f.String), "DROP TABLE", "the user's string is not echoed into the log")
		}
	}
	warned := false
	for _, e := range logs.All() {
		if e.Level == zapcore.WarnLevel {
			warned = true
		}
	}
	assert.True(t, warned, "the refusal is visible at Warn")

	// A provider the registry DOES enable may be named.
	form = url.Values{"provider": {"stripe"}}
	req = httptest.NewRequest("POST", "/dashboard/billing/renew", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, injectSessionWithTenant(req, id))
	assert.Equal(t, http.StatusSeeOther, w.Code)
	assert.Equal(t, fb.checkout, w.Header().Get("Location"))
}

func TestRenew_StartsCheckoutForTheRowsHouse(t *testing.T) {
	db := testDashDB(t)
	t.Cleanup(func() { _ = db.Close() })
	id := houseTenant(t, db, 0, 0)
	manualPaid(t, db, id, payments.PeriodAnnual)
	paidThrough := time.Now().Add(10 * 24 * time.Hour).Truncate(time.Second).UTC()
	_, err := db.Exec(`UPDATE entitlements SET paid_through = $2 WHERE tenant_id = $1`, id, paidThrough)
	require.NoError(t, err)
	fb := newFakeHouseBilling()
	h := HandleRenewHouse(fb.registry(db, true), db, "https://stored.test", zap.NewNop())

	req := httptest.NewRequest("POST", "/dashboard/billing/renew", strings.NewReader(url.Values{}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, injectSessionWithTenant(req, id))

	assert.Equal(t, http.StatusSeeOther, w.Code)
	assert.Equal(t, fb.checkout, w.Header().Get("Location"))
	assert.Equal(t, payments.PurposeRenew, fb.gotPurpose)
	assert.Equal(t, payments.Order{Std: 1, Period: payments.PeriodAnnual}, fb.gotOrder, "the row's house, not a form's")
	assert.Equal(t, "https://stored.test/dashboard/billing?renewed=1", fb.gotSuccess)
	assert.True(t, paidThrough.Equal(fb.gotStartAt), "a granted row's first charge lands on its paid_through (RP24-1), got %s", fb.gotStartAt)

	// A house a provider renews by itself has nothing to renew by hand.
	auto := houseTenant(t, db, 0, 0)
	grant(t, db, auto, payments.RenewalAuto)
	w = httptest.NewRecorder()
	req = httptest.NewRequest("POST", "/dashboard/billing/renew", nil)
	h.ServeHTTP(w, injectSessionWithTenant(req, auto))
	assert.Equal(t, http.StatusSeeOther, w.Code)
	cat, _ := flashOf(t, w)
	assert.Equal(t, "error", cat)
}

// The two provider refusals keep 409 + X-Error-Code (WP-PAY-2's tests) and
// render the page's error look instead of plain text.
func TestProviderRefusal_RendersPage(t *testing.T) {
	db := testDashDB(t)
	t.Cleanup(func() { _ = db.Close() })
	id := houseTenant(t, db, 0, 0)
	fb := newFakeHouseBilling()
	h := HandleHouseCheckout(fb.registry(db, false), db, houseFlags(true), "https://stored.test", zap.NewNop())

	w := postHouse(t, h, id, url.Values{"std": {"1"}, "period": {"annual"}})

	assert.Equal(t, http.StatusConflict, w.Code)
	assert.Equal(t, "provider_disabled", w.Header().Get("X-Error-Code"))
	assert.Contains(t, w.Header().Get("Content-Type"), "text/html")
	body := w.Body.String()
	assert.Contains(t, body, "<!DOCTYPE html>")
	assert.Contains(t, body, "switched off")
	assert.Contains(t, body, "/dashboard/billing")
	assert.NotContains(t, body, "provider_disabled:", "no raw code in the page")
}

// --- the admin Billing panel ---

func adminReq(req *http.Request) *http.Request {
	sd := &dashauth.SessionData{UserID: "00000000-0000-4000-8000-00000000ad01", TenantID: "admin-tenant", Email: "admin@stored.test", Role: "admin"}
	return req.WithContext(context.WithValue(req.Context(), dashauth.SessionKey, sd))
}

func TestAdminBillingPanel_OnTheTenantPage(t *testing.T) {
	db := testDashDB(t)
	t.Cleanup(func() { _ = db.Close() })
	id := houseTenant(t, db, 0, 0)
	paid := manualPaid(t, db, id, payments.PeriodAnnual)
	tmpl := template.Must(template.New("admin").Parse(`{{define "admin"}}{{block "content" .}}{{end}}{{end}}`))
	template.Must(tmpl.Parse(`{{define "content"}}<p class="sentence">{{.BillingSentence}}</p>{{range .BillingPayments}}<span class="row">{{.InvoiceNumber}} {{.Status}} {{.Amount}}</span>{{end}}{{end}}`))
	r := chi.NewRouter()
	r.Get("/admin/tenants/{id}", HandleTenantDetail(tmpl, db, zap.NewNop(), nil))

	w := httptest.NewRecorder()
	r.ServeHTTP(w, adminReq(httptest.NewRequest("GET", "/admin/tenants/"+id, nil)))

	require.Equal(t, 200, w.Code)
	assert.Contains(t, w.Body.String(), "renewal stopped")
	assert.Contains(t, w.Body.String(), paid.InvoiceNumber+" settled $53.88")
}

func TestAdminBillingPanel_ManualPaymentForm(t *testing.T) {
	db := testDashDB(t)
	t.Cleanup(func() { _ = db.Close() })
	id := houseTenant(t, db, 0, 0)
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM audit_logs WHERE tenant_id = $1`, id)
		_, _ = db.Exec(`DELETE FROM payments WHERE tenant_id = $1`, id)
		_, _ = db.Exec(`DELETE FROM entitlements WHERE tenant_id = $1`, id)
		_, _ = db.Exec(`DELETE FROM tenant_floor_quotas WHERE tenant_id = $1`, id)
	})
	r := chi.NewRouter()
	r.Post("/admin/tenants/{id}/billing/payment", HandleAdminManualPayment(db, zap.NewNop()))
	r.Post("/admin/tenants/{id}/billing/entitlement", HandleAdminAdjustEntitlement(db, zap.NewNop()))
	r.Post("/admin/tenants/{id}/billing/refund", HandleAdminRefund(db, zap.NewNop()))
	post := func(path string, form url.Values) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", path, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, adminReq(req))
		return w
	}

	// No reason → 400, nothing written.
	w := post("/admin/tenants/"+id+"/billing/payment", url.Values{"amount": {"53.88"}, "period": {"annual"}, "std_tb": {"1"}})
	assert.Equal(t, http.StatusBadRequest, w.Code)
	// A manual payment of a year on 1 TB.
	w = post("/admin/tenants/"+id+"/billing/payment", url.Values{"amount": {"53.88"}, "period": {"annual"}, "std_tb": {"1"}, "reason": {"wire"}, "note": {"ref 1"}})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "ST-")
	e, err := payments.Get(context.Background(), db, id)
	require.NoError(t, err)
	assert.Equal(t, payments.ProviderManual, e.Provider)
	var n int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM audit_logs WHERE tenant_id = $1 AND action = 'billing.payment_recorded_manual' AND performed_by::text = $2`,
		id, "00000000-0000-4000-8000-00000000ad01").Scan(&n))
	assert.Equal(t, 1, n, "audited with the admin as actor")

	// Shorten the date by hand.
	sooner := time.Now().Add(48 * time.Hour).UTC().Format("2006-01-02")
	w = post("/admin/tenants/"+id+"/billing/entitlement", url.Values{"paid_through": {sooner}, "reason": {"agreed"}})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	e, err = payments.Get(context.Background(), db, id)
	require.NoError(t, err)
	assert.Equal(t, sooner, e.PaidThrough.UTC().Format("2006-01-02"))

	// Refund it, shortening.
	var pid string
	require.NoError(t, db.QueryRow(`SELECT id::text FROM payments WHERE tenant_id = $1 AND kind = 'charge'`, id).Scan(&pid))
	w = post("/admin/tenants/"+id+"/billing/refund", url.Values{"payment_id": {pid}, "amount": {"53.88"}, "shorten": {"on"}, "reason": {"cancelled"}})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "refunded")
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM audit_logs WHERE tenant_id = $1 AND action = 'billing.refund_recorded'`, id).Scan(&n))
	assert.Equal(t, 1, n)
}
