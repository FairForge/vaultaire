package handlers

import (
	"database/sql"
	"html/template"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/FairForge/vaultaire/internal/payments"
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

// revenueQueries expects populateRevenue's queries (WP-PAY-7: the record):
// the houses, the live count, then the month's new / lapsed / live at start.
func revenueQueries(mock sqlmock.Sqlmock, houses *mockHouses, active, newN, churned, atStart int) {
	houses.expect(mock)
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM entitlements WHERE lapsed_at IS NULL`).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(active))
	mock.ExpectQuery(`FROM entitlements e, b`).
		WillReturnRows(sqlmock.NewRows([]string{"new", "lapsed", "at_start"}).AddRow(newN, churned, atStart))
}

func renderRevenue(t *testing.T, db *sql.DB) string {
	t.Helper()
	handler := HandleAdminRevenue(testRevenueTemplate(t), db, zap.NewNop())
	req := httptest.NewRequest("GET", "/admin/revenue", nil)
	req = req.WithContext(adminCtx(t))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)
	return w.Body.String()
}

// MRR is the houses at payments.QuoteHouse, by floor, and their owners.
func TestRevenue_MRRFromHouses(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	tb := int64(1024 * 1024 * 1024 * 1024)
	revenueQueries(mock, houseRows().
		AddRow("t-a", "alice@example.com", "annual", "standard", 6*tb, int64(0), 2*tb).
		AddRow("t-a", "alice@example.com", "annual", "vault", 1*tb, int64(0), 2*tb).
		AddRow("t-b", "bob@example.com", "monthly", "vault", 2*tb, int64(0), tb), 2, 0, 0, 0)

	body := renderRevenue(t, db)

	a := payments.QuoteHouse(6, 1, 0, payments.PeriodAnnual)
	b := payments.QuoteHouse(0, 2, 0, payments.PeriodMonthly)
	assert.Contains(t, body, `<span class="mrr">`+formatCents(a.MonthlyCents+b.MonthlyCents)+`</span>`)
	assert.Contains(t, body, `<span class="active">2</span>`)
	assert.Contains(t, body, "Standard 1")
	assert.Contains(t, body, "Vault 2")
	assert.Contains(t, body, "alice@example.com house (annual) "+formatCents(a.MonthlyCents))
	assert.Contains(t, body, "bob@example.com house (monthly) "+formatCents(b.MonthlyCents))
	assert.Less(t, strings.Index(body, "alice@"), strings.Index(body, "bob@"), "ranked by MRR")
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestRevenue_NewAndChurnThisMonth(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	// 5 new, 2 churned, 10 active at start → 20% churn.
	revenueQueries(mock, houseRows(), 0, 5, 2, 10)

	body := renderRevenue(t, db)

	assert.Contains(t, body, `<span class="new">5</span>`)
	assert.Contains(t, body, `<span class="churned">2</span>`)
	assert.Contains(t, body, `<span class="churn-rate">20.0%</span>`)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestRevenue_ZeroState(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	revenueQueries(mock, houseRows(), 0, 0, 0, 0)

	body := renderRevenue(t, db)

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
