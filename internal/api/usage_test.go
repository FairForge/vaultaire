// internal/api/usage_test.go
package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/FairForge/vaultaire/internal/database"
	"github.com/FairForge/vaultaire/internal/usage"
	_ "github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setupTestUsageAPI(t *testing.T) (*Server, *sql.DB) {
	dsn := database.GetTestDSN()
	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)

	// Clean and setup
	_, _ = db.Exec("DROP TABLE IF EXISTS quota_usage_events")
	_, _ = db.Exec("DROP TABLE IF EXISTS tenant_quotas")

	quotaMgr := usage.NewQuotaManager(db)
	require.NoError(t, quotaMgr.InitializeSchema(context.Background()))

	// Create test tenant with some usage
	require.NoError(t, quotaMgr.CreateTenant(context.Background(), "tenant-123", "starter", 1073741824)) // 1GB
	_, err = quotaMgr.CheckAndReserve(context.Background(), "tenant-123", 524288000)                     // 500MB used
	require.NoError(t, err)

	server := &Server{
		quotaManager: quotaMgr,
	}

	return server, db
}

func TestUsageAPI_GetUsageStats(t *testing.T) {
	server, db := setupTestUsageAPI(t)
	defer func() { _ = db.Close() }()

	req := httptest.NewRequest("GET", "/api/v1/usage/stats", nil)
	req = req.WithContext(context.WithValue(req.Context(), tenantIDKey, "tenant-123"))
	w := httptest.NewRecorder()

	server.handleGetUsageStats(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	var response UsageStats
	err := json.NewDecoder(w.Body).Decode(&response)
	require.NoError(t, err)

	assert.Equal(t, int64(524288000), response.StorageUsed)
	assert.Equal(t, int64(1073741824), response.StorageLimit)
	// Fix: 524288000 / 1073741824 * 100 = 48.828125%
	assert.InDelta(t, 48.828125, response.UsagePercent, 0.001)
}

func TestUsageAPI_GetUsageAlerts(t *testing.T) {
	server, db := setupTestUsageAPI(t)
	defer func() { _ = db.Close() }()

	// Fix: To get >90%, we need more than 966367641 bytes (90% of 1GB)
	// Let's use 970000000 bytes total (about 90.3%)
	_, err := server.quotaManager.(*usage.QuotaManager).CheckAndReserve(
		context.Background(), "tenant-123", 445712000) // 524288000 + 445712000 = 970000000
	require.NoError(t, err)

	req := httptest.NewRequest("GET", "/api/v1/usage/alerts", nil)
	req = req.WithContext(context.WithValue(req.Context(), tenantIDKey, "tenant-123"))
	w := httptest.NewRecorder()

	server.handleGetUsageAlerts(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	var alerts []UsageAlert
	err = json.NewDecoder(w.Body).Decode(&alerts)
	require.NoError(t, err)

	assert.Len(t, alerts, 1)
	assert.Equal(t, "CRITICAL", alerts[0].Level)
	assert.Contains(t, alerts[0].Message, "90%")
}
