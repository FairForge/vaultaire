package handlers

import (
	"context"
	"html/template"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/FairForge/vaultaire/internal/usage"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// TestHouse_WriteScreenshotFixtures renders the REAL billing page (layout +
// template) for the house states and writes them under $DASH_OUT so they
// can be screenshotted / Lighthoused without a running server or a login:
//
//	DASH_OUT=/tmp/dash go test ./internal/dashboard/handlers -run TestHouse_WriteScreenshotFixtures
//
// Static asset links are rewritten to be relative (copy static/ next to
// the output). Skipped unless DASH_OUT is set.
func TestHouse_WriteScreenshotFixtures(t *testing.T) {
	out := os.Getenv("DASH_OUT")
	if out == "" {
		t.Skip("DASH_OUT not set")
	}
	require.NoError(t, os.MkdirAll(out, 0o755))
	db := testDashDB(t)
	t.Cleanup(func() { _ = db.Close() })

	base := template.Must(template.New("").Funcs(TemplateFuncs()).ParseFS(os.DirFS("../templates"), "layouts/base.html"))
	tmpl := template.Must(base.Clone())
	template.Must(tmpl.ParseFS(os.DirFS("../templates"), "customer/billing.html"))

	render := func(name, tenantID string, fb *fakeHouseBilling, query string) {
		h := HandleBilling(tmpl, fb, db, houseFlags(true), zap.NewNop())
		req := injectSessionWithTenant(httptest.NewRequest("GET", "/dashboard/billing"+query, nil), tenantID)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		require.Equal(t, 200, w.Code, name)
		html := strings.ReplaceAll(w.Body.String(), `"/static/`, `"static/`)
		require.NoError(t, os.WriteFile(filepath.Join(out, name+".html"), []byte(html), 0o644))
	}

	// 1. Fresh tenant arriving with the site's house (6 TB downstairs, 1 TB attic), prices verified.
	fresh := houseTenant(t, db, 6, 1)
	render("billing-house-new", fresh, newFakeHouseBilling(), "")

	// 2. Same, but the Stripe prices are not verified yet (what ships dark).
	fb := newFakeHouseBilling()
	fb.ready, fb.why = false, "house prices not verified yet"
	render("billing-house-not-ready", fresh, fb, "")

	// 3. An existing monthly house being resized, with usage on both floors.
	owner := houseTenant(t, db, 0, 0)
	qm := usage.NewQuotaManager(db)
	require.NoError(t, qm.SetHouse(context.Background(), owner, usage.HouseFromTB(3, 2, 1)))
	_, err := db.Exec(`UPDATE tenants SET plan = 'house', subscription_status = 'active', stripe_subscription_id = 'sub_fixture', house_period = 'monthly' WHERE id = $1`, owner)
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE tenant_floor_quotas SET storage_used_bytes = CASE floor WHEN 'standard' THEN $1::bigint ELSE $2::bigint END WHERE tenant_id = $3`,
		3*usage.TB/2, usage.TB/4, owner)
	require.NoError(t, err)
	render("billing-house-resize", owner, newFakeHouseBilling(), "?resized=1")
}
