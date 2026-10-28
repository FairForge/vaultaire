package handlers

import (
	"database/sql"
	"html/template"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/FairForge/vaultaire/internal/testutil"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func testAdminSystemTemplate(t *testing.T) *template.Template {
	t.Helper()
	tmpl := template.Must(template.New("admin").Parse(
		`{{define "admin"}}{{block "content" .}}{{end}}{{end}}`))
	template.Must(tmpl.Parse(
		`{{define "title"}}System{{end}}` +
			`{{define "content"}}` +
			`<h1>System Health</h1>` +
			`<span class="goroutines">{{.Goroutines}}</span>` +
			`<span class="memory">{{.MemAllocFmt}}</span>` +
			`<span class="gc">{{.NumGC}}</span>` +
			`{{end}}`))
	return tmpl
}

func TestHandleAdminSystem_NoSession(t *testing.T) {
	handler := HandleAdminSystem(testAdminSystemTemplate(t), nil, zap.NewNop())

	req := httptest.NewRequest("GET", "/admin/system", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusSeeOther, w.Code)
	assert.Equal(t, "/login", w.Header().Get("Location"))
}

func TestHandleAdminSystem_AdminSession(t *testing.T) {
	handler := HandleAdminSystem(testAdminSystemTemplate(t), nil, zap.NewNop())

	req := httptest.NewRequest("GET", "/admin/system", nil)
	req = req.WithContext(adminCtx(t))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	body := w.Body.String()
	assert.Contains(t, body, "System Health")
	// Runtime stats should always have values.
	assert.NotContains(t, body, `<span class="goroutines">0</span>`)
	assert.NotContains(t, body, `<span class="memory">0 B</span>`)
}

// WP-R13-3: the System page lists job_runs, what GET /api/v1/admin/jobs
// returns, for an operator without a JWT at hand.
func TestHandleAdminSystem_ListsBackgroundJobs(t *testing.T) {
	// Arrange: a job of this test's own (job_runs is shared by every package).
	db, err := sql.Open("postgres", testutil.DSN())
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Ping(); err != nil {
		t.Skipf("no test database: %v", err)
	}
	job := "test_system_page_" + uuid.New().String()[:8]
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM job_runs WHERE job = $1`, job) })
	_, err = db.Exec(`INSERT INTO job_runs (job, last_started_at, last_success_at, last_outcome, last_error, rows_affected)
		VALUES ($1, NOW() - INTERVAL '2 minutes', NOW() - INTERVAL '26 hours', 'error', 'could not list tenants', 7)`, job)
	require.NoError(t, err)
	tmpl := template.Must(template.New("admin").Parse(`{{define "admin"}}{{block "content" .}}{{end}}{{end}}`))
	template.Must(tmpl.Parse(`{{define "content"}}{{range .Jobs}}[{{.Job}}|{{.Outcome}}|{{.Rows}}|{{.Detail}}|{{.LastSuccess}}]{{end}}{{end}}`))

	// Act
	req := httptest.NewRequest("GET", "/admin/system", nil).WithContext(adminCtx(t))
	w := httptest.NewRecorder()
	HandleAdminSystem(tmpl, db, zap.NewNop()).ServeHTTP(w, req)

	// Assert
	require.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "["+job+"|error|7|could not list tenants|")
	assert.NotContains(t, w.Body.String(), job+"|error|7|could not list tenants|never", "the last success is shown")
}

// WP-R7-5: the Routing Truth line reads the routing_truth job's last run from
// job_runs (its note and the unknown-backend rows in its JSON result), so it
// is right after a restart.
func TestHandleAdminSystem_ShowsTheRoutingTruthLine(t *testing.T) {
	// Arrange: a routing_truth row as the job writes it. The job name is the
	// production one, the page reads exactly that row, so the row is
	// written only when no other process holds one, and restored after.
	db, err := sql.Open("postgres", testutil.DSN())
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Ping(); err != nil {
		t.Skipf("no test database: %v", err)
	}
	// A live server on this database may own the row: it is set aside for
	// the test and put back afterwards.
	_, err = db.Exec(`UPDATE job_runs SET job = 'routing_truth__set_aside' WHERE job = 'routing_truth'`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM job_runs WHERE job = 'routing_truth'`)
		_, _ = db.Exec(`UPDATE job_runs SET job = 'routing_truth' WHERE job = 'routing_truth__set_aside'`)
	})
	_, err = db.Exec(`INSERT INTO job_runs (job, last_started_at, last_success_at, last_outcome, last_error, rows_affected, result)
		VALUES ('routing_truth', NOW() - INTERVAL '5 minutes', NOW() - INTERVAL '5 minutes', 'ok',
		        'idrive: 300 of 2037 rows sampled, 12 present, 288 missing; rows on no registered backend: (NULL) 62, onedrive 2613', 300,
		        '{"unknown": {"": 62, "onedrive": 2613}, "summary": "…"}'::jsonb)`)
	require.NoError(t, err)
	tmpl := template.Must(template.New("admin").Parse(`{{define "admin"}}{{block "content" .}}{{end}}{{end}}`))
	template.Must(tmpl.Parse(`{{define "content"}}{{with .Routing}}[{{.Outcome}}|{{.LastRun}}|{{.Summary}}|{{.UnknownRows}}|{{.Unknown}}]{{end}}{{end}}`))

	// Act
	req := httptest.NewRequest("GET", "/admin/system", nil).WithContext(adminCtx(t))
	w := httptest.NewRecorder()
	HandleAdminSystem(tmpl, db, zap.NewNop()).ServeHTTP(w, req)

	// Assert
	require.Equal(t, http.StatusOK, w.Code)
	body := w.Body.String()
	assert.Contains(t, body, "[ok|")
	assert.Contains(t, body, "288 missing")
	assert.Contains(t, body, "|2675|(NULL) 62, onedrive 2613]")
}

