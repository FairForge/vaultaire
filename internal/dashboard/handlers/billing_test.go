package handlers

import (
	"context"
	"html/template"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
			`{{define "head"}}{{end}}` +
			`{{define "content"}}` +
			`<h1>Billing</h1>` +
			`<span class="plan">{{.Plan}}</span>` +
			`<span class="status">{{.StatusLabel}}</span>` +
			`<span class="storage">{{.StorageUsedFmt}}</span>` +
			`{{range .Providers}}<span class="provider">{{.Name}}:{{.StorageCost}}:{{.EgressCost}}:{{.TotalCost}}</span>{{end}}` +
			`<span class="savings">{{.TotalSavingsVsAWS}}</span>` +
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

func TestCostComparison_WithUsage(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	// 1 TB storage
	mock.ExpectQuery(`SELECT storage_used_bytes FROM tenant_quotas`).
		WithArgs("tenant-1").
		WillReturnRows(sqlmock.NewRows([]string{"storage_used_bytes"}).AddRow(int64(1099511627776)))

	// 500 GB egress
	mock.ExpectQuery(`SELECT COALESCE\(SUM\(egress_bytes\), 0\) FROM bandwidth_usage_daily`).
		WithArgs("tenant-1").
		WillReturnRows(sqlmock.NewRows([]string{"sum"}).AddRow(int64(536870912000)))

	data := map[string]any{}
	populateCostComparison(context.Background(), db, data, "tenant-1")

	providers := data["Providers"].([]ProviderCost)
	require.Len(t, providers, 4)

	// stored.ge: 1 TB * $3.99 = $3.99, egress = $0.00
	assert.Equal(t, "stored.ge", providers[0].Name)
	assert.Equal(t, "$3.99", providers[0].StorageCost)
	assert.Equal(t, "$0.00", providers[0].EgressCost)
	assert.Equal(t, "$3.99", providers[0].TotalCost)
	assert.True(t, providers[0].Highlight)

	// AWS S3: 1 TB * $23.00 = $23.00, 0.5 TB * $90 = $45.00 (approx, float)
	assert.Equal(t, "AWS S3", providers[1].Name)
	assert.Equal(t, "$23.00", providers[1].StorageCost)
	assert.Contains(t, providers[1].EgressCost, "$4") // ~$44.96 (500GB = 0.4997 TB)
	assert.False(t, providers[1].Highlight)

	// B2: 1 TB * $6.00 = $6.00
	assert.Equal(t, "Backblaze B2", providers[2].Name)
	assert.Equal(t, "$6.00", providers[2].StorageCost)

	// Wasabi: 1 TB * $6.99 = $6.99, egress = $0.00
	assert.Equal(t, "Wasabi", providers[3].Name)
	assert.Equal(t, "$6.99", providers[3].StorageCost)
	assert.Equal(t, "$0.00", providers[3].EgressCost)

	// Savings vs AWS: (23 + ~45) - (3.99 + 0) = ~$64
	savings := data["TotalSavingsVsAWS"].(string)
	assert.Contains(t, savings, "$")

	assert.NoError(t, mock.ExpectationsWereMet())
}

func TestCostComparison_ZeroUsage(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	mock.ExpectQuery(`SELECT storage_used_bytes FROM tenant_quotas`).
		WithArgs("tenant-1").
		WillReturnRows(sqlmock.NewRows([]string{"storage_used_bytes"}).AddRow(int64(0)))

	mock.ExpectQuery(`SELECT COALESCE\(SUM\(egress_bytes\), 0\) FROM bandwidth_usage_daily`).
		WithArgs("tenant-1").
		WillReturnRows(sqlmock.NewRows([]string{"sum"}).AddRow(int64(0)))

	data := map[string]any{}
	populateCostComparison(context.Background(), db, data, "tenant-1")

	providers := data["Providers"].([]ProviderCost)
	require.Len(t, providers, 4)
	for _, p := range providers {
		assert.Equal(t, "$0.00", p.StorageCost, "provider %s storage", p.Name)
		assert.Equal(t, "$0.00", p.EgressCost, "provider %s egress", p.Name)
		assert.Equal(t, "$0.00", p.TotalCost, "provider %s total", p.Name)
	}
	assert.Equal(t, "$0.00", data["TotalSavingsVsAWS"])
	assert.NoError(t, mock.ExpectationsWereMet())
}

func TestCostComparison_NilDB(t *testing.T) {
	data := map[string]any{}
	populateCostComparison(context.Background(), nil, data, "tenant-1")

	providers := data["Providers"].([]ProviderCost)
	require.Len(t, providers, 4)
	assert.True(t, providers[0].Highlight)
	assert.Equal(t, "$0.00", providers[0].TotalCost)
	assert.Equal(t, "$0.00", data["TotalSavingsVsAWS"])
	assert.Equal(t, "0 B", data["EgressThisMonth"])
}

func TestCostComparison_StorageOnly(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	// 1 TB storage, 0 egress
	mock.ExpectQuery(`SELECT storage_used_bytes FROM tenant_quotas`).
		WithArgs("tenant-1").
		WillReturnRows(sqlmock.NewRows([]string{"storage_used_bytes"}).AddRow(int64(1099511627776)))

	mock.ExpectQuery(`SELECT COALESCE\(SUM\(egress_bytes\), 0\) FROM bandwidth_usage_daily`).
		WithArgs("tenant-1").
		WillReturnRows(sqlmock.NewRows([]string{"sum"}).AddRow(int64(0)))

	data := map[string]any{}
	populateCostComparison(context.Background(), db, data, "tenant-1")

	providers := data["Providers"].([]ProviderCost)
	require.Len(t, providers, 4)

	assert.Equal(t, "$3.99", providers[0].StorageCost)
	assert.Equal(t, "$0.00", providers[0].EgressCost)
	assert.Equal(t, "$3.99", providers[0].TotalCost)

	assert.Equal(t, "$23.00", providers[1].StorageCost)
	assert.Equal(t, "$0.00", providers[1].EgressCost)
	assert.Equal(t, "$23.00", providers[1].TotalCost)

	assert.Equal(t, "$6.00", providers[2].StorageCost)
	assert.Equal(t, "$0.00", providers[2].EgressCost)
	assert.Equal(t, "$6.00", providers[2].TotalCost)

	assert.Equal(t, "$6.99", providers[3].StorageCost)
	assert.Equal(t, "$0.00", providers[3].EgressCost)
	assert.Equal(t, "$6.99", providers[3].TotalCost)

	assert.Equal(t, "$19.01", data["TotalSavingsVsAWS"])
	assert.Equal(t, "0 B", data["EgressThisMonth"])
	assert.NoError(t, mock.ExpectationsWereMet())
}
