package handlers

import (
	"context"
	"fmt"
	"html/template"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/FairForge/vaultaire/internal/flags"
	"github.com/FairForge/vaultaire/internal/usage"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// TestAdmin_WriteScreenshotFixtures renders the admin flags, waitlist and
// tenants pages through the REAL admin layout for screenshots
// (DASH_OUT=dir go test -run TestAdmin_WriteScreenshotFixtures).
func TestAdmin_WriteScreenshotFixtures(t *testing.T) {
	out := os.Getenv("DASH_OUT")
	if out == "" {
		t.Skip("DASH_OUT not set")
	}
	require.NoError(t, os.MkdirAll(out, 0o755))
	db := testDashDB(t)
	t.Cleanup(func() { _ = db.Close() })

	page := func(name string) *template.Template {
		return template.Must(template.New("").Funcs(TemplateFuncs()).ParseFS(os.DirFS("../templates"), "layouts/admin.html", "admin/"+name))
	}
	write := func(name string, w *httptest.ResponseRecorder) {
		require.Equal(t, 200, w.Code, name)
		html := strings.ReplaceAll(w.Body.String(), `"/static/`, `"static/`)
		require.NoError(t, os.WriteFile(filepath.Join(out, "admin-"+name+".html"), []byte(html), 0o644))
	}

	svc := flags.New(nil, zap.NewNop())
	for _, k := range []string{"signups", "quota_checkout", "house_overview", "chunking", "smart_demotion"} {
		svc.Register(k, k == "signups" || k == "chunking")
	}
	w := httptest.NewRecorder()
	HandleAdminFlags(page("flags.html"), svc, zap.NewNop()).ServeHTTP(w, httptest.NewRequest("GET", "/admin/flags", nil).WithContext(adminCtx(t)))
	write("flags", w)

	stamp := fmt.Sprintf("%d", time.Now().UnixNano())
	for _, r := range []struct {
		email      string
		std, vault int
	}{{"ana-" + stamp + "@example.com", 6, 1}, {"ben-" + stamp + "@example.com", 0, 2}, {"cy-" + stamp + "@example.com", 0, 0}} {
		_, err := db.Exec(`INSERT INTO waitlist_signups (email, source, plan_std_tb, plan_vault_tb) VALUES ($1, 'landing', $2, $3)`, r.email, r.std, r.vault)
		require.NoError(t, err)
	}
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM waitlist_signups WHERE email LIKE $1`, "%-"+stamp+"@example.com") })
	w = httptest.NewRecorder()
	HandleAdminWaitlist(page("waitlist.html"), db, zap.NewNop()).ServeHTTP(w, httptest.NewRequest("GET", "/admin/waitlist", nil).WithContext(adminCtx(t)))
	write("waitlist", w)

	// traffic: a few days of cookieless aggregates, keyed by a fixture referrer
	ref := "fixture-" + stamp + ".example"
	for _, r := range []struct {
		ago            int
		kind, name, cc string
		n              int
	}{{0, "view", "/", "DE", 41}, {0, "view", "/docs/rclone", "US", 9}, {0, "event", "builder.edit", "DE", 12}, {1, "view", "/", "GB", 33}, {2, "view", "/", "FR", 27}} {
		_, err := db.Exec(`INSERT INTO site_stats_daily (day, kind, name, referrer, utm_source, utm_medium, utm_campaign, country, device, n)
			VALUES (CURRENT_DATE - $1::int, $2, $3, $4, 'let', '', 'launch', $5, 'desktop', $6)
			ON CONFLICT (day, kind, name, referrer, utm_source, utm_medium, utm_campaign, country, device) DO UPDATE SET n = EXCLUDED.n`,
			r.ago, r.kind, r.name, ref, r.cc, r.n)
		require.NoError(t, err)
	}
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM site_stats_daily WHERE referrer = $1`, ref) })
	w = httptest.NewRecorder()
	HandleAdminStats(page("stats.html"), db, zap.NewNop()).ServeHTTP(w, httptest.NewRequest("GET", "/admin/stats", nil).WithContext(adminCtx(t)))
	write("stats", w)

	id := houseTenant(t, db, 0, 0)
	require.NoError(t, usage.NewQuotaManager(db).SetHouse(context.Background(), id, usage.HouseFromTB(6, 1, 0)))
	_, err := db.Exec(`UPDATE tenants SET plan = 'house', subscription_status = 'active', house_period = 'annual', name = 'Casa' WHERE id = $1`, id)
	require.NoError(t, err)
	w = httptest.NewRecorder()
	HandleTenantList(page("tenants.html"), db, zap.NewNop()).ServeHTTP(w, httptest.NewRequest("GET", "/admin/tenants?q="+id, nil).WithContext(adminCtx(t)))
	write("tenants", w)
}
