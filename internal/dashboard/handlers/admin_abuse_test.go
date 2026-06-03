package handlers

import (
	"html/template"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func testAdminAbuseTemplate(t *testing.T) *template.Template {
	t.Helper()
	tmpl := template.Must(template.New("admin").Parse(
		`{{define "admin"}}{{block "content" .}}{{end}}{{end}}`))
	template.Must(tmpl.Parse(
		`{{define "title"}}Abuse{{end}}` +
			`{{define "content"}}` +
			`{{range .Reports}}<div class="report">{{.ReporterEmail}} {{.ReportType}} {{.Status}}</div>{{end}}` +
			`{{if not .Reports}}<p>No reports found.</p>{{end}}` +
			`{{end}}`))
	return tmpl
}

func testAbusePublicTemplate(t *testing.T) *template.Template {
	t.Helper()
	return template.Must(template.New("base").Parse(
		`{{define "base"}}` +
			`{{if .Submitted}}<p>Thank you</p>{{end}}` +
			`{{if .Error}}<p class="error">{{.Error}}</p>{{end}}` +
			`{{if .ReporterEmail}}<span class="email">{{.ReporterEmail}}</span>{{end}}` +
			`<form></form>` +
			`{{end}}`))
}

var abuseCols = []string{"id", "reporter_email", "report_type", "url", "status", "tenant_id", "created_at"}

func TestHandleAdminAbuse_NoSession(t *testing.T) {
	handler := HandleAdminAbuse(testAdminAbuseTemplate(t), nil, zap.NewNop())
	req := httptest.NewRequest("GET", "/admin/abuse", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusSeeOther, w.Code)
	assert.Equal(t, "/login", w.Header().Get("Location"))
}

func TestHandleAdminAbuse_ListsReports(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	rows := sqlmock.NewRows(abuseCols).
		AddRow(1, "alice@test.com", "copyright", "https://example.com/file", "open", nil, time.Now()).
		AddRow(2, "bob@test.com", "malware", "", "open", nil, time.Now())

	mock.ExpectQuery(`SELECT id, reporter_email, report_type, url, status, tenant_id, created_at`).
		WithArgs("open").
		WillReturnRows(rows)

	handler := HandleAdminAbuse(testAdminAbuseTemplate(t), db, zap.NewNop())
	req := httptest.NewRequest("GET", "/admin/abuse", nil)
	req = req.WithContext(adminCtx(t))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	body := w.Body.String()
	assert.Contains(t, body, "alice@test.com")
	assert.Contains(t, body, "bob@test.com")
	assert.Contains(t, body, "copyright")
	assert.Contains(t, body, "malware")
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestHandleAdminAbuse_EmptyState(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	rows := sqlmock.NewRows(abuseCols)
	mock.ExpectQuery(`SELECT id, reporter_email, report_type, url, status, tenant_id, created_at`).
		WithArgs("open").
		WillReturnRows(rows)

	handler := HandleAdminAbuse(testAdminAbuseTemplate(t), db, zap.NewNop())
	req := httptest.NewRequest("GET", "/admin/abuse", nil)
	req = req.WithContext(adminCtx(t))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "No reports found.")
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestHandleAdminAbuse_FilterByStatus(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	rows := sqlmock.NewRows(abuseCols).
		AddRow(3, "charlie@test.com", "spam", "", "dismissed", nil, time.Now())

	mock.ExpectQuery(`SELECT id, reporter_email, report_type, url, status, tenant_id, created_at`).
		WithArgs("dismissed").
		WillReturnRows(rows)

	handler := HandleAdminAbuse(testAdminAbuseTemplate(t), db, zap.NewNop())
	req := httptest.NewRequest("GET", "/admin/abuse?status=dismissed", nil)
	req = req.WithContext(adminCtx(t))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	body := w.Body.String()
	assert.Contains(t, body, "charlie@test.com")
	assert.Contains(t, body, "dismissed")
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestHandleAbuseAction_Valid(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	mock.ExpectExec(`UPDATE abuse_reports SET status`).
		WithArgs("actioned", "admin@stored.ge", int64(42)).
		WillReturnResult(sqlmock.NewResult(0, 1))

	handler := HandleAbuseAction(db, zap.NewNop())
	form := url.Values{"action": {"actioned"}}
	req := httptest.NewRequest("POST", "/admin/abuse/42/action", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	ctx := adminCtx(t)
	ctx = withChiParam(ctx, "id", "42")
	req = req.WithContext(ctx)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusSeeOther, w.Code)
	assert.Equal(t, "/admin/abuse/42", w.Header().Get("Location"))
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestHandleAbuseAction_InvalidAction(t *testing.T) {
	handler := HandleAbuseAction(nil, zap.NewNop())
	form := url.Values{"action": {"nuke"}}
	req := httptest.NewRequest("POST", "/admin/abuse/1/action", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	ctx := adminCtx(t)
	ctx = withChiParam(ctx, "id", "1")
	req = req.WithContext(ctx)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestHandleAbuseSubmit_Valid(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	mock.ExpectExec(`INSERT INTO abuse_reports`).
		WithArgs("reporter@test.com", "Jane", "malware", "Found malware in bucket", "https://example.com").
		WillReturnResult(sqlmock.NewResult(1, 1))

	mock.ExpectExec(`INSERT INTO admin_notifications`).
		WillReturnResult(sqlmock.NewResult(1, 1))

	handler := HandleAbuseSubmit(testAbusePublicTemplate(t), db, zap.NewNop())
	form := url.Values{
		"reporter_email": {"reporter@test.com"},
		"reporter_name":  {"Jane"},
		"report_type":    {"malware"},
		"description":    {"Found malware in bucket"},
		"url":            {"https://example.com"},
	}
	req := httptest.NewRequest("POST", "/abuse", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "Thank you")
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestHandleAbuseSubmit_MissingFields(t *testing.T) {
	handler := HandleAbuseSubmit(testAbusePublicTemplate(t), nil, zap.NewNop())

	form := url.Values{
		"reporter_email": {""},
		"report_type":    {"malware"},
		"description":    {"some description"},
	}
	req := httptest.NewRequest("POST", "/abuse", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "email")
}

func TestHandleAbuseForm_Renders(t *testing.T) {
	handler := HandleAbuseForm(testAbusePublicTemplate(t), zap.NewNop())
	req := httptest.NewRequest("GET", "/abuse", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "<form>")
}