// WP-EDGE-3 step 13: the Data-plane hosts card lists the registry — host,
// kind, health, up (a node: a heartbeat within 90 s), last seen, version,
// and whether the host is tenant-scoped — never which tenant (the System
// page names no customer).
func TestHandleAdminSystem_ShowsTheDataPlaneHosts(t *testing.T) {
	db, err := sql.Open("postgres", testutil.DSN())
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Ping(); err != nil {
		t.Skipf("no test database: %v", err)
	}
	node := "card-" + uuid.New().String()[:8] + ".lab.test"
	tenantID := "card-" + uuid.New().String()[:8]
	_, err = db.Exec(`INSERT INTO tenants (id, name, email) VALUES ($1, 'Card', $2)`, tenantID, tenantID+"@test.local")
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO data_plane_hosts (host, kind, address, token_hash, tenant_id, weight, health, last_seen, version)
		VALUES ($1, 'node', '198.51.100.99', $2, $3, 1, 'up', NOW() - INTERVAL '2 minutes', 'abc1234')`, node, "h-"+node, tenantID)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM data_plane_hosts WHERE host = $1`, node)
		_, _ = db.Exec(`DELETE FROM tenants WHERE id = $1`, tenantID)
	})
	tmpl := template.Must(template.New("admin").Parse(`{{define "admin"}}{{block "content" .}}{{end}}{{end}}`))
	template.Must(tmpl.Parse(`{{define "content"}}{{range .DataPlaneHosts}}[{{.Host}}|{{.Kind}}|{{.Health}}|{{.Up}}|{{.TenantScoped}}|{{.Version}}]{{end}}{{end}}`))

	req := httptest.NewRequest("GET", "/admin/system", nil).WithContext(adminCtx(t))
	w := httptest.NewRecorder()
	HandleAdminSystem(tmpl, db, zap.NewNop()).ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	body := w.Body.String()
	assert.Contains(t, body, "["+node+"|node|up|false|true|abc1234]", "up in the registry, no heartbeat for 2 min: not up")
	assert.Contains(t, body, "[s3.stored.cloud|edge|up|true|false|]")
	assert.NotContains(t, body, tenantID, "never which tenant")
}
