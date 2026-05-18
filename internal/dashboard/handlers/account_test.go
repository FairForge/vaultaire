package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	dashauth "github.com/FairForge/vaultaire/internal/dashboard/auth"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"golang.org/x/crypto/bcrypt"
)

func injectAccountSession(req *http.Request) *http.Request {
	sd := &dashauth.SessionData{
		UserID:   "user-123",
		TenantID: "tenant-456",
		Email:    "test@stored.ge",
		Role:     "user",
	}
	ctx := context.WithValue(req.Context(), dashauth.SessionKey, sd)
	return req.WithContext(ctx)
}

func TestHandleExportData_NoSession(t *testing.T) {
	handler := HandleExportData(nil, zap.NewNop())

	req := httptest.NewRequest("POST", "/dashboard/settings/export", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusSeeOther, w.Code)
	assert.Equal(t, "/login", w.Header().Get("Location"))
}

func TestHandleExportData_NoDB(t *testing.T) {
	handler := HandleExportData(nil, zap.NewNop())

	req := injectAccountSession(httptest.NewRequest("POST", "/dashboard/settings/export", nil))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusInternalServerError, w.Code)
}

func TestHandleExportData_ReturnsJSON(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	// User query.
	mock.ExpectQuery(`SELECT email, company, role, status, created_at FROM users`).
		WithArgs("user-123").
		WillReturnRows(sqlmock.NewRows([]string{"email", "company", "role", "status", "created_at"}).
			AddRow("test@stored.ge", "TestCo", "user", "active", time.Now()))

	// Tenant query.
	mock.ExpectQuery(`SELECT name, plan FROM tenants`).
		WithArgs("tenant-456").
		WillReturnRows(sqlmock.NewRows([]string{"name", "plan"}).
			AddRow("Test Tenant", "starter"))

	// Quota query.
	mock.ExpectQuery(`SELECT storage_used_bytes, storage_limit_bytes, tier FROM tenant_quotas`).
		WithArgs("tenant-456").
		WillReturnRows(sqlmock.NewRows([]string{"storage_used_bytes", "storage_limit_bytes", "tier"}).
			AddRow(500, 5000000000, "starter"))

	// Buckets query.
	mock.ExpectQuery(`SELECT name, visibility, created_at FROM buckets`).
		WithArgs("tenant-456").
		WillReturnRows(sqlmock.NewRows([]string{"name", "visibility", "created_at"}))

	// Objects query.
	mock.ExpectQuery(`SELECT bucket, object_key, size, content_type, last_modified FROM object_head_cache`).
		WithArgs("tenant-456").
		WillReturnRows(sqlmock.NewRows([]string{"bucket", "object_key", "size", "content_type", "last_modified"}))

	// API keys query.
	mock.ExpectQuery(`SELECT id, name, created_at FROM api_keys`).
		WithArgs("user-123").
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "created_at"}))

	// Bandwidth query.
	mock.ExpectQuery(`SELECT date, ingress_bytes, egress_bytes, requests FROM bandwidth_usage_daily`).
		WithArgs("tenant-456").
		WillReturnRows(sqlmock.NewRows([]string{"date", "ingress_bytes", "egress_bytes", "requests"}))

	handler := HandleExportData(db, zap.NewNop())

	req := injectAccountSession(httptest.NewRequest("POST", "/dashboard/settings/export", nil))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "application/json", w.Header().Get("Content-Type"))
	assert.Contains(t, w.Header().Get("Content-Disposition"), "stored-ge-export-")
	assert.Contains(t, w.Body.String(), `"exported_at"`)
	assert.Contains(t, w.Body.String(), `"user"`)
}

func TestHandleRequestDeletion_NoSession(t *testing.T) {
	handler := HandleRequestDeletion(nil, nil, zap.NewNop())

	req := httptest.NewRequest("POST", "/dashboard/settings/delete-account", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusSeeOther, w.Code)
	assert.Equal(t, "/login", w.Header().Get("Location"))
}

func TestHandleRequestDeletion_WrongPassword(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	hash, _ := bcrypt.GenerateFromPassword([]byte("correct-password"), bcrypt.MinCost)
	mock.ExpectQuery(`SELECT password_hash FROM users`).
		WithArgs("user-123").
		WillReturnRows(sqlmock.NewRows([]string{"password_hash"}).AddRow(string(hash)))

	handler := HandleRequestDeletion(db, nil, zap.NewNop())

	form := url.Values{"password": {"wrong-password"}}
	req := injectAccountSession(httptest.NewRequest("POST", "/dashboard/settings/delete-account", strings.NewReader(form.Encode())))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusSeeOther, w.Code)
	assert.Equal(t, "/dashboard/settings", w.Header().Get("Location"))
	// Flash cookie should contain error.
	cookies := w.Result().Cookies()
	hasFlash := false
	for _, c := range cookies {
		if c.Name == "flash" {
			hasFlash = true
			decoded, _ := url.ParseQuery(c.Value)
			assert.Contains(t, decoded.Get("error"), "Incorrect password")
		}
	}
	assert.True(t, hasFlash, "should set flash cookie on wrong password")
}

func TestHandleCancelDeletion_NoSession(t *testing.T) {
	handler := HandleCancelDeletion(nil, zap.NewNop())

	req := httptest.NewRequest("POST", "/dashboard/settings/cancel-deletion", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusSeeOther, w.Code)
	assert.Equal(t, "/login", w.Header().Get("Location"))
}

func TestHandleCancelDeletion_Success(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	mock.ExpectExec(`UPDATE users SET deletion_scheduled_at = NULL`).
		WithArgs("user-123").
		WillReturnResult(sqlmock.NewResult(0, 1))

	handler := HandleCancelDeletion(db, zap.NewNop())

	req := injectAccountSession(httptest.NewRequest("POST", "/dashboard/settings/cancel-deletion", nil))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusSeeOther, w.Code)
	assert.Equal(t, "/dashboard/settings", w.Header().Get("Location"))
	// Flash cookie should contain success.
	cookies := w.Result().Cookies()
	hasFlash := false
	for _, c := range cookies {
		if c.Name == "flash" {
			hasFlash = true
			decoded, _ := url.ParseQuery(c.Value)
			assert.Contains(t, decoded.Get("success"), "cancelled")
		}
	}
	assert.True(t, hasFlash, "should set flash cookie on cancel success")
}
