package handlers

import (
	"fmt"
	"html/template"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func statsTemplate(t *testing.T) *template.Template {
	t.Helper()
	return template.Must(template.New("").Funcs(TemplateFuncs()).ParseFS(os.DirFS("../templates"), "layouts/admin.html", "admin/stats.html"))
}

// The traffic page reads the cookieless aggregates back: a day row, the
// top tables and the funnel — against the migrated schema, since sqlmock
// cannot tell a phantom column from a real one (R9).
func TestHandleAdminStats_RendersAggregates(t *testing.T) {
	db := testDashDB(t)
	t.Cleanup(func() { _ = db.Close() })
	ref := fmt.Sprintf("stats-test-%d.example", time.Now().UnixNano())
	// The collector and the page both count UTC days. CURRENT_DATE is the
	// session's local date, so seeding with it failed every evening west of
	// Greenwich (and never in CI, which runs in UTC).
	today := time.Now().UTC().Format("2006-01-02")
	_, err := db.Exec(`INSERT INTO site_stats_daily (day, kind, name, referrer, utm_source, utm_medium, utm_campaign, country, device, n)
		VALUES ($2::date, 'view', '/', $1, 'lowendtalk', '', '', 'DE', 'phone', 7)`, ref, today)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO site_stats_daily (day, kind, name, referrer, utm_source, utm_medium, utm_campaign, country, device, n)
		VALUES ($2::date, 'event', 'builder.attic', $1, '', '', '', 'DE', 'phone', 3)`, ref, today)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM site_stats_daily WHERE referrer = $1`, ref) })

	w := httptest.NewRecorder()
	HandleAdminStats(statsTemplate(t), db, zap.NewNop()).ServeHTTP(w, httptest.NewRequest("GET", "/admin/stats", nil).WithContext(adminCtx(t)))

	require.Equal(t, 200, w.Code)
	body := w.Body.String()
	assert.Contains(t, body, "<h1>Traffic</h1>")
	assert.Contains(t, body, ref, "the referrer appears in the top table")
	assert.Contains(t, body, "builder.attic")
	assert.Contains(t, body, today, "today's row is listed")
	assert.Contains(t, body, "Funnel (all time)")
	assert.Contains(t, body, `href="/admin/stats" class="sidebar-link sidebar-link--active"`)
}

func TestHandleAdminStats_NilDBAndNoSession(t *testing.T) {
	w := httptest.NewRecorder()
	HandleAdminStats(statsTemplate(t), nil, zap.NewNop()).ServeHTTP(w, httptest.NewRequest("GET", "/admin/stats", nil).WithContext(adminCtx(t)))
	require.Equal(t, 200, w.Code)
	assert.Contains(t, w.Body.String(), "No traffic recorded yet")

	w = httptest.NewRecorder()
	HandleAdminStats(statsTemplate(t), nil, zap.NewNop()).ServeHTTP(w, httptest.NewRequest("GET", "/admin/stats", nil))
	assert.Equal(t, 303, w.Code)
	assert.Equal(t, "/login", w.Header().Get("Location"))
}
