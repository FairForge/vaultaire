package api

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/FairForge/vaultaire/internal/usage"
	_ "github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setupTestUsageAPI(t *testing.T) (*Server, *sql.DB) {
	db := setupTestDB(t)
	if db == nil {
		return nil, nil
	}

	// Clean and setup
	_, _ = db.Exec("DROP TABLE IF EXISTS quota_usage_events")
	_, _ = db.Exec("DROP TABLE IF EXISTS tenant_quotas")

	quotaMgr := usage.NewQuotaManager(db)
	require.NoError(t, quotaMgr.InitializeSchema(context.Background()))

	// Create test tenant with some usage
	require.NoError(t, quotaMgr.CreateTenant(context.Background(), "tenant-123", "starter", 1073741824)) // 1GB
	_, err := quotaMgr.CheckAndReserve(context.Background(), "tenant-123", 524288000)
	require.NoError(t, err)

	server := &Server{
		quotaManager: quotaMgr,
	}

	return server, db
}

func TestUsageAPI_GetUsageStats(t *testing.T) {
	server, db := setupTestUsageAPI(t)
	if db == nil {
		return // Test was skipped
	}
	defer func() { _ = db.Close() }()

	// Use unique tenant ID
	tenantID := "test-tenant-" + time.Now().Format("20060102150405")

	// Create tenant with proper quota - use server.quotaManager instead of qm
	err := server.quotaManager.CreateTenant(context.Background(), tenantID, "starter", 1073741824) // 1GB
	require.NoError(t, err)

	// Reserve some storage
	ok, err := server.quotaManager.CheckAndReserve(context.Background(), tenantID, 524288000) // 500MB
	require.NoError(t, err)
	assert.True(t, ok)

	// Get usage stats
	req := httptest.NewRequest("GET", "/api/v1/usage/stats?tenant_id="+tenantID, nil)
	w := httptest.NewRecorder()

	server.handleGetUsageStats(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
}

func TestUsageAPI_GetUsageAlerts(t *testing.T) {
	server, db := setupTestUsageAPI(t)
	if db == nil {
		return // Test was skipped
	}
	defer func() { _ = db.Close() }()

	// Use unique tenant ID
	tenantID := "test-alert-tenant-" + time.Now().Format("20060102150405")

	// Create tenant near limit
	err := server.quotaManager.CreateTenant(context.Background(), tenantID, "starter", 100000) // 100KB limit
	require.NoError(t, err)

	// Use 95KB
	ok, err := server.quotaManager.CheckAndReserve(context.Background(), tenantID, 95000)
	require.NoError(t, err)
	assert.True(t, ok)

	// Get usage alerts
	req := httptest.NewRequest("GET", "/api/v1/usage/alerts?tenant_id="+tenantID, nil)
	w := httptest.NewRecorder()

	server.handleGetUsageAlerts(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
}
