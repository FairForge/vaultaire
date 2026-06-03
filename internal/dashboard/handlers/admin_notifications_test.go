package handlers

import (
	"database/sql"
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

func testNotificationsTemplate(t *testing.T) *template.Template {
	t.Helper()
	tmpl := template.Must(template.New("admin").Parse(
		`{{define "admin"}}{{block "content" .}}{{end}}{{end}}`))
	template.Must(tmpl.Parse(
		`{{define "title"}}Notifications{{end}}` +
			`{{define "content"}}` +
			`{{range .Notifications}}<div class="notif">{{.Type}} {{.Message}}</div>{{end}}` +
			`{{if not .Notifications}}<p>No notifications yet.</p>{{end}}` +
			`<span class="unread">{{.UnreadCount}}</span>` +
			`{{end}}`))
	return tmpl
}

var notifCols = []string{"id", "type", "message", "tenant_id", "read_at", "created_at"}

func TestHandleAdminNotifications_NoSession(t *testing.T) {
	handler := HandleAdminNotifications(testNotificationsTemplate(t), nil, zap.NewNop())
	req := httptest.NewRequest("GET", "/admin/notifications", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusSeeOther, w.Code)
	assert.Equal(t, "/login", w.Header().Get("Location"))
}

func TestHandleAdminNotifications_ListsNotifications(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	rows := sqlmock.NewRows(notifCols).
		AddRow(1, "signup", "New user: alice@example.com", "t-1", nil, time.Now()).
		AddRow(2, "payment", "Payment failed for tenant t-2", "t-2", time.Now(), time.Now().Add(-time.Hour))

	mock.ExpectQuery(`SELECT id, type, message, tenant_id, read_at, created_at FROM admin_notifications`).
		WillReturnRows(rows)

	handler := HandleAdminNotifications(testNotificationsTemplate(t), db, zap.NewNop())
	req := httptest.NewRequest("GET", "/admin/notifications", nil)
	req = req.WithContext(adminCtx(t))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	body := w.Body.String()
	assert.Contains(t, body, "signup")
	assert.Contains(t, body, "New user: alice@example.com")
	assert.Contains(t, body, "payment")
	assert.Contains(t, body, "Payment failed for tenant t-2")
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestHandleAdminNotifications_EmptyState(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	mock.ExpectQuery(`SELECT id, type, message, tenant_id, read_at, created_at FROM admin_notifications`).
		WillReturnRows(sqlmock.NewRows(notifCols))

	handler := HandleAdminNotifications(testNotificationsTemplate(t), db, zap.NewNop())
	req := httptest.NewRequest("GET", "/admin/notifications", nil)
	req = req.WithContext(adminCtx(t))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "No notifications yet.")
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestHandleMarkRead_Valid(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	mock.ExpectExec(`UPDATE admin_notifications SET read_at`).
		WithArgs(int64(42)).
		WillReturnResult(sqlmock.NewResult(0, 1))

	mock.ExpectQuery(`SELECT type, message, tenant_id, created_at FROM admin_notifications`).
		WithArgs(int64(42)).
		WillReturnRows(sqlmock.NewRows([]string{"type", "message", "tenant_id", "created_at"}).
			AddRow("signup", "New user signed up", "t-1", time.Now()))

	handler := HandleMarkRead(db, zap.NewNop())
	req := httptest.NewRequest("POST", "/admin/notifications/42/read", nil)
	ctx := adminCtx(t)
	ctx = withChiParam(ctx, "id", "42")
	req = req.WithContext(ctx)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	body := w.Body.String()
	assert.Contains(t, body, "notif-row--read")
	assert.Contains(t, body, "signup")
	assert.Contains(t, body, "New user signed up")
	assert.Contains(t, body, "t-1")
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestHandleMarkAllRead(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	mock.ExpectExec(`UPDATE admin_notifications SET read_at`).
		WillReturnResult(sqlmock.NewResult(0, 3))

	handler := HandleMarkAllRead(db, zap.NewNop())
	req := httptest.NewRequest("POST", "/admin/notifications/read-all", nil)
	req = req.WithContext(adminCtx(t))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusSeeOther, w.Code)
	assert.Equal(t, "/admin/notifications", w.Header().Get("Location"))
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestHandleNotifCount_WithUnread(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	mock.ExpectQuery(`SELECT COUNT`).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(5))

	handler := HandleNotifCount(db, zap.NewNop())
	req := httptest.NewRequest("GET", "/admin/notifications/count", nil)
	req = req.WithContext(adminCtx(t))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	body := w.Body.String()
	assert.Contains(t, body, `notif-badge`)
	assert.Contains(t, body, "5")
}

func TestHandleNotifCount_Zero(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	mock.ExpectQuery(`SELECT COUNT`).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))

	handler := HandleNotifCount(db, zap.NewNop())
	req := httptest.NewRequest("GET", "/admin/notifications/count", nil)
	req = req.WithContext(adminCtx(t))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Empty(t, w.Body.String())
}

func TestCreateNotification_NilDB(t *testing.T) {
	err := CreateNotification(t.Context(), nil, "signup", "test", "t-1")
	assert.NoError(t, err)
}

func TestCreateNotification_Insert(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	mock.ExpectExec(`INSERT INTO admin_notifications`).
		WithArgs("signup", "New user", "t-1").
		WillReturnResult(sqlmock.NewResult(1, 1))

	err = CreateNotification(t.Context(), db, "signup", "New user", "t-1")
	assert.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestCreateNotification_EmptyTenantID(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	mock.ExpectExec(`INSERT INTO admin_notifications`).
		WithArgs("system", "System restarted", "").
		WillReturnResult(sqlmock.NewResult(1, 1))

	err = CreateNotification(t.Context(), db, "system", "System restarted", "")
	assert.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestHandleMarkRead_NotFound(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	mock.ExpectExec(`UPDATE admin_notifications SET read_at`).
		WithArgs(int64(999)).
		WillReturnResult(sqlmock.NewResult(0, 0))

	mock.ExpectQuery(`SELECT type, message, tenant_id, created_at FROM admin_notifications`).
		WithArgs(int64(999)).
		WillReturnError(sql.ErrNoRows)

	handler := HandleMarkRead(db, zap.NewNop())
	req := httptest.NewRequest("POST", "/admin/notifications/999/read", nil)
	ctx := adminCtx(t)
	ctx = withChiParam(ctx, "id", "999")
	req = req.WithContext(ctx)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusNotFound, w.Code)
	require.NoError(t, mock.ExpectationsWereMet())
}
