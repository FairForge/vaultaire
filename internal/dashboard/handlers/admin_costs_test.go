package handlers

import (
	"database/sql"
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

func testCostsTemplate(t *testing.T) *template.Template {
	t.Helper()
	tmpl := template.Must(template.New("admin").Parse(
		`{{define "admin"}}{{block "content" .}}{{end}}{{end}}`))
	template.Must(tmpl.Parse(
		`{{define "title"}}Costs{{end}}` +
			`{{define "content"}}` +
			`<span class="spend">{{.EstSpendFmt}}</span>` +
			`<span class="cogs">{{.BlendedCOGSFmt}}</span>` +
			`<span class="margin">{{.GrossMarginFmt}}</span>` +
			`<span class="neg-count">{{.NegativeMarginCount}}</span>` +
			`<span class="projected">{{.ProjectedSpendFmt}}</span>` +
			`{{range .ByBackend}}<span class="backend">{{.Backend}} {{.StorageFmt}} {{.CostFmt}} {{.FixedFmt}} {{.TotalFmt}}</span>{{end}}` +
			`{{range .MarginTable}}<span class="tenant-margin{{if .IsNegative}} negative{{end}}">{{.Email}} {{.Plan}} {{.Backend}} {{.RevenueFmt}} {{.CostFmt}} {{.EgressCostFmt}} {{.MarginFmt}}</span>{{end}}` +
			`{{range .ActualByBackend}}<span class="actual-backend">{{.Backend}} {{.ObjectCount}} {{.StorageFmt}} {{.CostFmt}} modelled:{{.ModelledFmt}}</span>{{end}}` +
			`<span class="subsidy">subsidy:{{.SubsidyFmt}}</span><span class="mode">{{.CostMode}}</span>` +
			`{{if not .ByBackend}}<p>No backends</p>{{end}}` +
			`{{if not .MarginTable}}<p>No margins</p>{{end}}` +
			`{{end}}`))
	return tmpl
}

func costsQueryRows(mock sqlmock.Sqlmock, rows *sqlmock.Rows) {
	mock.ExpectQuery(`SELECT t.email, COALESCE\(t.plan, ''\)`).WillReturnRows(rows)
}

func emptyActualBackendRows(mock sqlmock.Sqlmock) {
	mock.ExpectQuery(`SELECT backend_name, COUNT`).
		WillReturnRows(sqlmock.NewRows([]string{"backend_name", "count", "sum"}))
}

func TestCosts_PerBackendSpend(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	oneTB := int64(1024 * 1024 * 1024 * 1024)
	costsQueryRows(mock, sqlmock.NewRows([]string{"email", "plan", "tier", "storage_used_bytes", "egress"}).
		AddRow("alice@example.com", "vault5", "", oneTB, int64(0)).
		AddRow("bob@example.com", "vault3", "", oneTB, int64(0)))
	emptyActualBackendRows(mock)

	handler := HandleAdminCosts(testCostsTemplate(t), db, zap.NewNop())
	req := httptest.NewRequest("GET", "/admin/costs", nil)
	req = req.WithContext(adminCtx(t))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	body := w.Body.String()
	// 2 TB on geyser at 155 cents/TB = 310 cents variable + 15500 fixed = 15810 total.
	// Backend row: geyser 2.00 TB $3.10 $155.00 $158.10
	assert.Contains(t, body, "geyser")
	assert.Contains(t, body, "2.00 TB")
	assert.Contains(t, body, "$3.10")
	assert.Contains(t, body, "$155.00")
	assert.Contains(t, body, "$158.10")
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestCosts_MarginPerTenant(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	oneTB := int64(1024 * 1024 * 1024 * 1024)
	costsQueryRows(mock, sqlmock.NewRows([]string{"email", "plan", "tier", "storage_used_bytes", "egress"}).
		AddRow("alice@example.com", "vault5", "", oneTB, int64(0)))
	emptyActualBackendRows(mock)

	handler := HandleAdminCosts(testCostsTemplate(t), db, zap.NewNop())
	req := httptest.NewRequest("GET", "/admin/costs", nil)
	req = req.WithContext(adminCtx(t))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	body := w.Body.String()
	// vault5 revenue = $9.99 (999 cents), geyser cost for 1TB = $1.55 (155 cents).
	// Margin = 999 - 155 = 844 cents = $8.44
	assert.Contains(t, body, "alice@example.com")
	assert.Contains(t, body, "vault5")
	assert.Contains(t, body, "$9.99") // revenue
	assert.Contains(t, body, "$1.55") // cost
	assert.Contains(t, body, "$8.44") // margin
	assert.NotContains(t, body, "negative")
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestCosts_NegativeMarginFlagged(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	// Vault18 at full 18TB fill: revenue=1799, cost=18*155=2790 → negative margin.
	eighteenTB := int64(18 * 1024 * 1024 * 1024 * 1024)
	costsQueryRows(mock, sqlmock.NewRows([]string{"email", "plan", "tier", "storage_used_bytes", "egress"}).
		AddRow("whale@example.com", "vault18", "", eighteenTB, int64(0)))
	emptyActualBackendRows(mock)

	handler := HandleAdminCosts(testCostsTemplate(t), db, zap.NewNop())
	req := httptest.NewRequest("GET", "/admin/costs", nil)
	req = req.WithContext(adminCtx(t))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	body := w.Body.String()
	// revenue=1799 cents=$17.99, cost=18*155=2790 cents=$27.90, margin=-991 cents=-$9.91
	assert.Contains(t, body, "whale@example.com")
	assert.Contains(t, body, "$17.99") // revenue
	assert.Contains(t, body, "$27.90") // cost
	assert.Contains(t, body, "-$9.91") // negative margin
	assert.Contains(t, body, "negative")
	assert.Contains(t, body, `<span class="neg-count">1</span>`)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestCosts_BlendedCOGS(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	twoTB := int64(2 * 1024 * 1024 * 1024 * 1024)
	costsQueryRows(mock, sqlmock.NewRows([]string{"email", "plan", "tier", "storage_used_bytes", "egress"}).
		AddRow("a@test.com", "vault5", "", twoTB, int64(0)))
	emptyActualBackendRows(mock)

	handler := HandleAdminCosts(testCostsTemplate(t), db, zap.NewNop())
	req := httptest.NewRequest("GET", "/admin/costs", nil)
	req = req.WithContext(adminCtx(t))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	body := w.Body.String()
	// 2TB on geyser: variable=310 cents + geyser floor 15500 + gorilla 0 = 15810 total
	// Blended = 15810 / 2 = 7905 cents/TB = $79.05/TB
	assert.Contains(t, body, "$79.05/TB")
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestCosts_FixedCostsIncluded(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	oneTB := int64(1024 * 1024 * 1024 * 1024)
	costsQueryRows(mock, sqlmock.NewRows([]string{"email", "plan", "tier", "storage_used_bytes", "egress"}).
		AddRow("a@test.com", "vault5", "", oneTB, int64(0)))
	emptyActualBackendRows(mock)

	handler := HandleAdminCosts(testCostsTemplate(t), db, zap.NewNop())
	req := httptest.NewRequest("GET", "/admin/costs", nil)
	req = req.WithContext(adminCtx(t))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	body := w.Body.String()
	// 1TB geyser variable = 155 cents, geyser floor = 15500, gorilla = 0
	// Total est spend = 155 + 15500 + 0 = 15655 = $156.55
	assert.Contains(t, body, `<span class="spend">$156.55</span>`)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestCosts_ZeroState(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	costsQueryRows(mock, sqlmock.NewRows([]string{"email", "plan", "tier", "storage_used_bytes", "egress"}))
	emptyActualBackendRows(mock)

	handler := HandleAdminCosts(testCostsTemplate(t), db, zap.NewNop())
	req := httptest.NewRequest("GET", "/admin/costs", nil)
	req = req.WithContext(adminCtx(t))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	body := w.Body.String()
	assert.Contains(t, body, `<span class="spend">$0.00</span>`)
	assert.Contains(t, body, `<span class="cogs">$0.00/TB</span>`)
	assert.Contains(t, body, `<span class="margin">0%</span>`)
	assert.Contains(t, body, `<span class="neg-count">0</span>`)
	assert.Contains(t, body, "No backends")
	assert.Contains(t, body, "No margins")
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestCosts_NoSession(t *testing.T) {
	handler := HandleAdminCosts(testCostsTemplate(t), nil, zap.NewNop())
	req := httptest.NewRequest("GET", "/admin/costs", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusSeeOther, w.Code)
	assert.Equal(t, "/login", w.Header().Get("Location"))
}

func TestCosts_NilDB(t *testing.T) {
	handler := HandleAdminCosts(testCostsTemplate(t), nil, zap.NewNop())
	req := httptest.NewRequest("GET", "/admin/costs", nil)
	req = req.WithContext(adminCtx(t))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	body := w.Body.String()
	assert.Contains(t, body, `<span class="spend">$0.00</span>`)
	assert.Contains(t, body, `<span class="neg-count">0</span>`)
}

func TestBackendCostPerTB(t *testing.T) {
	cases := []struct {
		backend string
		want    int64
	}{
		{"geyser", 155},
		{"idrive", 330},
		{"hetzner", 381},
		{"onedrive", 0},
		{"gorilla", 0},
		{"local", 0},
		{"edge", 0},
	}
	for _, tc := range cases {
		got := backendCostPerTBCents[tc.backend]
		assert.Equal(t, tc.want, got, "backend=%q", tc.backend)
	}
}

func TestTierBackend(t *testing.T) {
	cases := []struct {
		plan string
		tier string
		want string
	}{
		{"vault1", "", "geyser"},
		{"vault3", "", "geyser"},
		{"vault5", "", "geyser"},
		{"vault10", "", "geyser"},
		{"vault18", "", "geyser"},
		{"vault50", "", "geyser"},
		{"vault100", "", "geyser"},
		{"starter", "standard", "idrive"},
		{"starter", "performance", "idrive"},
		{"free", "", "local"},
		{"", "", "local"},
	}
	for _, tc := range cases {
		got := tierBackend(tc.plan, tc.tier)
		assert.Equal(t, tc.want, got, "plan=%q tier=%q", tc.plan, tc.tier)
	}
}

func TestProjectedCosts(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	oneTB := int64(1024 * 1024 * 1024 * 1024)
	costsQueryRows(mock, sqlmock.NewRows([]string{"email", "plan", "tier", "storage_used_bytes", "egress"}).
		AddRow("a@test.com", "vault5", "", oneTB, int64(0)))
	emptyActualBackendRows(mock)

	handler := HandleAdminCosts(testCostsTemplate(t), db, zap.NewNop())
	req := httptest.NewRequest("GET", "/admin/costs", nil)
	req = req.WithContext(adminCtx(t))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	body := w.Body.String()
	// ProjectedSpendFmt should exist and not be zero (projected from current day).
	assert.Contains(t, body, `<span class="projected">$`)
	assert.NotContains(t, body, `<span class="projected">$0.00</span>`)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestDaysInCurrentMonth(t *testing.T) {
	cases := []struct {
		month int
		year  int
		want  int
	}{
		{1, 2026, 31},
		{2, 2026, 28},
		{2, 2024, 29}, // leap year
		{4, 2026, 30},
		{6, 2026, 30},
		{12, 2026, 31},
	}
	for _, tc := range cases {
		got := daysInCurrentMonth(
			time.Date(tc.year, time.Month(tc.month), 15, 0, 0, 0, 0, time.UTC))
		assert.Equal(t, tc.want, got, "month=%d year=%d", tc.month, tc.year)
	}
}

func TestAdminCosts_ActualBackendData(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	costsQueryRows(mock, sqlmock.NewRows([]string{"email", "plan", "tier", "storage_used_bytes", "egress"}))

	oneTB := int64(1024 * 1024 * 1024 * 1024)
	mock.ExpectQuery(`SELECT backend_name, COUNT`).
		WillReturnRows(sqlmock.NewRows([]string{"backend_name", "count", "sum"}).
			AddRow("idrive", int64(500), oneTB).
			AddRow("geyser", int64(200), int64(2)*oneTB))

	handler := HandleAdminCosts(testCostsTemplate(t), db, zap.NewNop())
	req := httptest.NewRequest("GET", "/admin/costs", nil)
	req = req.WithContext(adminCtx(t))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	body := w.Body.String()
	assert.Contains(t, body, "idrive")
	assert.Contains(t, body, "500")
	assert.Contains(t, body, "geyser")
	assert.Contains(t, body, "200")
	assert.Contains(t, body, "actual-backend")
	require.NoError(t, mock.ExpectationsWereMet())
}

// --- Modelled vs invoiced cost view (Lyve promo tracking) ---

func actualBackendRows(mock sqlmock.Sqlmock, rows *sqlmock.Rows) {
	mock.ExpectQuery(`SELECT backend_name, COUNT`).WillReturnRows(rows)
}

// tbBytes makes the per-TB rate arithmetic exact in these tests.
const tbBytes = int64(1024) * 1024 * 1024 * 1024

func renderCosts(t *testing.T, db *sql.DB, url string) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, url, nil)
	req = req.WithContext(adminCtx(t))
	rec := httptest.NewRecorder()
	HandleAdminCosts(testCostsTemplate(t), db, zap.NewNop()).ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	return rec.Body.String()
}

func TestActualBackends_ModelledCostForSubsidizedBackend(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	costsQueryRows(mock, sqlmock.NewRows([]string{"email", "plan", "tier", "storage", "egress"}))
	actualBackendRows(mock, sqlmock.NewRows([]string{"backend_name", "count", "sum"}).
		AddRow("lyve", 10, tbBytes))

	body := renderCosts(t, db, "/admin/costs")

	// 1 TB on Lyve at the modelled $7.99/TB, even though Lyve invoices $0.
	assert.Contains(t, body, "lyve 10 1 TB $7.99 modelled:$7.99")
}

func TestActualBackends_InvoicedModeZeroesSubsidizedBackend(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	costsQueryRows(mock, sqlmock.NewRows([]string{"email", "plan", "tier", "storage", "egress"}))
	actualBackendRows(mock, sqlmock.NewRows([]string{"backend_name", "count", "sum"}).
		AddRow("lyve", 10, tbBytes))

	body := renderCosts(t, db, "/admin/costs?costs=invoiced")

	// Cost column zeroed, but the modelled figure stays visible so the page
	// still shows what this footprint will cost once the promo ends.
	assert.Contains(t, body, "lyve 10 1 TB $0.00 modelled:$7.99")
}

func TestActualBackends_UnsubsidizedBackendCostedInBothModes(t *testing.T) {
	for _, mode := range []string{"/admin/costs", "/admin/costs?costs=invoiced"} {
		db, mock, err := sqlmock.New()
		require.NoError(t, err)

		costsQueryRows(mock, sqlmock.NewRows([]string{"email", "plan", "tier", "storage", "egress"}))
		actualBackendRows(mock, sqlmock.NewRows([]string{"backend_name", "count", "sum"}).
			AddRow("geyser", 5, tbBytes))

		body := renderCosts(t, db, mode)
		assert.Contains(t, body, "geyser 5 1 TB $1.55", "geyser is really invoiced, so it is costed in %s", mode)
		_ = db.Close()
	}
}

func TestCosts_SubsidyIsModelledMinusInvoiced(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	costsQueryRows(mock, sqlmock.NewRows([]string{"email", "plan", "tier", "storage", "egress"}))
	actualBackendRows(mock, sqlmock.NewRows([]string{"backend_name", "count", "sum"}).
		AddRow("lyve", 10, tbBytes))

	body := renderCosts(t, db, "/admin/costs")

	// The whole Lyve line is currently free, so the subsidy equals its modelled cost.
	assert.Contains(t, body, `subsidy:$7.99`)
}

func TestCostMode_DefaultsToModelled(t *testing.T) {
	assert.Equal(t, costModeModelled, parseCostMode(""))
	assert.Equal(t, costModeModelled, parseCostMode("garbage"))
	assert.Equal(t, costModeInvoiced, parseCostMode("invoiced"))
}

func TestLyveRatesArePresent(t *testing.T) {
	assert.Equal(t, int64(799), backendCostPerTBCents["lyve"], "Lyve tracked at $7.99/TB")
	assert.Positive(t, egressCostPerTBCents["lyve"], "Lyve needs a modelled egress rate")
	assert.True(t, subsidizedBackends["lyve"], "Lyve is invoiced at $0 during the promo")
}
