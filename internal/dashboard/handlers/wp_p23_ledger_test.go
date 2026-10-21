package handlers

import (
	"context"
	"database/sql"
	"html/template"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/FairForge/vaultaire/internal/payments"
	"github.com/FairForge/vaultaire/internal/usage"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// WP-PAY-7 PR 2: the admin pages, the billing page and onboarding read the
// entitlement record, never tenants.{subscription_status, house_period,
// stripe_subscription_id}. A granted entitlement with NO tenants columns
// renders; a tenants row saying 'house'/'active' with no entitlement does
// not (docs/PAYMENTS.md § 1.6 step 4).

// grantedHouse grants tenantID a manual prepaid house through the record
// (floors included).
func grantedHouse(t *testing.T, db *sql.DB, tenantID string, o payments.Order, paidThrough time.Time) {
	t.Helper()
	grantedHouseVia(t, db, tenantID, o, paidThrough, payments.ProviderManual, "", payments.RenewalPrepaid)
}

// grantedHouseVia grants through the named provider (a Stripe row renews).
func grantedHouseVia(t *testing.T, db *sql.DB, tenantID string, o payments.Order, paidThrough time.Time,
	provider, ref string, renewal payments.Renewal) {
	t.Helper()
	require.NoError(t, payments.InTx(context.Background(), db, func(tx *sql.Tx) error {
		return payments.Grant(context.Background(), tx, tenantID, o, paidThrough, provider, ref, renewal)
	}))
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM entitlements WHERE tenant_id = $1`, tenantID)
		_, _ = db.Exec(`DELETE FROM tenant_floor_quotas WHERE tenant_id = $1`, tenantID)
	})
}

// columnsOnlyHouse is the shape the record replaced: tenants columns that
// say 'house'/'active' and floors, with no entitlement row.
func columnsOnlyHouse(t *testing.T, db *sql.DB, tenantID string) {
	t.Helper()
	require.NoError(t, usage.NewQuotaManager(db).SetHouse(context.Background(), tenantID, usage.HouseFromTB(50, 0, 0)))
	_, err := db.Exec(`UPDATE tenants SET plan = 'house', subscription_status = 'active', stripe_subscription_id = 'sub_cols',
	                   house_period = 'annual' WHERE id = $1`, tenantID)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM tenant_floor_quotas WHERE tenant_id = $1`, tenantID) })
}

func TestAdminRevenue_FromLedger(t *testing.T) {
	// Arrange: a ledger house and a columns-only "house".
	db := testDashDB(t)
	t.Cleanup(func() { _ = db.Close() })
	ledger := houseTenant(t, db, 0, 0)
	grantedHouse(t, db, ledger, payments.Order{Std: 6, Vault: 1, Period: payments.PeriodAnnual}, time.Now().Add(200*24*time.Hour))
	cols := houseTenant(t, db, 0, 0)
	columnsOnlyHouse(t, db, cols)

	// Act
	houses := queryHouses(context.Background(), db, zap.NewNop())
	body := renderRevenue(t, db)

	// Assert
	byID := map[string]houseRevenue{}
	for _, h := range houses {
		byID[h.tenantID] = h
	}
	got, ok := byID[ledger]
	require.True(t, ok, "the entitlement's house is priced with no tenants column set")
	assert.Equal(t, payments.QuoteHouse(6, 1, 0, payments.PeriodAnnual).MonthlyCents, got.quote.MonthlyCents)
	assert.Equal(t, ledger+"@test.local", got.email)
	_, ok = byID[cols]
	assert.False(t, ok, "tenants columns alone are not a house")
	assert.Contains(t, body, ledger+"@test.local")
	assert.NotContains(t, body, cols+"@test.local")
}

