package api

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestCreateExport_NilDB(t *testing.T) {
	exporter := NewAccountExporter(nil, zap.NewNop())
	result, err := exporter.CreateExport(context.Background(), "user-1", "tenant-1")

	require.NoError(t, err)
	assert.Equal(t, "completed", result.Status)
	assert.Equal(t, json.RawMessage(`{}`), result.Data)
}

func TestCreateExport_CollectsUserData(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	// INSERT export record.
	mock.ExpectQuery(`INSERT INTO account_exports`).
		WithArgs("user-1", "tenant-1").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("export-uuid"))

	// User profile.
	mock.ExpectQuery(`SELECT id, email, company, role, status, created_at FROM users`).
		WithArgs("user-1").
		WillReturnRows(sqlmock.NewRows([]string{"id", "email", "company", "role", "status", "created_at"}).
			AddRow("user-1", "test@example.com", "Acme Inc", "user", "active", time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)))

	// Tenant.
	mock.ExpectQuery(`SELECT name, plan FROM tenants`).
		WithArgs("tenant-1").
		WillReturnRows(sqlmock.NewRows([]string{"name", "plan"}).
			AddRow("Acme Tenant", "starter"))

	// Quota.
	mock.ExpectQuery(`SELECT storage_used_bytes, storage_limit_bytes, tier FROM tenant_quotas`).
		WithArgs("tenant-1").
		WillReturnRows(sqlmock.NewRows([]string{"storage_used_bytes", "storage_limit_bytes", "tier"}).
			AddRow(1000, 5000000000, "starter"))

	// Buckets.
	mock.ExpectQuery(`SELECT name, visibility, created_at, COALESCE`).
		WithArgs("tenant-1").
		WillReturnRows(sqlmock.NewRows([]string{"name", "visibility", "created_at", "metadata"}).
			AddRow("my-bucket", "private", time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC), []byte("{}")))

	// Objects.
	mock.ExpectQuery(`SELECT bucket, object_key, size, content_type, last_modified FROM object_head_cache`).
		WithArgs("tenant-1").
		WillReturnRows(sqlmock.NewRows([]string{"bucket", "object_key", "size", "content_type", "last_modified"}).
			AddRow("my-bucket", "file.txt", 42, "text/plain", time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC)))

	// API keys.
	mock.ExpectQuery(`SELECT id, name, COALESCE\(permissions`).
		WithArgs("user-1").
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "permissions", "created_at"}).
			AddRow("key-1", "main-key", []byte(`["*"]`), time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)))

	// Bandwidth.
	mock.ExpectQuery(`SELECT date, ingress_bytes, egress_bytes, requests FROM bandwidth_usage_daily`).
		WithArgs("tenant-1").
		WillReturnRows(sqlmock.NewRows([]string{"date", "ingress_bytes", "egress_bytes", "requests"}))

	// Events.
	mock.ExpectQuery(`SELECT id, type, data, created_at FROM events`).
		WithArgs("tenant-1").
		WillReturnRows(sqlmock.NewRows([]string{"id", "type", "data", "created_at"}))

	// Update export status.
	mock.ExpectExec(`UPDATE account_exports SET status = 'completed'`).
		WillReturnResult(sqlmock.NewResult(0, 1))

	exporter := NewAccountExporter(db, zap.NewNop())
	result, err := exporter.CreateExport(context.Background(), "user-1", "tenant-1")

	require.NoError(t, err)
	assert.Equal(t, "completed", result.Status)
	assert.Equal(t, "export-uuid", result.ID)
	assert.Greater(t, result.SizeBytes, int64(0))

	// Verify JSON structure.
	var data map[string]interface{}
	require.NoError(t, json.Unmarshal(result.Data, &data))
	assert.Contains(t, data, "user")
	assert.Contains(t, data, "tenant")
	assert.Contains(t, data, "quota")
	assert.Contains(t, data, "buckets")
	assert.Contains(t, data, "objects")
	assert.Contains(t, data, "api_keys")
	assert.Contains(t, data, "bandwidth_usage")
	assert.Contains(t, data, "events")

	// Verify no password_hash in user data.
	userData, ok := data["user"].(map[string]interface{})
	require.True(t, ok, "user data should be a map")
	assert.NotContains(t, userData, "password_hash")
	assert.Equal(t, "test@example.com", userData["email"])

	require.NoError(t, mock.ExpectationsWereMet())
}

func TestGetExport_NilDB(t *testing.T) {
	exporter := NewAccountExporter(nil, zap.NewNop())
	_, err := exporter.GetExport(context.Background(), "some-id", "user-1")

	assert.Error(t, err)
	assert.Contains(t, err.Error(), "database unavailable")
}
