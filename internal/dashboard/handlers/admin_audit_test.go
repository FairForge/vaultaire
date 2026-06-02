package handlers

import (
	"fmt"
	"html/template"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func testAdminAuditTemplate(t *testing.T) *template.Template {
	t.Helper()
	tmpl := template.Must(template.New("admin").Parse(
		`{{define "admin"}}{{block "content" .}}{{end}}{{end}}`))
	template.Must(tmpl.Parse(
		`{{define "title"}}Audit{{end}}` +
			`{{define "content"}}` +
			`{{range .Events}}<div class="event">{{.Type}} {{.TenantID}}</div>{{end}}` +
			`{{if .HasMore}}<a class="next" href="{{.NextURL}}">Next</a>{{end}}` +
			`{{if not .Events}}<p>No events found.</p>{{end}}` +
			`{{end}}`))
	return tmpl
}

var auditCols = []string{"id", "type", "tenant_id", "data", "created_at"}

func TestHandleAdminAudit_RendersEvents(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	rows := sqlmock.NewRows(auditCols).
		AddRow("e1", "object.created", "t-1", []byte(`{"key":"photo.jpg"}`), time.Now()).
		AddRow("e2", "bucket.created", "t-2", []byte(`{"bucket":"my-bucket"}`), time.Now())

	mock.ExpectQuery(`SELECT id, type, tenant_id, data, created_at FROM events`).
		WillReturnRows(rows)

	handler := HandleAdminAudit(testAdminAuditTemplate(t), db, zap.NewNop())
	req := httptest.NewRequest("GET", "/admin/audit", nil)
	req = req.WithContext(adminCtx(t))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	body := w.Body.String()
	assert.Contains(t, body, "object.created")
	assert.Contains(t, body, "t-1")
	assert.Contains(t, body, "bucket.created")
	assert.Contains(t, body, "t-2")
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestHandleAdminAudit_FilterByType(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	rows := sqlmock.NewRows(auditCols).
		AddRow("e1", "object.created", "t-1", []byte(`{"key":"a"}`), time.Now())

	mock.ExpectQuery(`SELECT id, type, tenant_id, data, created_at FROM events`).
		WithArgs("object.created").
		WillReturnRows(rows)

	handler := HandleAdminAudit(testAdminAuditTemplate(t), db, zap.NewNop())
	req := httptest.NewRequest("GET", "/admin/audit?type=object.created", nil)
	req = req.WithContext(adminCtx(t))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "object.created")
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestHandleAdminAudit_FilterByTenant(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	rows := sqlmock.NewRows(auditCols).
		AddRow("e1", "key.created", "t-99", []byte(`{}`), time.Now())

	mock.ExpectQuery(`SELECT id, type, tenant_id, data, created_at FROM events`).
		WithArgs("t-99").
		WillReturnRows(rows)

	handler := HandleAdminAudit(testAdminAuditTemplate(t), db, zap.NewNop())
	req := httptest.NewRequest("GET", "/admin/audit?tenant=t-99", nil)
	req = req.WithContext(adminCtx(t))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "t-99")
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestHandleAdminAudit_Pagination(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	ts := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	rows := sqlmock.NewRows(auditCols)
	for i := 0; i < 50; i++ {
		rows.AddRow(fmt.Sprintf("e-%d", i), "object.created", "t-1", []byte(`{}`),
			ts.Add(-time.Duration(i)*time.Minute))
	}
	rows.AddRow("e-overflow", "OVERFLOW", "t-1", []byte(`{}`), ts.Add(-51*time.Minute))

	mock.ExpectQuery(`SELECT id, type, tenant_id, data, created_at FROM events`).
		WillReturnRows(rows)

	handler := HandleAdminAudit(testAdminAuditTemplate(t), db, zap.NewNop())
	req := httptest.NewRequest("GET", "/admin/audit", nil)
	req = req.WithContext(adminCtx(t))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	body := w.Body.String()
	assert.Contains(t, body, "Next")
	assert.NotContains(t, body, "OVERFLOW")
	assert.Equal(t, 50, strings.Count(body, `class="event"`))
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestHandleAdminAudit_EmptyState(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	mock.ExpectQuery(`SELECT id, type, tenant_id, data, created_at FROM events`).
		WillReturnRows(sqlmock.NewRows(auditCols))

	handler := HandleAdminAudit(testAdminAuditTemplate(t), db, zap.NewNop())
	req := httptest.NewRequest("GET", "/admin/audit", nil)
	req = req.WithContext(adminCtx(t))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "No events found.")
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestHandleAdminAudit_NoSession(t *testing.T) {
	handler := HandleAdminAudit(testAdminAuditTemplate(t), nil, zap.NewNop())
	req := httptest.NewRequest("GET", "/admin/audit", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusSeeOther, w.Code)
	assert.Equal(t, "/login", w.Header().Get("Location"))
}

func TestHandleAdminAudit_NilDB(t *testing.T) {
	handler := HandleAdminAudit(testAdminAuditTemplate(t), nil, zap.NewNop())
	req := httptest.NewRequest("GET", "/admin/audit", nil)
	req = req.WithContext(adminCtx(t))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "No events found.")
}

func TestHandleAdminAuditExport_CSV(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	rows := sqlmock.NewRows(auditCols).
		AddRow("e1", "object.created", "t-1",
			[]byte(`{"key":"file.txt"}`),
			time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC))

	mock.ExpectQuery(`SELECT id, type, tenant_id, data, created_at FROM events`).
		WillReturnRows(rows)

	handler := HandleAdminAuditExport(db, zap.NewNop())
	req := httptest.NewRequest("GET", "/admin/audit/export", nil)
	req = req.WithContext(adminCtx(t))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "text/csv", w.Header().Get("Content-Type"))
	assert.Contains(t, w.Header().Get("Content-Disposition"), "attachment")
	assert.Contains(t, w.Header().Get("Content-Disposition"), "audit-")

	body := w.Body.String()
	assert.Contains(t, body, "id,type,tenant_id,data,created_at")
	assert.Contains(t, body, "file.txt")
	assert.Contains(t, body, "object.created")
	require.NoError(t, mock.ExpectationsWereMet())
}
