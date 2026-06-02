package handlers

import (
	"html/template"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func testRevenueTemplate(t *testing.T) *template.Template {
	t.Helper()
	tmpl := template.Must(template.New("admin").Parse(
		`{{define "admin"}}{{block "content" .}}{{end}}{{end}}`))
	template.Must(tmpl.Parse(
		`{{define "title"}}Revenue{{end}}` +
			`{{define "content"}}` +
			`<span class="mrr">{{.MRRFmt}}</span>` +
			`<span class="active">{{.ActiveSubs}}</span>` +
			`<span class="new">{{.NewThisMonth}}</span>` +
			`<span class="churned">{{.ChurnedThisMonth}}</span>` +
			`<span class="churn-rate">{{.ChurnRateFmt}}</span>` +
			`{{range .ByTier}}<span class="tier">{{.Tier}} {{.Count}} {{.MRRFmt}}</span>{{end}}` +
			`{{range .TopCustomers}}<span class="customer">{{.Email}} {{.Plan}} {{.MRRFmt}} {{.StorageFmt}}</span>{{end}}` +
			`{{if not .ByTier}}<p>No tiers</p>{{end}}` +
			`{{if not .TopCustomers}}<p>No customers</p>{{end}}` +
			`{{end}}`))
	return tmpl
}

func TestRevenue_MRRFromFixedPlans(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	// Fixed MRR: two active Vault tenants.
	mock.ExpectQuery(`SELECT COALESCE\(plan, ''\) FROM tenants WHERE subscription_status = 'active'`).
		WillReturnRows(sqlmock.NewRows([]string{"plan"}).
			AddRow("vault3").
			AddRow("vault18"))

	// Metered MRR: no metered tenants.
	mock.ExpectQuery(`SELECT tq.tier, tq.storage_used_bytes`).
		WillReturnRows(sqlmock.NewRows([]string{"tier", "storage_used_bytes", "egress"}))

	// Active subs count.
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM tenants WHERE subscription_status = 'active'`).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(2))

	// New/churn/active-at-start.
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM subscriptions WHERE created_at`).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM subscriptions WHERE canceled_at`).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM subscriptions`).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(5))

	// By tier (fixed).
	mock.ExpectQuery(`SELECT COALESCE\(plan, 'free'\), COUNT`).
		WillReturnRows(sqlmock.NewRows([]string{"plan", "count"}).
			AddRow("vault3", 1).
			AddRow("vault18", 1))
	// By tier (metered).
	mock.ExpectQuery(`SELECT tq.tier, COUNT`).
		WillReturnRows(sqlmock.NewRows([]string{"tier", "count", "storage"}))

	// Top customers.
	mock.ExpectQuery(`SELECT t.email`).
		WillReturnRows(sqlmock.NewRows([]string{"email", "plan", "tier", "storage_used_bytes"}).
			AddRow("alice@example.com", "vault18", "", int64(5*1024*1024*1024)).
			AddRow("bob@example.com", "vault3", "", int64(1*1024*1024*1024)))

	// MRR trend.
	mock.ExpectQuery(`SELECT date_trunc\('month', created_at\)`).
		WillReturnRows(sqlmock.NewRows([]string{"month", "plan", "count"}))

	handler := HandleAdminRevenue(testRevenueTemplate(t), db, zap.NewNop())
	req := httptest.NewRequest("GET", "/admin/revenue", nil)
	req = req.WithContext(adminCtx(t))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	body := w.Body.String()
	// vault3=799 + vault18=1799 = 2598 cents = $25.98
	assert.Contains(t, body, "$25.98")
	assert.Contains(t, body, `<span class="active">2</span>`)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestRevenue_MRRIncludesMetered(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	// Fixed MRR: one vault1.
	mock.ExpectQuery(`SELECT COALESCE\(plan, ''\) FROM tenants WHERE subscription_status = 'active'`).
		WillReturnRows(sqlmock.NewRows([]string{"plan"}).AddRow("vault1"))

	// Metered MRR: one standard tenant with 2 TB.
	twoTB := int64(2 * 1024 * 1024 * 1024 * 1024)
	mock.ExpectQuery(`SELECT tq.tier, tq.storage_used_bytes`).
		WillReturnRows(sqlmock.NewRows([]string{"tier", "storage_used_bytes", "egress"}).
			AddRow("standard", twoTB, int64(0)))

	// Active subs.
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM tenants WHERE subscription_status = 'active'`).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(2))

	// New/churn/active-at-start.
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM subscriptions WHERE created_at`).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM subscriptions WHERE canceled_at`).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM subscriptions`).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))

	// By tier (fixed).
	mock.ExpectQuery(`SELECT COALESCE\(plan, 'free'\), COUNT`).
		WillReturnRows(sqlmock.NewRows([]string{"plan", "count"}).AddRow("vault1", 1))
	// By tier (metered).
	mock.ExpectQuery(`SELECT tq.tier, COUNT`).
		WillReturnRows(sqlmock.NewRows([]string{"tier", "count", "storage"}).
			AddRow("standard", 1, twoTB))

	// Top customers.
	mock.ExpectQuery(`SELECT t.email`).
		WillReturnRows(sqlmock.NewRows([]string{"email", "plan", "tier", "storage_used_bytes"}))

	// MRR trend.
	mock.ExpectQuery(`SELECT date_trunc\('month', created_at\)`).
		WillReturnRows(sqlmock.NewRows([]string{"month", "plan", "count"}))

	handler := HandleAdminRevenue(testRevenueTemplate(t), db, zap.NewNop())
	req := httptest.NewRequest("GET", "/admin/revenue", nil)
	req = req.WithContext(adminCtx(t))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	body := w.Body.String()
	// vault1=499 + standard 2TB = 2*3.99*100 = 798 cents. Total = 1297 = $12.97
	assert.Contains(t, body, "$12.97")
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestRevenue_ByTierBreakdown(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	// Stubs for MRR.
	mock.ExpectQuery(`SELECT COALESCE\(plan, ''\) FROM tenants`).
		WillReturnRows(sqlmock.NewRows([]string{"plan"}))
	mock.ExpectQuery(`SELECT tq.tier, tq.storage_used_bytes`).
		WillReturnRows(sqlmock.NewRows([]string{"tier", "storage_used_bytes", "egress"}))
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM tenants`).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM subscriptions WHERE created_at`).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM subscriptions WHERE canceled_at`).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM subscriptions`).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))

	// By-tier: vault5 x3, vault10 x2.
	mock.ExpectQuery(`SELECT COALESCE\(plan, 'free'\), COUNT`).
		WillReturnRows(sqlmock.NewRows([]string{"plan", "count"}).
			AddRow("vault5", 3).
			AddRow("vault10", 2))
	mock.ExpectQuery(`SELECT tq.tier, COUNT`).
		WillReturnRows(sqlmock.NewRows([]string{"tier", "count", "storage"}))

	mock.ExpectQuery(`SELECT t.email`).
		WillReturnRows(sqlmock.NewRows([]string{"email", "plan", "tier", "storage_used_bytes"}))
	mock.ExpectQuery(`SELECT date_trunc`).
		WillReturnRows(sqlmock.NewRows([]string{"month", "plan", "count"}))

	handler := HandleAdminRevenue(testRevenueTemplate(t), db, zap.NewNop())
	req := httptest.NewRequest("GET", "/admin/revenue", nil)
	req = req.WithContext(adminCtx(t))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	body := w.Body.String()
	// vault5: 3*999=2997 = $29.97
	assert.Contains(t, body, "vault5 3 $29.97")
	// vault10: 2*1299=2598 = $25.98
	assert.Contains(t, body, "vault10 2 $25.98")
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestRevenue_NewAndChurnThisMonth(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	mock.ExpectQuery(`SELECT COALESCE\(plan, ''\) FROM tenants`).
		WillReturnRows(sqlmock.NewRows([]string{"plan"}))
	mock.ExpectQuery(`SELECT tq.tier, tq.storage_used_bytes`).
		WillReturnRows(sqlmock.NewRows([]string{"tier", "storage_used_bytes", "egress"}))
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM tenants`).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))

	// 5 new, 2 churned, 10 active at start → 20% churn.
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM subscriptions WHERE created_at`).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(5))
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM subscriptions WHERE canceled_at`).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(2))
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM subscriptions`).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(10))

	mock.ExpectQuery(`SELECT COALESCE\(plan, 'free'\), COUNT`).
		WillReturnRows(sqlmock.NewRows([]string{"plan", "count"}))
	mock.ExpectQuery(`SELECT tq.tier, COUNT`).
		WillReturnRows(sqlmock.NewRows([]string{"tier", "count", "storage"}))
	mock.ExpectQuery(`SELECT t.email`).
		WillReturnRows(sqlmock.NewRows([]string{"email", "plan", "tier", "storage_used_bytes"}))
	mock.ExpectQuery(`SELECT date_trunc`).
		WillReturnRows(sqlmock.NewRows([]string{"month", "plan", "count"}))

	handler := HandleAdminRevenue(testRevenueTemplate(t), db, zap.NewNop())
	req := httptest.NewRequest("GET", "/admin/revenue", nil)
	req = req.WithContext(adminCtx(t))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	body := w.Body.String()
	assert.Contains(t, body, `<span class="new">5</span>`)
	assert.Contains(t, body, `<span class="churned">2</span>`)
	assert.Contains(t, body, `<span class="churn-rate">20.0%</span>`)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestRevenue_TopCustomers(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	mock.ExpectQuery(`SELECT COALESCE\(plan, ''\) FROM tenants`).
		WillReturnRows(sqlmock.NewRows([]string{"plan"}))
	mock.ExpectQuery(`SELECT tq.tier, tq.storage_used_bytes`).
		WillReturnRows(sqlmock.NewRows([]string{"tier", "storage_used_bytes", "egress"}))
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM tenants`).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM subscriptions WHERE created_at`).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM subscriptions WHERE canceled_at`).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM subscriptions`).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectQuery(`SELECT COALESCE\(plan, 'free'\), COUNT`).
		WillReturnRows(sqlmock.NewRows([]string{"plan", "count"}))
	mock.ExpectQuery(`SELECT tq.tier, COUNT`).
		WillReturnRows(sqlmock.NewRows([]string{"tier", "count", "storage"}))

	tenTB := int64(10 * 1024 * 1024 * 1024 * 1024)
	mock.ExpectQuery(`SELECT t.email`).
		WillReturnRows(sqlmock.NewRows([]string{"email", "plan", "tier", "storage_used_bytes"}).
			AddRow("whale@example.com", "vault100", "", tenTB).
			AddRow("metered@example.com", "starter", "standard", tenTB))

	mock.ExpectQuery(`SELECT date_trunc`).
		WillReturnRows(sqlmock.NewRows([]string{"month", "plan", "count"}))

	handler := HandleAdminRevenue(testRevenueTemplate(t), db, zap.NewNop())
	req := httptest.NewRequest("GET", "/admin/revenue", nil)
	req = req.WithContext(adminCtx(t))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	body := w.Body.String()
	assert.Contains(t, body, "whale@example.com")
	assert.Contains(t, body, "vault100")
	assert.Contains(t, body, "$84.99") // vault100 MRR
	assert.Contains(t, body, "metered@example.com")
	assert.Contains(t, body, "standard")
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestRevenue_ZeroState(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	// All queries return empty.
	mock.ExpectQuery(`SELECT COALESCE\(plan, ''\) FROM tenants`).
		WillReturnRows(sqlmock.NewRows([]string{"plan"}))
	mock.ExpectQuery(`SELECT tq.tier, tq.storage_used_bytes`).
		WillReturnRows(sqlmock.NewRows([]string{"tier", "storage_used_bytes", "egress"}))
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM tenants`).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM subscriptions WHERE created_at`).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM subscriptions WHERE canceled_at`).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM subscriptions`).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectQuery(`SELECT COALESCE\(plan, 'free'\), COUNT`).
		WillReturnRows(sqlmock.NewRows([]string{"plan", "count"}))
	mock.ExpectQuery(`SELECT tq.tier, COUNT`).
		WillReturnRows(sqlmock.NewRows([]string{"tier", "count", "storage"}))
	mock.ExpectQuery(`SELECT t.email`).
		WillReturnRows(sqlmock.NewRows([]string{"email", "plan", "tier", "storage_used_bytes"}))
	mock.ExpectQuery(`SELECT date_trunc`).
		WillReturnRows(sqlmock.NewRows([]string{"month", "plan", "count"}))

	handler := HandleAdminRevenue(testRevenueTemplate(t), db, zap.NewNop())
	req := httptest.NewRequest("GET", "/admin/revenue", nil)
	req = req.WithContext(adminCtx(t))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	body := w.Body.String()
	assert.Contains(t, body, "$0.00")
	assert.Contains(t, body, `<span class="active">0</span>`)
	assert.Contains(t, body, "No tiers")
	assert.Contains(t, body, "No customers")
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestRevenue_NoSession(t *testing.T) {
	handler := HandleAdminRevenue(testRevenueTemplate(t), nil, zap.NewNop())
	req := httptest.NewRequest("GET", "/admin/revenue", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusSeeOther, w.Code)
	assert.Equal(t, "/login", w.Header().Get("Location"))
}

