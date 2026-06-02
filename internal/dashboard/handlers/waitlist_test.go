package handlers

import (
	"html/template"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func testWaitlistTemplate(t *testing.T) *template.Template {
	t.Helper()
	return template.Must(template.New("admin").Parse(
		`{{define "admin"}}<span class="count">{{.SignupCount}}</span>` +
			`{{range .Signups}}<span class="row">{{.Email}}</span>{{end}}{{end}}`))
}

func TestHandleAdminWaitlist_NoSession(t *testing.T) {
	h := HandleAdminWaitlist(testWaitlistTemplate(t), nil, zap.NewNop())
	req := httptest.NewRequest("GET", "/admin/waitlist", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	assert.Equal(t, http.StatusSeeOther, w.Code)
	assert.Equal(t, "/login", w.Header().Get("Location"))
}

func TestHandleAdminWaitlist_ListsSignups(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM waitlist_signups`).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(2))
	mock.ExpectQuery(`SELECT email, source, created_at FROM waitlist_signups`).
		WillReturnRows(sqlmock.NewRows([]string{"email", "source", "created_at"}).
			AddRow("a@example.com", "landing", time.Now()).
			AddRow("b@example.com", "landing", time.Now()))

	h := HandleAdminWaitlist(testWaitlistTemplate(t), db, zap.NewNop())
	req := httptest.NewRequest("GET", "/admin/waitlist", nil).WithContext(adminCtx(t))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	body := w.Body.String()
	assert.Contains(t, body, `class="count">2`)
	assert.Contains(t, body, "a@example.com")
	assert.Contains(t, body, "b@example.com")
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestHandleAdminWaitlistExport_CSV(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	mock.ExpectQuery(`SELECT email, source, created_at FROM waitlist_signups`).
		WillReturnRows(sqlmock.NewRows([]string{"email", "source", "created_at"}).
			AddRow("a@example.com", "landing", time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)))

	h := HandleAdminWaitlistExport(db, zap.NewNop())
	req := httptest.NewRequest("GET", "/admin/waitlist/export", nil).WithContext(adminCtx(t))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "text/csv", w.Header().Get("Content-Type"))
	assert.Contains(t, w.Header().Get("Content-Disposition"), "attachment")
	body := w.Body.String()
	assert.Contains(t, body, "email,source,created_at")
	assert.Contains(t, body, "a@example.com,landing,2026-06-01T12:00:00Z")
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestHandleAdminWaitlistExport_NoSession(t *testing.T) {
	h := HandleAdminWaitlistExport(nil, zap.NewNop())
	req := httptest.NewRequest("GET", "/admin/waitlist/export", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	assert.Equal(t, http.StatusSeeOther, w.Code)
}
