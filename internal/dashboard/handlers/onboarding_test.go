package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	dashauth "github.com/FairForge/vaultaire/internal/dashboard/auth"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestOnboardingStatus_NewUser(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM buckets`).
		WithArgs("tenant-1").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM object_head_cache`).
		WithArgs("tenant-1").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM webhook_endpoints`).
		WithArgs("tenant-1").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectQuery(`SELECT access_key FROM tenants`).
		WithArgs("tenant-1").
		WillReturnRows(sqlmock.NewRows([]string{"access_key"}).AddRow("AKTEST123"))

	req := httptest.NewRequest("GET", "/dashboard/", nil)
	data := map[string]any{}
	populateOnboarding(context.Background(), db, "tenant-1", req, data)

	status, ok := data["Onboarding"].(*OnboardingStatus)
	require.True(t, ok)
	assert.False(t, status.HasBucket)
	assert.False(t, status.HasObject)
	assert.False(t, status.HasWebhook)
	assert.False(t, status.AllDone)
	assert.Equal(t, "AKTEST123", status.AccessKey)
	assert.NoError(t, mock.ExpectationsWereMet())
}

func TestOnboardingStatus_WithBucket(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM buckets`).
		WithArgs("tenant-1").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM object_head_cache`).
		WithArgs("tenant-1").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM webhook_endpoints`).
		WithArgs("tenant-1").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectQuery(`SELECT access_key FROM tenants`).
		WithArgs("tenant-1").
		WillReturnRows(sqlmock.NewRows([]string{"access_key"}).AddRow("AKTEST123"))

	req := httptest.NewRequest("GET", "/dashboard/", nil)
	data := map[string]any{}
	populateOnboarding(context.Background(), db, "tenant-1", req, data)

	status := data["Onboarding"].(*OnboardingStatus)
	assert.True(t, status.HasBucket)
	assert.False(t, status.HasObject)
	assert.False(t, status.HasWebhook)
	assert.False(t, status.AllDone)
	assert.NoError(t, mock.ExpectationsWereMet())
}

func TestOnboardingStatus_AllComplete(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM buckets`).
		WithArgs("tenant-1").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(2))
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM object_head_cache`).
		WithArgs("tenant-1").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(5))
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM webhook_endpoints`).
		WithArgs("tenant-1").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	mock.ExpectQuery(`SELECT access_key FROM tenants`).
		WithArgs("tenant-1").
		WillReturnRows(sqlmock.NewRows([]string{"access_key"}).AddRow("AKTEST123"))

	req := httptest.NewRequest("GET", "/dashboard/", nil)
	data := map[string]any{}
	populateOnboarding(context.Background(), db, "tenant-1", req, data)

	status := data["Onboarding"].(*OnboardingStatus)
	assert.True(t, status.HasBucket)
	assert.True(t, status.HasObject)
	assert.True(t, status.HasWebhook)
	assert.True(t, status.AllDone)
	assert.NoError(t, mock.ExpectationsWereMet())
}

func TestOnboardingStatus_Dismissed(t *testing.T) {
	req := httptest.NewRequest("GET", "/dashboard/", nil)
	req.AddCookie(&http.Cookie{Name: "onboarding_dismissed", Value: "1"})

	db, _, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	data := map[string]any{}
	populateOnboarding(context.Background(), db, "tenant-1", req, data)

	_, hasOnboarding := data["Onboarding"]
	assert.False(t, hasOnboarding)
	assert.True(t, data["OnboardingDismissed"].(bool))
}

func TestDismissOnboarding(t *testing.T) {
	handler := HandleDismissOnboarding(zap.NewNop())

	req := httptest.NewRequest("POST", "/dashboard/onboarding/dismiss", nil)
	ctx := context.WithValue(req.Context(), dashauth.SessionKey, &dashauth.SessionData{
		UserID:   "user-1",
		TenantID: "tenant-1",
		Email:    "test@stored.ge",
		Role:     "user",
	})
	req = req.WithContext(ctx)

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	cookies := w.Result().Cookies()
	require.Len(t, cookies, 1)
	assert.Equal(t, "onboarding_dismissed", cookies[0].Name)
	assert.Equal(t, "1", cookies[0].Value)
	assert.Equal(t, 365*24*60*60, cookies[0].MaxAge)
}

func TestOnboardingStatus_NilDB(t *testing.T) {
	req := httptest.NewRequest("GET", "/dashboard/", nil)
	data := map[string]any{}
	populateOnboarding(context.Background(), nil, "tenant-1", req, data)

	_, hasOnboarding := data["Onboarding"]
	assert.False(t, hasOnboarding)
}