func TestRevenue_NilDB(t *testing.T) {
	handler := HandleAdminRevenue(testRevenueTemplate(t), nil, zap.NewNop())
	req := httptest.NewRequest("GET", "/admin/revenue", nil)
	req = req.WithContext(adminCtx(t))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	body := w.Body.String()
	assert.Contains(t, body, "$0.00")
	assert.Contains(t, body, `<span class="active">0</span>`)
}

func TestRevenue_MRRTrend(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	mock.ExpectQuery(`SELECT COALESCE\(plan, ''\) FROM tenants`).
		WillReturnRows(sqlmock.NewRows([]string{"plan"}))
	mock.ExpectQuery(`SELECT tq.tier, tq.storage_used_bytes`).
		WillReturnRows(sqlmock.NewRows([]string{"tier", "storage_used_bytes", "egress"}))
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM tenants`).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM subscriptions WHERE created_at`).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM subscriptions WHERE canceled_at`).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM subscriptions`).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectQuery(`SELECT COALESCE\(plan, 'free'\), COUNT`).
		WillReturnRows(sqlmock.NewRows([]string{"plan", "count"}))
	mock.ExpectQuery(`SELECT tq.tier, COUNT`).
		WillReturnRows(sqlmock.NewRows([]string{"tier", "count", "storage"}))
	mock.ExpectQuery(`SELECT t.email`).
		WillReturnRows(sqlmock.NewRows([]string{"email", "plan", "tier", "storage_used_bytes"}))

	// Trend data: Jan 2026 has 2 vault5, Mar 2026 has 1 vault10.
	mock.ExpectQuery(`SELECT date_trunc\('month', created_at\)`).
		WillReturnRows(sqlmock.NewRows([]string{"month", "plan", "count"}).
			AddRow(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), "vault5", 2).
			AddRow(time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC), "vault10", 1))

	tmpl := template.Must(template.New("admin").Parse(
		`{{define "admin"}}{{block "content" .}}{{end}}{{end}}`))
	template.Must(tmpl.Parse(
		`{{define "title"}}Revenue{{end}}` +
			`{{define "content"}}` +
			`{{range .TrendBars}}<span class="bar">{{.Label}} {{.Fmt}}</span>{{end}}` +
			`<span class="max">{{.TrendMaxFmt}}</span>` +
			`{{end}}`))

	handler := HandleAdminRevenue(tmpl, db, zap.NewNop())
	req := httptest.NewRequest("GET", "/admin/revenue", nil)
	req = req.WithContext(adminCtx(t))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	body := w.Body.String()
	// Jan: 2*999=1998=$19.98, Mar: 1*1299=$12.99
	assert.Contains(t, body, "Jan $19.98")
	assert.Contains(t, body, "Mar $12.99")
	assert.Contains(t, body, `<span class="max">$19.98</span>`)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPlanMonthlyCents(t *testing.T) {
	cases := []struct {
		plan string
		want int64
	}{
		{"vault1", 499},
		{"vault3", 799},
		{"vault5", 999},
		{"vault10", 1299},
		{"vault18", 1799},
		{"vault50", 4499},
		{"vault100", 8499},
		{"starter", 0},
		{"free", 0},
		{"standard", 0},
		{"performance", 0},
		{"", 0},
		{"unknown", 0},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, planMonthlyCents(tc.plan), "plan=%q", tc.plan)
	}
}
