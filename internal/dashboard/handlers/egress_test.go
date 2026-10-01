package handlers

import (
	"context"
	"html/template"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/FairForge/vaultaire/internal/api/landing"
	"github.com/FairForge/vaultaire/internal/usage"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// The egress bar and the admin tenant page read the one allowance and the
// one month counter the throttle uses (WP-R10-9), through the real templates.

type fakeEgressReader struct {
	st    usage.EgressStatus
	asked []string
}

func (f *fakeEgressReader) EgressStatus(_ context.Context, tenantID string) (usage.EgressStatus, error) {
	f.asked = append(f.asked, tenantID)
	return f.st, nil
}

func renderOverview(t *testing.T, tenantID string, eg usage.EgressStatusReader) string {
	t.Helper()
	db := testDashDB(t)
	t.Cleanup(func() { _ = db.Close() })
	h := HandleOverview(overviewTemplate(t), db, zap.NewNop(), "local", houseOverviewFlags(false), eg)
	req := injectSessionWithTenant(httptest.NewRequest("GET", "/dashboard/", nil), tenantID)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	require.Equal(t, 200, w.Code)
	return w.Body.String()
}

func TestOverview_EgressBar_UnderTheAllowance(t *testing.T) {
	// Arrange: a free-tier tenant, no live counter wired — the recorded month.
	db := testDashDB(t)
	t.Cleanup(func() { _ = db.Close() })
	id := houseTenant(t, db, 0, 0)
	_, err := db.Exec(`INSERT INTO bandwidth_usage_daily (tenant_id, date, ingress_bytes, egress_bytes, requests_count)
		VALUES ($1, (NOW() AT TIME ZONE 'UTC')::date, $2, $3, 1)`, id, 900*usage.GB, usage.GB)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM bandwidth_usage_daily WHERE tenant_id = $1`, id) })

	// Act
	body := renderOverview(t, id, nil)

	// Assert: 1 GiB of the 2.5 GiB allowance; 900 GiB of uploads count for nothing.
	assert.Contains(t, body, `40% of 2.5 GB free this month, then rate-limited to 64 KB/s`)
	assert.Contains(t, body, `never billed`)
	assert.NotContains(t, body, `then slower`)
	assert.NotContains(t, body, `are rate-limited to`)
}

func TestOverview_EgressBar_ShowsTheThrottledStateWithRateAndResetDate(t *testing.T) {
	// Arrange: the server's counter says this tenant is being paced.
	db := testDashDB(t)
	t.Cleanup(func() { _ = db.Close() })
	id := houseTenant(t, db, 0, 0)
	eg := &fakeEgressReader{st: usage.EgressStatus{
		Allowance:       usage.EgressAllowance{Bytes: 4 * usage.TB, PlanBytes: 4 * usage.TB, Source: usage.EgressSourceHouse},
		UsedBytes:       5 * usage.TB,
		Over:            true,
		Throttled:       true,
		RateBytesPerSec: usage.DefaultEgressThrottle().Rate(4 * usage.TB),
		ResetAt:         time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC),
	}}

	// Act
	body := renderOverview(t, id, eg)

	// Assert
	assert.Equal(t, []string{id}, eg.asked)
	assert.Contains(t, body, `All 4 TB of this month's egress allowance is used: downloads are rate-limited to 1.6 MB/s until November 1 (UTC)`)
	assert.Contains(t, body, `never billed`)
	assert.Contains(t, body, `width: 100%`)
	assert.NotContains(t, body, `never refused`)

	// Over the allowance but not paced (the flag is off for the tenant):
	// the page must not say downloads are slowed.
	eg.st.Throttled = false
	body = renderOverview(t, id, eg)
	assert.NotContains(t, body, `are rate-limited to`)
	assert.Contains(t, body, `100% of 4 TB free this month`)
}

func adminTenantDetailTemplate(t *testing.T) *template.Template {
	t.Helper()
	return template.Must(template.New("").Funcs(TemplateFuncs()).ParseFS(os.DirFS("../templates"),
		"layouts/admin.html", "admin/tenant_detail.html"))
}

func TestTenantDetail_ShowsEgressUsedAllowanceOverrideAndState(t *testing.T) {
	// Arrange: a house of 6 + 1 TB (plan allowance 4 TB) under an admin
	// override of 100 GB, with 150 GB downloaded this month.
	db := testDashDB(t)
	t.Cleanup(func() { _ = db.Close() })
	id := houseTenant(t, db, 0, 0)
	require.NoError(t, usage.NewQuotaManager(db).SetHouse(context.Background(), id, usage.HouseFromTB(6, 1, 0)))
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM tenant_floor_quotas WHERE tenant_id = $1`, id) })
	_, err := db.Exec(`UPDATE tenant_quotas SET bandwidth_limit_bytes = $2 WHERE tenant_id = $1`, id, 100*usage.GB)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO bandwidth_usage_daily (tenant_id, date, ingress_bytes, egress_bytes, requests_count)
		VALUES ($1, (NOW() AT TIME ZONE 'UTC')::date, 0, $2, 1)`, id, 150*usage.GB)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM bandwidth_usage_daily WHERE tenant_id = $1`, id) })
	h := HandleTenantDetail(adminTenantDetailTemplate(t), db, zap.NewNop(), nil)
	req := httptest.NewRequest("GET", "/admin/tenants/"+id, nil).WithContext(withChiParam(adminCtx(t), "id", id))
	w := httptest.NewRecorder()

	// Act
	h.ServeHTTP(w, req)

	// Assert
	require.Equal(t, 200, w.Code)
	body := w.Body.String()
	assert.Contains(t, body, `Egress allowance`)
	assert.Contains(t, body, `150 GB of 100 GB`)
	assert.Contains(t, body, `Override: 100 GB`)
	assert.Contains(t, body, `Plan: 4 TB`)
	assert.Contains(t, body, `over, not slowed (egress_throttle is off for this tenant)`)
	assert.Contains(t, body, `0 = the plan's allowance`)
	assert.Contains(t, body, `value="100"`)
	assert.NotContains(t, body, `unlimited`)
	assert.NotContains(t, body, `Unlimited`)
}

// The terms page states the egress rule in the price file's own sentences.
func TestLegalTerms_EgressRuleComesFromThePriceFile(t *testing.T) {
	// Arrange: the real terms template.
	tmpl := template.Must(template.New("").Funcs(TemplateFuncs()).ParseFS(os.DirFS("../templates"),
		"layouts/base.html", "legal/terms.html"))
	w := httptest.NewRecorder()

	// Act
	HandleLegalPage(tmpl).ServeHTTP(w, httptest.NewRequest("GET", "/legal/terms", nil))

	// Assert
	require.Equal(t, 200, w.Code)
	body := w.Body.String()
	e := landing.Get().Egress
	assert.Contains(t, body, template.HTMLEscapeString(e.PastAllowance))
	assert.Contains(t, body, template.HTMLEscapeString(e.OneAllowance))
	assert.NotContains(t, body, "may be throttled")
	assert.NotContains(t, body, "are queued")
	assert.NotContains(t, body, "never refused")
}
