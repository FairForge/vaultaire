package handlers

import (
	"context"
	"database/sql"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/FairForge/vaultaire/internal/flags"
	"github.com/FairForge/vaultaire/internal/payments"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// Admin follow-through (dashboard plan Phase 4): launch lists from the
// waitlist, MRR by floor, the house on the tenants list, flag risk tiers.

func TestWaitlistExport_LaunchListFilters(t *testing.T) {
	db := testDashDB(t)
	t.Cleanup(func() { _ = db.Close() })
	stamp := fmt.Sprintf("%d", time.Now().UnixNano())
	for _, r := range []struct {
		email      string
		std, vault int
	}{{"house-" + stamp + "@t.local", 6, 1}, {"attic-" + stamp + "@t.local", 0, 2}, {"none-" + stamp + "@t.local", 0, 0}} {
		_, err := db.Exec(`INSERT INTO waitlist_signups (email, source, plan_std_tb, plan_vault_tb) VALUES ($1, 'test', $2, $3)`, r.email, r.std, r.vault)
		require.NoError(t, err)
	}
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM waitlist_signups WHERE email LIKE $1`, "%-"+stamp+"@t.local") })

	get := func(q string) string {
		w := httptest.NewRecorder()
		HandleAdminWaitlistExport(db, zap.NewNop()).ServeHTTP(w, httptest.NewRequest("GET", "/admin/waitlist/export"+q, nil).WithContext(adminCtx(t)))
		require.Equal(t, 200, w.Code, q)
		return w.Body.String()
	}
	all := get("")
	assert.Contains(t, all, "house-"+stamp)
	assert.Contains(t, all, "none-"+stamp)

	houses := get("?filter=house&fields=email")
	assert.Equal(t, "email\n", houses[:6], "one column")
	assert.Contains(t, houses, "house-"+stamp)
	assert.Contains(t, houses, "attic-"+stamp)
	assert.NotContains(t, houses, "none-"+stamp)
	assert.NotContains(t, houses, "test,", "no other columns")

	attic := get("?filter=attic")
	assert.Contains(t, attic, "attic-"+stamp)
	assert.Contains(t, attic, "house-"+stamp, "the 6+1 house has an attic too")
	assert.NotContains(t, attic, "none-"+stamp)

	down := get("?filter=downstairs")
	assert.Contains(t, down, "house-"+stamp)
	assert.NotContains(t, down, "attic-"+stamp)

	w := httptest.NewRecorder()
	HandleAdminWaitlistExport(db, zap.NewNop()).ServeHTTP(w, httptest.NewRequest("GET", "/admin/waitlist/export?filter=basement", nil).WithContext(adminCtx(t)))
	assert.Equal(t, 400, w.Code)
}

func TestRevenue_HouseMRRByFloor(t *testing.T) {
	db := testDashDB(t)
	t.Cleanup(func() { _ = db.Close() })
	soon := time.Now().Add(30 * 24 * time.Hour)
	annual := houseTenant(t, db, 0, 0)
	monthly := houseTenant(t, db, 0, 0)
	grantedHouse(t, db, annual, payments.Order{Std: 6, Vault: 1, Period: payments.PeriodAnnual}, soon)
	grantedHouse(t, db, monthly, payments.Order{Vault: 2, Period: payments.PeriodMonthly}, soon)
	// A lapsed house earns nothing.
	gone := houseTenant(t, db, 0, 0)
	grantedHouse(t, db, gone, payments.Order{Std: 50, Period: payments.PeriodAnnual}, soon)
	require.NoError(t, payments.InTx(context.Background(), db, func(tx *sql.Tx) error {
		return payments.Revoke(context.Background(), tx, gone)
	}))

	houses := queryHouses(context.Background(), db, zap.NewNop())
	total, lines := housesMRR(houses), houseRevenueLines(houses)
	// Other test tenants may hold houses too; check ours are counted correctly
	// by lower bounds and by the per-line shape.
	assert.GreaterOrEqual(t, total, int64(2894+510), "6×4.49+1×2.00 annual + 2×2.55 monthly")
	var std, vault int64
	var stdN, vaultN int
	for _, l := range lines {
		switch l.Tier {
		case "Standard":
			std, stdN = l.MRRCents, l.Count
		case "Vault":
			vault, vaultN = l.MRRCents, l.Count
		}
	}
	assert.GreaterOrEqual(t, std, int64(2694))
	assert.GreaterOrEqual(t, vault, int64(710))
	assert.GreaterOrEqual(t, stdN, 1)
	assert.GreaterOrEqual(t, vaultN, 2)

	rows := queryTenantList(context.Background(), db, annual, zap.NewNop())
	require.Len(t, rows, 1)
	assert.Equal(t, "6 TB downstairs · 1 TB attic", rows[0].House)
}

func TestAdminFlags_TiersAndOrder(t *testing.T) {
	svc := flags.New(nil, zap.NewNop())
	for _, k := range []string{"smart_demotion", "chunking", "zzz_custom", "signups", "quota_checkout", "house_overview"} {
		svc.Register(k, false)
	}
	views := flagViews(svc.Resolved())
	var keys []string
	for _, v := range views {
		keys = append(keys, v.Key)
	}
	assert.Equal(t, []string{"signups", "quota_checkout", "house_overview", "chunking", "smart_demotion", "zzz_custom"}, keys)
	assert.Equal(t, 2, views[1].Tier, "quota_checkout is billing: Tier 2")
	assert.Equal(t, 1, views[2].Tier)
	assert.True(t, strings.Contains(views[1].About, "STRIPE_PRICE"))
	assert.Equal(t, 0, views[5].Tier, "unknown flags carry no tier")
}
