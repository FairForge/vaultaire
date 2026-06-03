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

func testSupportTemplate(t *testing.T) *template.Template {
	t.Helper()
	return template.Must(template.New("admin").Parse(
		`{{define "admin"}}` +
			`<span class="query">{{.Query}}</span>` +
			`{{if .Searched}}` +
			`{{if .Results}}{{range .Results}}<span class="result">{{.Email}} {{.Name}} {{.Plan}} {{.Status}}</span>{{end}}` +
			`{{else}}<p>No customers found.</p>{{end}}` +
			`{{else}}<p>Search for a customer</p>{{end}}` +
			`{{end}}`))
}

func testCustomerDetailTemplate(t *testing.T) *template.Template {
	t.Helper()
	return template.Must(template.New("admin").Parse(
		`{{define "admin"}}` +
			`<h1>{{.TenantName}}</h1>` +
			`<span class="email">{{.TenantEmail}}</span>` +
			`{{range .Timeline}}<span class="event">{{.Type}}</span>{{end}}` +
			`{{range .Errors}}<span class="error">{{.Operation}} {{.StatusCode}}</span>{{end}}` +
			`{{range .Notes}}<span class="note">{{.Note}}</span>{{end}}` +
			`{{if .FlashSuccess}}<span class="flash-ok">{{.FlashSuccess}}</span>{{end}}` +
			`{{if .FlashError}}<span class="flash-err">{{.FlashError}}</span>{{end}}` +
			`{{end}}`))
}

// --- Search tests ---

func TestHandleAdminSupport_NoSession(t *testing.T) {
	h := HandleAdminSupport(testSupportTemplate(t), nil, zap.NewNop())
	req := httptest.NewRequest("GET", "/admin/support", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	assert.Equal(t, http.StatusSeeOther, w.Code)
	assert.Equal(t, "/login", w.Header().Get("Location"))
}

func TestHandleAdminSupport_NoQuery(t *testing.T) {
	h := HandleAdminSupport(testSupportTemplate(t), nil, zap.NewNop())
	req := httptest.NewRequest("GET", "/admin/support", nil).WithContext(adminCtx(t))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "Search for a customer")
}

func TestHandleAdminSupport_SearchByEmail(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	mock.ExpectQuery(`SELECT t.id, t.name, t.email`).
		WithArgs("%alice%", "alice").
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "email", "plan", "subscription_status", "suspended_at"}).
			AddRow("t-1", "Alice Corp", "alice@example.com", "vault3", "active", nil))

	h := HandleAdminSupport(testSupportTemplate(t), db, zap.NewNop())
	req := httptest.NewRequest("GET", "/admin/support?q=alice", nil).WithContext(adminCtx(t))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	body := w.Body.String()
	assert.Contains(t, body, "alice@example.com")
	assert.Contains(t, body, "Alice Corp")
	assert.Contains(t, body, "vault3")
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestHandleAdminSupport_SearchByTenantID(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	mock.ExpectQuery(`SELECT t.id, t.name, t.email`).
		WithArgs("%t-exact-123%", "t-exact-123").
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "email", "plan", "subscription_status", "suspended_at"}).
			AddRow("t-exact-123", "Exact Match", "exact@example.com", "starter", "", nil))

	h := HandleAdminSupport(testSupportTemplate(t), db, zap.NewNop())
	req := httptest.NewRequest("GET", "/admin/support?q=t-exact-123", nil).WithContext(adminCtx(t))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	body := w.Body.String()
	assert.Contains(t, body, "exact@example.com")
	assert.Contains(t, body, "Exact Match")
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestHandleAdminSupport_NoResults(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	mock.ExpectQuery(`SELECT t.id, t.name, t.email`).
		WithArgs("%nobody%", "nobody").
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "email", "plan", "subscription_status", "suspended_at"}))

	h := HandleAdminSupport(testSupportTemplate(t), db, zap.NewNop())
	req := httptest.NewRequest("GET", "/admin/support?q=nobody", nil).WithContext(adminCtx(t))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "No customers found.")
	require.NoError(t, mock.ExpectationsWereMet())
}

// --- Customer detail tests ---

func mockTenantDetailQueries(mock sqlmock.Sqlmock, tenantID string) {
	mock.ExpectQuery(`SELECT t.name, t.email`).
		WithArgs(tenantID).
		WillReturnRows(sqlmock.NewRows([]string{"name", "email", "plan", "subscription_status", "stripe_customer_id", "stripe_subscription_id", "suspended_at", "created_at"}).
			AddRow("Test Tenant", "test@example.com", "vault3", "active", nil, nil, nil, time.Now()))

	mock.ExpectQuery(`SELECT COALESCE\(storage_used_bytes`).
		WithArgs(tenantID).
		WillReturnRows(sqlmock.NewRows([]string{"used", "limit", "tier"}).
			AddRow(int64(1024), int64(1099511627776), "vault3"))

	mock.ExpectQuery(`SELECT COALESCE\(SUM\(ingress_bytes\)`).
		WithArgs(tenantID).
		WillReturnRows(sqlmock.NewRows([]string{"ingress", "egress", "requests"}).
			AddRow(int64(0), int64(0), int64(0)))

	mock.ExpectQuery(`SELECT bandwidth_limit_bytes`).
		WithArgs(tenantID).
		WillReturnRows(sqlmock.NewRows([]string{"bandwidth_limit_bytes"}).
			AddRow(nil))

	mock.ExpectQuery(`SELECT date, COALESCE\(ingress_bytes`).
		WithArgs(tenantID).
		WillReturnRows(sqlmock.NewRows([]string{"date", "ingress", "egress"}))
}