// The overview's and revenue page's "active subscriptions" is the count of
// granted entitlements.
func TestAdminOverview_ActiveSubsFromEntitlements(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	mock.MatchExpectationsInOrder(false)
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM tenants`).WillReturnRows(sqlmock.NewRows([]string{"n"}).AddRow(3))
	mock.ExpectQuery(`SUM\(storage_used_bytes\)`).WillReturnRows(sqlmock.NewRows([]string{"n"}).AddRow(0))
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM \(SELECT tenant_id FROM entitlements WHERE lapsed_at IS NULL\)`).
		WillReturnRows(sqlmock.NewRows([]string{"n"}).AddRow(7))
	mock.ExpectQuery(`FROM entitlements WHERE lapsed_at IS NULL`).
		WillReturnRows(sqlmock.NewRows([]string{"tenant_id", "std_tb", "vault_tb", "pin_hot_tb", "period", "paid_through",
			"renewal", "provider", "provider_ref", "created_at", "updated_at"}))

	_, _, active, _ := queryAdminStats(context.Background(), db, zap.NewNop())

	assert.Equal(t, 7, active)
	require.NoError(t, mock.ExpectationsWereMet())

	// And on a real database: the ledger house counts, the columns-only one does not.
	real := testDashDB(t)
	t.Cleanup(func() { _ = real.Close() })
	ledger := houseTenant(t, real, 0, 0)
	grantedHouse(t, real, ledger, payments.Order{Std: 1, Period: payments.PeriodMonthly}, time.Now().Add(24*time.Hour))
	cols := houseTenant(t, real, 0, 0)
	columnsOnlyHouse(t, real, cols)
	live, err := payments.LiveHouses(context.Background(), real)
	require.NoError(t, err)
	ids := map[string]bool{}
	for _, e := range live {
		ids[e.TenantID] = true
	}
	assert.True(t, ids[ledger])
	assert.False(t, ids[cols])
}

func planTemplate(t *testing.T) *template.Template {
	t.Helper()
	tmpl := template.Must(template.New("base").Parse(`{{define "base"}}{{block "content" .}}{{end}}{{end}}`))
	template.Must(tmpl.Parse(`{{define "content"}}plan={{.Plan}} status={{.SubscriptionStatus}} label={{.StatusLabel}} ` +
		`sub={{.HasSubscription}} has={{.House.HasHouse}}{{end}}`))
	return tmpl
}

func renderPlan(t *testing.T, db *sql.DB, tenantID string) string {
	t.Helper()
	h := HandleBilling(planTemplate(t), newFakeHouseBilling().registry(db, true), db, houseFlags(true), zap.NewNop())
	w := httptest.NewRecorder()
	h.ServeHTTP(w, injectSessionWithTenant(httptest.NewRequest("GET", "/dashboard/billing", nil), tenantID))
	require.Equal(t, 200, w.Code)
	return w.Body.String()
}

// RP21-3: after a lapse tenants.plan still says 'house'; the page reads the
// record and shows no house.
func TestBillingPage_LapsedHouseNotLive(t *testing.T) {
	// Arrange: a granted house, the columns as the webhook left them, then lapsed.
	db := testDashDB(t)
	t.Cleanup(func() { _ = db.Close() })
	id := houseTenant(t, db, 0, 0)
	grantedHouse(t, db, id, payments.Order{Std: 2, Period: payments.PeriodMonthly}, time.Now().Add(-30*24*time.Hour))
	_, err := db.Exec(`UPDATE tenants SET plan = 'house', subscription_status = 'active', stripe_subscription_id = 'sub_1',
	                   house_period = 'monthly' WHERE id = $1`, id)
	require.NoError(t, err)
	require.NoError(t, payments.InTx(context.Background(), db, func(tx *sql.Tx) error {
		return payments.Revoke(context.Background(), tx, id)
	}))

	// Act
	body := renderPlan(t, db, id)

	// Assert
	assert.Contains(t, body, "has=false")
	assert.Contains(t, body, "sub=false")
	assert.NotContains(t, body, "plan=house", "tenants.plan lies after a lapse")
	assert.NotContains(t, body, "label=Active")
}

// The pre-097 fallback is gone: tenants columns with no row are no house,
// and a granted row renders with no tenants column set.
func TestBillingPage_ReadsTheRecordOnly(t *testing.T) {
	db := testDashDB(t)
	t.Cleanup(func() { _ = db.Close() })
	cols := houseTenant(t, db, 0, 0)
	columnsOnlyHouse(t, db, cols)
	ledger := houseTenant(t, db, 0, 0)
	grantedHouse(t, db, ledger, payments.Order{Std: 3, Period: payments.PeriodAnnual}, time.Now().Add(100*24*time.Hour))

	colsBody := renderPlan(t, db, cols)
	ledgerBody := renderPlan(t, db, ledger)

	assert.Contains(t, colsBody, "has=false")
	assert.Contains(t, colsBody, "sub=false")
	assert.Contains(t, ledgerBody, "has=true")
	assert.Contains(t, ledgerBody, "sub=true")
	assert.Contains(t, ledgerBody, "plan=house")
	assert.Contains(t, ledgerBody, "label=Active")
}
