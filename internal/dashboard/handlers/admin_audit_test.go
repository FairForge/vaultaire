package handlers

import (
	"database/sql/driver"
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

// WP-R11-10 / Review R12: the admin audit page reads audit_logs (the operator
// trail internal/audit writes), not the events table.

func testAdminAuditTemplate(t *testing.T) *template.Template {
	t.Helper()
	tmpl := template.Must(template.New("admin").Parse(
		`{{define "admin"}}{{block "content" .}}{{end}}{{end}}`))
	template.Must(tmpl.Parse(
		`{{define "title"}}Audit{{end}}` +
			`{{define "content"}}` +
			`{{range .Events}}<div class="event">{{.Action}} {{.Actor}} {{.TenantID}} {{.IP}} {{.Result}}</div>{{end}}` +
			`{{if .HasMore}}<a class="next" href="{{.NextURL}}">Next</a>{{end}}` +
			`{{if not .Events}}<p>No audit events match.</p>{{end}}` +
			`{{end}}`))
	return tmpl
}

var auditLogCols = []string{"id", "timestamp", "user_id", "performed_by", "tenant_id", "event_type", "action",
	"resource", "result", "severity", "ip", "user_agent", "error_msg", "metadata"}

func auditRow(id, action, actor, tenant, ip, result string) []driver.Value {
	return []driver.Value{id, time.Now(), "u-subject", actor, tenant, strings.SplitN(action, ".", 2)[0], action,
		"key:k1", result, "info", ip, "curl/8", "", []byte(`{"email":"a@b.test"}`)}
}

func TestHandleAdminAudit_ReadsAuditLogs(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	rows := sqlmock.NewRows(auditLogCols).
		AddRow(auditRow("a1", "key.revoked", "admin-1", "t-1", "203.0.113.7", "success")...).
		AddRow(auditRow("a2", "auth.login_failed", "", "", "198.51.100.9", "success")...)
	mock.ExpectQuery(`FROM audit_logs WHERE TRUE ORDER BY timestamp DESC`).WithArgs(51).WillReturnRows(rows)

	handler := HandleAdminAudit(testAdminAuditTemplate(t), db, zap.NewNop())
	req := httptest.NewRequest("GET", "/admin/audit", nil).WithContext(adminCtx(t))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	body := w.Body.String()
	assert.Contains(t, body, "key.revoked admin-1 t-1 203.0.113.7 success")
	assert.Contains(t, body, "auth.login_failed")
	assert.NotContains(t, body, "object.created", "the events table is not this page")
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestHandleAdminAudit_FiltersByActorTenantIPTypeAction(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	mock.ExpectQuery(`FROM audit_logs WHERE TRUE AND tenant_id = \$1 AND performed_by::text = \$2 AND host\(ip\) = \$3 AND action = \$4 AND event_type = \$5`).
		WithArgs("t-1", "admin-1", "203.0.113.7", "key.revoked", "key", 51).
		WillReturnRows(sqlmock.NewRows(auditLogCols).AddRow(auditRow("a1", "key.revoked", "admin-1", "t-1", "203.0.113.7", "success")...))

	handler := HandleAdminAudit(testAdminAuditTemplate(t), db, zap.NewNop())
	req := httptest.NewRequest("GET", "/admin/audit?tenant=t-1&actor=admin-1&ip=203.0.113.7&type=key&action=key.revoked", nil).WithContext(adminCtx(t))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "key.revoked")
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestHandleAdminAudit_PaginationKeepsFilters(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	rows := sqlmock.NewRows(auditLogCols)
	for i := 0; i < auditPageSize+1; i++ {
		rows.AddRow(auditRow("id", "flag.set", "admin-1", "", "", "success")...)
	}
	mock.ExpectQuery(`FROM audit_logs WHERE TRUE AND event_type = \$1`).WithArgs("flag", 51).WillReturnRows(rows)

	handler := HandleAdminAudit(testAdminAuditTemplate(t), db, zap.NewNop())
	req := httptest.NewRequest("GET", "/admin/audit?type=flag", nil).WithContext(adminCtx(t))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	body := w.Body.String()
	assert.Equal(t, auditPageSize, strings.Count(body, `class="event"`))
	assert.Contains(t, body, `class="next"`)
	assert.Contains(t, body, "type=flag")
	assert.Contains(t, body, "cursor=")
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestHandleAdminAudit_EmptyNoSessionNilDB(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	mock.ExpectQuery(`FROM audit_logs`).WillReturnRows(sqlmock.NewRows(auditLogCols))
	handler := HandleAdminAudit(testAdminAuditTemplate(t), db, zap.NewNop())
	req := httptest.NewRequest("GET", "/admin/audit", nil).WithContext(adminCtx(t))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "No audit events match")

	w = httptest.NewRecorder()
	HandleAdminAudit(testAdminAuditTemplate(t), db, zap.NewNop()).ServeHTTP(w, httptest.NewRequest("GET", "/admin/audit", nil))
	assert.Equal(t, http.StatusSeeOther, w.Code)

	w = httptest.NewRecorder()
	HandleAdminAudit(testAdminAuditTemplate(t), nil, zap.NewNop()).ServeHTTP(w, httptest.NewRequest("GET", "/admin/audit", nil).WithContext(adminCtx(t)))
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestHandleAdminAuditExport_CSVIsAuditedAndEscaped(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	mock.ExpectExec(`INSERT INTO audit_logs`).WillReturnResult(sqlmock.NewResult(1, 1))
	rows := sqlmock.NewRows(auditLogCols).
		AddRow("a1", time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC), "u-1", "admin-1", "t-1", "auth", "auth.login_failed",
			"", "success", "warning", "203.0.113.7", "curl/8", "", []byte(`{"email":"=HYPERLINK(\"x\")@evil.test"}`))
	mock.ExpectQuery(`FROM audit_logs WHERE TRUE AND event_type = \$1`).WithArgs("auth", 101).WillReturnRows(rows)

	handler := HandleAdminAuditExport(db, zap.NewNop())
	req := httptest.NewRequest("GET", "/admin/audit/export?type=auth", nil).WithContext(adminCtx(t))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "text/csv", w.Header().Get("Content-Type"))
	body := w.Body.String()
	assert.True(t, strings.HasPrefix(body, "timestamp,event_type,action,result,severity,performed_by,user_id,tenant_id,resource,ip,user_agent,error,metadata\n"))
	assert.Contains(t, body, "2026-09-30T12:00:00Z,auth,auth.login_failed,success,warning,admin-1,u-1,t-1,,203.0.113.7,curl/8,,")
	assert.Contains(t, body, `"{""email"":""=HYPERLINK`, "the metadata cell is quoted JSON — it starts with { so no prefix")
	require.NoError(t, mock.ExpectationsWereMet())
}