func TestHandleCustomerDetail_NotFound(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	mock.ExpectQuery(`SELECT t.name, t.email`).
		WithArgs("t-bad").
		WillReturnRows(sqlmock.NewRows([]string{"name", "email", "plan", "subscription_status", "stripe_customer_id", "stripe_subscription_id", "suspended_at", "created_at"}))

	h := HandleCustomerDetail(testCustomerDetailTemplate(t), db, zap.NewNop())
	ctx := withChiParam(adminCtx(t), "id", "t-bad")
	req := httptest.NewRequest("GET", "/admin/support/t-bad", nil).WithContext(ctx)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	assert.Equal(t, http.StatusNotFound, w.Code)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestHandleCustomerDetail_LoadsTimeline(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	mockTenantDetailQueries(mock, "t-1")

	mock.ExpectQuery(`SELECT stripe_customer_id FROM tenants`).
		WithArgs("t-1").
		WillReturnRows(sqlmock.NewRows([]string{"stripe_customer_id"}).
			AddRow("cus_test123"))

	mock.ExpectQuery(`SELECT type, data::TEXT, created_at FROM events`).
		WithArgs("t-1").
		WillReturnRows(sqlmock.NewRows([]string{"type", "data", "created_at"}).
			AddRow("object.created", `{"key":"photo.jpg"}`, time.Now()).
			AddRow("bucket.created", `{"bucket":"my-bucket"}`, time.Now()))

	mock.ExpectQuery(`SELECT operation, bucket, object_key, status_code`).
		WithArgs("t-1").
		WillReturnRows(sqlmock.NewRows([]string{"operation", "bucket", "object_key", "status_code", "error_code", "source_ip", "logged_at"}).
			AddRow("GetObject", "test-bucket", "missing.txt", 404, "NoSuchKey", "10.0.0.1", time.Now()))

	mock.ExpectQuery(`SELECT n.note, u.email, n.created_at`).
		WithArgs("t-1").
		WillReturnRows(sqlmock.NewRows([]string{"note", "email", "created_at"}).
			AddRow("Customer contacted about billing", "admin@stored.ge", time.Now()))

	h := HandleCustomerDetail(testCustomerDetailTemplate(t), db, zap.NewNop())
	ctx := withChiParam(adminCtx(t), "id", "t-1")
	req := httptest.NewRequest("GET", "/admin/support/t-1", nil).WithContext(ctx)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	body := w.Body.String()
	assert.Contains(t, body, "Test Tenant")
	assert.Contains(t, body, "object.created")
	assert.Contains(t, body, "bucket.created")
	assert.Contains(t, body, "GetObject 404")
	assert.Contains(t, body, "Customer contacted about billing")
	require.NoError(t, mock.ExpectationsWereMet())
}

// --- Add note tests ---

func TestHandleAddNote_Valid(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	mock.ExpectExec(`INSERT INTO admin_notes`).
		WithArgs("t-1", "admin-1", "Test note content").
		WillReturnResult(sqlmock.NewResult(1, 1))

	h := HandleAddNote(db, zap.NewNop())
	form := url.Values{"note": {"Test note content"}, "csrf_token": {"fake"}}
	ctx := withChiParam(adminCtx(t), "id", "t-1")
	req := httptest.NewRequest("POST", "/admin/support/t-1/notes", strings.NewReader(form.Encode())).WithContext(ctx)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	assert.Equal(t, http.StatusSeeOther, w.Code)
	assert.Equal(t, "/admin/support/t-1", w.Header().Get("Location"))
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestHandleAddNote_Empty(t *testing.T) {
	h := HandleAddNote(nil, zap.NewNop())
	form := url.Values{"note": {""}, "csrf_token": {"fake"}}
	ctx := withChiParam(adminCtx(t), "id", "t-1")
	req := httptest.NewRequest("POST", "/admin/support/t-1/notes", strings.NewReader(form.Encode())).WithContext(ctx)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	assert.Equal(t, http.StatusSeeOther, w.Code)
	assert.Equal(t, "/admin/support/t-1", w.Header().Get("Location"))
	cookies := w.Result().Cookies()
	var flashCookie *http.Cookie
	for _, c := range cookies {
		if c.Name == "flash" {
			flashCookie = c
		}
	}
	require.NotNil(t, flashCookie, "flash cookie should be set")
	assert.Contains(t, flashCookie.Value, "error")
}
