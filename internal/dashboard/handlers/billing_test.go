package handlers

import (
	"html/template"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"go.uber.org/zap"
)

func testBillingTemplate(t *testing.T) *template.Template {
	t.Helper()
	tmpl := template.Must(template.New("base").Parse(
		`{{define "base"}}` +
			`{{block "nav" .}}{{end}}` +
			`{{block "content" .}}{{end}}` +
			`{{end}}`))
	template.Must(tmpl.Parse(
		`{{define "title"}}Billing{{end}}` +
			`{{define "content"}}` +
			`<h1>Billing</h1>` +
			`<span class="plan">{{.Plan}}</span>` +
			`<span class="status">{{.StatusLabel}}</span>` +
			`<span class="storage">{{.StorageUsedFmt}}</span>` +
			`<span class="aws">{{.AWSCost}}</span>` +
			`<span class="stored">{{.StoredCost}}</span>` +
			`{{if .Upgraded}}<span class="upgraded">yes</span>{{end}}` +
			`{{end}}`))
	return tmpl
}

func TestHandleBilling_NoDB(t *testing.T) {
	tmpl := testBillingTemplate(t)
	handler := HandleBilling(tmpl, nil, nil, zap.NewNop())

	req := httptest.NewRequest("GET", "/dashboard/billing", nil)
	req = req.WithContext(usageSessionCtx(t))

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	body := w.Body.String()
	assert.Contains(t, body, "starter")
	assert.Contains(t, body, "Free")
}

func TestHandleBilling_NoSession(t *testing.T) {
	tmpl := testBillingTemplate(t)
	handler := HandleBilling(tmpl, nil, nil, zap.NewNop())

	req := httptest.NewRequest("GET", "/dashboard/billing", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusSeeOther, w.Code)
	assert.Equal(t, "/login", w.Header().Get("Location"))
}

func TestHandleBilling_UpgradedParam(t *testing.T) {
	tmpl := testBillingTemplate(t)
	handler := HandleBilling(tmpl, nil, nil, zap.NewNop())

	req := httptest.NewRequest("GET", "/dashboard/billing?upgraded=1", nil)
	req = req.WithContext(usageSessionCtx(t))

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "upgraded")
}

func TestHandleUpgrade_NoStripe(t *testing.T) {
	handler := HandleUpgrade(nil, nil, zap.NewNop())

	req := httptest.NewRequest("POST", "/dashboard/billing/upgrade", nil)
	req = req.WithContext(usageSessionCtx(t))

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestHandleManageBilling_NoStripe(t *testing.T) {
	handler := HandleManageBilling(nil, zap.NewNop())

	req := httptest.NewRequest("POST", "/dashboard/billing/portal", nil)
	req = req.WithContext(usageSessionCtx(t))

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
}
