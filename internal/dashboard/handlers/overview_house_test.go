package handlers

import (
	"context"
	"html/template"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/FairForge/vaultaire/internal/flags"
	"github.com/FairForge/vaultaire/internal/usage"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// The house on the overview (Phase 2), rendered through the REAL templates
// (layout + dashboard page + the generated sprites/room/CSS) behind the
// house_overview flag.

func overviewTemplate(t *testing.T) *template.Template {
	t.Helper()
	base := template.Must(template.New("").Funcs(TemplateFuncs()).ParseFS(os.DirFS("../templates"), "layouts/base.html"))
	tmpl := template.Must(base.Clone())
	template.Must(tmpl.ParseFS(os.DirFS("../templates"), "customer/dashboard.html", "generated/house.html"))
	return tmpl
}

func houseOverviewFlags(on bool) *flags.Service {
	svc := flags.New(nil, nil)
	svc.Register(FlagHouseOverview, on)
	return svc
}

func TestOverview_HouseRendersFromLiveData(t *testing.T) {
	db := testDashDB(t)
	t.Cleanup(func() { _ = db.Close() })
	id := houseTenant(t, db, 0, 0)
	qm := usage.NewQuotaManager(db)
	require.NoError(t, qm.SetHouse(context.Background(), id, usage.HouseFromTB(6, 1, 0)))
	_, err := db.Exec(`UPDATE tenants SET intent_room = 'v2.oat.noir.Casa.box-1-1' WHERE id = $1`, id)
	require.NoError(t, err)
	for _, o := range []struct {
		bucket, key, floor string
		size               int64
	}{
		{"photos", "a.jpg", "standard", 3*usage.TB + usage.TB/5},
		{"backups", "b.tar", "standard", 1 * usage.TB},
		{"cold", "c.bin", "vault", usage.TB * 9 / 10},
	} {
		_, err := db.Exec(`INSERT INTO object_head_cache (tenant_id, bucket, object_key, size_bytes, etag, floor) VALUES ($1, $2, $3, $4, 'e', $5)`,
			id, o.bucket, o.key, o.size, o.floor)
		require.NoError(t, err)
	}
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM object_head_cache WHERE tenant_id = $1`, id) })
	require.NoError(t, qm.ReconcileTenantStorageUsage(context.Background(), id))

	h := HandleOverview(overviewTemplate(t), db, zap.NewNop(), "local", houseOverviewFlags(true), nil)
	req := injectSessionWithTenant(httptest.NewRequest("GET", "/dashboard/", nil), id)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	require.Equal(t, 200, w.Code)
	body := w.Body.String()

	assert.Contains(t, body, `class="house-room wall-oat fit-noir"`, "the vibe from the saved room")
	assert.Contains(t, body, `>Casa<`, "the sign on the wall")
	assert.Contains(t, body, `id="s-dresser"`, "sprite defs are on the page")
	assert.Contains(t, body, `href="/dashboard/buckets/photos"`, "a piece links to the bucket filling it")
	assert.Contains(t, body, `href="/dashboard/buckets/cold"`)
	assert.Contains(t, body, `photos, backups · 4.2 TB of 5 TB downstairs`)
	assert.Contains(t, body, `4.2 TB of 6 TB downstairs`)
	assert.Contains(t, body, `0.9 TB of 1 TB in the attic`)
	assert.Contains(t, body, `clip-path="url(#hs-clip-`, "fill level is drawn as a clip")
	assert.Contains(t, body, `class="hs-piece nf-`, "night tint classes")
	assert.Contains(t, body, `url(#lampfill)`, "the lamp's night pools")
	assert.Contains(t, body, `id="nightdim"`)
	assert.Contains(t, body, `/dashboard/billing?add=downstairs`)
	assert.Contains(t, body, `free this month, then rate-limited to`, "egress bar")
	assert.Contains(t, body, `of 4 TB free this month`, "egress allowance = 0.5×6 TB + 1 TB")
	assert.NotContains(t, body, `ZgotmplZ`, "no html/template escaping accidents")
	assert.NotContains(t, body, `Storage Used`, "fullness cards replace the gauge")

	// Flag off: the classic page, no house.
	h = HandleOverview(overviewTemplate(t), db, zap.NewNop(), "local", houseOverviewFlags(false), nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	require.Equal(t, 200, w.Code)
	assert.NotContains(t, w.Body.String(), `house-svg`)
	assert.Contains(t, w.Body.String(), `Storage Used`)
	assert.Contains(t, w.Body.String(), `free this month, then rate-limited to`, "the egress bar is not gated")
}

func TestOverview_LegacyTenantHouse(t *testing.T) {
	db := testDashDB(t)
	t.Cleanup(func() { _ = db.Close() })
	id := houseTenant(t, db, 0, 0) // free tier: 5 GB, no floor rows
	h := HandleOverview(overviewTemplate(t), db, zap.NewNop(), "local", houseOverviewFlags(true), nil)
	req := injectSessionWithTenant(httptest.NewRequest("GET", "/dashboard/", nil), id)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	require.Equal(t, 200, w.Code)
	body := w.Body.String()
	assert.Contains(t, body, `>5 GB<`, "one honest box for the free tier")
	assert.Contains(t, body, `0 B of 5 GB downstairs`)
	assert.Contains(t, body, `no attic`)
	assert.Contains(t, body, `2.5 GB`, "egress allowance = half the total")
}

// TestOverview_WriteScreenshotFixtures renders the overview with the house
// for screenshots (DASH_OUT=dir go test -run TestOverview_WriteScreenshotFixtures).
func TestOverview_WriteScreenshotFixtures(t *testing.T) {
	out := os.Getenv("DASH_OUT")
	if out == "" {
		t.Skip("DASH_OUT not set")
	}
	require.NoError(t, os.MkdirAll(out, 0o755))
	db := testDashDB(t)
	t.Cleanup(func() { _ = db.Close() })
	id := houseTenant(t, db, 0, 0)
	qm := usage.NewQuotaManager(db)
	require.NoError(t, qm.SetHouse(context.Background(), id, usage.HouseFromTB(6, 1, 0)))
	_, err := db.Exec(`UPDATE tenants SET intent_room = 'v2.midnight.midnight.my%20place.box-1-1' WHERE id = $1`, id)
	require.NoError(t, err)
	for _, o := range []struct {
		bucket, key, floor string
		size               int64
	}{
		{"photos", "a.jpg", "standard", 3*usage.TB + usage.TB/5},
		{"backups", "b.tar", "standard", 1 * usage.TB},
		{"cold", "c.bin", "vault", usage.TB * 9 / 10},
	} {
		_, err := db.Exec(`INSERT INTO object_head_cache (tenant_id, bucket, object_key, size_bytes, etag, floor) VALUES ($1, $2, $3, $4, 'e', $5)`,
			id, o.bucket, o.key, o.size, o.floor)
		require.NoError(t, err)
	}
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM object_head_cache WHERE tenant_id = $1`, id) })
	require.NoError(t, qm.ReconcileTenantStorageUsage(context.Background(), id))

	h := HandleOverview(overviewTemplate(t), db, zap.NewNop(), "local", houseOverviewFlags(true), nil)
	req := injectSessionWithTenant(httptest.NewRequest("GET", "/dashboard/", nil), id)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	require.Equal(t, 200, w.Code)
	html := strings.ReplaceAll(w.Body.String(), `"/static/`, `"static/`)
	require.NoError(t, os.WriteFile(filepath.Join(out, "overview-house.html"), []byte(html), 0o644))
}
