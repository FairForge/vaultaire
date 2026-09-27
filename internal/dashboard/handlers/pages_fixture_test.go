package handlers

import (
	"html/template"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/FairForge/vaultaire/internal/auth"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// TestPages_WriteScreenshotFixtures renders the buckets and settings pages
// through the REAL templates for screenshots and Lighthouse
// (DASH_OUT=dir go test -run TestPages_WriteScreenshotFixtures). The
// overview and billing pages have their own fixture tests.
func TestPages_WriteScreenshotFixtures(t *testing.T) {
	out := os.Getenv("DASH_OUT")
	if out == "" {
		t.Skip("DASH_OUT not set")
	}
	require.NoError(t, os.MkdirAll(out, 0o755))
	db := testDashDB(t)
	t.Cleanup(func() { _ = db.Close() })
	id := houseTenant(t, db, 0, 0)

	page := func(name string) *template.Template {
		base := template.Must(template.ParseFS(os.DirFS("../templates"), "layouts/base.html"))
		tmpl := template.Must(base.Clone())
		template.Must(tmpl.ParseFS(os.DirFS("../templates"), "customer/"+name))
		return tmpl
	}
	write := func(name string, w *httptest.ResponseRecorder) {
		require.Equal(t, 200, w.Code, name)
		html := strings.ReplaceAll(w.Body.String(), `"/static/`, `"static/`)
		require.NoError(t, os.WriteFile(filepath.Join(out, name+".html"), []byte(html), 0o644))
	}

	// Buckets: two rooms, one in the attic.
	for _, b := range []struct{ name, tier, vis string }{{"photos", "standard", "private"}, {"cold-archive", "archive", "private"}, {"site-assets", "auto", "public-read"}} {
		_, err := db.Exec(`INSERT INTO buckets (tenant_id, name, tier_preference, visibility) VALUES ($1, $2, $3, $4)`, id, b.name, b.tier, b.vis)
		require.NoError(t, err)
	}
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM buckets WHERE tenant_id = $1`, id) })
	w := httptest.NewRecorder()
	HandleBuckets(page("buckets.html"), db, t.TempDir(), zap.NewNop()).ServeHTTP(w,
		injectSessionWithTenant(httptest.NewRequest("GET", "/dashboard/buckets", nil), id))
	write("buckets", w)

	// Settings.
	w = httptest.NewRecorder()
	HandleSettings(page("settings.html"), auth.NewAuthService(nil, db), db, nil, zap.NewNop()).ServeHTTP(w,
		injectSessionWithTenant(httptest.NewRequest("GET", "/dashboard/settings", nil), id))
	write("settings", w)
}
