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

// WP-R13-3: the System page lists job_runs — what GET /api/v1/admin/jobs
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
